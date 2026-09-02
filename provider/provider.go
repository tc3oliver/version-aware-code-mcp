// Package provider defines the three contracts a backend implements: search,
// graph and source. Tools are written against these interfaces, so Zoekt, CBM
// and git stay behind adapters and a different engine — SCIP, another graph
// service, something proprietary — can be dropped in without forking anything.
//
// Every method takes both a standard library [context.Context] for
// cancellation and a [vacctx.CodeContext] for version scope. The second one is
// not optional context: a provider that ignores it answers from the wrong
// version, which is the failure this whole server exists to prevent.
//
// Anything beyond those three is an optional capability: a separate interface an
// adapter may also implement, found by type asserting the backend for it. A
// capability a backend does not have is then a missing interface the caller can
// see rather than a contract method that promised an answer and returns "not
// supported".
package provider

import (
	"context"

	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
)

// SearchProvider searches code. Implementations must confine every match to the
// repository and branch of codeCtx.
type SearchProvider interface {
	Search(ctx context.Context, codeCtx vacctx.CodeContext, query SearchQuery) ([]SearchResult, error)
}

// SearchQuery is what the caller is looking for. The repository and branch to
// look in are not in here: they come from the context, so a query cannot widen
// its own scope.
type SearchQuery struct {
	Query string
}

// SearchResult is one match, located in the source of the context's revision.
type SearchResult struct {
	Path    string
	Line    int
	Snippet string
}

// GraphProvider answers structural questions about code. Implementations must
// query the graph named by the context's GraphRef and no other.
type GraphProvider interface {
	TraceCalls(ctx context.Context, codeCtx vacctx.CodeContext, req TraceRequest) (*CallGraph, error)
}

// Direction is which way TraceCalls walks the call graph.
type Direction string

const (
	// Callers walks towards the functions that call the symbol.
	Callers Direction = "callers"
	// Callees walks towards the functions the symbol calls.
	Callees Direction = "callees"
)

// TraceRequest asks for the call graph around one symbol, up to Depth levels
// away from it.
type TraceRequest struct {
	Symbol    string
	Direction Direction
	Depth     int
}

// CallGraph is the traversal result. Symbol is the symbol the provider actually
// resolved the request to, which is not always the string that was asked for.
type CallGraph struct {
	Symbol string
	Edges  []CallEdge
}

// CallEdge is one call relation. It carries where the call is written, because
// a result that cannot be cited is not an answer this server may return.
type CallEdge struct {
	Caller string
	Callee string
	Path   string
	Line   int
}

// SourceProvider reads source code.
//
// Implementations must fail closed: if the content available is not the
// revision the context declares, return a vacerr SOURCE_MISMATCH error and stop.
// Returning content from another revision, or downgrading the mismatch to a
// warning, is never allowed.
type SourceProvider interface {
	Read(ctx context.Context, codeCtx vacctx.CodeContext, path string, start, end int) (*SourceContent, error)
}

// SourceContent is a slice of one file. Revision is the revision the content was
// actually read at, so the caller can check the claim rather than trust it.
type SourceContent struct {
	Path      string
	StartLine int
	EndLine   int
	Content   string
	Revision  string
}

// SourceDiffer is the optional capability of comparing one path across two
// version contexts. It is separate from [SourceProvider] because not every
// source backend has a second revision to compare against, and a backend that
// cannot diff should be a type assertion that fails rather than a Read-shaped
// promise that returns an apology.
//
// The two contexts are the entire scope of the comparison, exactly as the single
// one is for [SourceProvider.Read]: an implementation compares the revisions
// they declare and nothing else, so a diff cannot quietly answer about a third
// version nobody asked for.
type SourceDiffer interface {
	Diff(ctx context.Context, from, to vacctx.CodeContext, req SourceDiffRequest) (*SourceDiff, error)
}

// SourceDiffRequest is the file to compare. The revisions are not in here: they
// come from the two contexts, so a request cannot widen its own scope.
type SourceDiffRequest struct {
	Path string
}

// SourceDiff is what happened to one path between the two revisions. Path is the
// path that was asked about.
//
// Hunks is a structured diff, not diff text: the caller decides how to render
// it, and can read line numbers out of it without parsing anything. It is empty
// when there is nothing to show line by line, which happens for an unchanged
// file and for a binary one — Binary tells those apart, because "nothing
// changed" and "the change is not text" are different answers.
type SourceDiff struct {
	Path   string
	Change DiffChange
	Binary bool
	Hunks  []DiffHunk
}

// DiffChange is what happened to the compared path between the two revisions.
type DiffChange string

