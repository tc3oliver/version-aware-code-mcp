// Package git reads source code out of a local git repository at the revision a
// context declares.
//
// It reads the object database — `git show <commit>:<path>` — and never the
// working tree. That is the whole design:
//
//   - The content is correct by construction. There is no window in which the
//     bytes returned belong to another version, because the commit they are
//     read from is the one the context named.
//   - One clone serves every context pointing at it, at the same time. A
//     working tree has a single HEAD, so an adapter reading the checkout could
//     serve only one of two contexts over one repository, and doc-1's first
//     v0.1.0 success criterion — two versions of a repository coexisting —
//     would be unreachable.
//
// [resolver.VerifyWorktree] is therefore not on the normal path: there is no
// checkout being served for it to check. It is reached in the one case where
// the object database cannot produce the revision's bytes at all, where the
// only source left on the machine is the working tree. The adapter does not
// serve those bytes; it asks the resolver whether the tree is even on the
// declared revision and hands back that verdict, fail closed.
package git

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"strings"
	"time"

	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/internal/deadline"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/resolver"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// Provider is the git implementation of [provider.SourceProvider].
type Provider struct {
	repositories map[string]config.Repository

	// This provider's own budgets. Per-Provider rather than package-level so
	// that an embedder whose repository is bigger than the defaults assume can
	// raise them, and so that the tests do it the same way. See the defaults
	// below.
	readBudget    time.Duration
	diffBudget    time.Duration
	historyBudget time.Duration
}

// Option adjusts a Provider at construction. See [WithReadBudget],
// [WithDiffBudget] and [WithHistoryBudget].
type Option func(*Provider)

// WithReadBudget sets how long one Read may take. d must be greater than zero;
// zero is not unlimited and panics, as does a negative duration.
func WithReadBudget(d time.Duration) Option {
	return func(p *Provider) { p.readBudget = deadline.Positive("git.WithReadBudget", d) }
}

// WithDiffBudget sets how long one Diff may take. d must be greater than zero;
// zero is not unlimited and panics, as does a negative duration.
func WithDiffBudget(d time.Duration) Option {
	return func(p *Provider) { p.diffBudget = deadline.Positive("git.WithDiffBudget", d) }
}

// WithHistoryBudget sets how long one SearchHistory may take. d must be greater
// than zero; zero is not unlimited and panics, as does a negative duration.
//
// It is the one most likely to need raising: `git log -S` is a pickaxe over
// every commit in range, and a history large enough to outrun two minutes is a
// real repository asking a real question, not a wedged git.
func WithHistoryBudget(d time.Duration) Option {
	return func(p *Provider) { p.historyBudget = deadline.Positive("git.WithHistoryBudget", d) }
}

