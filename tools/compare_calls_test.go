package tools

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/evidence"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

const comparedSymbol = "Process"

// compareGraph is a graph backend holding one call graph per version, keyed by
// context ID. A version it has no graph for does not have the symbol, which is
// SYMBOL_NOT_FOUND from a real backend and is how a one-sided comparison is set
// up here.
type compareGraph map[string]*provider.CallGraph

func (g compareGraph) TraceCalls(_ context.Context, codeCtx vacctx.CodeContext, req provider.TraceRequest) (*provider.CallGraph, error) {
	graph, ok := g[codeCtx.ID]
	if !ok {
		return nil, vacerr.New(
			vacerr.SymbolNotFound,
			"no symbol "+req.Symbol+" in "+codeCtx.ID,
			map[string]any{"symbol": req.Symbol, "context": codeCtx.ID},
		)
	}
	return graph, nil
}

// The two versions' call graphs. Process calls a handler that was replaced and a
// logger that was not, and the surviving call moved down a line — so an
// unchanged relation still has a different call site in each version, which is
// exactly what a merged evidence list could not report.
var (
	compareFromGraph = &provider.CallGraph{Symbol: "demo.Process", Edges: []provider.CallEdge{
		{Caller: "Process", Callee: "LegacyHandler", Path: comparedPath, Line: 5},
		{Caller: "Process", Callee: "Log", Path: comparedPath, Line: 6},
	}}
	compareToGraph = &provider.CallGraph{Symbol: "demo.Process", Edges: []provider.CallEdge{
		{Caller: "Process", Callee: "NewHandler", Path: comparedPath, Line: 5},
		{Caller: "Process", Callee: "Log", Path: comparedPath, Line: 7},
	}}
)

type callRelationWire struct {
	Caller       string              `json:"caller"`
	Callee       string              `json:"callee"`
	Path         string              `json:"path"`
	FromEvidence []evidence.Evidence `json:"from_evidence"`
	ToEvidence   []evidence.Evidence `json:"to_evidence"`
}

type compareCallsWire struct {
	From               *comparisonSideWire `json:"from"`
	To                 *comparisonSideWire `json:"to"`
	Presence           string              `json:"presence"`
	RequestedSymbol    string              `json:"requested_symbol"`
	FromResolvedSymbol string              `json:"from_resolved_symbol"`
	ToResolvedSymbol   string              `json:"to_resolved_symbol"`
	Added              []callRelationWire  `json:"added"`
	Removed            []callRelationWire  `json:"removed"`
	Unchanged          []callRelationWire  `json:"unchanged"`
}

// AC #2: the input schema accepts the two context ids, the symbol, the
// direction, the depth and the repository to compare in, and nothing else. A
// branch or revision property here would let a caller walk a graph the
// configuration never granted it.
//
// repository is not that: it selects one repository both contexts already name,
// which is what a context naming several needs to be walked at all, and a
// repository neither names is refused rather than reached.
func TestCompareCallsInputSchemaIsTwoContextsAndOneQuestion(t *testing.T) {
	tool := comparisonTool(t, "compare_calls")

	raw, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatalf("marshal input schema: %v", err)
	}
	var schema struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("decode input schema %s: %v", raw, err)
	}
	t.Logf("compare_calls input schema = %s", raw)

	want := []string{"depth", "direction", "from_context", "repository", "symbol", "to_context"}
	if got := slices.Sorted(maps.Keys(schema.Properties)); !slices.Equal(got, want) {
		t.Errorf("input schema has the properties %v, want exactly %v", got, want)
	}
	for _, forbidden := range []string{"branch", "revision"} {
		if _, ok := schema.Properties[forbidden]; ok {
			t.Errorf("input schema accepts a %s override: %s", forbidden, raw)
		}
	}
}

