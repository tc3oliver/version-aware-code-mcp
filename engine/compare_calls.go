package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/tc3oliver/version-aware-code-mcp/evidence"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// CompareCallsRequest is one symbol's call graph in two version contexts, asked
// for so the two can be set against each other.
//
// It names its scope with two context IDs and nothing else, for the reason every
// request here does: the repository, branch and revision of each side come from
// the configuration those IDs name, so a caller cannot compare a version it was
// not granted. Symbol, Direction and Depth are asked of both sides identically —
// there is deliberately no way to walk the from side one way and the to side
// another, because a difference produced by asking two different questions is
// not a difference between the two versions.
type CompareCallsRequest struct {
	FromContext string
	ToContext   string
	Symbol      string
	Direction   provider.Direction
	Depth       int
}

// CallRelation is one caller-callee pair on one path, with the call sites it was
// written at in each version.
//
// Its fields are exported because it carries no claim of its own: it is a leaf
// of a result, in the way [provider.CallEdge] and [provider.SearchResult] are,
// and every version claim around it lives on the [ComparisonSide]s of the
// [CompareCallsResult] that no caller can forge.
//
// The two evidence lists are kept apart for the reason [ComparisonSide]
// documents: handler.go:42 in the from context and handler.go:42 in the to
// context are two different lines that happen to share a spelling, so a merged
// list could not say which version any entry came from. A relation only the to
// side has cites nothing on the from side, and the other way round.
type CallRelation struct {
	Caller       string
	Callee       string
	Path         string
	FromEvidence []evidence.Evidence
	ToEvidence   []evidence.Evidence
}

// CompareCallsResult is what changed about one symbol's call graph between two
// versions, with each version's half of the answer kept as its own
// [ComparisonSide].
//
// It has no exported fields, like every other result here, so the only values
// carrying a presence and a set of relations are the ones [Engine.CompareCalls]
// built. It deliberately offers no Context or Evidence of its own: a comparison
// is answered in two versions, and one merged pair of those would be a
// cross-version answer wearing the clothes of a version-scoped one.
type CompareCallsResult struct {
	from       ComparisonSide
	to         ComparisonSide
	requested  string
	fromSymbol string
	toSymbol   string
	presence   SymbolPresence
	added      []CallRelation
	removed    []CallRelation
	unchanged  []CallRelation
}

// From reports the from version's half of the comparison: the version it was
// traced in and the call sites backing it. It reports Present false when that
// version had no such symbol.
func (r CompareCallsResult) From() ComparisonSide { return r.from }

// To reports the to version's half, the mirror of [CompareCallsResult.From].
func (r CompareCallsResult) To() ComparisonSide { return r.to }

// RequestedSymbol reports the symbol as it was asked for, unresolved.
func (r CompareCallsResult) RequestedSymbol() string { return r.requested }

// FromResolvedSymbol reports what the from version resolved the request to,
// which is not always the string that was asked for and is empty when that
// version had no such symbol.
func (r CompareCallsResult) FromResolvedSymbol() string { return r.fromSymbol }

// ToResolvedSymbol reports what the to version resolved the request to, the
// mirror of [CompareCallsResult.FromResolvedSymbol]. The two are reported
// separately because one symbol can be renamed between versions and still be
// the symbol the caller asked about.
func (r CompareCallsResult) ToResolvedSymbol() string { return r.toSymbol }

// Presence reports which versions had the symbol at all. It is the question
// answered before any relation is compared, because two sides can be diffed and
// one cannot.
func (r CompareCallsResult) Presence() SymbolPresence { return r.presence }

// Added reports the relations only the to version has. On a result built here it
// is non-nil and empty when nothing was added, so "compared, nothing added" is
// told apart from the zero result nobody built.
func (r CompareCallsResult) Added() []CallRelation { return r.added }

// Removed reports the relations only the from version has.
func (r CompareCallsResult) Removed() []CallRelation { return r.removed }

// Unchanged reports the relations both versions have. The call sites behind one
// may still differ, which is why each relation keeps both versions' lines.
func (r CompareCallsResult) Unchanged() []CallRelation { return r.unchanged }

// relation is the identity a call is compared by: who calls whom, in which file.
//
// The line is deliberately not part of it. Code moving down a file is not a call
// relation appearing and another disappearing, and reporting it as one would
// bury the calls that really were added under every edit above them. The path is
// part of it, so a relation whose call moved to another file is one removed
// relation and one added one — two facts that are true, rather than one
// "changed" that hides which file to look in.
type relation struct{ caller, callee, path string }

