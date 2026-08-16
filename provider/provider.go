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
