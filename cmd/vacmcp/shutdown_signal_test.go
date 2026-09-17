//go:build unix

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tc3oliver/version-aware-code-mcp/managed"
	"github.com/tc3oliver/version-aware-code-mcp/store"
)

// A signal has to end a run the way a client disconnecting already does: by
// making serve return, so the defers it registered get to run. Before this it
// did not — the process died where it stood, and in managed mode that left the
// server lock file behind, which is what every management command tests before
// it will do anything.
//
// Unix only, for the reason lock_server_test.go is: the lock between two
// processes is the file lock, and SIGTERM is the signal a supervisor sends.

// TestSIGTERMReleasesTheManagedServerLock is the one that was actually broken.
// It watches the lock through the management plane rather than by taking it,
// because the server holds it exclusively and a test that asked for it the same
// way would block instead of reporting.
func TestSIGTERMReleasesTheManagedServerLock(t *testing.T) {
	data := t.TempDir()

	child := serveManagedChild(t, data, nil)

	waitUntil(t, "the managed server takes its lock", func() bool {
		return managementIsRefused(t, data)
	})

	if err := child.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}

	state, err := child.Process.Wait()
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if state.ExitCode() != 0 {
		t.Errorf("the server exited %d on SIGTERM, want 0 — it should stop, not be killed", state.ExitCode())
	}

	// The lock is a file lock, so the kernel drops it when the process goes
	// whatever way it went. What this asserts is therefore not that the fd was
	// closed but that the management plane is usable again, which is the thing
	// an operator cares about and the thing that stayed broken when serve was
	// killed mid-run.
	if managementIsRefused(t, data) {
		t.Fatal("the management plane is still refusing commands after the server was signalled")
	}
}

// TestSIGTERMClosesTheEngine is the other defer: the CBM session serve opened
// has to be closed on the way out.
//
// The stub CBM writes a marker when its session closes, and serveAsChild exits
// non-zero if serve returned without that marker being there — so the exit
// status is the assertion, exactly as it is for the disconnect tests. The trace
// first is what makes there be a session at all; without it this would pass
// having closed nothing.
func TestSIGTERMClosesTheEngine(t *testing.T) {
	data := t.TempDir()
	source := sourceRepo(t)
	if _, err := repoRun(t, data, "add", "demo", "--url", source); err != nil {
		t.Fatalf("repo add: %v", err)
	}
	revision := gitOut(t, "-C", source, "rev-parse", "HEAD")
	if err := openStore(t, data).PutContext(store.Context{
		ID: "app",
		Members: []store.ContextMember{{
			Repository: "demo",
			Branch:     "vacmcp/app-" + revision[:12],
			Revision:   revision,
			GraphRef:   "vacmcp-demo-app-" + revision[:12],
		}},
		State: managed.ContextReady,
	}); err != nil {
		t.Fatalf("PutContext: %v", err)
	}

	marker := markerPath(t)
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		serveManagedEnv+"="+data,
		serveManagedCBMEnv+"="+os.Args[0],
		cbmStubEnv+"="+marker,
	)
	cmd.Stderr = os.Stderr

	client := mcp.NewClient(&mcp.Implementation{Name: "vacmcp-test", Version: version}, nil)
	session, err := client.Connect(t.Context(), &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	// Opens the CBM session the shutdown then has to close. The stub's empty
	// graph makes this SYMBOL_NOT_FOUND, and an answer arriving is how this
	// test knows a session exists to be closed.
	if _, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "trace_calls",
		Arguments: map[string]any{"context": "app", "symbol": "nothing", "direction": "callees", "depth": 1},
	}); err != nil {
		t.Fatalf("tools/call trace_calls: %v", err)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}

	// Close waits for the process and reports what it exited with. A serve that
	// returned on the signal without closing the engine arrives here as a
	// non-zero exit, because serveAsChild checks the marker before exiting.
	if err := session.Close(); err != nil {
		t.Errorf("session.Close = %v, want serve to have closed the CBM session on SIGTERM", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the CBM session marker is missing after SIGTERM: %v", err)
	}
}

// TestSIGTERMWritesNothingToTheProtocolStream guards the rule STDIO mode is
// built on: stdout carries JSON-RPC and nothing else. A shutdown that reported
// itself there — a goodbye line, an error, a flush of anything — would corrupt
// the stream for a client still reading it.
//
// Nothing is asked of this server, so every byte on stdout would be the
// shutdown's own.
func TestSIGTERMWritesNothingToTheProtocolStream(t *testing.T) {
	data := t.TempDir()

	var out bytes.Buffer
	child := serveManagedChild(t, data, &out)

	waitUntil(t, "the managed server takes its lock", func() bool {
		return managementIsRefused(t, data)
	})

	if err := child.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	if err := child.Wait(); err != nil {
		t.Fatalf("the server exited badly on SIGTERM: %v", err)
	}

	if out.Len() != 0 {
		t.Errorf("shutdown wrote %q to stdout, want the protocol stream left untouched", out.String())
	}
}

// TestASecondSIGTERMIsNotSwallowed covers the half of the signal handling that
// exists for the case where the first one is not enough: once the context is
// done, SIGINT and SIGTERM go back to their default disposition, so a second
// one ends the process rather than being absorbed by a handler that has already
// done its job.
//
// What is asserted here is that the second signal is safe and the process still
// exits — not that it interrupted a drain, which would need a server wedged
// mid-request to observe and is not something to fake.
func TestASecondSIGTERMIsNotSwallowed(t *testing.T) {
	data := t.TempDir()

	child := serveManagedChild(t, data, nil)

	waitUntil(t, "the managed server takes its lock", func() bool {
		return managementIsRefused(t, data)
	})

	for range 2 {
		if err := child.Process.Signal(syscall.SIGTERM); err != nil {
			// The process is already gone, which is the outcome this is about.
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the server did not exit after two SIGTERMs")
	}

	if managementIsRefused(t, data) {
		t.Fatal("the management plane is still refusing commands after the server was signalled twice")
	}
}

// serveManagedChild starts `vacmcp serve --stdio --managed` on data as a real
// subprocess, with stdout going to out when one is given.
//
// The stdin pipe is the point of this helper and is deliberately never written
// to or closed: these tests drive the server with a signal rather than with a
// client, and a STDIO server whose stdin is closed stops at EOF — which it did,
// before the lock these tests watch for was ever taken.
func serveManagedChild(t *testing.T, data string, out *bytes.Buffer) *exec.Cmd {
	t.Helper()

	child := exec.Command(os.Args[0])
	child.Env = append(os.Environ(), serveManagedEnv+"="+data)
	child.Stderr = os.Stderr
	if out != nil {
		child.Stdout = out
	}
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	if err := child.Start(); err != nil {
		t.Fatalf("start the managed server: %v", err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = stdin.Close()
	})
	return child
}

// managementIsRefused reports whether a managed server holds the lock on data,
// asked the way a user would find out: by running a command that is refused
// while one does. The command names a context that does not exist, because the
// refusal happens before any record is read.
func managementIsRefused(t *testing.T, data string) bool {
	t.Helper()

	err := run([]string{"context", "remove", "no-such-context", "--data-dir", data}, os.Stderr)
	return err != nil && strings.Contains(err.Error(), "managed server is running")
}

func waitUntil(t *testing.T, what string, done func() bool) {
	t.Helper()

	for range 600 {
		if done() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// markerPath is the file the stub CBM writes when its session closes.
func markerPath(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%s/cbm-session-closed", t.TempDir())
}