// AC #3: a successful comparison carries both sides, each with its own full
// context and its own citations, and every relation keeps the call sites of each
// version apart too. handler.go:6 in one version and handler.go:7 in the other
// are two different lines that happen to name one relation, so a merged list
// could not say which version either came from.
func TestCompareCallsReportsBothSidesSeparately(t *testing.T) {
	session := compareCallsSession(t, compareGraph{compareV1.ID: compareFromGraph, compareV2.ID: compareToGraph})
	got, raw := compareCallsCall(t, session, compareV1.ID, compareV2.ID, comparedSymbol)
	t.Logf("compare_calls BOTH wire = %s", raw)

	keys := topLevelKeys(t, raw)
	want := []string{"added", "from", "from_resolved_symbol", "presence", "removed", "requested_symbol", "to", "to_resolved_symbol", "unchanged"}
	if !slices.Equal(keys, want) {
		t.Errorf("result has the keys %v, want exactly %v", keys, want)
	}

	if got.From == nil || got.To == nil {
		t.Fatalf("a symbol both versions have reported a side as absent: %s", raw)
	}
	assertSideContext(t, "from", got.From, compareV1)
	assertSideContext(t, "to", got.To, compareV2)

	// Each side cites the call sites of its own version, and only those. The
	// shared relation is written on line 6 in one version and line 7 in the
	// other, so a merged list would be a citation nobody could check.
	fromCites := []evidence.Evidence{evidence.At(comparedPath, 5, 5, ""), evidence.At(comparedPath, 6, 6, "")}
	toCites := []evidence.Evidence{evidence.At(comparedPath, 5, 5, ""), evidence.At(comparedPath, 7, 7, "")}
	if !slices.Equal(got.From.Evidence, fromCites) {
		t.Errorf("from.evidence = %+v, want %+v", got.From.Evidence, fromCites)
	}
	if !slices.Equal(got.To.Evidence, toCites) {
		t.Errorf("to.evidence = %+v, want %+v", got.To.Evidence, toCites)
	}

	if got.Presence != string(engine.PresenceBoth) {
		t.Errorf("presence = %q, want BOTH", got.Presence)
	}
	if got.RequestedSymbol != comparedSymbol {
		t.Errorf("requested_symbol = %q, want the symbol as it was asked for, %q", got.RequestedSymbol, comparedSymbol)
	}
	if got.FromResolvedSymbol != compareFromGraph.Symbol || got.ToResolvedSymbol != compareToGraph.Symbol {
		t.Errorf("resolved symbols = %q/%q, want %q/%q", got.FromResolvedSymbol, got.ToResolvedSymbol, compareFromGraph.Symbol, compareToGraph.Symbol)
	}

	assertRelations(t, "removed", got.Removed, []callRelationWire{{
		Caller: "Process", Callee: "LegacyHandler", Path: comparedPath,
		FromEvidence: []evidence.Evidence{evidence.At(comparedPath, 5, 5, "")},
		ToEvidence:   []evidence.Evidence{},
	}})
	assertRelations(t, "added", got.Added, []callRelationWire{{
		Caller: "Process", Callee: "NewHandler", Path: comparedPath,
		FromEvidence: []evidence.Evidence{},
		ToEvidence:   []evidence.Evidence{evidence.At(comparedPath, 5, 5, "")},
	}})
	assertRelations(t, "unchanged", got.Unchanged, []callRelationWire{{
		Caller: "Process", Callee: "Log", Path: comparedPath,
		FromEvidence: []evidence.Evidence{evidence.At(comparedPath, 6, 6, "")},
		ToEvidence:   []evidence.Evidence{evidence.At(comparedPath, 7, 7, "")},
	}})

	if strings.Contains(raw, "graph") {
		t.Errorf("compare_calls leaked the graph reference: %s", raw)
	}
}

// AC #3, the other half: the version that does not have the symbol is null, in
// both directions, and cannot be mistaken for a side carrying a context and
// citations.
func TestCompareCallsAbsentSideIsNull(t *testing.T) {
	for name, tc := range map[string]struct {
		graphs   compareGraph
		presence string
		present  vacctx.CodeContext
		absent   string
	}{
		"only the to version has it":   {compareGraph{compareV2.ID: compareToGraph}, "TO_ONLY", compareV2, "from"},
		"only the from version has it": {compareGraph{compareV1.ID: compareFromGraph}, "FROM_ONLY", compareV1, "to"},
	} {
		t.Run(name, func(t *testing.T) {
			got, raw := compareCallsCall(t, compareCallsSession(t, tc.graphs), compareV1.ID, compareV2.ID, comparedSymbol)
			t.Logf("compare_calls %s wire = %s", tc.presence, raw)

			if got.Presence != tc.presence {
				t.Errorf("presence = %q, want %q", got.Presence, tc.presence)
			}
			absent, present := got.From, got.To
			if tc.absent == "to" {
				absent, present = got.To, got.From
			}
			if absent != nil {
				t.Errorf("the %s side is %+v, want null: the version does not have the symbol", tc.absent, absent)
			}
			if present == nil {
				t.Fatalf("the version that has the symbol reported no side at all: %s", raw)
			}
			assertSideContext(t, "present", present, tc.present)
			if !strings.Contains(raw, `"`+tc.absent+`":null`) {
				t.Errorf("the %s side is not on the wire as null: %s", tc.absent, raw)
			}

			// The surviving version's whole graph is on one side of the diff, and
			// the relations still keep the two versions' citations apart: the side
			// that has nothing cites nothing.
			one, other := got.Added, got.Removed
			if tc.absent == "to" {
				one, other = got.Removed, got.Added
			}
			if len(one) != 2 || len(other) != 0 || len(got.Unchanged) != 0 {
				t.Fatalf("relations = %d/%d added/removed and %d unchanged, want the whole graph on one side: %s", len(got.Added), len(got.Removed), len(got.Unchanged), raw)
			}
			for _, rel := range one {
				empty, cited := rel.FromEvidence, rel.ToEvidence
				if tc.absent == "to" {
					empty, cited = rel.ToEvidence, rel.FromEvidence
				}
				if len(empty) != 0 {
					t.Errorf("%s->%s cites %+v in a version that does not have the symbol", rel.Caller, rel.Callee, empty)
				}
				if len(cited) == 0 {
					t.Errorf("%s->%s cites nothing in the version that has it: %s", rel.Caller, rel.Callee, raw)
				}
			}
		})
	}
}

