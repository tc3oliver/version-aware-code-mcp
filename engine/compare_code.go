package engine

import (
	"context"
	"fmt"

	"github.com/tc3oliver/version-aware-code-mcp/evidence"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// CompareCodeRequest is one file in two version contexts, asked for so the two
// can be set against each other.
//
// It names its scope with two context IDs and nothing else, for the reason every
// request here does: the repository and the revision of each side come from the
// configuration those IDs name, so a caller cannot compare a version it was not
// granted. The path is asked of both sides identically — there is deliberately
// no way to read one file on the from side and another on the to side, because a
// difference produced by asking about two different files is not a difference
// between the two versions.
type CompareCodeRequest struct {
	FromContext string
	ToContext   string
	Path        string
}

// CompareCodeResult is what happened to one file between two versions, with each
// version's half of the answer kept as its own [ComparisonSide].
//
// It has no exported fields, like every other result here, so the only values
// carrying a change and a set of hunks are the ones [Engine.CompareCode] built.
// It deliberately offers no Context or Evidence of its own: a comparison is
// answered in two versions, and one merged pair of those would be a
// cross-version answer wearing the clothes of a version-scoped one.
type CompareCodeResult struct {
	from   ComparisonSide
	to     ComparisonSide
	path   string
	change CodeChange
	hunks  []provider.DiffHunk
	binary bool
}

// From reports the from version's half of the comparison: the version the file
// was compared at. It reports Present false when that version did not have the
// file at all, which is the absence [CodeAdded] is the answer to.
func (r CompareCodeResult) From() ComparisonSide { return r.from }

// To reports the to version's half, the mirror of [CompareCodeResult.From]: it
// reports Present false when the file is gone by that version, which is
// [CodeRemoved].
func (r CompareCodeResult) To() ComparisonSide { return r.to }

// Path reports the file that was compared, as the provider resolved it. It is
// what the line numbers in [CompareCodeResult.Hunks] are line numbers of.
func (r CompareCodeResult) Path() string { return r.path }

// Change reports the one-word answer: whether the two versions hold the same
// file, different files, or only one of them holds it at all.
func (r CompareCodeResult) Change() CodeChange { return r.change }

// Hunks reports the changed regions in unified diff terms, each carrying the
// lines it covers in both versions. On a result built here it is non-nil and
// empty when there is nothing to show line by line, so "compared, nothing to
// show" is told apart from the zero result nobody built. An unchanged file and a
// binary one both have no hunks, and [CompareCodeResult.Binary] is what tells
// those two apart.
func (r CompareCodeResult) Hunks() []provider.DiffHunk { return r.hunks }

// Binary reports whether the compared file is binary, which is why a modified
// one can still have nothing to show line by line.
func (r CompareCodeResult) Binary() bool { return r.binary }

// codeChange translates the provider's classification into this package's.
//
// It is written out rather than converted between the two string types. They are
// declared independently in two packages and happen to spell their values the
// same way today; a conversion would keep compiling if one of them were renamed,
// and hand a caller a [CodeChange] this package never declared.
func codeChange(change provider.DiffChange) (CodeChange, bool) {
	switch change {
	case provider.ChangeUnchanged:
		return CodeUnchanged, true
	case provider.ChangeModified:
		return CodeModified, true
	case provider.ChangeAdded:
		return CodeAdded, true
	case provider.ChangeRemoved:
		return CodeRemoved, true
	default:
		return "", false
	}
}

// CompareCode compares req.Path between the two versions and reports how the
// file differs.
//
// Diffing is an optional capability rather than part of
// [provider.SourceProvider]: not every source backend has a second revision to
// compare against, so the configured provider is type asserted for
// [provider.SourceDiffer] and one without it fails with
// [vacerr.SourceDiffUnavailable] rather than being asked a question it cannot
// answer. What the diff says — added, removed, modified, unchanged, and the
// hunks behind it — is then the backend's answer, translated here and not
// second-guessed.
//
// Both contexts must name the same repository. Two repositories have no shared
// history, so a diff across them is not a comparison that means anything: it is
// [vacerr.InvalidArgument], refused here before any provider is reached rather
// than left to whichever backend would happen to notice.
//
// A file only one version has is an answer rather than a failure — that is what
// [CodeAdded] and [CodeRemoved] are for — and the version without it is the
// absent [ComparisonSide], which cites nothing because it had nothing.
func (e *Engine) CompareCode(ctx context.Context, req CompareCodeRequest) (CompareCodeResult, error) {
	// Both sides before either provider call, and in the order they were asked
	// for: an ID naming no configured version has no file to read, and a
	// comparison that read one side first would report the other's failure after
	// doing work in a version the caller may not have meant.
	fromCtx, err := e.resolveMember(ctx, req.FromContext, "")
	if err != nil {
		return CompareCodeResult{}, err
	}
	toCtx, err := e.resolveMember(ctx, req.ToContext, "")
	if err != nil {
		return CompareCodeResult{}, err
	}

	if fromCtx.Repository != toCtx.Repository {
		return CompareCodeResult{}, vacerr.New(
			vacerr.InvalidArgument,
			fmt.Sprintf("compare_code: contexts %q and %q name different repositories (%q and %q), which have no shared history to compare",
				req.FromContext, req.ToContext, fromCtx.Repository, toCtx.Repository),
			map[string]any{
				"from_context":    req.FromContext,
				"to_context":      req.ToContext,
				"from_repository": fromCtx.Repository,
				"to_repository":   toCtx.Repository,
			},
		)
	}

	// The compromise [Engine.GetCode] documents, for the same reason: the code
	// set has no source equivalent of the two provider-unavailable codes, and
	// with no source provider there is no repository this server can read,
	// whatever the contexts declare. SOURCE_DIFF_UNAVAILABLE is deliberately not
	// this case — it says the source this server does have cannot diff, which is
	// a different fact and the next check's to report.
	if e.source == nil {
		return CompareCodeResult{}, vacerr.New(
			vacerr.RepositoryNotFound,
			"compare_code: this server was built with no source provider, so no repository can be read",
			map[string]any{"from_context": req.FromContext, "to_context": req.ToContext, "path": req.Path},
		)
	}

	// A capability a backend does not have is a type assertion that fails, which
	// is the whole point of [provider.SourceDiffer] being its own interface: the
	// caller is told this server cannot compare code here, rather than handed an
	// apology shaped like an answer.
	differ, ok := e.source.(provider.SourceDiffer)
	if !ok {
		return CompareCodeResult{}, vacerr.New(
			vacerr.SourceDiffUnavailable,
			"compare_code: this server's source provider reads one version at a time and cannot compare two",
			map[string]any{"from_context": req.FromContext, "to_context": req.ToContext, "path": req.Path},
		)
	}

	diff, err := differ.Diff(ctx, fromCtx, toCtx, provider.SourceDiffRequest{Path: req.Path})
	if err != nil {
		return CompareCodeResult{}, err
	}

	change, ok := codeChange(diff.Change)
	if !ok {
		return CompareCodeResult{}, vacerr.New(
			vacerr.SourceDiffUnavailable,
			fmt.Sprintf("compare_code: the source provider classified %q as %q, which is not one of the four outcomes this server reports", diff.Path, diff.Change),
			map[string]any{
				"from_context": req.FromContext,
				"to_context":   req.ToContext,
				"path":         req.Path,
				"change":       string(diff.Change),
			},
		)
	}

	result := CompareCodeResult{
		path:   diff.Path,
		change: change,
		// Built non-nil so a comparison with nothing to show is still an answer:
		// an unchanged file has an empty hunk list, not no hunk list.
		hunks:  append([]provider.DiffHunk{}, diff.Hunks...),
		binary: diff.Binary,
	}

	// The version that does not have the file stays the zero ComparisonSide,
	// which reports Present false and cites nothing — the absence is the answer
	// rather than a gap in it. A present side cites an empty list instead: a
	// whole file's diff has no one line to point at, and "present with nothing
	// worth citing" is not the same as "absent".
	if change != CodeAdded {
		result.from = ComparisonSide{inOneMember(fromCtx, []evidence.Evidence{})}
	}
	if change != CodeRemoved {
		result.to = ComparisonSide{inOneMember(toCtx, []evidence.Evidence{})}
	}
	return result, nil
}
