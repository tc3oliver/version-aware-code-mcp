// Package resolver turns a context ID into the version scope a tool works in.
//
// It is the one place where "which version is this?" is answered, and the one
// place where the answer is checked. Resolving is not a lookup: a context is
// only handed out after the repository on disk has been confirmed to be at the
// revision the context declares. When it is not, the resolver returns
// [vacerr.SourceMismatch] and stops. That is the project's central trust
// boundary — answering from the wrong version is the exact failure this server
// exists to prevent — so there is deliberately no way to resolve without
// verifying, no warning-level variant and no fallback to a default context.
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

// Resolve returns the verified [vacctx.CodeContext] named by id.
//
// Every failure is a *[vacerr.Error]: an unknown ID is
// [vacerr.ContextNotFound], a repository that cannot be read is
// [vacerr.RepositoryNotFound], a revision git does not know is
// [vacerr.RevisionNotFound], and a repository sitting on a different commit
// than the context declares is [vacerr.SourceMismatch]. On any of them the
// returned context is the zero value: nothing usable escapes a failed
// verification.
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

	actual, err := revParse(ctx, repo.Path, "HEAD")
	if err != nil {
		return vacctx.CodeContext{}, vacerr.New(
			vacerr.RepositoryNotFound,
			fmt.Sprintf("context %q: cannot read repository %q at %s: %v", id, codeCtx.Repository, repo.Path, err),
			map[string]any{"context": id, "repository": codeCtx.Repository, "path": repo.Path},
		)
	}
	// Both sides go through rev-parse so a context declaring a short SHA or a
	// tag is compared as the commit it names, not as the string it is written
	// as. Comparing the raw strings would report a mismatch that is not one.
	declared, err := revParse(ctx, repo.Path, codeCtx.Revision)
	if err != nil {
		return vacctx.CodeContext{}, vacerr.New(
			vacerr.RevisionNotFound,
			fmt.Sprintf("context %q: repository %q has no revision %q: %v", id, codeCtx.Repository, codeCtx.Revision, err),
			map[string]any{"context": id, "repository": codeCtx.Repository, "revision": codeCtx.Revision},
		)
	}

	if declared != actual {
		return vacctx.CodeContext{}, vacerr.NewSourceMismatch(declared, actual, map[string]any{
			"context":      id,
			"repository":   codeCtx.Repository,
			"path":         repo.Path,
			"declared_ref": codeCtx.Revision,
		})
	}

	return codeCtx, nil
}

// revParse resolves rev to the full SHA of the commit it names in the git
// repository at repoPath. --end-of-options keeps a revision string that starts
// with a dash from being read as a flag, and ^{commit} rejects anything that
// resolves to a non-commit.
func revParse(ctx context.Context, repoPath, rev string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", repoPath, "rev-parse", "--verify", "--end-of-options", rev+"^{commit}").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
