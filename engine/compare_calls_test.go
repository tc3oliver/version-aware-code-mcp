package engine_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/evidence"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// The two versions every comparison test is answered between. They are complete
// and name their own graph, because what is under test here is the comparison
// and not the resolving that the engine tests already pin down.
var (
	compareV1 = vacctx.CodeContext{
		ID: "demo@v1", Repository: "demo", Branch: "v1",
		Revision: "1111111111111111111111111111111111111111", GraphRef: "demo-v1",
	}
	compareV2 = vacctx.CodeContext{
		ID: "demo@v2", Repository: "demo", Branch: "v2",
		Revision: "2222222222222222222222222222222222222222", GraphRef: "demo-v2",
	}
)

// versionedGraph is a [provider.GraphProvider] that answers each version
// differently, which the single-graph fakeGraph cannot: a comparison is only
// worth testing when the two sides can disagree.
//
// A context with no graph of its own is a version that does not have the symbol,
// reported the way the CBM adapter reports it — SYMBOL_NOT_FOUND — so a test
// says "v2 dropped this function" by leaving it out. errs overrides that, for
// the failures that are not an absence.
type versionedGraph struct {
	graphs map[string]provider.CallGraph
	errs   map[string]error
	asked  []string
	reqs   []provider.TraceRequest
}

func (g *versionedGraph) TraceCalls(_ context.Context, codeCtx vacctx.CodeContext, req provider.TraceRequest) (*provider.CallGraph, error) {
	g.asked = append(g.asked, codeCtx.ID)
	g.reqs = append(g.reqs, req)
	if err, ok := g.errs[codeCtx.ID]; ok {
		return nil, err
	}
	graph, ok := g.graphs[codeCtx.ID]
	if !ok {
		return nil, vacerr.New(
			vacerr.SymbolNotFound,
			"trace_calls: context "+codeCtx.ID+" has no such symbol",
			map[string]any{"context": codeCtx.ID, "symbol": req.Symbol, "graph_ref": codeCtx.GraphRef},
		)
	}
	return &graph, nil
}

// compareEngine is an Engine over the two versions above, answering through the
// graph provider handed in and no other provider at all: a comparison of call
// graphs reads neither code nor an index, and building it with the other two
// nil is what keeps it that way.
func compareEngine(graph provider.GraphProvider) *engine.Engine {
	return engine.New(mapContexts{compareV1.ID: single(compareV1), compareV2.ID: single(compareV2)}, nil, graph, nil)
}

func compareRequest() engine.CompareCallsRequest {
	return engine.CompareCallsRequest{
		FromContext: compareV1.ID,
		ToContext:   compareV2.ID,
		Symbol:      "Process",
		Direction:   provider.Callers,
		Depth:       2,
	}
}

// names reports the relations as "caller->callee@path", which is what a failure
// message needs to read at a glance.
func names(relations []engine.CallRelation) []string {
	out := make([]string, 0, len(relations))
	for _, r := range relations {
		out = append(out, r.Caller+"->"+r.Callee+"@"+r.Path)
	}
	return out
}

func assertRelations(t *testing.T, got []engine.CallRelation, what string, want ...string) {
	t.Helper()
	if names := names(got); !slices.Equal(names, want) {
		t.Fatalf("%s is %v, want %v", what, names, want)
	}
}

// Acceptance criterion 1: a comparison names its scope with two context IDs and
// nothing else. A repository, branch or revision field here would let a caller
// compare a version the configuration never granted it, which is the whole
// guarantee given away in a struct literal.
func TestCompareCallsRequestIsScopedByContextIDsAlone(t *testing.T) {
	typ := reflect.TypeOf(engine.CompareCallsRequest{})
	var got []string
	for i := range typ.NumField() {
		got = append(got, typ.Field(i).Name)
	}
	want := []string{"FromContext", "ToContext", "Symbol", "Direction", "Depth"}
	if !slices.Equal(got, want) {
		t.Fatalf("CompareCallsRequest has the fields %v, want exactly %v", got, want)
	}
}