// group indexes one side's edges by relation, and returns the relations in the
// order the provider listed them so the comparison reports in a stable one.
//
// Every call site is kept, not one per relation: [Engine.TraceCalls] lists a
// location once because a citation is a place to look, but here the lines are
// the content being compared — three calls in one function becoming one is a
// change worth seeing, and a deduplicated list could not show it. An absent
// side groups to nothing, which is what makes it compare as "every relation the
// other side has".
func group(graph *provider.CallGraph) (map[relation][]evidence.Evidence, []relation) {
	sites := map[relation][]evidence.Evidence{}
	var order []relation
	if graph == nil {
		return sites, order
	}
	for _, edge := range graph.Edges {
		key := relation{edge.Caller, edge.Callee, edge.Path}
		if _, seen := sites[key]; !seen {
			order = append(order, key)
		}
		sites[key] = append(sites[key], evidence.At(edge.Path, edge.Line, edge.Line, ""))
	}
	return sites, order
}

// citeEdges is one side's citations: where that version's half of the answer can
// be checked. Several calls written on one line are one place to look, so a
// location is listed once, exactly as [Engine.TraceCalls] cites a single-context
// walk.
func citeEdges(edges []provider.CallEdge) []evidence.Evidence {
	citations := make([]evidence.Evidence, 0, len(edges))
	seen := map[evidence.Evidence]bool{}
	for _, edge := range edges {
		at := evidence.At(edge.Path, edge.Line, edge.Line, "")
		if !seen[at] {
			seen[at] = true
			citations = append(citations, at)
		}
	}
	return citations
}

// traceSide walks one version's call graph and tells "this version does not have
// the symbol" apart from "this version could not be asked".
//
// A missing symbol comes back as a nil graph and no error, because on its own it
// is not yet an outcome: the same absence is FROM_ONLY beside a from side that
// has it, and a failed comparison beside a to side that does not. Only the
// caller, holding both answers, knows which.
//
// Everything else fails the comparison then and there. An ambiguous symbol is
// re-reported with the side it was ambiguous on and the provider's own details —
// the candidates above all — kept, because a caller told only "ambiguous" cannot
// name one of them and cannot tell which version to name it in. Any other
// failure is the provider's and is returned unchanged: a backend that could not
// answer has not said the symbol is absent, and reading it as absence would
// report a version's whole call graph as deleted on the strength of a timeout.
func (e *Engine) traceSide(ctx context.Context, codeCtx vacctx.CodeContext, req provider.TraceRequest, side string) (*provider.CallGraph, error) {
	graph, err := e.graph.TraceCalls(ctx, codeCtx, req)
	if err == nil {
		return graph, nil
	}

	var vErr *vacerr.Error
	if !errors.As(err, &vErr) {
		return nil, err
	}
	switch vErr.Code {
	case vacerr.SymbolNotFound:
		return nil, nil
	case vacerr.SymbolAmbiguous:
		details := map[string]any{}
		maps.Copy(details, vErr.Details)
		details["side"] = side
		return nil, vacerr.New(
			vacerr.SymbolAmbiguous,
			fmt.Sprintf("compare_calls: the %s side is ambiguous: %s", side, vErr.Message),
			details,
		)
	default:
		return nil, err
	}
}

