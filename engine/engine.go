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
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/tc3oliver/version-aware-code-mcp/evidence"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
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

// ContextSource is where an Engine gets its version scopes from. It is the
// method set of [github.com/tc3oliver/version-aware-code-mcp/resolver.Resolver],
// named here so the engine states what it needs rather than which type supplies
// it.
//
// Both methods answer with a [vacctx.Workspace], the set of repositories one
// context ID names. A context over a single repository is a workspace with one
// member, so there is one shape here and not two, and the length is the only
// thing that tells them apart.
//
// Both take a [context.Context] and both can fail. Listing what is configured
// reads no repository, so the resolver's own listing never fails — but a source
// that decided which contexts a request may see, and could not reach whatever it
// asks, would have only the empty list left to answer with. An empty list is a
// statement ("there are no versions here") and not a failure, so a source with
// no way to report one would have to make that statement falsely. Resolve fails
// for the more ordinary reason: it checks the versions it names are really
// there.
//
// An implementation is not trusted to have validated what it returns, nor to
// have ordered it: see [Engine.resolve] and [Engine.ListContexts].
type ContextSource interface {
	Contexts(ctx context.Context) ([]vacctx.Workspace, error)
	Resolve(ctx context.Context, id string) (vacctx.Workspace, error)
}

// Engine answers the four queries of doc-1 §5 against one configuration.
type Engine struct {
	contexts ContextSource
	search   provider.SearchProvider
	graph    provider.GraphProvider
	source   provider.SourceProvider
}

// New returns an Engine resolving context IDs with contexts and answering
// through the three providers.
//
// contexts is required and is the one dependency with no degraded mode: every
// query starts by resolving its context ID, so an Engine built with a nil
// [ContextSource] panics on [Engine.SearchCode], [Engine.TraceCalls],
// [Engine.GetCode] and [Engine.ListContexts] alike. It is a precondition rather
// than a checked argument because there is no version-scoped answer to give
// without it, and failing at the first query rather than at construction would
// only move the same programming error further from its cause.
//
// Any of the three providers may be nil, for a caller that has only some of
// them: the query needing the absent one then fails with a *[vacerr.Error]
// naming what is missing, and the other queries are unaffected, which is doc-1
// §23's eighth success criterion — search answers whether or not CBM is
// present — held for every provider rather than only that pair. The code is
// [vacerr.SearchProviderUnavailable], [vacerr.GraphProviderUnavailable] and,
// for want of a source equivalent, [vacerr.RepositoryNotFound]: see
// [Engine.GetCode].
//
// New starts nothing, so it cannot fail and has nothing to roll back. That
// leaves partial-startup cleanup with the caller, which is the only party that
// can do it: a caller that starts a CBM subprocess, then fails while building
// the next provider, must close what it already started before returning the
// error, because there is no Engine yet to close it on the caller's behalf.
// From the moment New returns, [Engine.Close] takes that over.
func New(contexts ContextSource, search provider.SearchProvider, graph provider.GraphProvider, source provider.SourceProvider) *Engine {
	return &Engine{contexts: contexts, search: search, graph: graph, source: source}
}

// Close releases the dependencies this Engine was handed that can be released.
// One implementing [io.Closer] is closed; one that does not is left untouched.
// Every dependency gets its turn whether or not an earlier one failed, and the
// failures are joined, so a provider that will not shut down is reported
// without hiding the shutdowns after it.
//
// That feature detection is the ownership contract. [New] takes providers the
// caller has already built, so nothing here can be inferred from construction:
// handing over a provider that can be closed is what says "close this one for
// me". A caller keeping ownership — an embedder sharing one CBM session
// between several Engines, say — hands over a provider with no Close method,
// or wraps it in a type that does not have one:
//
//	type keepOpen struct{ provider.GraphProvider }
//
// An embedded interface promotes that interface's methods and no others, so
// the wrapper is a GraphProvider that is not an io.Closer, and Engine cannot
// close what its caller still needs. The Engine remains usable after Close
// only as far as its providers do; closing twice closes the providers twice,
// which is why [github.com/tc3oliver/version-aware-code-mcp/adapters/cbm.Provider.Close]
// tolerates it.
func (e *Engine) Close() error {
	var errs []error
	for _, dependency := range []any{e.contexts, e.search, e.graph, e.source} {
		if closer, ok := dependency.(io.Closer); ok {
			errs = append(errs, closer.Close())
		}
	}
	return errors.Join(errs...)
}

