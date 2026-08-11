// Package evidence_test exercises the contract from outside the package, which
// is also what proves the compile-time half of the guarantee: Output's fields
// are unexported, so `evidence.Output{codeCtx: c}` does not compile here and
// New is the only way to build a successful output.
package evidence_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/evidence"
)

var fullContext = evidence.Context{
	ID:         "app-v2",
	Repository: "example/backend",
	Branch:     "release/2.x",
	Revision:   "94cb821",
}

func TestNewMarshalsContractShape(t *testing.T) {
	out, err := evidence.New(fullContext, evidence.At("internal/process.go", 12, 14, "NewHandler()"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	got, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	want := `{"context":{"id":"app-v2","repository":"example/backend","branch":"release/2.x","revision":"94cb821"},` +
		`"evidence":[{"location":{"path":"internal/process.go","start_line":12,"end_line":14},"snippet":"NewHandler()"}]}`
	if string(got) != want {
		t.Errorf("Marshal() =\n%s\nwant\n%s", got, want)
	}
}

func TestNewMarshalsEmptyEvidenceAsArray(t *testing.T) {
	out, err := evidence.New(fullContext)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	got, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	want := `{"context":{"id":"app-v2","repository":"example/backend","branch":"release/2.x","revision":"94cb821"},"evidence":[]}`
	if string(got) != want {
		t.Errorf("Marshal() =\n%s\nwant\n%s", got, want)
	}
}

// TestZeroOutputCannotMarshalAsSuccess covers the one path around New: the zero
// value. It must not serialize into something a client could read as a result.
func TestZeroOutputCannotMarshalAsSuccess(t *testing.T) {
	var out evidence.Output

	got, err := json.Marshal(out)
	if err == nil {
		t.Fatalf("Marshal(zero Output) = %s, want error", got)
	}
	if !errors.Is(err, evidence.ErrIncompleteContext) {
		t.Errorf("Marshal(zero Output) error = %v, want ErrIncompleteContext", err)
	}
}

func TestNewRejectsIncompleteContext(t *testing.T) {
	tests := map[string]evidence.Context{
		"empty":            {},
		"missing id":       {Repository: "example/backend", Branch: "release/2.x", Revision: "94cb821"},
		"missing repo":     {ID: "app-v2", Branch: "release/2.x", Revision: "94cb821"},
		"missing branch":   {ID: "app-v2", Repository: "example/backend", Revision: "94cb821"},
		"missing revision": {ID: "app-v2", Repository: "example/backend", Branch: "release/2.x"},
		"blank revision":   {ID: "app-v2", Repository: "example/backend", Branch: "release/2.x", Revision: "  "},
	}

	for name, codeCtx := range tests {
		t.Run(name, func(t *testing.T) {
			out, err := evidence.New(codeCtx, evidence.At("internal/process.go", 12, 14, "NewHandler()"))
			if !errors.Is(err, evidence.ErrIncompleteContext) {
				t.Fatalf("New() error = %v, want ErrIncompleteContext", err)
			}
			if got, err := json.Marshal(out); err == nil {
				t.Errorf("Marshal(rejected Output) = %s, want error", got)
			}
		})
	}
}

func TestWithResultMergesToolFields(t *testing.T) {
	out, err := evidence.New(fullContext)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	got, err := json.Marshal(out.WithResult(struct {
		Matches int `json:"matches"`
	}{Matches: 3}))
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	want := `{"context":{"id":"app-v2","repository":"example/backend","branch":"release/2.x","revision":"94cb821"},` +
		`"evidence":[],"matches":3}`
	if string(got) != want {
		t.Errorf("Marshal() =\n%s\nwant\n%s", got, want)
	}
}

// A payload that is not a JSON object would silently drop the tool's fields, so
// it is rejected instead.
func TestWithResultRejectsNonObject(t *testing.T) {
	out, err := evidence.New(fullContext)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if got, err := json.Marshal(out.WithResult([]string{"bare"})); err == nil {
		t.Errorf("Marshal() = %s, want error", got)
	}
}
