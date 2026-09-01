// Package evidence_test exercises the contract from outside the package, which
// is also what proves the compile-time half of the guarantee: Output's fields
// are unexported, so `evidence.Output{workspace: w}` does not compile here and
// New and NewWorkspace are the only ways to build a successful output.
package evidence_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/evidence"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
)

// fullContext carries a GraphRef on purpose: every test that marshals it also
// asserts the exact output, so a leak of the internal field fails the suite.
var fullContext = vacctx.CodeContext{
	ID:         "app-v2",
	Repository: "example/backend",
	Branch:     "release/2.x",
	Revision:   "94cb821",
	GraphRef:   "backend-v2",
}

// workspace is the same version scope over two repositories, and carries a
// GraphRef on each member for the reason fullContext does.
var workspace = vacctx.Workspace{
	ID: "app-v2",
	Members: []vacctx.CodeContext{
		{
			ID:         "app-v2",
			Repository: "example/backend",
			Branch:     "release/2.x",
			Revision:   "94cb821",
			GraphRef:   "backend-v2",
		},
		{
			ID:         "app-v2",
			Repository: "example/frontend",
			Branch:     "main",
			Revision:   "0d1f4ac",
			GraphRef:   "frontend-main",
		},
	},
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

// TestGraphRefNeverReachesTheWire pins the one field of a CodeContext that is
// internal: it tells the graph adapter which CBM project to query and means
// nothing to a client.
func TestGraphRefNeverReachesTheWire(t *testing.T) {
	out, err := evidence.New(fullContext)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	got, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if bytes.Contains(got, []byte(fullContext.GraphRef)) {
		t.Fatalf("Marshal() leaked graph_ref: %s", got)
	}

	var payload struct {
		Context map[string]string `json:"context"`
	}
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	want := []string{"branch", "id", "repository", "revision"}
	if diff := slices.Sorted(maps.Keys(payload.Context)); !slices.Equal(diff, want) {
		t.Errorf("context keys = %v, want %v", diff, want)
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
	tests := map[string]vacctx.CodeContext{
		"empty":            {},
		"missing id":       {Repository: "example/backend", Branch: "release/2.x", Revision: "94cb821"},
		"missing repo":     {ID: "app-v2", Branch: "release/2.x", Revision: "94cb821"},
		"missing branch":   {ID: "app-v2", Repository: "example/backend", Revision: "94cb821"},
		"missing revision": {ID: "app-v2", Repository: "example/backend", Branch: "release/2.x"},
		"blank revision":   {ID: "app-v2", Repository: "example/backend", Branch: "release/2.x", Revision: "  "},
		// A graph_ref is not part of the wire contract, so it is neither
		// required nor a substitute for the fields that are.
		"only graph ref": {GraphRef: "backend-v2"},
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

// TestSingleMemberMatchesV040Bytes is the compatibility test the two-shape
// output exists under: a context over one repository must marshal to what
// v0.4.0 sent, byte for byte, so a client written against that release reads
// the same answer from this one.
//
// The golden files are not hand-written. They were produced by the v0.4.0
// implementation itself — `git show HEAD:evidence/evidence.go` at the commit
// before this package learned the second shape, compiled and asked to marshal
// exactly the values below. Comparing whole bytes rather than decoded values is
// the point: a decoded comparison would pass an added field, a dropped
// omitempty or a reordered key, and each of those is a change to what a client
// receives.
func TestSingleMemberMatchesV040Bytes(t *testing.T) {
	cited, err := evidence.New(fullContext,
		evidence.At("internal/process.go", 12, 14, "NewHandler()"),
		evidence.At("internal/wire.go", 30, 30, ""),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	empty, err := evidence.New(fullContext)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	withResult, err := evidence.New(fullContext, evidence.At("internal/process.go", 12, 14, "NewHandler()"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	outputs := map[string]any{
		"cited": cited,
		"empty": empty,
		"with_result": withResult.WithResult(struct {
			Matches []string `json:"matches"`
			Total   int      `json:"total"`
		}{Matches: []string{"internal/process.go"}, Total: 1}),
	}

	for name, out := range outputs {
		t.Run(name, func(t *testing.T) {
			want, err := os.ReadFile(filepath.Join("testdata", "v0.4.0", name+".json"))
			if err != nil {
				t.Fatalf("ReadFile() error = %v", err)
			}
			got, err := json.Marshal(out)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			if !bytes.Equal(got, bytes.TrimSuffix(want, []byte("\n"))) {
				t.Errorf("Marshal() =\n%s\nwant v0.4.0 bytes\n%s", got, want)
			}
		})
	}
}

// TestWorkspaceMarshalsMembersInDeclaredOrder pins the several-member shape
// whole: the members array in the order the workspace declares them, and every
// citation carrying the repository and revision of the member it was found in.
//
// The order is asserted rather than treated as incidental. A client reading a
// members array has nothing but the position to match it against the
// configuration it wrote, so a sorted or a map-shuffled order would silently
// rename which repository is which.
func TestWorkspaceMarshalsMembersInDeclaredOrder(t *testing.T) {
	out, err := evidence.NewWorkspace(workspace,
		[]evidence.Evidence{evidence.At("internal/process.go", 12, 14, "NewHandler()")},
		[]evidence.Evidence{evidence.At("src/app.ts", 7, 7, "handler()")},
	)
	if err != nil {
		t.Fatalf("NewWorkspace() error = %v", err)
	}

	got, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	want := `{"context":{"id":"app-v2","members":[` +
		`{"repository":"example/backend","branch":"release/2.x","revision":"94cb821"},` +
		`{"repository":"example/frontend","branch":"main","revision":"0d1f4ac"}]},` +
		`"evidence":[` +
		`{"location":{"path":"internal/process.go","start_line":12,"end_line":14},"snippet":"NewHandler()","repository":"example/backend","revision":"94cb821"},` +
		`{"location":{"path":"src/app.ts","start_line":7,"end_line":7},"snippet":"handler()","repository":"example/frontend","revision":"0d1f4ac"}]}`
	if string(got) != want {
		t.Errorf("Marshal() =\n%s\nwant\n%s", got, want)
	}
}

// TestWorkspaceAttributesEveryCitation is the multi-repository half of the
// version guarantee: an answer over several repositories must be able to say,
// of each citation on its own, which repository at which revision it can be
// checked in. A citation without that pair is one a client can only resolve by
// guessing among the members.
func TestWorkspaceAttributesEveryCitation(t *testing.T) {
	out, err := evidence.NewWorkspace(workspace,
		[]evidence.Evidence{
			evidence.At("internal/process.go", 12, 14, ""),
			evidence.At("internal/wire.go", 30, 30, ""),
		},
		[]evidence.Evidence{evidence.At("src/app.ts", 7, 7, "")},
	)
	if err != nil {
		t.Fatalf("NewWorkspace() error = %v", err)
	}

	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var payload struct {
		Evidence []struct {
			Location   evidence.Location `json:"location"`
			Repository string            `json:"repository"`
			Revision   string            `json:"revision"`
		} `json:"evidence"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	want := map[string][2]string{
		"internal/process.go": {"example/backend", "94cb821"},
		"internal/wire.go":    {"example/backend", "94cb821"},
		"src/app.ts":          {"example/frontend", "0d1f4ac"},
	}
	if len(payload.Evidence) != len(want) {
		t.Fatalf("Marshal() cited %d items, want %d: %s", len(payload.Evidence), len(want), raw)
	}
	for _, item := range payload.Evidence {
		if got := [2]string{item.Repository, item.Revision}; got != want[item.Location.Path] {
			t.Errorf("citation %s attributed to %v, want %v", item.Location.Path, got, want[item.Location.Path])
		}
	}
}

// TestWorkspaceGraphRefNeverReachesTheWire is
// [TestGraphRefNeverReachesTheWire] for the other shape: a second wire shape is
// a second chance to leak the internal field, so it is pinned in both.
func TestWorkspaceGraphRefNeverReachesTheWire(t *testing.T) {
	out, err := evidence.NewWorkspace(workspace, nil, []evidence.Evidence{evidence.At("src/app.ts", 7, 7, "")})
	if err != nil {
		t.Fatalf("NewWorkspace() error = %v", err)
	}

	got, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	for _, member := range workspace.Members {
		if bytes.Contains(got, []byte(member.GraphRef)) {
			t.Fatalf("Marshal() leaked graph_ref: %s", got)
		}
	}

	var payload struct {
		Context struct {
			Members []map[string]string `json:"members"`
		} `json:"context"`
	}
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	want := []string{"branch", "repository", "revision"}
	for i, member := range payload.Context.Members {
		if keys := slices.Sorted(maps.Keys(member)); !slices.Equal(keys, want) {
			t.Errorf("member %d keys = %v, want %v", i, keys, want)
		}
	}
}

// TestNewWorkspaceRejectsIncompleteMember covers the failure this package fails
// closed on: one member of the workspace cannot say which version it is, so the
// whole answer is refused rather than emitted with the members that can. Half a
// context on the wire would read as the whole of a narrower one.
func TestNewWorkspaceRejectsIncompleteMember(t *testing.T) {
	complete := workspace.Members[0]
	tests := map[string]vacctx.Workspace{
		"no id":            {Members: []vacctx.CodeContext{complete, workspace.Members[1]}},
		"no members":       {ID: "app-v2"},
		"missing repo":     {ID: "app-v2", Members: []vacctx.CodeContext{complete, {ID: "app-v2", Branch: "main", Revision: "0d1f4ac"}}},
		"missing branch":   {ID: "app-v2", Members: []vacctx.CodeContext{complete, {ID: "app-v2", Repository: "example/frontend", Revision: "0d1f4ac"}}},
		"missing revision": {ID: "app-v2", Members: []vacctx.CodeContext{complete, {ID: "app-v2", Repository: "example/frontend", Branch: "main"}}},
		"blank revision":   {ID: "app-v2", Members: []vacctx.CodeContext{complete, {ID: "app-v2", Repository: "example/frontend", Branch: "main", Revision: "  "}}},
	}

	for name, w := range tests {
		t.Run(name, func(t *testing.T) {
			cited := make([][]evidence.Evidence, len(w.Members))
			out, err := evidence.NewWorkspace(w, cited...)
			if !errors.Is(err, evidence.ErrIncompleteContext) {
				t.Fatalf("NewWorkspace() error = %v, want ErrIncompleteContext", err)
			}
			if got, err := json.Marshal(out); err == nil {
				t.Errorf("Marshal(rejected Output) = %s, want error", got)
			}
		})
	}
}

// TestNewWorkspaceRejectsUnattributedEvidence is the other half of provenance:
// a citation is attributed by which member's list it is in, so a call that does
// not hand over exactly one list per member is refused. Padding or truncating
// the lists here would attribute somebody's citations to whichever member
// happened to line up.
func TestNewWorkspaceRejectsUnattributedEvidence(t *testing.T) {
	cited := []evidence.Evidence{evidence.At("internal/process.go", 12, 14, "")}
	tests := map[string][][]evidence.Evidence{
		"no lists":  nil,
		"one short": {cited},
		"one spare": {cited, nil, nil},
	}

	for name, lists := range tests {
		t.Run(name, func(t *testing.T) {
			out, err := evidence.NewWorkspace(workspace, lists...)
			if !errors.Is(err, evidence.ErrUnattributedEvidence) {
				t.Fatalf("NewWorkspace() error = %v, want ErrUnattributedEvidence", err)
			}
			if got, err := json.Marshal(out); err == nil {
				t.Errorf("Marshal(rejected Output) = %s, want error", got)
			}
		})
	}
}

// TestOutputHasNoExportedFields states in a test what the compiler already
// enforces one file up: a struct literal of Output cannot be filled in from
// outside this package. It is here because that guarantee is invisible in a
// diff — a field renamed to an exported spelling would open a second
// construction path, past every check New and NewWorkspace make, without
// breaking a single existing test.
func TestOutputHasNoExportedFields(t *testing.T) {
	outputType := reflect.TypeOf(evidence.Output{})
	for i := range outputType.NumField() {
		if field := outputType.Field(i); field.IsExported() {
			t.Errorf("Output.%s is exported, which is a construction path around New", field.Name)
		}
	}
}