// require refuses a resolved context whose named field is blank.
//
// The code is [vacerr.InvalidArgument] for the reason config's own equivalent
// check uses it: a required field is absent. It is deliberately not
// [vacerr.SourceMismatch], which reports two revisions that disagree and has no
// second revision to name here.
func require(id, name, value string) error {
	if strings.TrimSpace(value) != "" {
		return nil
	}
	return vacerr.New(
		vacerr.InvalidArgument,
		fmt.Sprintf("context %q resolved without a %s, so there is no version to answer in", id, name),
		map[string]any{"context": id, "field": name},
	)
}

// resolve returns the one member of the workspace id names, once every member
// of it is complete enough to scope a query with.
//
// The fields are re-checked here, after the [ContextSource] has already had its
// say, because [ContextSource] is an interface: whichever implementation is
// installed, an incomplete scope must not reach a provider. A context missing
// its revision is not a narrower question, it is a different one — a provider
// handed an empty revision reads whatever the checkout happens to be on, which
// is the cross-version answer this server exists to refuse. The ID is checked
// with them because it is what every result and every citation is scoped by:
// without it [evidence.New] refuses the answer downstream, with an error that
// is not a *[vacerr.Error] and names no context.
//
// Every member is checked and not only the one that will be used, because the
// workspace is what the caller named: a member this query happens not to reach
// is still part of the scope it asked about, and one that is unusable makes the
// scope unusable rather than smaller.
//
// GraphRef is not checked here, because only [Engine.TraceCalls] reads a graph:
// requiring it of all three would leave a caller with no graph backend at all
// unable to search, which is the independent failure of the providers given up
// at the [ContextSource]. It is checked where it is used.
//
// The single member is where this server currently stops. A context naming
// several repositories parses, resolves and lists, and then every query refuses
// it with the error [severalRepositories] builds, because expanding a query over
// several members is not implemented here yet. Answering in the first member
// would be the one thing worse than refusing: a whole repository's worth of code
// silently outside the scope of an answer that names the context the caller
// asked for.
func (e *Engine) resolve(ctx context.Context, id string) (vacctx.CodeContext, error) {
	workspace, err := e.contexts.Resolve(ctx, id)
	if err != nil {
		return vacctx.CodeContext{}, err
	}
	for _, member := range workspace.Members {
		for _, field := range []struct{ name, value string }{
			{"id", member.ID},
			{"repository", member.Repository},
			{"branch", member.Branch},
			{"revision", member.Revision},
		} {
			if err := require(id, field.name, field.value); err != nil {
				return vacctx.CodeContext{}, err
			}
		}
	}
	if len(workspace.Members) != 1 {
		return vacctx.CodeContext{}, severalRepositories(id, workspace)
	}
	return workspace.Members[0], nil
}

