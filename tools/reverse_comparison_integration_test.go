//go:build integration

package tools

import (
	"slices"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
)

// A comparison has a direction, and the two directions are one fact told twice.
// What release/v2 added, release/v1 removed; what one release calls and the
// other does not is added one way round and removed the other; and what neither
// changed is unchanged either way. This file is the evidence that vacmcp says
// so — that from and to are the caller's to choose rather than a bias in the
// answer.
//
// It matters because a comparison that only ever ran one way round could get
// the direction backwards and never be caught: demo-v1 -> demo-v2 reporting
// newonly.go as ADDED is right, and so is an implementation that reports
// everything the *to* side has as ADDED regardless of which context that is.
// Asking both ways is what tells the two apart.
//
// It goes through [engine.Engine] rather than MCP, for the reason
// concurrency_integration_test.go gives for the same choice: the property is
// the comparison logic's, not the adapter's, and
// TestEngineAndMCPCompareIdentically already holds the tool to what the engine
// decides. Reading typed results here also keeps the assertions on the sides
// and the classification rather than on a JSON round trip that would prove
// nothing extra. It lives in this package because the prepared fixture does.

// TestComparingTheOtherWayRoundInvertsTheCodeAnswer is the file half: a file
// only one release has is ADDED when compared towards that release and REMOVED
// when compared away from it, with the side that has it moving with the answer.
func TestComparingTheOtherWayRoundInvertsTheCodeAnswer(t *testing.T) {
	cfg := parityFixture(t)
	eng := parityEngine(t, cfg)

	// One file per release, so a comparison biased towards either context fails
	// on one of them rather than passing on the one that agrees with the bias.
	tests := map[string]struct {
		path              string
		forward, backward engine.CodeChange
	}{
		"a file only release/v2 ever had": {"newonly.go", engine.CodeAdded, engine.CodeRemoved},
		"a file only release/v1 ever had": {"oldonly.go", engine.CodeRemoved, engine.CodeAdded},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			forward := compareCodeBetween(t, eng, v1, v2, tc.path)
			backward := compareCodeBetween(t, eng, v2, v1, tc.path)

			if forward.Change() != tc.forward || backward.Change() != tc.backward {
				t.Errorf("%s is %s from %s to %s and %s the other way round, want %s and %s",
					tc.path, forward.Change(), v1, v2, backward.Change(), tc.forward, tc.backward)
			}
			if forward.Path() != tc.path || backward.Path() != tc.path {
				t.Errorf("%s came back as %q one way and %q the other", tc.path, forward.Path(), backward.Path())
			}

			// The sides swapped with the answer. ADDED has no from side and
			// REMOVED has no to side, so the version that has the file is the
			// present one in both directions — and it is a different side of the
			// result each time, which is the whole claim.
			assertSideAt(t, "forward from", forward.From(), tc.forward != engine.CodeAdded, v1)
			assertSideAt(t, "forward to", forward.To(), tc.forward != engine.CodeRemoved, v2)
			assertSideAt(t, "backward from", backward.From(), tc.backward != engine.CodeAdded, v2)
			assertSideAt(t, "backward to", backward.To(), tc.backward != engine.CodeRemoved, v1)

			t.Logf("compare_code(%s) = %s one way and %s the other", tc.path, forward.Change(), backward.Change())
		})
	}
}

