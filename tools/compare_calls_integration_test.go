//go:build integration

package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// compare_calls over a real MCP session against the two real codebase-memory-mcp
// projects testdata/prepare-fixture.sh builds, one per release. There is no fake
// graph engine: the property under test — that each side is walked in its own
// version's graph and what comes back is the difference between the two — is a
// property of those two real projects, and a mock would only prove the tool
// agrees with our own idea of CBM.

// TestCompareCallsReportsAddedAndRemovedEdges is the handler swap seen from the
// symbol both versions keep. Process is in both graphs and the one call it makes
// is a different call in each, which is one added relation and one removed one —
// never a "changed" one, and never a relation carrying the other version's call
// site, which is what a comparison answered from a single graph looks like.
func TestCompareCallsReportsAddedAndRemovedEdges(t *testing.T) {
	cfg := traceFixture(t)

	got, raw := compareCallsOnFixture(t, paritySession(t, cfg), map[string]any{
		"from_context": v1, "to_context": v2,
		"symbol": "Process", "direction": "callees", "depth": 1,
	})

	if got.Presence != "BOTH" {
		t.Errorf("presence = %q, want BOTH: both releases declare Process", got.Presence)
	}
	if got.RequestedSymbol != "Process" || got.FromResolvedSymbol == "" || got.ToResolvedSymbol == "" {
		t.Errorf("requested %q, resolved %q and %q, want both versions to have resolved it",
			got.RequestedSymbol, got.FromResolvedSymbol, got.ToResolvedSymbol)
	}
	assertComparedSide(t, "from", got.From, raw, true, cfg.Contexts[v1])
	assertComparedSide(t, "to", got.To, raw, true, cfg.Contexts[v2])

	// The v2 call, which only the to version makes.
	added := relationIn(t, got.Added, "added", "Process", "NewHandler")
	if added.Path != "processor.go" {
		t.Errorf("the added relation is written in %q, want processor.go", added.Path)
	}
	assertCites(t, added, "from", false)
	assertCites(t, added, "to", true)

	// And the v1 call, which only the from version makes.
	removed := relationIn(t, got.Removed, "removed", "Process", "LegacyHandler")
	if removed.Path != "processor.go" {
		t.Errorf("the removed relation is written in %q, want processor.go", removed.Path)
	}
	assertCites(t, removed, "from", true)
	assertCites(t, removed, "to", false)

	// Neither handler may turn up as unchanged: that would be the two graphs
	// having been read as one.
	for _, rel := range got.Unchanged {
		if rel.Callee == "NewHandler" || rel.Callee == "LegacyHandler" {
			t.Errorf("%s -> %s is reported unchanged, but each release calls only its own handler", rel.Caller, rel.Callee)
		}
	}
	t.Logf("compare_calls(%s, %s, Process) = %s", v1, v2, raw)
}

// TestCompareCallsReportsAnUnchangedEdge is what keeps the test above honest: a
// comparison reporting everything as added and removed would pass it.
//
// Keep -> SharedHandler is committed on main before either release is cut, so
// both graphs hold the same call in the same file. It has to come back as
// unchanged, and carrying both versions' call sites rather than one merged list.
func TestCompareCallsReportsAnUnchangedEdge(t *testing.T) {
	cfg := traceFixture(t)

	got, raw := compareCallsOnFixture(t, paritySession(t, cfg), map[string]any{
		"from_context": v1, "to_context": v2,
		"symbol": "Keep", "direction": "callees", "depth": 1,
	})

	if got.Presence != "BOTH" {
		t.Errorf("presence = %q, want BOTH: both releases declare Keep", got.Presence)
	}
	assertComparedSide(t, "from", got.From, raw, true, cfg.Contexts[v1])
	assertComparedSide(t, "to", got.To, raw, true, cfg.Contexts[v2])

	unchanged := relationIn(t, got.Unchanged, "unchanged", "Keep", "SharedHandler")
	if unchanged.Path != "shared.go" {
		t.Errorf("the unchanged relation is written in %q, want shared.go", unchanged.Path)
	}
	assertCites(t, unchanged, "from", true)
	assertCites(t, unchanged, "to", true)

	if len(got.Added) != 0 || len(got.Removed) != 0 {
		t.Errorf("added = %+v and removed = %+v, want neither: nothing about Keep changed between the releases", got.Added, got.Removed)
	}
	t.Logf("compare_calls(%s, %s, Keep) = %s", v1, v2, raw)
}