// severalRepositories refuses a workspace this server cannot answer a question
// in: one naming several repositories, or none at all.
//
// The code is [vacerr.InvalidArgument] rather than one of its own. The code set
// is part of the public tool API — ten codes fixed by the v0.1.0 specification
// and one added since — so a new one is a change to that API, and it belongs
// with the work that makes a multi-repository context answerable rather than
// with the work that makes it configurable. What is true today is that the
// request named a scope this server cannot answer in, which is what
// INVALID_ARGUMENT says, and the message says the rest plainly.
//
// The repositories travel with it because the caller cannot see the
// configuration: told only that its context names several, it cannot tell
// whether that is the context it meant.
func severalRepositories(id string, workspace vacctx.Workspace) error {
	if len(workspace.Members) == 0 {
		return vacerr.New(
			vacerr.InvalidArgument,
			fmt.Sprintf("context %q resolved without a repository, so there is no version to answer in", id),
			map[string]any{"context": id},
		)
	}

	repositories := make([]string, 0, len(workspace.Members))
	for _, member := range workspace.Members {
		repositories = append(repositories, member.Repository)
	}
	return vacerr.New(
		vacerr.InvalidArgument,
		fmt.Sprintf("context %q names %d repositories (%s), and this server can only answer a question about one",
			id, len(repositories), strings.Join(repositories, ", ")),
		map[string]any{"context": id, "repositories": repositories},
	)
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
// is non-nil, and empty only when there was nothing to cite.
//
// nil is the zero value, and so is what every failed call reports: the error
// paths of [Engine.SearchCode], [Engine.TraceCalls] and [Engine.GetCode] all
// return the zero result beside the error. nil therefore means "this is not an
// answer" — a result nobody built, or one that was not reached — and never "an
// answer that cited nothing", which is the empty list.
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
// The order is imposed here rather than taken on trust: [ContextSource] is an
// interface and promises no order, so a caller paging or diffing this list gets
// the same answer whichever implementation is installed. The sort is on a copy,
// because a source is entitled to hand back a slice it reuses.
//
// An empty configuration is an answer and not a failure: this is what a caller
// asks before it knows which versions exist, so there is nothing yet for it to
// have got wrong. The error is the source's own and nothing else: a source that
// could not say which contexts exist has not said there are none.
//
// A workspace naming several repositories is listed like any other, even though
// no query can be answered in it yet: what exists in the configuration and what
// this server can currently answer are two different facts, and hiding the first
// behind the second would leave a caller unable to see the context it wrote.
func (e *Engine) ListContexts(ctx context.Context) ([]vacctx.Workspace, error) {
	contexts, err := e.contexts.Contexts(ctx)
	if err != nil {
		return nil, err
	}
	listed := slices.Clone(contexts)
	slices.SortFunc(listed, func(a, b vacctx.Workspace) int {
		return strings.Compare(a.ID, b.ID)
	})
	return listed, nil
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
	codeCtx, err := e.resolve(ctx, req.Context)
	if err != nil {
		return SearchCodeResult{}, err
	}

	// After the resolve, not before it: whether a version exists is the
	// configuration's answer and must not depend on which providers this
	// server was built with, so an unconfigured ID is CONTEXT_NOT_FOUND here
	// exactly as it is everywhere else. The same holds in TraceCalls and
	// GetCode.
	if e.search == nil {
		return SearchCodeResult{}, vacerr.New(
			vacerr.SearchProviderUnavailable,
			"search_code: this server was built with no search provider",
			map[string]any{"context": req.Context},
		)
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

	codeCtx, err := e.resolve(ctx, req.Context)
	if err != nil {
		return TraceCallsResult{}, err
	}

	if e.graph == nil {
		return TraceCallsResult{}, vacerr.New(
			vacerr.GraphProviderUnavailable,
			"trace_calls: this server was built with no graph provider",
			map[string]any{"context": req.Context, "symbol": req.Symbol},
		)
	}

	// The one field [Engine.resolve] leaves to its caller, because this is the
	// only query that names a graph. It is checked after the provider, so that a
	// server built with no graph at all says so once rather than blaming
	// whichever context was asked about.
	if err := require(req.Context, "graph_ref", codeCtx.GraphRef); err != nil {
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
// Beyond [Engine.resolve]'s completeness check it holds no version check of its
// own: resolving an ID is the [ContextSource]'s, and the fail-closed
// [vacerr.SourceMismatch] check is
// [github.com/tc3oliver/version-aware-code-mcp/resolver.VerifyWorktree], which
// the source provider reaches on the one path where the object database cannot
// serve the declared revision's bytes. A second implementation here would be a
// second thing to keep correct. What this method guarantees is the other half of
// failing closed: an error from either dependency is returned with no content
// beside it.
//
// With no source provider it fails with [vacerr.RepositoryNotFound], not a
// provider-unavailable code: the code set has
// [vacerr.SearchProviderUnavailable] and [vacerr.GraphProviderUnavailable] and
// deliberately no source equivalent, so rather than add one — a change to the
// public tool API — this reuses the code that already says what is true, that
// there is no repository this server can read. The asymmetry is the compromise,
// and it is here rather than in the taxonomy.
func (e *Engine) GetCode(ctx context.Context, req GetCodeRequest) (GetCodeResult, error) {
	codeCtx, err := e.resolve(ctx, req.Context)
	if err != nil {
		return GetCodeResult{}, err
	}

	// There is no SOURCE_PROVIDER_UNAVAILABLE in the v0.1.0 code set, and a new
	// code would be a change to the public tool API for a case the existing
	// set already describes: with no source provider there is no repository
	// this server can read, whatever the context declares, which is what
	// REPOSITORY_NOT_FOUND says. It is deliberately not INVALID_ARGUMENT, which
	// would blame the caller's request for how the server was built.
	if e.source == nil {
		return GetCodeResult{}, vacerr.New(
			vacerr.RepositoryNotFound,
			"get_code: this server was built with no source provider, so no repository can be read",
			map[string]any{"context": req.Context, "path": req.Path},
		)
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
