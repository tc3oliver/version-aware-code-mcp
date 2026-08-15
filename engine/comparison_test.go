package engine_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"slices"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
)

// A comparison side is the same bargain as the result types: every field is
// unexported, so no composite literal outside the package can fill one in. The
// only side a caller can write for itself is the zero one, which reports
// Present false and carries neither a version nor a citation — so a side
// claiming a version is always one an engine method built.
func TestAComparisonSideCannotBeBuiltOutsideTheEngine(t *testing.T) {
	typ := reflect.TypeOf(engine.ComparisonSide{})
	for i := range typ.NumField() {
		if field := typ.Field(i); field.IsExported() {
			t.Errorf("ComparisonSide.%s is exported: a caller can build a side claiming a version it never queried",
				field.Name)
		}
	}

	// contextual is satisfied by ComparisonSide, so this is also the compile-time
	// check that a side answers Context and Evidence like every other result.
	var side engine.ComparisonSide
	assertNotAnAnswer(t, side)
	if side.Present() {
		t.Error("the zero ComparisonSide reports Present true, so a caller can claim a side was found by writing one")
	}
}

// The absent side is a value a comparison returns, not a broken one: only one
// context had the symbol, and the other side says so by answering nothing at
// all. Reading it is therefore normal and must not panic.
func TestAnAbsentComparisonSideAnswersNothing(t *testing.T) {
	var absent engine.ComparisonSide

	if absent.Present() {
		t.Fatal("the absent side reports Present true")
	}
	if got := absent.Context(); got != (vacctx.CodeContext{}) {
		t.Errorf("the absent side reports context %+v, want the zero context", got)
	}
	if got := absent.Evidence(); got != nil {
		t.Errorf("the absent side reports evidence %+v, want none: it had nothing to cite", got)
	}
}

// declared reports the names of every constant of type typeName declared in
// comparison.go.
//
// An enum in Go is a convention rather than a type, so asserting the expected
// values exist would never notice a fifth one declared beside them — and a
// fifth value is a new outcome a caller has to handle, arriving without anyone
// deciding to add it. The source is read back for that reason and no other.
func declared(t *testing.T, typeName string) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "comparison.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing comparison.go: %v", err)
	}

	var names []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if ident, ok := value.Type.(*ast.Ident); !ok || ident.Name != typeName {
				continue
			}
			for _, name := range value.Names {
				names = append(names, name.Name)
			}
		}
	}
	slices.Sort(names)
	return names
}

func assertDeclaredExactly(t *testing.T, typeName string, want ...string) {
	t.Helper()
	slices.Sort(want)
	if got := declared(t, typeName); !slices.Equal(got, want) {
		t.Fatalf("%s has the values %v, want exactly %v", typeName, got, want)
	}
}

// The four outcomes of a comparison, and no fifth.
func TestCodeChangeCoversExactlyTheFourOutcomes(t *testing.T) {
	assertDeclaredExactly(t, "CodeChange", "CodeUnchanged", "CodeModified", "CodeAdded", "CodeRemoved")

	// Duplicate constant keys would not compile, so this literal is also where
	// two outcomes sharing a value are caught.
	values := map[engine.CodeChange]string{
		engine.CodeUnchanged: "UNCHANGED",
		engine.CodeModified:  "MODIFIED",
		engine.CodeAdded:     "ADDED",
		engine.CodeRemoved:   "REMOVED",
	}
	if len(values) != 4 {
		t.Fatalf("the four outcomes are %d distinct values: %v", len(values), values)
	}
	for got, want := range values {
		if string(got) != want {
			t.Errorf("CodeChange value %q, want %q", string(got), want)
		}
	}
}

// The three ways two sides can be populated: both found, only the to context,
// only the from context. The last two are what ADDED and REMOVED are answers
// to.
func TestSymbolPresenceCoversExactlyTheThreeCases(t *testing.T) {
	assertDeclaredExactly(t, "SymbolPresence", "PresenceBoth", "PresenceToOnly", "PresenceFromOnly")

	values := map[engine.SymbolPresence]string{
		engine.PresenceBoth:     "BOTH",
		engine.PresenceToOnly:   "TO_ONLY",
		engine.PresenceFromOnly: "FROM_ONLY",
	}
	if len(values) != 3 {
		t.Fatalf("the three cases are %d distinct values: %v", len(values), values)
	}
	for got, want := range values {
		if string(got) != want {
			t.Errorf("SymbolPresence value %q, want %q", string(got), want)
		}
	}
}