// Acceptance criterion 1, second half: depth is bounded at 1 to 5 exactly as
// trace_calls bounds it, and out of range is refused rather than clamped —
// a clamped walk on either side would answer a comparison nobody asked for and
// look like the answer to the one they did.
func TestCompareCallsRefusesDepthOutsideTheRange(t *testing.T) {
	for _, depth := range []int{0, -1, 6} {
		graph := &versionedGraph{}
		req := compareRequest()
		req.Depth = depth

		if _, err := compareEngine(graph).CompareCalls(context.Background(), req); err != nil {
			assertCode(t, err, vacerr.InvalidArgument)
		} else {
			t.Fatalf("depth %d was accepted", depth)
		}
		if len(graph.asked) != 0 {
			t.Fatalf("depth %d reached the graph provider", depth)
		}
	}
}

// Acceptance criterion 2: an ID naming no configured version is refused on
// either side, before any graph is walked. Guessing one, or comparing against an
// empty graph, would report another version's calls — or their absence — as this
// one's.
func TestCompareCallsUnknownContextIsContextNotFound(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*engine.CompareCallsRequest)
		unknown string
	}{
		{"from", func(r *engine.CompareCallsRequest) { r.FromContext = "demo@v9" }, "demo@v9"},
		{"to", func(r *engine.CompareCallsRequest) { r.ToContext = "demo@v9" }, "demo@v9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := &versionedGraph{graphs: map[string]provider.CallGraph{
				compareV1.ID: {Symbol: "demo.Process"},
				compareV2.ID: {Symbol: "demo.Process"},
			}}
			req := compareRequest()
			tc.mutate(&req)

			if _, err := compareEngine(graph).CompareCalls(context.Background(), req); err != nil {
				assertCode(t, err, vacerr.ContextNotFound)
			} else {
				t.Fatalf("a comparison answered with %s naming an unconfigured version", tc.name)
			}
			if len(graph.asked) != 0 {
				t.Fatalf("an unconfigured version reached the graph provider: %v", graph.asked)
			}
		})
	}
}

// Only a comparison of call graphs needs a graph, and it needs one on both
// sides: a context naming no graph has no version for its half to be walked in.
// With no graph provider at all the server says that once, rather than blaming
// whichever context was asked about.
func TestCompareCallsNeedsAGraphOnBothSides(t *testing.T) {
	t.Run("no graph provider", func(t *testing.T) {
		eng := engine.New(mapContexts{compareV1.ID: single(compareV1), compareV2.ID: single(compareV2)}, &fakeSearch{}, nil, &fakeSource{})

		if _, err := eng.CompareCalls(context.Background(), compareRequest()); err != nil {
			assertCode(t, err, vacerr.GraphProviderUnavailable)
		} else {
			t.Fatal("a comparison answered with no graph provider behind it")
		}
	})

	for _, side := range []string{"from", "to"} {
		t.Run("no graph_ref on the "+side+" side", func(t *testing.T) {
			from, to := compareV1, compareV2
			if side == "from" {
				from.GraphRef = ""
			} else {
				to.GraphRef = ""
			}
			graph := &versionedGraph{graphs: map[string]provider.CallGraph{
				from.ID: {Symbol: "demo.Process"},
				to.ID:   {Symbol: "demo.Process"},
			}}
			eng := engine.New(mapContexts{from.ID: single(from), to.ID: single(to)}, nil, graph, nil)

			if _, err := eng.CompareCalls(context.Background(), compareRequest()); err != nil {
				assertCode(t, err, vacerr.InvalidArgument)
			} else {
				t.Fatalf("a comparison answered with no graph_ref on the %s side", side)
			}
			if len(graph.asked) != 0 {
				t.Fatalf("a context with no graph_ref reached the graph provider: %v", graph.asked)
			}
		})
	}
}

// Acceptance criterion 3: neither version has the symbol, so there is nothing to
// compare. An empty comparison would read as "it is in both versions and calls
// nothing", which is a different answer about code that does not exist.
func TestCompareCallsFailsWhenNeitherVersionHasTheSymbol(t *testing.T) {
	graph := &versionedGraph{}

	out, err := compareEngine(graph).CompareCalls(context.Background(), compareRequest())
	if err == nil {
		t.Fatal("a comparison of a symbol neither version has answered successfully")
	}
	assertCode(t, err, vacerr.SymbolNotFound)
	assertNotAComparison(t, out)
	if len(graph.asked) != 2 {
		t.Fatalf("the graph provider was asked %v, want both versions: an absent from side is not an answer on its own", graph.asked)
	}
}

