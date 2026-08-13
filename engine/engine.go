// Package engine holds what vacmcp answers, with nothing about how it is
// asked. Resolving a context ID, checking the arguments that cross a trust
// boundary, calling the provider and reporting which version answered all live
// here; MCP, JSON-RPC and HTTP live above it and are not imported from here.
//
// The split is what makes the version guarantee testable on its own: an engine
// call needs no server, no transport and no wire schema, so a test of "this
// query ran in that version and no other" is a function call.
//
// Every request type names its scope with a context ID and nothing else. There
// is deliberately no repository, branch or revision field on any of them: the
// scope comes from the configuration the context ID names, so a caller cannot
// widen or redirect the version it is answered in.
//
// Every result type answers the same way round: it has no exported fields, so
// the only ones that exist outside this package came out of a method here, and
// each of those carries the version it was answered in and the evidence backing
// it. doc-1's Tool Contract — never a bare answer — is therefore a property of
// the types rather than a rule each method has to remember.
package engine

import (
	"context"
	"fmt"

	"github.com/tc3oliver/version-aware-code-mcp/evidence"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/resolver"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// The levels [Engine.TraceCalls] will walk. doc-1 fixes the range; a request
// outside it is refused rather than clamped, because a clamped walk answers a
// question nobody asked and looks exactly like the answer to the one they did.
const (
	minDepth = 1
	maxDepth = 5
)

// Engine answers the four queries of doc-1 §5 against one configuration.
type Engine struct {
	contexts *resolver.Resolver
	search   provider.SearchProvider
	graph    provider.GraphProvider
	source   provider.SourceProvider
}

// New returns an Engine resolving context IDs with contexts and answering
// through the three providers.
func New(contexts *resolver.Resolver, search provider.SearchProvider, graph provider.GraphProvider, source provider.SourceProvider) *Engine {
	return &Engine{contexts: contexts, search: search, graph: graph, source: source}
}

// answer is the half of a successful result that doc-1's Tool Contract fixes:
// the version the query ran in, and the citations that make the answer
// checkable at its source. It is embedded in all three result types so that
// half is written once and cannot be forgotten by one of them.
//
// Its fields are unexported and so is the type, which is what makes the
// contract structural: no code outside this package can name the field to fill
// it in, so the only results carrying a context are the ones a method here
// built.
type answer struct {
	codeCtx   vacctx.CodeContext
	citations []evidence.Evidence
}

// Context reports the version the query was answered in — the same context the
// provider was handed, except that [Engine.GetCode] reports the revision the
// bytes actually came from.
func (a answer) Context() vacctx.CodeContext { return a.codeCtx }

// Evidence reports where the answer can be checked. On a successful result it
// is non-nil and empty only when there was nothing to cite; nil is the zero
// value, which is to say a result that no method returned.
func (a answer) Evidence() []evidence.Evidence { return a.citations }

// SearchCodeRequest is a search inside one version context. Where to search is
// the context's to say, which is why there is no repository or branch here.
type SearchCodeRequest struct {
	Context string
	Query   string
}

// SearchCodeResult is the matches with the version they were found in and one
// citation per match. The matches and the evidence carry the same facts on
// purpose: for a search the match is its own citation.
type SearchCodeResult struct {
	answer
	matches []provider.SearchResult
}

// Matches reports the matches in the provider's ranked order. It is empty, not
// an error, when nothing in this version matched.
func (r SearchCodeResult) Matches() []provider.SearchResult { return r.matches }

// TraceCallsRequest is a walk of the call graph around one symbol, inside one
// version context.
type TraceCallsRequest struct {
	Context   string
	Symbol    string
	Direction provider.Direction
	Depth     int
}

// TraceCallsResult is the traversal with the version it was traced in, cited at
// the call sites it walked.
type TraceCallsResult struct {
	answer
	graph provider.CallGraph
}

// Graph reports the traversal. Its Symbol is what the provider resolved the
// request to, which is not always the string that was asked for.
func (r TraceCallsResult) Graph() provider.CallGraph { return r.graph }

// GetCodeRequest is a read of one line range, at the revision a context
// declares.
type GetCodeRequest struct {
	Context   string
	Path      string
	StartLine int
	EndLine   int
}

// GetCodeResult is the content with the version it was read at, cited at the
// line range it came from. Its Context's Revision is the revision the bytes
// actually came from, not the spelling the configuration used.
type GetCodeResult struct {
	answer
	source provider.SourceContent
}

// Source reports the content read, with the path and line range it covers.
func (r GetCodeResult) Source() provider.SourceContent { return r.source }

// ListContexts returns every configured version context, sorted by ID.
//
// An empty configuration is an answer and not a failure: this is what a caller
// asks before it knows which versions exist, so there is nothing yet for it to
// have got wrong.
func (e *Engine) ListContexts(context.Context) []vacctx.CodeContext {
	return e.contexts.Contexts()
}

// SearchCode searches the code of the context req names.
//
// The context is resolved before anything else: an ID that names no configured
// version has no search to run, and guessing one would answer from a version
// the caller never asked for. Every failure is the resolver's or the provider's
// own *[vacerr.Error], returned unchanged — an unconfigured ID is
// [vacerr.ContextNotFound] and never an empty result, which would read as "this
// version has no such code".
//
// It reaches no graph provider, which is doc-1 §23's eighth success criterion
// held in the code: search answers whether or not CBM is present.
func (e *Engine) SearchCode(ctx context.Context, req SearchCodeRequest) (SearchCodeResult, error) {
	codeCtx, err := e.contexts.Resolve(ctx, req.Context)
	if err != nil {
		return SearchCodeResult{}, err
	}

	matches, err := e.search.Search(ctx, codeCtx, provider.SearchQuery{Query: req.Query})
	if err != nil {
		return SearchCodeResult{}, err
	}

	// Built empty rather than nil so a result that cited nothing is still a
	// result that carries evidence: "this version has no such code" is an answer
	// with an empty citation list, not an answer with no citation list.
	citations := make([]evidence.Evidence, 0, len(matches))
	for _, match := range matches {
		citations = append(citations, evidence.At(match.Path, match.Line, match.Line, match.Snippet))
	}
	return SearchCodeResult{answer{codeCtx, citations}, matches}, nil
}

// TraceCalls walks the call graph around req.Symbol in the graph the context
// names, and no other.
//
// Depth is checked here rather than passed on, because it crosses a trust
// boundary and doc-1 bounds it at 1 to 5: quietly clamping an out-of-range
// depth would answer a shallower question than the caller asked without saying
// so. Everything else about the request — the symbol, the direction, whether
// the symbol names exactly one function — is checked downstream, in the same
// error model, and is not second-guessed here.
func (e *Engine) TraceCalls(ctx context.Context, req TraceCallsRequest) (TraceCallsResult, error) {
	if req.Depth < minDepth || req.Depth > maxDepth {
		return TraceCallsResult{}, vacerr.New(
			vacerr.InvalidArgument,
			fmt.Sprintf("trace_calls: depth %d is outside the supported range %d-%d", req.Depth, minDepth, maxDepth),
			map[string]any{"context": req.Context, "symbol": req.Symbol, "depth": req.Depth},
		)
	}

	codeCtx, err := e.contexts.Resolve(ctx, req.Context)
	if err != nil {
		return TraceCallsResult{}, err
	}

	graph, err := e.graph.TraceCalls(ctx, codeCtx, provider.TraceRequest{
		Symbol:    req.Symbol,
		Direction: req.Direction,
		Depth:     req.Depth,
	})
	if err != nil {
		return TraceCallsResult{}, err
	}

	citations := make([]evidence.Evidence, 0, len(graph.Edges))
	seen := map[evidence.Evidence]bool{}
	for _, edge := range graph.Edges {
		// Several calls written in one function cite one location, so the same
		// citation is listed once.
		at := evidence.At(edge.Path, edge.Line, edge.Line, "")
		if !seen[at] {
			seen[at] = true
			citations = append(citations, at)
		}
	}
	return TraceCallsResult{answer{codeCtx, citations}, *graph}, nil
}

// GetCode reads lines [req.StartLine, req.EndLine] of req.Path as they are at
// the revision the context declares.
//
// The returned context reports the revision the provider actually read at. A
// context may declare a branch name or a short SHA; reporting the commit the
// bytes came from is what lets a caller check the claim instead of trusting it.
//
// It holds no version check of its own: resolving an ID is the resolver's, and
// the fail-closed [vacerr.SourceMismatch] check is
// [resolver.VerifyWorktree], which the source provider reaches on the one path
// where the object database cannot serve the declared revision's bytes. A
// second implementation here would be a second thing to keep correct. What this
// method guarantees is the other half of failing closed: an error from either
// dependency is returned with no content beside it.
func (e *Engine) GetCode(ctx context.Context, req GetCodeRequest) (GetCodeResult, error) {
	codeCtx, err := e.contexts.Resolve(ctx, req.Context)
	if err != nil {
		return GetCodeResult{}, err
	}

	src, err := e.source.Read(ctx, codeCtx, req.Path, req.StartLine, req.EndLine)
	if err != nil {
		return GetCodeResult{}, err
	}
	codeCtx.Revision = src.Revision

	// The one citation is the range that was read, at the revision it was read
	// at. No snippet: the content is the result, and repeating it as evidence
	// would cite the answer with itself.
	citations := []evidence.Evidence{evidence.At(src.Path, src.StartLine, src.EndLine, "")}
	return GetCodeResult{answer{codeCtx, citations}, *src}, nil
}