// AC #4: every typed error reaches the client as the error model's own envelope,
// with the code intact and no half-answer beside it.
func TestCompareCallsTypedErrorsRoundTrip(t *testing.T) {
	contexts := compareContexts{compareV1.ID: single(compareV1), compareV2.ID: single(compareV2), compareOther.ID: single(compareOther)}
	graphs := compareGraph{compareV1.ID: compareFromGraph, compareV2.ID: compareToGraph, compareOther.ID: compareToGraph}

	for name, tc := range map[string]struct {
		eng  *engine.Engine
		args map[string]any
		want vacerr.Code
	}{
		"cross repository": {
			engine.New(contexts, nil, graphs, nil),
			map[string]any{"from_context": compareV1.ID, "to_context": compareOther.ID, "symbol": comparedSymbol, "direction": "callees", "depth": 2},
			vacerr.InvalidArgument,
		},
		"depth out of range": {
			engine.New(contexts, nil, graphs, nil),
			map[string]any{"from_context": compareV1.ID, "to_context": compareV2.ID, "symbol": comparedSymbol, "direction": "callees", "depth": 9},
			vacerr.InvalidArgument,
		},
		"unknown context": {
			engine.New(contexts, nil, graphs, nil),
			map[string]any{"from_context": compareV1.ID, "to_context": "demo-v9", "symbol": comparedSymbol, "direction": "callees", "depth": 2},
			vacerr.ContextNotFound,
		},
		"neither version has the symbol": {
			engine.New(contexts, nil, compareGraph{}, nil),
			map[string]any{"from_context": compareV1.ID, "to_context": compareV2.ID, "symbol": "Vanished", "direction": "callees", "depth": 2},
			vacerr.SymbolNotFound,
		},
		"no graph provider": {
			engine.New(contexts, nil, nil, nil),
			map[string]any{"from_context": compareV1.ID, "to_context": compareV2.ID, "symbol": comparedSymbol, "direction": "callees", "depth": 2},
			vacerr.GraphProviderUnavailable,
		},
	} {
		t.Run(name, func(t *testing.T) {
			vErr, raw := compareError(t, compareSession(t, tc.eng), "compare_calls", tc.args)
			t.Logf("%s -> %s", name, raw)

			if vErr.Code != tc.want {
				t.Errorf("code = %q, want %q", vErr.Code, tc.want)
			}
			for _, leak := range []string{`"from"`, `"to"`, `"presence"`, `"added"`} {
				if strings.Contains(raw, leak) {
					t.Errorf("error result carries %s: %s", leak, raw)
				}
			}
		})
	}
}

// compareCallsSession serves the comparison tools over an engine whose only
// provider is the graph: comparing calls reaches neither source nor an index,
// and building it with those two nil is what keeps it that way.
func compareCallsSession(t *testing.T, graphs compareGraph) *mcp.ClientSession {
	t.Helper()
	return compareSession(t, engine.New(
		compareContexts{compareV1.ID: single(compareV1), compareV2.ID: single(compareV2), compareOther.ID: single(compareOther)},
		nil, graphs, nil,
	))
}

// compareCallsCall calls compare_calls and requires it to have succeeded.
func compareCallsCall(t *testing.T, session *mcp.ClientSession, from, to, symbol string) (compareCallsWire, string) {
	t.Helper()

	res, raw := compareRaw(t, session, "compare_calls", map[string]any{
		"from_context": from, "to_context": to, "symbol": symbol, "direction": "callees", "depth": 2,
	})
	if res.IsError {
		t.Fatalf("compare_calls(%s, %s, %s) failed: %s", from, to, symbol, raw)
	}
	var got compareCallsWire
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return got, raw
}

// assertRelations checks one of the three relation lists, call sites included.
func assertRelations(t *testing.T, name string, got, want []callRelationWire) {
	t.Helper()

	if !slices.EqualFunc(got, want, func(a, b callRelationWire) bool {
		return a.Caller == b.Caller && a.Callee == b.Callee && a.Path == b.Path &&
			slices.Equal(a.FromEvidence, b.FromEvidence) && slices.Equal(a.ToEvidence, b.ToEvidence)
	}) {
		t.Errorf("%s = %+v, want %+v", name, got, want)
	}
}