// Acceptance criterion 4: an ambiguous symbol fails the whole comparison, and
// says which version it was ambiguous in. Both halves matter: a caller told only
// "ambiguous" cannot name a candidate, and one told the candidates but not the
// side cannot tell which version to name it in.
func TestCompareCallsReportsWhichSideIsAmbiguous(t *testing.T) {
	for _, tc := range []struct {
		side      string
		ambiguous vacctx.CodeContext
		other     vacctx.CodeContext
	}{
		{"from", compareV1, compareV2},
		{"to", compareV2, compareV1},
	} {
		t.Run(tc.side, func(t *testing.T) {
			candidates := []map[string]any{
				{"qualified_name": "demo.Process", "path": "process.go", "line": 12},
				{"qualified_name": "demo.internal.Process", "path": "internal/process.go", "line": 4},
			}
			graph := &versionedGraph{
				graphs: map[string]provider.CallGraph{tc.other.ID: {Symbol: "demo.Process"}},
				errs: map[string]error{tc.ambiguous.ID: vacerr.New(
					vacerr.SymbolAmbiguous,
					"trace_calls: 2 symbols match",
					map[string]any{"context": tc.ambiguous.ID, "symbol": "Process", "candidates": candidates},
				)},
			}

			out, err := compareEngine(graph).CompareCalls(context.Background(), compareRequest())
			if err == nil {
				t.Fatalf("an ambiguous %s side answered successfully", tc.side)
			}
			assertCode(t, err, vacerr.SymbolAmbiguous)
			assertNotAComparison(t, out)

			var vErr *vacerr.Error
			if !errors.As(err, &vErr) {
				t.Fatalf("error is %v, want a *vacerr.Error", err)
			}
			if vErr.Details["side"] != tc.side {
				t.Errorf("details say side %v, want %q", vErr.Details["side"], tc.side)
			}
			// The provider's own details are kept: without the candidates the
			// caller has nothing to name, and picking one for them would answer
			// about a symbol they did not ask about.
			got, ok := vErr.Details["candidates"].([]map[string]any)
			if !ok || len(got) != len(candidates) {
				t.Fatalf("details carry candidates %v, want the provider's %v", vErr.Details["candidates"], candidates)
			}
			if vErr.Details["context"] != tc.ambiguous.ID {
				t.Errorf("details say context %v, want the ambiguous version %q", vErr.Details["context"], tc.ambiguous.ID)
			}
		})
	}
}

