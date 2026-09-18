//go:build unix

package managed

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tc3oliver/version-aware-code-mcp/store"
)

// An operator pressing Ctrl-C has to reach the process that is actually taking
// the time. Before the CLI passed a cancellable context these managers were
// handed context.Background(), so exec.CommandContext had nothing to cancel on:
// the terminal came back and `git clone` carried on behind it.
//
// Every external binary here is a stub on PATH that parks, because the real ones
// finish. The stub `exec`s its sleep, so the process that gets killed is the one
// whose pid it recorded — a shell left holding a sleeping grandchild would prove
// something weaker than it looks.
//
// Unix only, like the rest of this package's process-level tests: the kill and
// the file lock are both the platform's.

// TestCancellingRepositoryAddStopsGit is the plainest case: `repo add` clones,
// the clone is a git subprocess, and cancelling has to end both the call and the
// process.
func TestCancellingRepositoryAddStopsGit(t *testing.T) {
	pidFile := stubOnPath(t, "git")
	data := t.TempDir()

	repositories, err := NewRepositoryManager(data)
	if err != nil {
		t.Fatalf("NewRepositoryManager: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, addErr := repositories.Add(ctx, "demo", "https://example.invalid/demo.git")
		done <- addErr
	}()

	pid := waitForStub(t, pidFile)
	cancel()

	_ = assertCancelled(t, "repo add", done)
	assertProcessGone(t, "git", pid)
	assertRepositoryLockFree(t, data, "demo")
}

// TestCancellingRepositorySyncStopsGit is the same for the fetch, which is the
// long one in practice: a sync of a large repository is minutes of network.
func TestCancellingRepositorySyncStopsGit(t *testing.T) {
	data := t.TempDir()
	seedRepositoryRecord(t, data, "demo")

	pidFile := stubOnPath(t, "git")
	repositories, err := NewRepositoryManager(data)
	if err != nil {
		t.Fatalf("NewRepositoryManager: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, syncErr := repositories.Sync(ctx, []string{"demo"})
		done <- syncErr
	}()

	pid := waitForStub(t, pidFile)
	cancel()

	_ = assertCancelled(t, "repo sync", done)
	assertProcessGone(t, "git", pid)
	assertRepositoryLockFree(t, data, "demo")

	// And the record is as it was. An interrupted fetch is not a broken
	// repository: marking it FAILED would outlive the Ctrl-C that caused it, and
	// `repo sync --all` stopped at the first repository would have gone on to
	// mark every later one the same way against an already-dead context.
	s, err := store.Open(data)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	record, err := s.Repository("demo")
	if err != nil {
		t.Fatalf("Repository: %v", err)
	}
	if record.State != RepositoryReady {
		t.Errorf("demo is %s after a cancelled sync, want it left %s", record.State, RepositoryReady)
	}
}

// TestCancellingContextRemoveStopsTheGraphEngine reaches the other family of
// subprocess: codebase-memory-mcp, which is what `context remove` asks to delete
// the graph.
//
// It matters twice over. The CBM calls are the ones that used to classify a
// cancellation as GRAPH_PROVIDER_UNAVAILABLE — an operator's Ctrl-C reported as a
// fault in a graph engine that was working perfectly.
func TestCancellingContextRemoveStopsTheGraphEngine(t *testing.T) {
	data := t.TempDir()
	seedRepositoryRecord(t, data, "demo")
	seedContextRecord(t, data, "app", "demo")

	pidFile := stubOnPath(t, CBMCommand)
	contexts, err := NewContextManager(data)
	if err != nil {
		t.Fatalf("NewContextManager: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- contexts.Remove(ctx, "app") }()

	pid := waitForStub(t, pidFile)
	cancel()

	err = assertCancelled(t, "context remove", done)
	// The specific regression: not a provider-unavailable report.
	if strings.Contains(err.Error(), "GRAPH_PROVIDER_UNAVAILABLE") ||
		strings.Contains(err.Error(), "codebase-memory-mcp") {
		t.Errorf("a cancelled context remove failed with %v, want the cancellation itself rather than a fault report about the graph engine", err)
	}
	assertProcessGone(t, CBMCommand, pid)
	assertRepositoryLockFree(t, data, "demo")
}

// TestAnUncancelledCommandIsUnaffected is the other half: nothing above may have
// turned an ordinary failure into a cancellation, or started reporting one when
// the caller never cancelled.
func TestAnUncancelledCommandIsUnaffected(t *testing.T) {
	data := t.TempDir()
	repositories, err := NewRepositoryManager(data)
	if err != nil {
		t.Fatalf("NewRepositoryManager: %v", err)
	}

	// A real git, a URL that cannot be cloned: a genuine failure, on a context
	// that is alive throughout.
	_, err = repositories.Add(context.Background(), "demo", "https://example.invalid/demo.git")
	if err == nil {
		t.Fatal("repo add of an unreachable URL succeeded, want the clone to fail")
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a clone that failed on its own reported %v, want the git failure rather than a cancellation", err)
	}
	if !strings.Contains(err.Error(), "git") {
		t.Errorf("repo add failed with %v, want the git error it actually got", err)
	}
	// And the lock is released on the failure path too.
	assertRepositoryLockFree(t, data, "demo")
}

// assertCancelled requires the call to have come back, promptly, reporting the
// caller's own cancellation.
func assertCancelled(t *testing.T, what string, done <-chan error) error {
	t.Helper()

	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("%s returned no error after being cancelled", what)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("%s failed with %v, want it to report context.Canceled", what, err)
		}
		return err
	case <-time.After(30 * time.Second):
		t.Fatalf("%s did not return after its context was cancelled", what)
		return nil
	}
}

// assertProcessGone requires the subprocess to have been killed rather than left
// running behind a call that already returned.
func assertProcessGone(t *testing.T, name string, pid int) {
	t.Helper()

	for range 300 {
		// Signal 0 tests for the process without touching it.
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("the %s subprocess (pid %d) was still running after the call it belonged to was cancelled", name, pid)
}

// assertRepositoryLockFree requires the per-repository lock to be available
// again, which is what a later command needs and what a cancelled call that
// skipped its deferred release would deny it.
func assertRepositoryLockFree(t *testing.T, data, repository string) {
	t.Helper()

	s, err := store.Open(data)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	// Taken the way the managers take it. withRepositoryLock blocks until the
	// lock is free, so the timeout below is the assertion: a cancelled call that
	// skipped its deferred release leaves this waiting for ever.
	taken := make(chan error, 1)
	go func() {
		taken <- withRepositoryLock(s, repository, func() error { return nil })
	}()

	select {
	case err := <-taken:
		if err != nil {
			t.Fatalf("the repository lock could not be taken after the cancellation: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the repository lock is still held after the cancellation, so a later command would block on it for ever")
	}
}

// stubOnPath puts a binary called name at the front of PATH which records its own
// pid and then parks. It returns the file the pid is written to.
//
// `exec sleep` rather than `sleep &`: the recorded pid has to be the process
// exec.CommandContext will kill, or this would assert that a shell died while its
// sleeping child carried on.
func stubOnPath(t *testing.T, name string) string {
	t.Helper()

	dir := t.TempDir()
	pidFile := filepath.Join(dir, name+".pid")
	script := fmt.Sprintf("#!/bin/sh\necho $$ >%q\nexec sleep 300\n", pidFile)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil {
		t.Fatalf("write the %s stub: %v", name, err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return pidFile
}

// waitForStub blocks until the stub has recorded its pid, so the cancellation
// lands while the subprocess is really running rather than before it started.
func waitForStub(t *testing.T, pidFile string) int {
	t.Helper()

	for range 600 {
		raw, err := os.ReadFile(pidFile)
		if err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw))); convErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the stub at %s never started", pidFile)
	return 0
}

func seedRepositoryRecord(t *testing.T, data, name string) {
	t.Helper()

	s, err := store.Open(data)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	clone, err := s.RepositoryDir(name)
	if err != nil {
		t.Fatalf("RepositoryDir: %v", err)
	}
	if err := os.MkdirAll(clone, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := s.PutRepository(store.Repository{
		Name:  name,
		URL:   "https://example.invalid/" + name + ".git",
		State: RepositoryReady,
	}); err != nil {
		t.Fatalf("PutRepository: %v", err)
	}
}

func seedContextRecord(t *testing.T, data, id, repository string) {
	t.Helper()

	s, err := store.Open(data)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := s.PutContext(store.Context{
		ID: id,
		Members: []store.ContextMember{{
			Repository: repository,
			Branch:     "vacmcp/" + id,
			Revision:   strings.Repeat("a", 40),
			GraphRef:   "vacmcp-" + repository + "-" + id,
		}},
		State: ContextReady,
	}); err != nil {
		t.Fatalf("PutContext: %v", err)
	}
}