// TestComparingTheOtherWayRoundInvertsTheCallAnswer is the call graph half:
// every relation added one way round is removed the other, and every unchanged
// one stays unchanged.
//
// Process and Keep are both asked, because each on its own leaves half the
// property untested — Process has nothing unchanged to keep there, and Keep has
// nothing added or removed to swap.
func TestComparingTheOtherWayRoundInvertsTheCallAnswer(t *testing.T) {
	cfg := parityFixture(t)
	eng := parityEngine(t, cfg)

	tests := map[string]struct {
		symbol                  string
		wantSwapped, wantStable []string
	}{
		"the handler each release delegates to": {
			symbol:      "Process",
			wantSwapped: []string{"Process -> LegacyHandler"},
		},
		"the call both releases inherited from main": {
			symbol:     "Keep",
			wantStable: []string{"Keep -> SharedHandler"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			forward := compareCallsBetween(t, eng, v1, v2, tc.symbol)
			backward := compareCallsBetween(t, eng, v2, v1, tc.symbol)

			// The property itself, as sets of relations: what one direction
			// removed the other added, and the other way round.
			assertSameRelations(t, "what "+v1+" -> "+v2+" removed", forward.Removed(), "what "+v2+" -> "+v1+" added", backward.Added())
			assertSameRelations(t, "what "+v1+" -> "+v2+" added", forward.Added(), "what "+v2+" -> "+v1+" removed", backward.Removed())
			assertSameRelations(t, "what "+v1+" -> "+v2+" left unchanged", forward.Unchanged(), "what "+v2+" -> "+v1+" left unchanged", backward.Unchanged())

			// And the sets are the ones the fixture wrote, so a comparison
			// reporting nothing at all in either direction does not satisfy the
			// symmetry above by reporting nothing twice.
			if got := relationNames(forward.Removed()); !slices.Equal(got, tc.wantSwapped) {
				t.Errorf("%s -> %s removed %v, want %v", v1, v2, got, tc.wantSwapped)
			}
			if got := relationNames(forward.Unchanged()); !slices.Equal(got, tc.wantStable) {
				t.Errorf("%s -> %s left %v unchanged, want %v", v1, v2, got, tc.wantStable)
			}

			// The citations swapped with the relations: a relation only one
			// version has is cited on that version's side of the result, which
			// is a different side in each direction.
			for _, rel := range forward.Removed() {
				mirror := relationNamed(t, backward.Added(), rel.Caller, rel.Callee)
				if !slices.Equal(rel.FromEvidence, mirror.ToEvidence) || len(rel.ToEvidence) != 0 || len(mirror.FromEvidence) != 0 {
					t.Errorf("%s -> %s cites %+v/%+v one way and %+v/%+v the other, want the same call sites on the side that has them",
						rel.Caller, rel.Callee, rel.FromEvidence, rel.ToEvidence, mirror.FromEvidence, mirror.ToEvidence)
				}
			}

			// Presence follows the direction too: a symbol only the to version
			// has is TO_ONLY one way and FROM_ONLY the other.
			if forward.Presence() != engine.PresenceBoth || backward.Presence() != engine.PresenceBoth {
				t.Errorf("presence is %s one way and %s the other, want BOTH either way: both releases declare %s",
					forward.Presence(), backward.Presence(), tc.symbol)
			}
			t.Logf("compare_calls(%s): removed %v one way and added them the other", tc.symbol, relationNames(forward.Removed()))
		})
	}
}

// compareCodeBetween compares one path in one direction, failing the test unless
// the engine answered.
func compareCodeBetween(t *testing.T, eng *engine.Engine, from, to, path string) engine.CompareCodeResult {
	t.Helper()
	result, err := eng.CompareCode(t.Context(), engine.CompareCodeRequest{FromContext: from, ToContext: to, Path: path})
	if err != nil {
		t.Fatalf("CompareCode(%s, %s, %s): %v", from, to, path, err)
	}
	return result
}

// compareCallsBetween walks one symbol's callees in one direction. Depth 1
// because the property is about the relations directly around the symbol, and a
// deeper walk would only add relations that are the same in both versions.
func compareCallsBetween(t *testing.T, eng *engine.Engine, from, to, symbol string) engine.CompareCallsResult {
	t.Helper()
	result, err := eng.CompareCalls(t.Context(), engine.CompareCallsRequest{
		FromContext: from, ToContext: to, Symbol: symbol, Direction: provider.Callees, Depth: 1,
	})
	if err != nil {
		t.Fatalf("CompareCalls(%s, %s, %s): %v", from, to, symbol, err)
	}
	return result
}

// assertSideAt holds one side to the version it should carry, or to being
// absent.
func assertSideAt(t *testing.T, which string, side engine.ComparisonSide, present bool, contextID string) {
	t.Helper()
	if !present {
		if side.Present() {
			t.Errorf("the %s side is %+v, want it absent: that version does not have this file", which, side.Context())
		}
		return
	}
	if !side.Present() {
		t.Errorf("the %s side is absent, want it answered in %s", which, contextID)
		return
	}
	if got := side.Context().ID; got != contextID {
		t.Errorf("the %s side was answered in context %q, want %q", which, got, contextID)
	}
}

// assertSameRelations requires two classification lists to hold the same
// relations. Order is not compared: the two directions list from's relations
// first and from is a different version in each, so the same set legitimately
// comes back in a different order.
func assertSameRelations(t *testing.T, whatA string, a []engine.CallRelation, whatB string, b []engine.CallRelation) {
	t.Helper()
	gotA, gotB := relationNames(a), relationNames(b)
	slices.Sort(gotA)
	slices.Sort(gotB)
	if !slices.Equal(gotA, gotB) {
		t.Errorf("%s is %v and %s is %v, want the same relations", whatA, gotA, whatB, gotB)
	}
}

// relationNames names a classification list as the calls it holds.
func relationNames(list []engine.CallRelation) []string {
	named := make([]string, 0, len(list))
	for _, rel := range list {
		named = append(named, rel.Caller+" -> "+rel.Callee)
	}
	return named
}

// relationNamed returns the caller -> callee relation from a list, failing the
// test when it is not there.
func relationNamed(t *testing.T, list []engine.CallRelation, caller, callee string) engine.CallRelation {
	t.Helper()
	for _, rel := range list {
		if rel.Caller == caller && rel.Callee == callee {
			return rel
		}
	}
	t.Fatalf("%+v does not report %s -> %s", list, caller, callee)
	return engine.CallRelation{}
}