// Acceptance criterion 5: a symbol only one version has is an answer, not a
// failure — it is the answer "this was added" or "this was removed". The
// surviving version's whole graph is on its side of the comparison, and the
// other side is absent rather than empty: it cites nothing because it had
// nothing, which is not the same as having had nothing to cite.
func TestCompareCallsReportsASymbolFoundInOnlyOneVersion(t *testing.T) {
	only := provider.CallGraph{
		Symbol: "demo.Process",
		Edges: []provider.CallEdge{
			{Caller: "demo.Main", Callee: "demo.Process", Path: "main.go", Line: 4},
			{Caller: "demo.Serve", Callee: "demo.Process", Path: "serve.go", Line: 9},
		},
	}

	for _, tc := range []struct {
		name     string
		present  vacctx.CodeContext
		presence engine.SymbolPresence
	}{
		{"only the to version has it", compareV2, engine.PresenceToOnly},
		{"only the from version has it", compareV1, engine.PresenceFromOnly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := &versionedGraph{graphs: map[string]provider.CallGraph{tc.present.ID: only}}

			out, err := compareEngine(graph).CompareCalls(context.Background(), compareRequest())
			if err != nil {
				t.Fatalf("CompareCalls: %v", err)
			}
			if out.Presence() != tc.presence {
				t.Fatalf("presence is %q, want %q", out.Presence(), tc.presence)
			}

			toOnly := tc.presence == engine.PresenceToOnly
			side, absent := out.From(), out.To()
			listed, empty := out.Removed(), out.Added()
			if toOnly {
				side, absent = out.To(), out.From()
				listed, empty = out.Added(), out.Removed()
			}

			if !side.Present() || side.Context() != tc.present {
				t.Fatalf("the surviving side reports %+v and Present %v, want the version that has the symbol", side.Context(), side.Present())
			}
			if absent.Present() {
				t.Fatalf("the version without the symbol reports Present true and context %+v", absent.Context())
			}
			if absent.Evidence() != nil {
				t.Errorf("the version without the symbol cites %+v, want nothing", absent.Evidence())
			}

			assertRelations(t, listed, "the surviving version's relations",
				"demo.Main->demo.Process@main.go", "demo.Serve->demo.Process@serve.go")
			if len(empty) != 0 || len(out.Unchanged()) != 0 {
				t.Fatalf("a one-sided comparison reported %v as the other side and %v as unchanged", names(empty), names(out.Unchanged()))
			}

			// The relation cites the version it was found in, and nothing on the
			// side that never had it.
			cited, blank := listed[0].ToEvidence, listed[0].FromEvidence
			if !toOnly {
				cited, blank = listed[0].FromEvidence, listed[0].ToEvidence
			}
			if want := []evidence.Evidence{evidence.At("main.go", 4, 4, "")}; !slices.Equal(cited, want) {
				t.Errorf("the relation cites %+v, want %+v", cited, want)
			}
			if blank != nil {
				t.Errorf("the relation cites %+v in a version that does not have the symbol", blank)
			}

			// The symbol each version resolved the request to, reported per side:
			// the version that has none resolved nothing.
			resolved, none := out.ToResolvedSymbol(), out.FromResolvedSymbol()
			if !toOnly {
				resolved, none = out.FromResolvedSymbol(), out.ToResolvedSymbol()
			}
			if resolved != "demo.Process" || none != "" {
				t.Errorf("resolved symbols are from=%q to=%q, want only the surviving version's", out.FromResolvedSymbol(), out.ToResolvedSymbol())
			}
			if out.RequestedSymbol() != "Process" {
				t.Errorf("requested symbol is %q, want the string that was asked for", out.RequestedSymbol())
			}
		})
	}
}

// Acceptance criterion 6: a call is compared by who calls whom in which file,
// and not by the line it is written on. Two calls to the same function from the
// same function are one relation with two call sites, in both versions: code
// moving down a file is not a relation appearing and another disappearing, and
// reporting it as one would bury the calls that really were added.
//
// The call sites are all kept, unlike the citations of a single-version walk:
// here the lines are the content being compared, so three calls becoming two is
// a change a deduplicated list could not show.
func TestCompareCallsGroupsEveryCallSiteOfOneRelation(t *testing.T) {
	graph := &versionedGraph{graphs: map[string]provider.CallGraph{
		compareV1.ID: {Symbol: "demo.Process", Edges: []provider.CallEdge{
			{Caller: "demo.Main", Callee: "demo.Process", Path: "main.go", Line: 10},
			{Caller: "demo.Main", Callee: "demo.Process", Path: "main.go", Line: 20},
		}},
		compareV2.ID: {Symbol: "demo.Process", Edges: []provider.CallEdge{
			// The same relation, moved down the file and called once more.
			{Caller: "demo.Main", Callee: "demo.Process", Path: "main.go", Line: 30},
			// A relation only this version has, also written twice.
			{Caller: "demo.Serve", Callee: "demo.Process", Path: "serve.go", Line: 7},
			{Caller: "demo.Serve", Callee: "demo.Process", Path: "serve.go", Line: 8},
		}},
	}}

	out, err := compareEngine(graph).CompareCalls(context.Background(), compareRequest())
	if err != nil {
		t.Fatalf("CompareCalls: %v", err)
	}
	if out.Presence() != engine.PresenceBoth {
		t.Fatalf("presence is %q, want %q", out.Presence(), engine.PresenceBoth)
	}

	assertRelations(t, out.Unchanged(), "unchanged", "demo.Main->demo.Process@main.go")
	assertRelations(t, out.Added(), "added", "demo.Serve->demo.Process@serve.go")
	assertRelations(t, out.Removed(), "removed")

	kept := out.Unchanged()[0]
	wantFrom := []evidence.Evidence{evidence.At("main.go", 10, 10, ""), evidence.At("main.go", 20, 20, "")}
	if !slices.Equal(kept.FromEvidence, wantFrom) {
		t.Errorf("the relation cites %+v in the from version, want both call sites %+v", kept.FromEvidence, wantFrom)
	}
	if wantTo := []evidence.Evidence{evidence.At("main.go", 30, 30, "")}; !slices.Equal(kept.ToEvidence, wantTo) {
		t.Errorf("the relation cites %+v in the to version, want %+v", kept.ToEvidence, wantTo)
	}

	added := out.Added()[0]
	wantAdded := []evidence.Evidence{evidence.At("serve.go", 7, 7, ""), evidence.At("serve.go", 8, 8, "")}
	if !slices.Equal(added.ToEvidence, wantAdded) {
		t.Errorf("the added relation cites %+v, want both call sites %+v", added.ToEvidence, wantAdded)
	}
	if added.FromEvidence != nil {
		t.Errorf("the added relation cites %+v in the version that does not have it", added.FromEvidence)
	}
}