// New returns a Provider serving the repositories declared in cfg. A context
// names its repository by the key it is filed under there, not by a path, so
// the adapter owns that lookup.
//
// With no options the budgets are the defaults below, which is what every
// caller before they existed gets.
func New(cfg *config.Config, opts ...Option) *Provider {
	p := &Provider{
		repositories:  cfg.Repositories,
		readBudget:    defaultReadBudget,
		diffBudget:    defaultDiffBudget,
		historyBudget: defaultHistoryBudget,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Read returns lines [start, end] of filePath as they are at the revision
// codeCtx declares. Revision on the result is the full commit SHA the content
// was read from, so the caller can check the claim rather than trust it.
//
// Every failure is a *[vacerr.Error]. filePath and the line range come from a
// tool caller, so an illegal one is [vacerr.InvalidArgument] rather than a panic
// or a silently clamped range that would misreport which lines were read. A
// repository that is not on this machine is [vacerr.RepositoryNotFound], a
// revision it does not have is [vacerr.RevisionNotFound], and a repository that
// cannot produce the declared revision's content is [vacerr.SourceMismatch].
// The exceptions are the caller's own two clocks: a cancellation comes back as
// [context.Canceled] and an expired caller deadline as
// [context.DeadlineExceeded], unwrapped, while this adapter's own budget
// expiring is [vacerr.OperationTimeout].
func (p *Provider) Read(ctx context.Context, codeCtx vacctx.CodeContext, filePath string, start, end int) (*provider.SourceContent, error) {
	cleanPath, err := validatePath(filePath)
	if err != nil {
		return nil, err
	}
	if start < 1 || end < start {
		return nil, invalid(
			fmt.Sprintf("get_code: line range [%d, %d] is not a range of 1-based lines", start, end),
			map[string]any{"path": cleanPath, "start_line": start, "end_line": end},
		)
	}

	repo, ok := p.repositories[codeCtx.Repository]
	if !ok {
		return nil, vacerr.New(
			vacerr.RepositoryNotFound,
			fmt.Sprintf("context %q references repository %q, which is not configured", codeCtx.ID, codeCtx.Repository),
			map[string]any{"context": codeCtx.ID, "repository": codeCtx.Repository},
		)
	}

	// The budget covers resolving and reading together, because they are one
	// answer to the caller: a read that pinned its revision and then hung is not
	// half-finished, it is a call that did not come back.
	ctx, cancel := deadline.With(ctx, p.readBudget, deadline.Git, "read")
	defer cancel()

	revision, err := p.resolve(ctx, codeCtx, repo.Path)
	if err != nil {
		return nil, err
	}

	// The commit is pinned to its full SHA before reading, so the read cannot
	// follow a ref that moved between resolving and reading. That also makes
	// the argument start with a hex digit, which is why it needs no protection
	// against being read as a flag.
	blob, err := gitOutput(ctx, repo.Path, "show", revision+":"+cleanPath)
	if err != nil {
		// Whose deadline it was is asked first: a budget that expired, or a
		// caller that stopped waiting, is not a repository that cannot be read.
		if ended := deadline.Ended(ctx); ended != nil {
			return nil, ended
		}
		return nil, p.readFailure(ctx, codeCtx, repo.Path, cleanPath, revision, err)
	}

	lines := splitLines(blob)
	if end > len(lines) {
		// Clamping here would return fewer lines than the range says were read,
		// and the caller cites that range as evidence. Refusing is the honest
		// answer, and the line count tells the caller what to ask for instead.
		return nil, invalid(
			fmt.Sprintf("get_code: %s has %d lines at revision %s, line range [%d, %d] is out of range", cleanPath, len(lines), revision, start, end),
			map[string]any{"path": cleanPath, "start_line": start, "end_line": end, "line_count": len(lines), "revision": revision},
		)
	}

	return &provider.SourceContent{
		Path:      cleanPath,
		StartLine: start,
		EndLine:   end,
		Content:   strings.Join(lines[start-1:end], ""),
		Revision:  revision,
	}, nil
}

// resolve turns the revision codeCtx declares — a full SHA, a short one, a tag
// or a branch name — into the full SHA of the commit it names, telling apart a
// path that is not a usable repository from a repository that does not have
// this revision.
//
// It classifies its own failures against the budget it was handed, which is the
// rule everything below the three entry points follows: whoever ran the git
// command answers for the clock, so a caller does not have to remember to ask
// again after every helper that might have run one.
func (p *Provider) resolve(ctx context.Context, codeCtx vacctx.CodeContext, repoPath string) (string, error) {
	revision, err := gitLine(ctx, repoPath, "rev-parse", "--verify", "--end-of-options", codeCtx.Revision+"^{commit}")
	if err == nil {
		return revision, nil
	}
	// The probe is a second git command, so it is a second one that can hang,
	// and the verdict below would then be a claim about a repository nothing
	// ever read. deadline.Override is what keeps the clock ahead of the verdict.
	if _, repoErr := gitLine(ctx, repoPath, "rev-parse", "--git-dir"); repoErr != nil {
		return "", deadline.Override(ctx, vacerr.New(
			vacerr.RepositoryNotFound,
			fmt.Sprintf("context %q: cannot read repository %q at %s: %v", codeCtx.ID, codeCtx.Repository, repoPath, repoErr),
			map[string]any{"context": codeCtx.ID, "repository": codeCtx.Repository, "path": repoPath},
		))
	}
	return "", deadline.Override(ctx, vacerr.New(
		vacerr.RevisionNotFound,
		fmt.Sprintf("context %q: repository %q has no revision %q: %v", codeCtx.ID, codeCtx.Repository, codeCtx.Revision, err),
		map[string]any{"context": codeCtx.ID, "repository": codeCtx.Repository, "revision": codeCtx.Revision, "path": repoPath},
	))
}

// readFailure classifies a failed read of a commit that exists. ls-tree answers
// from the commit's tree, which stays readable when the file's blob is not, so
// it separates the caller asking for a path that revision never had from the
// object database being unable to produce content the revision does record.
func (p *Provider) readFailure(ctx context.Context, codeCtx vacctx.CodeContext, repoPath, filePath, revision string, cause error) error {
	entry, err := gitLine(ctx, repoPath, "ls-tree", "--name-only", revision, "--", filePath)
	if err != nil || entry == "" {
		// An ls-tree that did not come back says nothing about the tree, and
		// "this revision has no such file" is the worst thing to say about a
		// path when the truth is that nobody looked: the caller would go and
		// correct a path that was right all along.
		return deadline.Override(ctx, invalid(
			fmt.Sprintf("get_code: revision %s has no file %s", revision, filePath),
			map[string]any{"context": codeCtx.ID, "path": filePath, "revision": revision},
		))
	}

	// The revision records this file but the object database cannot hand it
	// over: a partial clone whose blobs were never fetched, a pruned object
	// store. The only bytes for this path left on the machine are the working
	// tree's, and those belong to whatever HEAD is on. Serving them is the
	// cross-version answer this server exists to prevent, so the tree goes to
	// the resolver's fail-closed check and its verdict is returned unchanged.
	if err := resolver.VerifyWorktree(ctx, repoPath, codeCtx); err != nil {
		// VerifyWorktree honours the budget it was handed, so a check that ran
		// out of time already arrives as one; Override is what covers this
		// function's last verdict below, and costs nothing here.
		return deadline.Override(ctx, err)
	}
	return deadline.Override(ctx, vacerr.New(
		vacerr.RepositoryNotFound,
		fmt.Sprintf("context %q: repository %q cannot read %s at revision %s: %v", codeCtx.ID, codeCtx.Repository, filePath, revision, cause),
		map[string]any{"context": codeCtx.ID, "repository": codeCtx.Repository, "path": filePath, "revision": revision},
	))
}

// validatePath rejects everything that is not a plain path inside the
// repository. The caller is across a trust boundary, so an absolute path or one
// climbing out with ".." is refused here rather than left for git to interpret.
func validatePath(filePath string) (string, error) {
	if strings.TrimSpace(filePath) == "" {
		return "", invalid("get_code: path is required", nil)
	}
	cleaned := path.Clean(filePath)
	if path.IsAbs(cleaned) {
		return "", invalid(
			fmt.Sprintf("get_code: path %q must be relative to the repository root", filePath),
			map[string]any{"path": filePath},
		)
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", invalid(
			fmt.Sprintf("get_code: path %q escapes the repository root", filePath),
			map[string]any{"path": filePath},
		)
	}
	return cleaned, nil
}

// splitLines splits content into lines that keep their terminator, so joining a
// range back together reproduces those bytes exactly. A file ending in a
// newline does not get a phantom empty last line.
func splitLines(content string) []string {
	lines := strings.SplitAfter(content, "\n")
	if last := len(lines) - 1; last >= 0 && lines[last] == "" {
		lines = lines[:last]
	}
	return lines
}

// The budgets this adapter sets for itself. They are wedged-process guards, not
// performance targets: a git that never returns has to stop being waited on, and
// a git that is merely slow because the repository is large must not be killed.
//
// Neither number comes from the fixture. Measured against it on a developer
// machine, a read is 9ms and a diff 17ms, which says only that nothing normal is
// anywhere near these — a real monorepo's `git show` of a large blob is orders of
// magnitude more, and the point of the budget is the process that has stopped
// making progress at all. They follow the guards this project already sets: a
// Zoekt request at 30s, a doctor probe at 30s, a CBM session start at 2 minutes.
//
// history is the outlier and deliberately so. `git log -S` is a pickaxe over
// every commit in range, and on a large history that is minutes of real work for
// a question the caller genuinely asked; defaultHistoryLimit bounds the result,
// not the walk. Killing it at 30s would fail the query this adapter exists to
// answer.
//
// They are defaults rather than fixed limits. Nothing reads them after [New]
// has run: each Provider carries its own three, and [WithReadBudget],
// [WithDiffBudget] and [WithHistoryBudget] replace them for one Provider. A
// caller's own context deadline can only ever shorten an operation, so before
// the options existed an embedder whose `git log -S` needed longer than
// historyBudget had no way to say so. The tests take that same path, so the
// path an embedder uses is the one CI exercises.
const (
	defaultReadBudget    = 30 * time.Second
	defaultDiffBudget    = 30 * time.Second
	defaultHistoryBudget = 2 * time.Minute
)

// gitOutput runs one git command in the repository at repoPath and returns its
// standard output verbatim, which is what reading file content needs. Any
// stderr is folded into the error so the reason reaches the error details.
func gitOutput(ctx context.Context, repoPath string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", repoPath}, args...)...).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exit.Stderr)))
		}
		return "", err
	}
	return string(out), nil
}

// gitLine is gitOutput for the commands whose output is a single value to
// compare or pass on, rather than file content.
func gitLine(ctx context.Context, repoPath string, args ...string) (string, error) {
	out, err := gitOutput(ctx, repoPath, args...)
	return strings.TrimSpace(out), err
}

func invalid(message string, details map[string]any) *vacerr.Error {
	return vacerr.New(vacerr.InvalidArgument, message, details)
}
