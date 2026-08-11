package vacerr_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// codes lists every v0.1.0 error code with the exact string the specification
// requires on the wire.
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
}

func TestEveryCodeSerialisesToSpecShape(t *testing.T) {
	if len(codes) != 10 {
		t.Fatalf("expected the 10 v0.1.0 codes, got %d", len(codes))
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
