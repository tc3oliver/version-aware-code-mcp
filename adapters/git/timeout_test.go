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
		budget    func(time.Duration) Option
		call      func(context.Context, *Provider) error
	}{
		{
			name: "read", operation: "read", budget: WithReadBudget,
			call: func(ctx context.Context, p *Provider) error {
				_, err := p.Read(ctx, timeoutContext, "a.go", 1, 2)
				return err
			},
		},
		{
			name: "diff", operation: "diff", budget: WithDiffBudget,
			call: func(ctx context.Context, p *Provider) error {
				_, err := p.Diff(ctx, timeoutContext, timeoutContext, provider.SourceDiffRequest{Path: "a.go"})
				return err
			},
		},
		{
			name: "history", operation: "history", budget: WithHistoryBudget,
			call: func(ctx context.Context, p *Provider) error {
				_, err := p.SearchHistory(ctx, timeoutContext, provider.HistoryQuery{})
				return err
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			pidFile := hangingGit(t)

			err := testCase.call(context.Background(), timeoutProvider(t, testCase.budget(stubBudget)))

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
		p := timeoutProvider(t, WithReadBudget(time.Hour))

		ctx, cancel := context.WithTimeout(context.Background(), stubBudget)
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

// stubBudget is what every budget in this file is set to, and every caller
// deadline set to.
//
// It has to outlast the operating system starting the stub, not the stub doing
// anything: a shell killed before it reaches its first command records no pid,
// and the test is then unable to tell a subprocess that was reaped from one that
// never ran. Under `-race`, with every package's tests running at once, that
// startup has been seen to take longer than two seconds on an otherwise idle
// developer machine — which is a fact about process startup under load, not
// about the code, so the margin is generous rather than tuned.
const stubBudget = 5 * time.Second

var timeoutContext = vacctx.CodeContext{
	ID: "timeout", Repository: "demo", Branch: "main",
	Revision: "7777777777777777777777777777777777777777", GraphRef: "demo-main",
}

// timeoutProvider builds the provider under test, with whatever budgets the
// test needs passed as options.
//
// The options are the point, not a convenience: waiting out a real budget would
// make the suite take minutes, and the only way to shorten one is the exact
// call an embedder makes. Nothing here reaches into the package's state, so
// there is no test-only path left to rot.
func timeoutProvider(t *testing.T, opts ...Option) *Provider {
	t.Helper()
	return New(&config.Config{Repositories: map[string]config.Repository{
		"demo": {Path: t.TempDir()},
	}}, opts...)
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

// The second git is the one nobody watches.
//
// Several paths here answer a failed git command by running another one to find
// out why it failed: a rev-parse that failed is classified by asking for the
// git directory, a `git show` that failed by asking the tree whether the path
// is there, an empty diff by asking whether it ever was. The tests above hang
// the *first* command, which every one of those paths notices. These hang the
// second one, which is the case that used to answer with the diagnostic's own
// verdict — REPOSITORY_NOT_FOUND for a repository nobody read, "revision has no
// file" about a tree nobody listed — for a call whose real problem was that
// this server stopped waiting.
func TestATimeoutInTheDiagnosticIsStillATimeout(t *testing.T) {
	// Each stub answers the first command straight away and parks on the
	// classifying one. The pid of the parked git is recorded so the test can
	// also require it to be reaped.
	cases := []struct {
		name      string
		operation string
		budget    func(time.Duration) Option
		script    string
		call      func(context.Context, *Provider) error
		// what the diagnostic would have concluded, had it been allowed to
		wrong vacerr.Code
	}{
		{
			name: "the read's ls-tree hangs", operation: "read", budget: WithReadBudget,
			script: `
				"rev-parse --verify"*) echo ` + stubRevision + `; exit 0 ;;
				"ls-tree"*) hang ;;
			`,
			call: func(ctx context.Context, p *Provider) error {
				_, err := p.Read(ctx, timeoutContext, "a.go", 1, 2)
				return err
			},
			wrong: vacerr.InvalidArgument,
		},
		{
			name: "the read's worktree check hangs", operation: "read", budget: WithReadBudget,
			// The path IS in the tree, so the read goes on to the fail-closed
			// worktree check — and that check runs two more gits of its own.
			script: `
				"rev-parse --verify"*) once || hang; echo ` + stubRevision + `; exit 0 ;;
				"ls-tree"*) echo a.go; exit 0 ;;
			`,
			call: func(ctx context.Context, p *Provider) error {
				_, err := p.Read(ctx, timeoutContext, "a.go", 1, 2)
				return err
			},
			wrong: vacerr.RepositoryNotFound,
		},
		{
			name: "the empty diff's ls-tree hangs", operation: "diff", budget: WithDiffBudget,
			// git prints nothing for two identical revisions and for a path
			// neither of them has; ls-tree is what tells those apart.
			script: `
				"rev-parse --verify"*) echo ` + stubRevision + `; exit 0 ;;
				"diff"*) exit 0 ;;
				"ls-tree"*) hang ;;
			`,
			call: func(ctx context.Context, p *Provider) error {
				_, err := p.Diff(ctx, timeoutContext, timeoutContext, provider.SourceDiffRequest{Path: "a.go"})
				return err
			},
			wrong: vacerr.InvalidArgument,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			pidFile := scriptedGit(t, testCase.script)

			err := testCase.call(context.Background(), timeoutProvider(t, testCase.budget(stubBudget)))

			var vErr *vacerr.Error
			if !errors.As(err, &vErr) {
				t.Fatalf("%s = %v, want a *vacerr.Error", testCase.name, err)
			}
			if vErr.Code == testCase.wrong {
				t.Fatalf("a diagnostic that ran out of time was reported as %s, the verdict it never reached", vErr.Code)
			}
			if vErr.Code != vacerr.OperationTimeout {
				t.Fatalf("code = %s, want %s", vErr.Code, vacerr.OperationTimeout)
			}
			if vErr.Details["provider"] != "git" || vErr.Details["operation"] != testCase.operation {
				t.Errorf("details = %v, want provider git and operation %s", vErr.Details, testCase.operation)
			}
			assertChildReaped(t, pidFile)
		})
	}
}

// stubRevision is what the scripted git answers a rev-parse with: a full SHA,
// because that is what the adapter pins a read to and passes on.
const stubRevision = "4444444444444444444444444444444444444444"

// scriptedGit puts a git on PATH that answers by rule. body is the inside of a
// `case` over the command as the adapter spells it, with the leading `-C <dir>`
// dropped — the repository is a t.TempDir(), whose path carries the test's own
// name, and a pattern matched against that would match the name of the command
// it is looking for in every invocation. `hang` records the pid and parks for
// ever, and `once` is true the first time it is called and false afterwards,
// for telling two runs of the same command apart. Anything the rules do not
// match fails the way git fails an unusable repository.
func scriptedGit(t *testing.T, body string) string {
	t.Helper()

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "git.pid")
	script := fmt.Sprintf(`#!/bin/sh
shift 2 # the -C and the repository path
hang() { echo $$ >>%[1]q; exec sleep 300; }
once() {
	n=$(cat %[2]q 2>/dev/null || echo 0)
	echo $((n + 1)) >%[2]q
	[ "$n" = 0 ]
}
case "$*" in
%[3]s
esac
echo 'not a git repository' >&2
exit 128
`, pidFile, filepath.Join(dir, "count"), body)
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700); err != nil {
		t.Fatalf("write the git stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return pidFile
}