// CompareCalls walks the call graph around req.Symbol in both versions and
// reports how the two differ.
//
// It asks nothing the four single-context queries do not: both sides are
// resolved through the same [Engine.resolve], walked through the same
// [provider.GraphProvider] with the same [provider.TraceRequest], and the
// comparison itself is a set difference over what came back. There is no
// compare capability behind a provider, so a backend gains this query by
// answering the one it already answers, and no backend can disagree with
// another about what "the same call" means.
//
// The identity a call is compared by is caller, callee and path, without the
// line: see [relation]. A symbol found in only one version is an answer rather
// than a failure — that is what PresenceFromOnly and PresenceToOnly are for —
// and only a symbol neither version has leaves nothing to compare, which is
// [vacerr.SymbolNotFound].
func (e *Engine) CompareCalls(ctx context.Context, req CompareCallsRequest) (CompareCallsResult, error) {
	if req.Depth < minDepth || req.Depth > maxDepth {
		return CompareCallsResult{}, vacerr.New(
			vacerr.InvalidArgument,
			fmt.Sprintf("compare_calls: depth %d is outside the supported range %d-%d", req.Depth, minDepth, maxDepth),
			map[string]any{
				"from_context": req.FromContext,
				"to_context":   req.ToContext,
				"symbol":       req.Symbol,
				"depth":        req.Depth,
			},
		)
	}

	// Both sides before either provider call, and in the order they were asked
	// for: an ID naming no configured version has no graph to walk, and a
	// comparison that walked one side first would report the other's failure
	// after doing work in a version the caller may not have meant.
	fromCtx, err := e.resolve(ctx, req.FromContext)
	if err != nil {
		return CompareCallsResult{}, err
	}
	toCtx, err := e.resolve(ctx, req.ToContext)
	if err != nil {
		return CompareCallsResult{}, err
	}

	if e.graph == nil {
		return CompareCallsResult{}, vacerr.New(
			vacerr.GraphProviderUnavailable,
			"compare_calls: this server was built with no graph provider",
			map[string]any{"from_context": req.FromContext, "to_context": req.ToContext, "symbol": req.Symbol},
		)
	}

	// Once per side, because each side is walked in its own graph and a context
	// naming none has no version to be walked in.
	if err := require(req.FromContext, "graph_ref", fromCtx.GraphRef); err != nil {
		return CompareCallsResult{}, err
	}
	if err := require(req.ToContext, "graph_ref", toCtx.GraphRef); err != nil {
		return CompareCallsResult{}, err
	}

	traceReq := provider.TraceRequest{Symbol: req.Symbol, Direction: req.Direction, Depth: req.Depth}
	fromGraph, err := e.traceSide(ctx, fromCtx, traceReq, "from")
	if err != nil {
		return CompareCallsResult{}, err
	}
	toGraph, err := e.traceSide(ctx, toCtx, traceReq, "to")
	if err != nil {
		return CompareCallsResult{}, err
	}

	result := CompareCallsResult{
		requested: req.Symbol,
		added:     []CallRelation{},
		removed:   []CallRelation{},
		unchanged: []CallRelation{},
	}
	switch {
	case fromGraph == nil && toGraph == nil:
		// Neither version has it, so there is no comparison to answer with. This
		// is the one presence that is a failure: an empty result would read as
		// "the symbol is in both versions and calls nothing".
		return CompareCallsResult{}, vacerr.New(
			vacerr.SymbolNotFound,
			fmt.Sprintf("compare_calls: neither context %q nor context %q has a symbol matching %q", req.FromContext, req.ToContext, req.Symbol),
			map[string]any{"from_context": req.FromContext, "to_context": req.ToContext, "symbol": req.Symbol},
		)
	case fromGraph == nil:
		result.presence = PresenceToOnly
	case toGraph == nil:
		result.presence = PresenceFromOnly
	default:
		result.presence = PresenceBoth
	}

	// The absent side stays the zero ComparisonSide, which reports Present false
	// and cites nothing — the absence is the answer rather than a gap in it.
	if fromGraph != nil {
		result.from = ComparisonSide{answer{fromCtx, citeEdges(fromGraph.Edges)}}
		result.fromSymbol = fromGraph.Symbol
	}
	if toGraph != nil {
		result.to = ComparisonSide{answer{toCtx, citeEdges(toGraph.Edges)}}
		result.toSymbol = toGraph.Symbol
	}

	fromSites, fromOrder := group(fromGraph)
	toSites, toOrder := group(toGraph)

	// From's order first, then to's: every relation is classified once, and the
	// report is in the order the two versions listed them rather than in a map's.
	// An absent side groups to nothing, so a one-sided comparison falls out of
	// the same loop with the whole surviving graph on one side of the diff.
	seen := map[relation]bool{}
	for _, key := range slices.Concat(fromOrder, toOrder) {
		if seen[key] {
			continue
		}
		seen[key] = true

		fromCites, inFrom := fromSites[key]
		toCites, inTo := toSites[key]
		found := CallRelation{
			Caller:       key.caller,
			Callee:       key.callee,
			Path:         key.path,
			FromEvidence: fromCites,
			ToEvidence:   toCites,
		}
		switch {
		case inFrom && inTo:
			result.unchanged = append(result.unchanged, found)
		case inFrom:
			result.removed = append(result.removed, found)
		default:
			result.added = append(result.added, found)
		}
	}
	return result, nil
}
