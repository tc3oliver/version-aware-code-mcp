// Package resolver turns a context ID into the version scope a tool works in.
//
// Version correctness is checked in two places, because there are two different
// claims to check.
//
// [Resolver.Resolve] verifies the declared revision is *available*: it exists in
// that repository and names a commit. It deliberately does not require the
// repository to be checked out at it. Git serves any commit in the object
// database — `git show <rev>:<path>` needs no checkout — and one repository
// holds several versions at once, which is what lets two contexts over one
// clone (see config/example.yaml) both resolve. Requiring a matching HEAD would
// make the first of doc-1's success criteria, two versions of one repository
// coexisting, impossible to satisfy.
//
// [VerifyWorktree] verifies the working tree is *on* the declared revision.
// That is the fail-closed check, [vacerr.SourceMismatch], and it is what a
// provider must call when it can only serve the checkout rather than an
// arbitrary revision. It reports a mismatch as an error and nothing else: there
// is no warning-level variant and no way to proceed past it.
//
// Neither path guesses. An unconfigured context ID is an error, never a fuzzy
// match and never a default.
package resolver

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// Resolver answers context IDs from a loaded configuration.
type Resolver struct {
	contexts     map[string]vacctx.CodeContext
	repositories map[string]config.Repository
}

// New returns a Resolver serving the contexts of cfg.
func New(cfg *config.Config) *Resolver {
	return &Resolver{contexts: cfg.Contexts, repositories: cfg.Repositories}
}

// Resolve returns the [vacctx.CodeContext] named by id, once its repository is
// readable and its revision resolves to a commit there.
//
// Every failure is a *[vacerr.Error]: an unknown ID is
// [vacerr.ContextNotFound], a repository that cannot be read is
// [vacerr.RepositoryNotFound], and a revision this repository does not have is
// [vacerr.RevisionNotFound]. On any of them the returned context is the zero
// value: nothing usable escapes a failed check.
//
// A resolved context says the version exists, not that any particular working
// tree is on it. A caller serving content from a checkout must additionally
// call [VerifyWorktree].
func (r *Resolver) Resolve(ctx context.Context, id string) (vacctx.CodeContext, error) {
	codeCtx, ok := r.contexts[id]
	if !ok {
		// No fuzzy matching, no "the only configured context", no default. An
		// unconfigured ID is an error by design: guessing here would answer
		// from a version the caller never asked for.
		return vacctx.CodeContext{}, vacerr.New(
			vacerr.ContextNotFound,
			fmt.Sprintf("context %q is not configured", id),
			map[string]any{"context": id},
		)
	}
	// config.Load fills this in from the key; a hand-built Config may not have.
	// A context without an ID cannot be put on the wire by evidence.
	codeCtx.ID = id

	repo, ok := r.repositories[codeCtx.Repository]
	if !ok {
		return vacctx.CodeContext{}, vacerr.New(
			vacerr.RepositoryNotFound,
			fmt.Sprintf("context %q references repository %q, which is not configured", id, codeCtx.Repository),
			map[string]any{"context": id, "repository": codeCtx.Repository},
		)
	}

	if _, err := revParse(ctx, repo.Path, codeCtx.Revision); err != nil {
		// One failure, two causes worth telling apart: a path that is not a
		// usable repository, and a repository that simply does not have this
		// revision. Only the second one is the user's context being wrong.
		if _, repoErr := gitDir(ctx, repo.Path); repoErr != nil {
			return vacctx.CodeContext{}, vacerr.New(
				vacerr.RepositoryNotFound,
				fmt.Sprintf("context %q: cannot read repository %q at %s: %v", id, codeCtx.Repository, repo.Path, repoErr),
				map[string]any{"context": id, "repository": codeCtx.Repository, "path": repo.Path},
			)
		}
		return vacctx.CodeContext{}, vacerr.New(
			vacerr.RevisionNotFound,
			fmt.Sprintf("context %q: repository %q has no revision %q: %v", id, codeCtx.Repository, codeCtx.Revision, err),
			map[string]any{"context": id, "repository": codeCtx.Repository, "revision": codeCtx.Revision, "path": repo.Path},
		)
	}

	return codeCtx, nil
}

// VerifyWorktree reports whether the working tree at repoPath is on the
// revision codeCtx declares, and returns a fail-closed [vacerr.SourceMismatch]
// error when it is not.
//
// A provider that reads the checkout — rather than reading an arbitrary
// revision out of the object database, which is what the git source adapter
// does — MUST call this before serving any content, and MUST NOT serve content
// when it returns an error. A mismatch means the bytes on disk belong to a
// different version than the caller asked for; returning them anyway, or
// reporting the mismatch as a warning and continuing, is the exact failure this
// server exists to prevent.
//
// The error is a *[vacerr.Error]: [vacerr.SourceMismatch] when the tree is on
// another commit, [vacerr.RevisionNotFound] or [vacerr.RepositoryNotFound] when
// the comparison could not be made at all. A check that could not be carried
// out is not a check that passed.
func VerifyWorktree(ctx context.Context, repoPath string, codeCtx vacctx.CodeContext) error {
	// Both sides go through rev-parse so a context declaring a short SHA, a tag
	// or a branch name is compared as the commit it names. Comparing the raw
	// strings would report a mismatch that is not one.
	declared, err := revParse(ctx, repoPath, codeCtx.Revision)
	if err != nil {
		if _, repoErr := gitDir(ctx, repoPath); repoErr != nil {
			return vacerr.New(
				vacerr.RepositoryNotFound,
				fmt.Sprintf("context %q: cannot read repository at %s: %v", codeCtx.ID, repoPath, repoErr),
				map[string]any{"context": codeCtx.ID, "path": repoPath},
			)
		}
		return vacerr.New(
			vacerr.RevisionNotFound,
			fmt.Sprintf("context %q: repository at %s has no revision %q: %v", codeCtx.ID, repoPath, codeCtx.Revision, err),
			map[string]any{"context": codeCtx.ID, "revision": codeCtx.Revision, "path": repoPath},
		)
	}

	actual, err := revParse(ctx, repoPath, "HEAD")
	if err != nil {
		return vacerr.New(
			vacerr.RepositoryNotFound,
			fmt.Sprintf("context %q: cannot read HEAD of the repository at %s: %v", codeCtx.ID, repoPath, err),
			map[string]any{"context": codeCtx.ID, "path": repoPath},
		)
	}

	if declared != actual {
		return vacerr.NewSourceMismatch(declared, actual, map[string]any{
			"context":      codeCtx.ID,
			"repository":   codeCtx.Repository,
			"path":         repoPath,
			"declared_ref": codeCtx.Revision,
		})
	}
	return nil
}

// revParse resolves rev to the full SHA of the commit it names in the git
// repository at repoPath. --end-of-options keeps a revision string that starts
// with a dash from being read as a flag, and ^{commit} rejects anything that
// resolves to a non-commit.
func revParse(ctx context.Context, repoPath, rev string) (string, error) {
	return git(ctx, repoPath, "rev-parse", "--verify", "--end-of-options", rev+"^{commit}")
}

// gitDir reports whether repoPath is a usable git repository, by asking git for
// its git directory. Unlike revParse it succeeds on a repository with no
// commits yet.
func gitDir(ctx context.Context, repoPath string) (string, error) {
	return git(ctx, repoPath, "rev-parse", "--git-dir")
}

func git(ctx context.Context, repoPath string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", repoPath}, args...)...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
