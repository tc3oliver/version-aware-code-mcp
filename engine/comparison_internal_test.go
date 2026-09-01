package engine

import (
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/evidence"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
)

// The construction path the external tests cannot reach: inside this package a
// side is the same composite literal every result type here is built with, so a
// present side is exactly a side that was handed a version.
//
// What it checks after that is the invariant the doc comment states: each side
// reports its own version and its own citations, and neither borrows the
// other's. A test cannot stop a later method flattening the two lists into one,
// but it can pin down that the type has nowhere to flatten them into.
func TestASideBuiltHereKeepsItsOwnVersionAndCitations(t *testing.T) {
	fromCtx := vacctx.CodeContext{ID: "demo@v1", Repository: "demo", Branch: "v1", Revision: "8af31e2"}
	toCtx := vacctx.CodeContext{ID: "demo@v2", Repository: "demo", Branch: "v2", Revision: "94cb821"}
	fromCite := evidence.At("handler.go", 42, 44, "")
	toCite := evidence.At("handler.go", 51, 55, "")

	from := ComparisonSide{inOneMember(fromCtx, []evidence.Evidence{fromCite})}
	to := ComparisonSide{inOneMember(toCtx, []evidence.Evidence{toCite})}

	for _, side := range []struct {
		name    string
		side    ComparisonSide
		codeCtx vacctx.CodeContext
		cite    evidence.Evidence
	}{
		{"from", from, fromCtx, fromCite},
		{"to", to, toCtx, toCite},
	} {
		if !side.side.Present() {
			t.Errorf("the %s side was built with a version and reports Present false", side.name)
		}
		// One member, and it is this side's own version: a side is a workspace
		// like every other answer here, and the one it is answered in has the
		// version it was built with and nothing of the other side's.
		if got := side.side.Context(); got.ID != side.codeCtx.ID ||
			len(got.Members) != 1 || got.Members[0] != side.codeCtx {
			t.Errorf("the %s side reports context %+v, want a workspace of only %+v", side.name, got, side.codeCtx)
		}
		got := side.side.Evidence()
		if len(got) != 1 || len(got[0]) != 1 || got[0][0] != side.cite {
			t.Errorf("the %s side cites %+v, want only %+v: a side cites its own version and no other",
				side.name, got, side.cite)
		}
	}

	// The zero value is the absent side, and it is the only side this package
	// can build without naming a version.
	if (ComparisonSide{}).Present() {
		t.Error("the zero side reports Present true")
	}
}
