package vacerr_test

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// codes lists every error code with the exact string it must have on the wire:
// the ten the v0.1.0 specification fixed, then the one added after them. The
// list is checked against the constants vacerr.go declares, so a code added
// without a line here fails rather than going untested.
var codes = []struct {
	code vacerr.Code
	want string
}{
	{vacerr.ContextNotFound, "CONTEXT_NOT_FOUND"},
	{vacerr.ContextAmbiguous, "CONTEXT_AMBIGUOUS"},
	{vacerr.RepositoryNotFound, "REPOSITORY_NOT_FOUND"},
	{vacerr.RevisionNotFound, "REVISION_NOT_FOUND"},
	{vacerr.SymbolNotFound, "SYMBOL_NOT_FOUND"},
	{vacerr.SymbolAmbiguous, "SYMBOL_AMBIGUOUS"},
	{vacerr.SearchProviderUnavailable, "SEARCH_PROVIDER_UNAVAILABLE"},
	{vacerr.GraphProviderUnavailable, "GRAPH_PROVIDER_UNAVAILABLE"},
	{vacerr.SourceMismatch, "SOURCE_MISMATCH"},
	{vacerr.InvalidArgument, "INVALID_ARGUMENT"},

	// Added after v0.1.0, for compare_code.
	{vacerr.SourceDiffUnavailable, "SOURCE_DIFF_UNAVAILABLE"},

	// Added after v0.5.0, for search_history.
	{vacerr.SourceHistoryUnavailable, "SOURCE_HISTORY_UNAVAILABLE"},
}

func TestEveryCodeSerialisesToSpecShape(t *testing.T) {
	if len(codes) != 12 {
		t.Fatalf("expected the 10 v0.1.0 codes and the two added after them, got %d", len(codes))
	}
	for _, c := range codes {
		t.Run(c.want, func(t *testing.T) {
			if string(c.code) != c.want {
				t.Fatalf("code value = %q, want %q", c.code, c.want)
			}
			got, err := json.Marshal(vacerr.New(c.code, "boom", nil))
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			want := `{"error":{"code":"` + c.want + `","message":"boom","details":{}}}`
			if string(got) != want {
				t.Fatalf("got  %s\nwant %s", got, want)
			}
			if !json.Valid(got) {
				t.Fatalf("not valid JSON: %s", got)
			}
		})
	}
}

// Every code this package declares is documented where it is declared and
// pinned to its wire string above. Both halves are the same guarantee: a code
// nobody wrote down is one no tool author can produce on purpose and no caller
// can be told to expect, and its string is part of the public tool API whether
// or not anyone remembered to test it.
//
// SourceDiffUnavailable is checked by name because it is the one code added
// after the specification: its comment has to say which tool produces it and
// under what condition, exactly as the ten before it do.
func TestEveryDeclaredCodeIsDocumentedAndPinned(t *testing.T) {
	declared := declaredCodes(t)

	var wire []string
	for name, code := range declared {
		wire = append(wire, code.wire)
		if strings.TrimSpace(code.doc) == "" {
			t.Errorf("%s is declared with no doc comment, so nothing says where it is produced", name)
		}
	}
	var pinned []string
	for _, c := range codes {
		pinned = append(pinned, c.want)
	}
	slices.Sort(wire)
	slices.Sort(pinned)
	if !slices.Equal(wire, pinned) {
		t.Fatalf("vacerr.go declares %v, the list above pins %v", wire, pinned)
	}

	// Each code added after the specification has to say which tool produces it
	// and which optional interface it reports missing, exactly as the ten before
	// them do.
	for name, wants := range map[string][]string{
		"SourceDiffUnavailable":    {"compare_code", "SourceDiffer"},
		"SourceHistoryUnavailable": {"search_history", "HistoryProvider"},
	} {
		doc := declared[name].doc
		for _, want := range wants {
			if !strings.Contains(doc, want) {
				t.Errorf("%s's doc comment does not mention %q, so it does not say when it is produced:\n%s", name, want, doc)
			}
		}
	}
}

type codeDecl struct{ wire, doc string }

// declaredCodes reads back the Code constants vacerr.go declares, each with the
// string it has on the wire and the comment above it.
//
// The source is read rather than the constants listed by hand, because a
// hand-maintained list is exactly what cannot notice a twelfth code being
// declared beside the eleven — and a new code is a new failure every caller has
// to handle, arriving without anyone deciding to add it.
func declaredCodes(t *testing.T) map[string]codeDecl {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "vacerr.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing vacerr.go: %v", err)
	}

	found := map[string]codeDecl{}
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
			if ident, isIdent := value.Type.(*ast.Ident); !isIdent || ident.Name != "Code" || len(value.Values) != 1 {
				continue
			}
			name := value.Names[0].Name
			lit, ok := value.Values[0].(*ast.BasicLit)
			if !ok {
				t.Fatalf("%s is not declared as a string literal, so its wire string cannot be read back", name)
			}
			wire, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			// A constant declared on its own carries its comment on the
			// declaration; one inside a block carries its own.
			doc := value.Doc
			if doc == nil {
				doc = gen.Doc
			}
			found[name] = codeDecl{wire, doc.Text()}
		}
	}
	return found
}

func TestMarshalWithDetails(t *testing.T) {
	got, err := json.Marshal(vacerr.New(vacerr.InvalidArgument, "bad range", map[string]any{"start_line": 10}))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"error":{"code":"INVALID_ARGUMENT","message":"bad range","details":{"start_line":10}}}`
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestNewSourceMismatchCarriesBothRevisions(t *testing.T) {
	err := vacerr.NewSourceMismatch("8af31e2", "94cb821", map[string]any{"path": "handler.go"})

	got, marshalErr := json.Marshal(err)
	if marshalErr != nil {
		t.Fatalf("Marshal: %v", marshalErr)
	}
	var payload struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if unmarshalErr := json.Unmarshal(got, &payload); unmarshalErr != nil {
		t.Fatalf("Unmarshal: %v", unmarshalErr)
	}
	if payload.Error.Code != "SOURCE_MISMATCH" {
		t.Fatalf("code = %q", payload.Error.Code)
	}
	if payload.Error.Details["declared_revision"] != "8af31e2" || payload.Error.Details["actual_revision"] != "94cb821" {
		t.Fatalf("details lost the revisions: %v", payload.Error.Details)
	}
	if payload.Error.Details["path"] != "handler.go" {
		t.Fatalf("caller details dropped: %v", payload.Error.Details)
	}
}

func TestErrorsAsRecoversCode(t *testing.T) {
	wrapped := errors.Join(errors.New("adapter failed"), vacerr.New(vacerr.SourceMismatch, "boom", nil))

	var got *vacerr.Error
	if !errors.As(wrapped, &got) {
		t.Fatal("errors.As did not find *vacerr.Error")
	}
	if got.Code != vacerr.SourceMismatch {
		t.Fatalf("code = %q", got.Code)
	}
	if got.Error() != "SOURCE_MISMATCH: boom" {
		t.Fatalf("Error() = %q", got.Error())
	}
}