// TestCompareCallsReportsASymbolOnlyOneVersionHas is the presence half of the
// contract, both ways round. A symbol one release does not declare is an answer
// rather than a failure — the version that has it is reported and the version
// that does not is null — and asking in both directions is what stops the
// absence being one context simply answering nothing for everything.
func TestCompareCallsReportsASymbolOnlyOneVersionHas(t *testing.T) {
	cfg := traceFixture(t)
	session := paritySession(t, cfg)

	tests := map[string]struct {
		symbol, presence, list string
		from, to               bool
	}{
		"NewHandler is declared on release/v2 alone":    {"NewHandler", "TO_ONLY", "added", false, true},
		"LegacyHandler is declared on release/v1 alone": {"LegacyHandler", "FROM_ONLY", "removed", true, false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, raw := compareCallsOnFixture(t, session, map[string]any{
				"from_context": v1, "to_context": v2,
				"symbol": tc.symbol, "direction": "callers", "depth": 1,
			})

			if got.Presence != tc.presence {
				t.Errorf("presence = %q, want %s", got.Presence, tc.presence)
			}
			assertComparedSide(t, "from", got.From, raw, tc.from, cfg.Contexts[v1])
			assertComparedSide(t, "to", got.To, raw, tc.to, cfg.Contexts[v2])

			// The version without the symbol resolved nothing, and says so with an
			// empty string rather than by echoing the request back.
			if got.RequestedSymbol != tc.symbol {
				t.Errorf("requested_symbol = %q, want %s", got.RequestedSymbol, tc.symbol)
			}
			if (tc.from && got.FromResolvedSymbol == "") || (!tc.from && got.FromResolvedSymbol != "") {
				t.Errorf("from_resolved_symbol = %q on a %s comparison", got.FromResolvedSymbol, tc.presence)
			}
			if (tc.to && got.ToResolvedSymbol == "") || (!tc.to && got.ToResolvedSymbol != "") {
				t.Errorf("to_resolved_symbol = %q on a %s comparison", got.ToResolvedSymbol, tc.presence)
			}

			// The whole of the one surviving graph is on one side of the diff: the
			// call into the handler is added where only the to version has it, and
			// removed where only the from version does.
			relations, other := got.Added, got.Removed
			if tc.list == "removed" {
				relations, other = got.Removed, got.Added
			}
			rel := relationIn(t, relations, tc.list, "Process", tc.symbol)
			if rel.Path != "processor.go" {
				t.Errorf("the %s relation is written in %q, want processor.go", tc.list, rel.Path)
			}
			assertCites(t, rel, "from", tc.from)
			assertCites(t, rel, "to", tc.to)
			if len(other) != 0 || len(got.Unchanged) != 0 {
				t.Errorf("the other lists are %+v and %+v, want both empty: only one version has this symbol at all", other, got.Unchanged)
			}
			t.Logf("compare_calls(%s, %s, %s) = %s", v1, v2, tc.symbol, raw)
		})
	}
}

// compareCallsOnFixture makes one call and decodes what came back, failing the
// test unless the tool answered. compare_calls_test.go's own helper fixes the
// direction and the depth, which these tests vary.
func compareCallsOnFixture(t *testing.T, session *mcp.ClientSession, args map[string]any) (compareCallsWire, string) {
	t.Helper()

	res, raw := compareRaw(t, session, "compare_calls", args)
	if res.IsError {
		t.Fatalf("compare_calls(%v) failed: %s", args, raw)
	}
	var got compareCallsWire
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	// The graph reference is the CBM project behind a context: internal, and a
	// tool's output is the only place it could leak from.
	if strings.Contains(raw, "graph_ref") || strings.Contains(raw, "vacmcp-demo-") {
		t.Errorf("compare_calls leaked the graph reference: %s", raw)
	}
	return got, raw
}

// relationIn returns the caller -> callee relation from one of the three lists,
// failing the test when that list does not report it.
func relationIn(t *testing.T, list []callRelationWire, name, caller, callee string) callRelationWire {
	t.Helper()
	for _, rel := range list {
		if rel.Caller == caller && rel.Callee == callee {
			return rel
		}
	}
	t.Fatalf("%s = %+v, want it to report %s -> %s", name, list, caller, callee)
	return callRelationWire{}
}

// assertCites holds one relation's citations in one version to what that version
// has: the call sites where it makes the call, and nothing at all where it does
// not. A relation cited on the side that does not have it would be the other
// version's line reported as this one's.
func assertCites(t *testing.T, rel callRelationWire, which string, cited bool) {
	t.Helper()
	cites := rel.FromEvidence
	if which == "to" {
		cites = rel.ToEvidence
	}
	if !cited {
		if len(cites) != 0 {
			t.Errorf("%s -> %s cites %+v on the %s side, want nothing: that version does not make this call",
				rel.Caller, rel.Callee, cites, which)
		}
		return
	}
	if len(cites) == 0 {
		t.Errorf("%s -> %s cites nothing on the %s side, want the call site it is written at", rel.Caller, rel.Callee, which)
	}
	for _, cite := range cites {
		if cite.Location.Path == "" || cite.Location.StartLine < 1 {
			t.Errorf("%s -> %s cites %+v on the %s side, which locates nothing", rel.Caller, rel.Callee, cite, which)
		}
	}
}
