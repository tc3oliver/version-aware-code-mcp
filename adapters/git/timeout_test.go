//go:build unix

package git

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
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// The three clocks that can end a git call, told apart. A budget of this
// server's is OPERATION_TIMEOUT; a caller's cancellation and a caller's own
// deadline are returned as themselves, because neither is this server's
// decision to report.
//
// The git that never answers is a stub on PATH that records its pid and then
// `exec`s a sleep — `exec` so the pid recorded is the one that gets killed
// rather than a shell holding a sleeping child.

func TestGitBudgetsProduceOperationTimeout(t *testing.T) {
	cases := []struct {
		name      string
		operation string
		budget    *time.Duration
		call      func(context.Context, *Provider) error
	}{
		{
			name: "read", operation: "read", budget: &readBudget,
			call: func(ctx context.Context, p *Provider) error {
				_, err := p.Read(ctx, timeoutContext, "a.go", 1, 2)
				return err
			},
		},
		{
			name: "diff", operation: "diff", budget: &diffBudget,
			call: func(ctx context.Context, p *Provider) error {
				_, err := p.Diff(ctx, timeoutContext, timeoutContext, provider.SourceDiffRequest{Path: "a.go"})
				return err
			},
		},
		{
			name: "history", operation: "history", budget: &historyBudget,
			call: func(ctx context.Context, p *Provider) error {
				_, err := p.SearchHistory(ctx, timeoutContext, provider.HistoryQuery{})
				return err
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			pidFile := hangingGit(t)
			// Long enough for the stub to start and record its pid — a budget
			// tighter than shell startup kills it before it can, and then the
			// test cannot say whether the child was reaped or never ran.
			shrinkBudget(t, testCase.budget, 500*time.Millisecond)

			err := testCase.call(context.Background(), timeoutProvider(t))

			var vErr *vacerr.Error
			if !errors.As(err, &vErr) || vErr.Code != vacerr.OperationTimeout {
				t.Fatalf("%s against a git that never answers = %v, want %s",
					testCase.name, err, vacerr.OperationTimeout)
			}
			if vErr.Details["provider"] != "git" || vErr.Details["operation"] != testCase.operation {
				t.Errorf("details = %v, want provider git and operation %s", vErr.Details, testCase.operation)
			}
			assertChildReaped(t, pidFile)
		})
	}
}

// TestGitReportsTheCallersOwnClock is the boundary: neither of the caller's two
// ways of ending a call may arrive as this server's code.
func TestGitReportsTheCallersOwnClock(t *testing.T) {
	t.Run("caller cancelled", func(t *testing.T) {
		pidFile := hangingGit(t)
		p := timeoutProvider(t)

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			waitForChild(t, pidFile)
			cancel()
		}()

		_, err := p.Read(ctx, timeoutContext, "a.go", 1, 2)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Read = %v, want context.Canceled", err)
		}
		assertUnclassified(t, err)
		assertChildReaped(t, pidFile)
	})

	t.Run("caller deadline expired", func(t *testing.T) {
		pidFile := hangingGit(t)
		// The caller's deadline is the tighter one, so it is the one that fires.
		shrinkBudget(t, &readBudget, time.Hour)
		p := timeoutProvider(t)

		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()

		_, err := p.Read(ctx, timeoutContext, "a.go", 1, 2)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Read = %v, want context.DeadlineExceeded", err)
		}
		assertUnclassified(t, err)
		assertChildReaped(t, pidFile)
	})
}

// TestAGitFailureBeforeTheBudgetKeepsItsOwnCode: a repository that is genuinely
// unreadable is still REPOSITORY_NOT_FOUND, not a timeout. Nothing about adding
// budgets may reclassify an ordinary failure.
func TestAGitFailureBeforeTheBudgetKeepsItsOwnCode(t *testing.T) {
	p := New(&config.Config{Repositories: map[string]config.Repository{
		"demo": {Path: t.TempDir()}, // a directory, but not a git repository
	}})

	_, err := p.SearchHistory(context.Background(), timeoutContext, provider.HistoryQuery{})
	var vErr *vacerr.Error
	if !errors.As(err, &vErr) {
		t.Fatalf("SearchHistory = %v, want a *vacerr.Error", err)
	}
	if vErr.Code == vacerr.OperationTimeout {
		t.Fatalf("a repository that is not a repository was reported as %s", vErr.Code)
	}
	if vErr.Code != vacerr.RepositoryNotFound {
		t.Errorf("code = %s, want %s", vErr.Code, vacerr.RepositoryNotFound)
	}
}

var timeoutContext = vacctx.CodeContext{
	ID: "timeout", Repository: "demo", Branch: "main",
	Revision: "7777777777777777777777777777777777777777", GraphRef: "demo-main",
}

func timeoutProvider(t *testing.T) *Provider {
	t.Helper()
	return New(&config.Config{Repositories: map[string]config.Repository{
		"demo": {Path: t.TempDir()},
	}})
}

// shrinkBudget stands the budget at d for one test. The budgets are vars for
// exactly this: waiting out a real one would make the suite take minutes.
func shrinkBudget(t *testing.T, budget *time.Duration, d time.Duration) {
	t.Helper()
	previous := *budget
	*budget = d
	t.Cleanup(func() { *budget = previous })
}

// hangingGit puts a `git` on PATH that records its pid and parks for ever.
func hangingGit(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "git.pid")
	script := fmt.Sprintf("#!/bin/sh\necho $$ >>%q\nexec sleep 300\n", pidFile)
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700); err != nil {
		t.Fatalf("write the git stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return pidFile
}

func waitForChild(t *testing.T, pidFile string) int {
	t.Helper()

	for range 600 {
		if pids := recordedPids(pidFile); len(pids) > 0 {
			return pids[0]
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the git stub at %s never started", pidFile)
	return 0
}

func recordedPids(pidFile string) []int {
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Fields(string(raw)) {
		if pid, err := strconv.Atoi(line); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids
}

// assertChildReaped requires every git the call started to be gone. A budget
// that returned while leaving the process behind would be a leak dressed as a
// timeout.
func assertChildReaped(t *testing.T, pidFile string) {
	t.Helper()

	// Waited for rather than read once: the call returns as soon as the child is
	// killed, and the child's own write of its pid can land a moment after that.
	var pids []int
	for range 200 {
		if pids = recordedPids(pidFile); len(pids) > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(pids) == 0 {
		t.Fatalf("no git stub ever ran, so the call did not reach one")
	}
	for _, pid := range pids {
		alive := true
		for range 300 {
			if err := syscall.Kill(pid, 0); err != nil {
				alive = false
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if alive {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Errorf("the git subprocess (pid %d) was still running after the call returned", pid)
		}
	}
}

func assertUnclassified(t *testing.T, err error) {
	t.Helper()

	var vErr *vacerr.Error
	if errors.As(err, &vErr) {
		t.Errorf("the caller's own context error was classified as %s, want it propagated unchanged", vErr.Code)
	}
}