// Acceptance criterion 7: the same call written in another file is one relation
// removed and one added, not one relation quietly changed. Both facts are true,
// and reporting them separately is what tells the caller which file to look in.
func TestCompareCallsTreatsAMovedCallAsRemovedAndAdded(t *testing.T) {
	graph := &versionedGraph{graphs: map[string]provider.CallGraph{
		compareV1.ID: {Symbol: "demo.Process", Edges: []provider.CallEdge{
			{Caller: "demo.Main", Callee: "demo.Process", Path: "main.go", Line: 4},
		}},
		compareV2.ID: {Symbol: "demo.Process", Edges: []provider.CallEdge{
			{Caller: "demo.Main", Callee: "demo.Process", Path: "cmd/main.go", Line: 4},
		}},
	}}

	out, err := compareEngine(graph).CompareCalls(context.Background(), compareRequest())
	if err != nil {
		t.Fatalf("CompareCalls: %v", err)
	}
	assertRelations(t, out.Removed(), "removed", "demo.Main->demo.Process@main.go")
	assertRelations(t, out.Added(), "added", "demo.Main->demo.Process@cmd/main.go")
	assertRelations(t, out.Unchanged(), "unchanged")
}

// Acceptance criterion 8: a version that could not be asked has not said the
// symbol is absent. Reading a backend failure as an absence would report a whole
// call graph as added or removed on the strength of a timeout, which is a
// confident wrong answer about a version nobody managed to read.
func TestCompareCallsFailsWhenEitherVersionCannotBeAsked(t *testing.T) {
	populated := provider.CallGraph{
		Symbol: "demo.Process",
		Edges:  []provider.CallEdge{{Caller: "demo.Main", Callee: "demo.Process", Path: "main.go", Line: 4}},
	}

	for _, tc := range []struct {
		side    string
		failing vacctx.CodeContext
		other   vacctx.CodeContext
	}{
		{"from", compareV1, compareV2},
		{"to", compareV2, compareV1},
	} {
		t.Run(tc.side, func(t *testing.T) {
			failure := vacerr.New(
				vacerr.GraphProviderUnavailable,
				"the graph backend would not answer",
				map[string]any{"context": tc.failing.ID},
			)
			graph := &versionedGraph{
				graphs: map[string]provider.CallGraph{tc.other.ID: populated},
				errs:   map[string]error{tc.failing.ID: failure},
			}

			out, err := compareEngine(graph).CompareCalls(context.Background(), compareRequest())
			if err == nil {
				t.Fatalf("a comparison answered with the %s version unreadable", tc.side)
			}
			assertCode(t, err, vacerr.GraphProviderUnavailable)
			if !errors.Is(err, failure) {
				t.Fatalf("error is %v, want the provider's own error", err)
			}
			// The half that answered is not reported as the whole truth: no
			// relations at all, and neither side claiming a version.
			assertNotAComparison(t, out)
		})
	}
}