const (
	// ChangeAdded: only the to revision has the path.
	ChangeAdded DiffChange = "ADDED"
	// ChangeRemoved: only the from revision has the path, the mirror of
	// [ChangeAdded].
	ChangeRemoved DiffChange = "REMOVED"
	// ChangeModified: both revisions have the path and the content differs.
	ChangeModified DiffChange = "MODIFIED"
	// ChangeUnchanged: both revisions have the path and the content is
	// identical. Not "no diff was computed": the comparison ran and found
	// nothing, which is an answer.
	ChangeUnchanged DiffChange = "UNCHANGED"
)

// DiffHunk is one changed region in unified diff terms: it begins at line
// OldStart of the from revision and spans OldLines of it, and at line NewStart
// of the to revision spanning NewLines. Both starts are 1-based, like every
// other line number this server reports, so a hunk can be cited as it stands.
//
// A side that contributes nothing to the hunk has a zero count, and git writes
// the start of an empty side as the line it would follow rather than as a line
// that exists.
type DiffHunk struct {
	OldStart int
	OldLines int
	NewStart int
	NewLines int
	Lines    []DiffLine
}

// DiffLine is one line of a hunk. Content is the line's own text: the diff's
// marker character is stripped, and so is the line terminator, because a line's
// membership of a revision is what Kind already says and a terminator would only
// be a second thing to get wrong.
type DiffLine struct {
	Kind    DiffLineKind
	Content string
}

// DiffLineKind is which revisions a hunk line belongs to.
type DiffLineKind string

const (
	// LineContext: the line is in both revisions, carried along to locate the
	// change.
	LineContext DiffLineKind = "CONTEXT"
	// LineAdded: the line is only in the to revision.
	LineAdded DiffLineKind = "ADDED"
	// LineRemoved: the line is only in the from revision.
	LineRemoved DiffLineKind = "REMOVED"
)

// HistoryProvider is the optional capability of reading a repository's commit
// history. It is separate from [SourceProvider] for the reason [SourceDiffer]
// is: a source backend that can read a revision's bytes cannot necessarily walk
// its history, and a caller should be told so by a type assertion that fails
// rather than by an empty answer that looks like "this version has no history".
//
// Like every provider here it is handed ONE [vacctx.CodeContext] per call, and
// that context's Revision is the whole scope of the walk: history is read as of
// the commit the context pins, never as of the checkout or the default branch.
// Walking from HEAD would report commits that are not in the version being
// asked about, which is the wrong-version answer this server exists to prevent.
type HistoryProvider interface {
	SearchHistory(ctx context.Context, codeCtx vacctx.CodeContext, req HistoryQuery) ([]HistoryEntry, error)
}

// HistoryQuery is a search of one version's commit history. Every field is
// optional, and the ones that are set are combined with AND: a commit has to
// satisfy all of them to be reported. Nothing is relaxed when a filter matches
// nothing — an empty result is an answer ("no commit in this version matches"),
// not a reason to widen the search and answer a question nobody asked.
type HistoryQuery struct {
	// Query is a free-text search of the commit message, subject and body. It is
	// a literal, case-insensitive substring test, deterministic and repeatable —
	// there is no ranking, embedding or relevance model anywhere in it.
	Query string
	// Symbol is a git pickaxe search: it selects the commits where the number of
	// occurrences of this exact string changed.
	//
	// This is STRING history, not symbol resolution. It does not parse the
	// language, does not know that two spellings name one function, and does not
	// follow a rename.
	//
	// It is git's -S, the count test, and not -G, the touched-line test: a commit
	// that edits the line a symbol is declared on WITHOUT adding or removing an
	// occurrence of it — reformatting it, or changing a comment beside it — does
	// not change the count and is not reported. -S answers "where did this name
	// come and go", which is the question a history search is for; -G answers
	// "which commits touched a line mentioning it", which matches far more
	// commits and is not the same question.
	Symbol string
	// Path restricts the walk to commits that touched this path, and the reported
	// entries to that path. It is relative to the repository root.
	Path string
	// Limit caps how many entries are returned, applied after ordering so the cap
	// is reproducible. 0 means the provider's own default bound; a negative value
	// is refused rather than treated as unbounded.
	Limit int
}

// HistoryEntry is ONE COMMIT-PATH OCCURRENCE: a commit together with one of the
// paths it changed.
//
// A commit that touches several paths therefore produces several entries, one
// per path, rather than one entry naming an arbitrary "the" path. Picking one
// file out of a multi-file commit would silently drop the provenance of the
// rest, and which file got picked would depend on the order git happened to
// print them.
type HistoryEntry struct {
	// Commit is the full immutable commit id. It is never a branch, a tag, HEAD
	// or an abbreviation: an answer has to keep naming the same commit later.
	Commit string
	// Path is the one path of this commit that this entry reports.
	Path string
	// Author is the commit's author name and email.
	Author string
	// Timestamp is the author date in RFC3339, so two runs of the same query on
	// the same repository produce the same bytes regardless of the reader's
	// locale or timezone settings.
	Timestamp string
	// Message is the commit message, subject and body, as git stored it.
	Message string
}
