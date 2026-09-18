//go:build unix

package resolver

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

	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// Resolution is the step every tool takes, so a git that never returns here
// hangs all seven rather than one. It is also the step most likely to be blamed
// for something it did not do: without a budget, a wedged rev-parse eventually
// surfaced as REVISION_NOT_FOUND or REPOSITORY_NOT_FOUND — a claim about the
// caller's context when the truth was that git stopped answering.

func TestResolveBudgetProducesOperationTimeout(t *testing.T) {
	hangingGit(t)
	shrinkResolveBudget(t, stubBudget)

	_, err := resolverFor(t).Resolve(context.Background(), "app")

	var vErr *vacerr.Error
	if !errors.As(err, &vErr) {
		t.Fatalf("Resolve = %v, want a *vacerr.Error", err)
	}
	if vErr.Code != vacerr.OperationTimeout {
		t.Fatalf("a git that never answers = %s, want %s", vErr.Code, vacerr.OperationTimeout)
	}
	for key, want := range map[string]any{"provider": "git", "operation": "resolve"} {
		if vErr.Details[key] != want {
			t.Errorf("details[%s] = %v, want %v", key, vErr.Details[key], want)
		}
	}
}

func TestResolveReportsTheCallersOwnClock(t *testing.T) {
	t.Run("caller cancelled", func(t *testing.T) {
		hangingGit(t)
		shrinkResolveBudget(t, time.Hour)

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(700 * time.Millisecond)
			cancel()
		}()

		_, err := resolverFor(t).Resolve(ctx, "app")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Resolve = %v, want context.Canceled", err)
		}
		assertUnclassified(t, err)
	})

	t.Run("caller deadline expired", func(t *testing.T) {
		hangingGit(t)
		shrinkResolveBudget(t, time.Hour)

		ctx, cancel := context.WithTimeout(context.Background(), stubBudget)
		defer cancel()

		_, err := resolverFor(t).Resolve(ctx, "app")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Resolve = %v, want context.DeadlineExceeded", err)
		}
		assertUnclassified(t, err)
	})
}

// TestAnUnconfiguredContextIsStillNotFound: resolution's own refusals happen
// before any git runs and are unaffected by the budget.
func TestAnUnconfiguredContextIsStillNotFound(t *testing.T) {
	_, err := resolverFor(t).Resolve(context.Background(), "no-such-context")

	var vErr *vacerr.Error
	if !errors.As(err, &vErr) || vErr.Code != vacerr.ContextNotFound {
		t.Fatalf("Resolve of an unconfigured context = %v, want %s", err, vacerr.ContextNotFound)
	}
}

// stubBudget is what the budget is shrunk to here, and what a caller deadline
// is set to: long enough that starting the stub is never what the test races
// against. See the git adapter's copy for the whole reason.
const stubBudget = 5 * time.Second

func resolverFor(t *testing.T) *Resolver {
	t.Helper()

	return New(&config.Config{
		Repositories: map[string]config.Repository{"demo": {Path: t.TempDir()}},
		Contexts: map[string]vacctx.Workspace{
			"app": {ID: "app", Members: []vacctx.CodeContext{{
				Repository: "demo", Branch: "main",
				Revision: "1212121212121212121212121212121212121212",
				GraphRef: "demo-main",
			}}},
		},
	})
}

func hangingGit(t *testing.T) {
	t.Helper()

	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\necho $$ >>%q\nexec sleep 300\n", filepath.Join(dir, "git.pid"))
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700); err != nil {
		t.Fatalf("write the git stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func shrinkResolveBudget(t *testing.T, d time.Duration) {
	t.Helper()
	previous := resolveBudget
	resolveBudget = d
	t.Cleanup(func() { resolveBudget = previous })
}

func assertUnclassified(t *testing.T, err error) {
	t.Helper()

	var vErr *vacerr.Error
	if errors.As(err, &vErr) {
		t.Errorf("the caller's own context error was classified as %s, want it propagated unchanged", vErr.Code)
	}
}

// TestATimeoutInTheDiagnosticIsStillATimeout hangs the *second* git rather than
// the first. A rev-parse that fails is not yet an answer: it can mean the path
// is not a repository or that the repository has no such revision, and asking
// git for the git directory is what tells those apart. That question can itself
// go unanswered, and when it does, the repository has not been found missing —
// it has not been looked at. REPOSITORY_NOT_FOUND would send the caller to
// check a path that is fine.
func TestATimeoutInTheDiagnosticIsStillATimeout(t *testing.T) {
	pidFile := scriptedGit(t, `
		"rev-parse --git-dir"*) hang ;;
	`)
	shrinkResolveBudget(t, stubBudget)

	_, err := resolverFor(t).Resolve(context.Background(), "app")

	var vErr *vacerr.Error
	if !errors.As(err, &vErr) {
		t.Fatalf("Resolve = %v, want a *vacerr.Error", err)
	}
	if vErr.Code == vacerr.RepositoryNotFound || vErr.Code == vacerr.RevisionNotFound {
		t.Fatalf("a git-dir probe that ran out of time was reported as %s, a verdict about a repository nothing read", vErr.Code)
	}
	if vErr.Code != vacerr.OperationTimeout {
		t.Fatalf("code = %s, want %s", vErr.Code, vacerr.OperationTimeout)
	}
	assertReaped(t, pidFile)
}

// scriptedGit puts a git on PATH that answers by rule: body is the inside of a
// `case` over the command with its leading `-C <dir>` dropped, and `hang`
// records the pid and parks. The first rev-parse — the one that asks for the
// revision — is deliberately left to the catch-all below, so it fails at once
// and the classifying command is what the budget is spent on.
func scriptedGit(t *testing.T, body string) string {
	t.Helper()

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "git.pid")
	script := fmt.Sprintf(`#!/bin/sh
shift 2 # the -C and the repository path
hang() { echo $$ >>%q; exec sleep 300; }
case "$*" in
%s
esac
echo 'bad revision' >&2
exit 128
`, pidFile, body)
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700); err != nil {
		t.Fatalf("write the git stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return pidFile
}

// assertReaped requires the parked git to be gone once the call has returned. A
// budget that gave up while leaving the process behind is a leak dressed as a
// timeout.
func assertReaped(t *testing.T, pidFile string) {
	t.Helper()

	var pid int
	for range 200 {
		raw, err := os.ReadFile(pidFile)
		if err == nil {
			if pid, _ = strconv.Atoi(strings.TrimSpace(string(raw))); pid > 0 {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatalf("no git ever parked, so the diagnostic command did not run")
	}
	for range 300 {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Errorf("the git subprocess (pid %d) was still running after Resolve returned", pid)
}