// Acceptance criterion 9: comparing a version with itself is a question with an
// answer, and the answer is "nothing changed". It is not special-cased anywhere:
// the same graph on both sides groups to the same relations, so every one of
// them is on both sides of the set difference.
func TestCompareCallsOfOneVersionWithItselfReportsNoChange(t *testing.T) {
	whole := provider.CallGraph{
		Symbol: "demo.Process",
		Edges: []provider.CallEdge{
			{Caller: "demo.Main", Callee: "demo.Process", Path: "main.go", Line: 4},
			{Caller: "demo.Serve", Callee: "demo.Process", Path: "serve.go", Line: 9},
			{Caller: "demo.Serve", Callee: "demo.Process", Path: "serve.go", Line: 11},
		},
	}
	graph := &versionedGraph{graphs: map[string]provider.CallGraph{compareV1.ID: whole}}
	req := compareRequest()
	req.ToContext = req.FromContext

	out, err := compareEngine(graph).CompareCalls(context.Background(), req)
	if err != nil {
		t.Fatalf("CompareCalls: %v", err)
	}
	if out.Presence() != engine.PresenceBoth {
		t.Fatalf("presence is %q, want %q", out.Presence(), engine.PresenceBoth)
	}
	if len(out.Added()) != 0 || len(out.Removed()) != 0 {
		t.Fatalf("comparing a version with itself reported added %v and removed %v", names(out.Added()), names(out.Removed()))
	}
	assertRelations(t, out.Unchanged(), "unchanged",
		"demo.Main->demo.Process@main.go", "demo.Serve->demo.Process@serve.go")

	// The whole graph, call sites included: the second call site of the second
	// relation is on both sides rather than dropped as a duplicate.
	moved := out.Unchanged()[1]
	want := []evidence.Evidence{evidence.At("serve.go", 9, 9, ""), evidence.At("serve.go", 11, 11, "")}
	if !slices.Equal(moved.FromEvidence, want) || !slices.Equal(moved.ToEvidence, want) {
		t.Fatalf("the relation cites from=%+v to=%+v, want both versions citing %+v", moved.FromEvidence, moved.ToEvidence, want)
	}
	if out.From().Context() != compareV1 || out.To().Context() != compareV1 {
		t.Fatalf("the sides report from=%+v to=%+v, want both in the one version compared", out.From().Context(), out.To().Context())
	}
}

// Acceptance criterion 10: the contract is structural. Every field of
// CompareCallsResult is unexported, so no composite literal outside the package
// can fill one in, and the only value a caller can write for itself is the zero
// one — which reports a presence that is none of the three, cites nothing on
// either side and lists no relations.
//
// It answers no Context and no Evidence of its own either: a comparison is
// answered in two versions, and one merged pair would be a cross-version answer
// wearing the clothes of a version-scoped one.
func TestACompareCallsResultCannotBeBuiltOutsideTheEngine(t *testing.T) {
	typ := reflect.TypeOf(engine.CompareCallsResult{})
	for i := range typ.NumField() {
		if field := typ.Field(i); field.IsExported() {
			t.Errorf("CompareCallsResult.%s is exported: a caller can build a comparison claiming versions it never queried",
				field.Name)
		}
	}

	if _, isContextual := any(engine.CompareCallsResult{}).(contextual); isContextual {
		t.Error("CompareCallsResult answers one Context and one Evidence, which cannot say which of the two versions they came from")
	}

	assertNotAComparison(t, engine.CompareCallsResult{})
	for _, presence := range []engine.SymbolPresence{engine.PresenceBoth, engine.PresenceToOnly, engine.PresenceFromOnly} {
		if (engine.CompareCallsResult{}).Presence() == presence {
			t.Errorf("the zero CompareCallsResult reports presence %q, so a caller can claim one by writing one", presence)
		}
	}
}

