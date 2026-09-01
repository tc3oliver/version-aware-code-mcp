// Package resolver turns a context ID into the version scope a tool works in.
//
// Version correctness is checked in two places, because there are two different
// claims to check.
//
// [Resolver.Resolve] verifies the declared revision is *available*, for every
// repository the context names: it exists in that repository and names a
// commit. Every member is checked and one failure fails the whole context,
// because a workspace resolved down to the repositories that happened to be
// readable is an answer scoped to less than it claims. It deliberately does not require the
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
	"maps"
	"os/exec"
	"slices"
	"strings"

	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// Resolver answers context IDs from a loaded configuration.
type Resolver struct {
	contexts     map[string]vacctx.Workspace
	repositories map[string]config.Repository
}

// New returns a Resolver serving the contexts of cfg.
func New(cfg *config.Config) *Resolver {
	return &Resolver{contexts: cfg.Contexts, repositories: cfg.Repositories}
}

// Contexts returns every configured context, sorted by ID so repeated calls
// answer identically.
//
// It reports what is configured, not what currently resolves: unlike
// [Resolver.Resolve] it reads no repository, because listing the versions a
// caller may ask for is the answer to a different question than whether one of
// them is serviceable right now. Nothing here can fail, and the error is
// returned anyway because the interface this satisfies has one: a source that
// cannot report a failure can only report an empty list instead, which is the
// silent fallback this server does not have.
func (r *Resolver) Contexts(context.Context) ([]vacctx.Workspace, error) {
	// Not a nil slice: to a caller putting this on a wire, null and [] are
	// different answers, and "there are none" is [].
	listed := make([]vacctx.Workspace, 0, len(r.contexts))
	for _, id := range slices.Sorted(maps.Keys(r.contexts)) {
		listed = append(listed, vacctx.Workspace{ID: id, Members: membersOf(r.contexts[id], id)})
	}
	return listed, nil
}

// membersOf returns workspace's members filed under id, on a slice of their own.
//
// config.Load fills the IDs in from the key; a hand-built Config may not have,
// so the key is the source of truth — a member without an ID cannot be put on
// the wire by evidence. The copy is what keeps that from writing back into the
// configuration the Resolver was built over: a caller handed the stored slice
// could find the file's own contexts rewritten under it.
func membersOf(workspace vacctx.Workspace, id string) []vacctx.CodeContext {
	members := make([]vacctx.CodeContext, 0, len(workspace.Members))
	for _, member := range workspace.Members {
		member.ID = id
		members = append(members, member)
	}
	return members
}

// Resolve returns the [vacctx.Workspace] named by id, once every one of its
// members has a readable repository and a revision that resolves to a commit
// there.
//
// Every failure is a *[vacerr.Error]: an unknown ID is
// [vacerr.ContextNotFound], a repository that cannot be read is
// [vacerr.RepositoryNotFound], and a revision this repository does not have is
// [vacerr.RevisionNotFound]. On any of them the returned workspace is the zero
// value: nothing usable escapes a failed check.
//
// One member failing fails the whole workspace, and there is deliberately no
// partial answer. A workspace is the scope of one question, so a half-resolved
// one would answer that question over the repositories that happened to be
// readable and say nothing about the rest — an answer scoped to less than it
// claims, which is the same failure as answering in the wrong version.
//
// A resolved context says the version exists, not that any particular working
// tree is on it. A caller serving content from a checkout must additionally
// call [VerifyWorktree].
func (r *Resolver) Resolve(ctx context.Context, id string) (vacctx.Workspace, error) {
	workspace, ok := r.contexts[id]
	if !ok {
		// No fuzzy matching, no "the only configured context", no default. An
		// unconfigured ID is an error by design: guessing here would answer
		// from a version the caller never asked for.
		return vacctx.Workspace{}, vacerr.New(
			vacerr.ContextNotFound,
			fmt.Sprintf("context %q is not configured", id),
			map[string]any{"context": id},
		)
	}
	members := membersOf(workspace, id)

	for _, member := range members {
		repo, ok := r.repositories[member.Repository]
		if !ok {
			return vacctx.Workspace{}, vacerr.New(
				vacerr.RepositoryNotFound,
				fmt.Sprintf("context %q references repository %q, which is not configured", id, member.Repository),
				map[string]any{"context": id, "repository": member.Repository},
			)
		}

		if _, err := revParse(ctx, repo.Path, member.Revision); err != nil {
			// One failure, two causes worth telling apart: a path that is not a
			// usable repository, and a repository that simply does not have this
			// revision. Only the second one is the user's context being wrong.
			if _, repoErr := gitDir(ctx, repo.Path); repoErr != nil {
				return vacctx.Workspace{}, vacerr.New(
					vacerr.RepositoryNotFound,
					fmt.Sprintf("context %q: cannot read repository %q at %s: %v", id, member.Repository, repo.Path, repoErr),
					map[string]any{"context": id, "repository": member.Repository, "path": repo.Path},
				)
			}
			return vacctx.Workspace{}, vacerr.New(
				vacerr.RevisionNotFound,
				fmt.Sprintf("context %q: repository %q has no revision %q: %v", id, member.Repository, member.Revision, err),
				map[string]any{"context": id, "repository": member.Repository, "revision": member.Revision, "path": repo.Path},
			)
		}
	}

	return vacctx.Workspace{ID: id, Members: members}, nil
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
