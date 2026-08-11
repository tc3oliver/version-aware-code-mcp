// Package provider defines the three contracts a backend implements: search,
// graph and source. Tools are written against these interfaces, so Zoekt, CBM
// and git stay behind adapters and a different engine — SCIP, another graph
// service, something proprietary — can be dropped in without forking anything.
//
// Every method takes both a standard library [context.Context] for
// cancellation and a [vacctx.CodeContext] for version scope. The second one is
// not optional context: a provider that ignores it answers from the wrong
// version, which is the failure this whole server exists to prevent.
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