// assertNotAComparison holds the other half of the anti-forgery contract: what a
// failed comparison returns beside its error. Neither side claims a version, and
// nothing is reported as added, removed or unchanged, so a caller that ignored
// the error still cannot mistake the result for an answer.
func assertNotAComparison(t *testing.T, out engine.CompareCallsResult) {
	t.Helper()
	assertNotAnAnswer(t, out.From())
	assertNotAnAnswer(t, out.To())
	if out.From().Present() || out.To().Present() {
		t.Errorf("a result that is not an answer reports Present from=%v to=%v", out.From().Present(), out.To().Present())
	}
	if len(out.Added()) != 0 || len(out.Removed()) != 0 || len(out.Unchanged()) != 0 {
		t.Errorf("a result that is not an answer lists added %v, removed %v, unchanged %v",
			names(out.Added()), names(out.Removed()), names(out.Unchanged()))
	}
	if out.RequestedSymbol() != "" || out.FromResolvedSymbol() != "" || out.ToResolvedSymbol() != "" {
		t.Errorf("a result that is not an answer names symbols requested=%q from=%q to=%q",
			out.RequestedSymbol(), out.FromResolvedSymbol(), out.ToResolvedSymbol())
	}
}

// Acceptance criterion 11: comparing call graphs reads no code and no search
// index, so an embedder with neither provider can still do it. The engine built
// here has a context source and a graph and nothing else, and a comparison that
// had grown a dependency on either of the others would not answer at all.
func TestCompareCallsNeedsOnlyContextsAndAGraph(t *testing.T) {
	graph := &versionedGraph{graphs: map[string]provider.CallGraph{
		compareV1.ID: {Symbol: "demo.Process", Edges: []provider.CallEdge{
			{Caller: "demo.Main", Callee: "demo.Process", Path: "main.go", Line: 4},
			{Caller: "demo.Serve", Callee: "demo.Process", Path: "serve.go", Line: 9},
		}},
		compareV2.ID: {Symbol: "demo.Handle", Edges: []provider.CallEdge{
			{Caller: "demo.Main", Callee: "demo.Handle", Path: "main.go", Line: 4},
			{Caller: "demo.Serve", Callee: "demo.Process", Path: "serve.go", Line: 9},
		}},
	}}
	eng := engine.New(mapContexts{compareV1.ID: single(compareV1), compareV2.ID: single(compareV2)}, nil, graph, nil)

	out, err := eng.CompareCalls(context.Background(), compareRequest())
	if err != nil {
		t.Fatalf("CompareCalls with no search or source provider: %v", err)
	}

	// Both versions were asked the same question, each in its own scope.
	if !slices.Equal(graph.asked, []string{compareV1.ID, compareV2.ID}) {
		t.Fatalf("the graph provider was asked %v, want each version once", graph.asked)
	}
	want := provider.TraceRequest{Symbol: "Process", Direction: provider.Callers, Depth: 2}
	for _, got := range graph.reqs {
		if got != want {
			t.Fatalf("a side was walked with %+v, want both walked with %+v", got, want)
		}
	}

	if out.Presence() != engine.PresenceBoth {
		t.Fatalf("presence is %q, want %q", out.Presence(), engine.PresenceBoth)
	}
	// A symbol can resolve differently in each version and still be the symbol
	// the caller asked about, so the two are reported apart.
	if out.FromResolvedSymbol() != "demo.Process" || out.ToResolvedSymbol() != "demo.Handle" {
		t.Errorf("resolved symbols are from=%q to=%q, want each version's own", out.FromResolvedSymbol(), out.ToResolvedSymbol())
	}
	assertRelations(t, out.Removed(), "removed", "demo.Main->demo.Process@main.go")
	assertRelations(t, out.Added(), "added", "demo.Main->demo.Handle@main.go")
	assertRelations(t, out.Unchanged(), "unchanged", "demo.Serve->demo.Process@serve.go")

	// Each side reports its own version and cites it, and neither borrows the
	// other's: the same line number in two versions is two different lines.
	if out.From().Context() != compareV1 || out.To().Context() != compareV2 {
		t.Fatalf("the sides report from=%+v to=%+v, want the two versions compared", out.From().Context(), out.To().Context())
	}
	wantCites := []evidence.Evidence{evidence.At("main.go", 4, 4, ""), evidence.At("serve.go", 9, 9, "")}
	if !slices.Equal(out.From().Evidence(), wantCites) || !slices.Equal(out.To().Evidence(), wantCites) {
		t.Fatalf("the sides cite from=%+v to=%+v, want each version's own call sites %+v",
			out.From().Evidence(), out.To().Evidence(), wantCites)
	}
}
