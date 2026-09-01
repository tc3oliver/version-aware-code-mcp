package tools

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/evidence"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/server"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// The two versions every comparison here is between, plus one in another
// repository so a cross-repository call has something to be refused for. The
// revisions are full SHAs because that is what a resolved context carries, and
// they differ so a merged answer cannot hide behind two equal strings.
var (
	compareV1 = vacctx.CodeContext{
		ID: "demo-v1", Repository: "example/demo", Branch: "release/1.x",
		Revision: "1111111111111111111111111111111111111111", GraphRef: "demo-v1-graph",
	}
	compareV2 = vacctx.CodeContext{
		ID: "demo-v2", Repository: "example/demo", Branch: "release/2.x",
		Revision: "2222222222222222222222222222222222222222", GraphRef: "demo-v2-graph",
	}
	compareOther = vacctx.CodeContext{
		ID: "other-v1", Repository: "example/other", Branch: "main",
		Revision: "3333333333333333333333333333333333333333", GraphRef: "other-v1-graph",
	}
)

const comparedPath = "processor.go"

// compareContexts is an [engine.ContextSource] that is a Go map and nothing
// else: what a tool test is about is what reaches the wire, and a real resolver
// would answer that question no differently while making the test need git.
//
// Its values are the workspaces each context is — one repository apiece here,
// which is what these tools can be asked about — so the member a comparison was
// answered in is visible in the test rather than manufactured by the fake.
type compareContexts map[string]vacctx.Workspace

func (c compareContexts) Contexts(context.Context) ([]vacctx.Workspace, error) {
	listed := slices.Collect(maps.Values(c))
	slices.SortFunc(listed, func(a, b vacctx.Workspace) int { return strings.Compare(a.ID, b.ID) })
	return listed, nil
}

func (c compareContexts) Resolve(_ context.Context, id string) (vacctx.Workspace, error) {
	workspace, ok := c[id]
	if !ok {
		return vacctx.Workspace{}, vacerr.New(vacerr.ContextNotFound, "no context named "+id, map[string]any{"context": id})
	}
	return workspace, nil
}

// single is the workspace a one-repository context is, filed under its own ID.
func single(codeCtx vacctx.CodeContext) vacctx.Workspace {
	return vacctx.Workspace{ID: codeCtx.ID, Members: []vacctx.CodeContext{codeCtx}}
}

// compareDiffSource is a source backend with the optional
// [provider.SourceDiffer] capability, answering with whatever it was built with.
// Read fails: compare_code must not read source, and one that tried would fail
// the test rather than be quietly tolerated.
type compareDiffSource struct {
	diff *provider.SourceDiff
	err  error
}

func (s *compareDiffSource) Read(context.Context, vacctx.CodeContext, string, int, int) (*provider.SourceContent, error) {
	return nil, errors.New("compare_code read source")
}

func (s *compareDiffSource) Diff(context.Context, vacctx.CodeContext, vacctx.CodeContext, provider.SourceDiffRequest) (*provider.SourceDiff, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.diff, nil
}

// compareReadOnlySource is a source backend that reads one version at a time and
// has no way to compare two, which is what SOURCE_DIFF_UNAVAILABLE reports.
type compareReadOnlySource struct{}

func (compareReadOnlySource) Read(context.Context, vacctx.CodeContext, string, int, int) (*provider.SourceContent, error) {
	return &provider.SourceContent{}, nil
}

// comparisonSideWire is one version's half of a comparison as a client receives
// it. Decoding into it is what pins the field names: a wrong json tag leaves its
// field empty and the assertion on it fails.
type comparisonSideWire struct {
	Context struct {
		ID         string `json:"id"`
		Repository string `json:"repository"`
		Branch     string `json:"branch"`
		Revision   string `json:"revision"`
	} `json:"context"`
	Evidence []evidence.Evidence `json:"evidence"`
}

type diffLineWire struct {
	Kind    string `json:"kind"`
	Content string `json:"content"`
}

type hunkWire struct {
	OldStart int            `json:"old_start"`
	OldLines int            `json:"old_lines"`
	NewStart int            `json:"new_start"`
	NewLines int            `json:"new_lines"`
	Lines    []diffLineWire `json:"lines"`
}

type compareCodeWire struct {
	From   *comparisonSideWire `json:"from"`
	To     *comparisonSideWire `json:"to"`
	Change string              `json:"change"`
	Path   string              `json:"path"`
	Binary bool                `json:"binary"`
	Hunks  []hunkWire          `json:"hunks"`
}

// modifiedDiff is one file the two versions spell differently.
func modifiedDiff() *provider.SourceDiff {
	return &provider.SourceDiff{
		Path:   comparedPath,
		Change: provider.ChangeModified,
		Hunks: []provider.DiffHunk{{
			OldStart: 4, OldLines: 2, NewStart: 4, NewLines: 2,
			Lines: []provider.DiffLine{
				{Kind: provider.LineContext, Content: "func Process() {"},
				{Kind: provider.LineRemoved, Content: "\tLegacyHandler()"},
				{Kind: provider.LineAdded, Content: "\tNewHandler()"},
			},
		}},
	}
}

// AC #1: the input schema accepts the two context ids and the path, and nothing
// else. A repository, branch or revision property here would let a caller
// compare a version the configuration never granted it — the whole guarantee
// given away in a JSON field.
func TestCompareCodeInputSchemaIsTwoContextsAndAPath(t *testing.T) {
	tool := comparisonTool(t, "compare_code")

	raw, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatalf("marshal input schema: %v", err)
	}
	var schema struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("decode input schema %s: %v", raw, err)
	}
	t.Logf("compare_code input schema = %s", raw)

	want := []string{"from_context", "path", "to_context"}
	if got := slices.Sorted(maps.Keys(schema.Properties)); !slices.Equal(got, want) {
		t.Errorf("input schema has the properties %v, want exactly %v", got, want)
	}
	for _, field := range want {
		if !slices.Contains(schema.Required, field) {
			t.Errorf("input schema does not require %s: %s", field, raw)
		}
	}
	// Named explicitly rather than left to the set comparison: these three are
	// the fields whose presence would be a version-isolation hole, so they are
	// checked by name and against the whole schema text, jsonschema descriptions
	// included.
	for _, forbidden := range []string{"repository", "branch", "revision"} {
		if _, ok := schema.Properties[forbidden]; ok {
			t.Errorf("input schema accepts a %s override: %s", forbidden, raw)
		}
	}
}

// AC #3: a successful comparison carries both sides, each with its own full
// context and its own evidence, and no merged pair of either. A citation only
// means anything at the revision it was read at, so one flattened context or
// evidence list would be a cross-version answer wearing the clothes of a
// version-scoped one.
func TestCompareCodeReportsBothSidesSeparately(t *testing.T) {
	got, raw := compareCodeCall(t, compareCodeSession(t, &compareDiffSource{diff: modifiedDiff()}), compareV1.ID, compareV2.ID, comparedPath)
	t.Logf("compare_code MODIFIED wire = %s", raw)

	// The top level is the two sides plus the facts about the pair. context and
	// evidence are deliberately not among them: there is no single version this
	// answer was given in.
	keys := topLevelKeys(t, raw)
	want := []string{"binary", "change", "from", "hunks", "path", "to"}
	if !slices.Equal(keys, want) {
		t.Errorf("result has the keys %v, want exactly %v", keys, want)
	}

	if got.From == nil || got.To == nil {
		t.Fatalf("a comparison of a file both versions have reported a side as absent: %s", raw)
	}
	assertSideContext(t, "from", got.From, compareV1)
	assertSideContext(t, "to", got.To, compareV2)
	if got.From.Context.Revision == got.To.Context.Revision {
		t.Errorf("both sides report revision %s, the two versions have been merged", got.From.Context.Revision)
	}
	// Each side carries its own evidence list. compare_code cites a whole file's
	// diff with nothing to point at, so both are empty — and empty rather than
	// absent, because "nothing worth citing" is an answer.
	for name, got := range map[string]*comparisonSideWire{"from": got.From, "to": got.To} {
		if got.Evidence == nil {
			t.Errorf("%s side has no evidence list at all: %s", name, raw)
		}
	}

	if got.Change != string(engine.CodeModified) || got.Path != comparedPath || got.Binary {
		t.Errorf("change/path/binary = %q/%q/%v, want MODIFIED/%s/false", got.Change, got.Path, got.Binary, comparedPath)
	}
	wantHunks := []hunkWire{{
		OldStart: 4, OldLines: 2, NewStart: 4, NewLines: 2,
		Lines: []diffLineWire{
			{Kind: "CONTEXT", Content: "func Process() {"},
			{Kind: "REMOVED", Content: "\tLegacyHandler()"},
			{Kind: "ADDED", Content: "\tNewHandler()"},
		},
	}}
	if !slices.EqualFunc(got.Hunks, wantHunks, func(a, b hunkWire) bool {
		return a.OldStart == b.OldStart && a.OldLines == b.OldLines &&
			a.NewStart == b.NewStart && a.NewLines == b.NewLines && slices.Equal(a.Lines, b.Lines)
	}) {
		t.Errorf("hunks = %+v, want %+v", got.Hunks, wantHunks)
	}

	// GraphRef is the CBM project backing a context: internal, and a tool's
	// output is the only place it could leak from.
	if strings.Contains(raw, "graph") {
		t.Errorf("compare_code leaked the graph reference: %s", raw)
	}
}

// AC #3, the other half: the version that does not have the file is null, which
// no client can mistake for a side carrying a context and citations. The two
// one-sided outcomes are checked in both directions, because a tool that
// reported the wrong side as absent would still pass a test that only ever saw
// one of them.
func TestCompareCodeAbsentSideIsNull(t *testing.T) {
	for name, tc := range map[string]struct {
		change     provider.DiffChange
		wantChange string
		present    *vacctx.CodeContext
		absent     string
	}{
		"added":   {provider.ChangeAdded, "ADDED", &compareV2, "from"},
		"removed": {provider.ChangeRemoved, "REMOVED", &compareV1, "to"},
	} {
		t.Run(name, func(t *testing.T) {
			source := &compareDiffSource{diff: &provider.SourceDiff{Path: comparedPath, Change: tc.change}}
			got, raw := compareCodeCall(t, compareCodeSession(t, source), compareV1.ID, compareV2.ID, comparedPath)
			t.Logf("compare_code %s wire = %s", tc.wantChange, raw)

			if got.Change != tc.wantChange {
				t.Errorf("change = %q, want %q", got.Change, tc.wantChange)
			}
			absent, present := got.From, got.To
			if tc.absent == "to" {
				absent, present = got.To, got.From
			}
			if absent != nil {
				t.Errorf("the %s side is %+v, want null: the version does not have the file", tc.absent, absent)
			}
			if present == nil {
				t.Fatalf("the version that has the file reported no side at all: %s", raw)
			}
			assertSideContext(t, "present", present, *tc.present)

			// Checked on the bytes as well as on the decode: a missing key would
			// decode to a nil pointer too, and "absent" has to be something the
			// client is told rather than something it infers from silence.
			if !strings.Contains(raw, `"`+tc.absent+`":null`) {
				t.Errorf("the %s side is not on the wire as null: %s", tc.absent, raw)
			}
		})
	}
}

// AC #4: every typed error reaches the client as the error model's own envelope,
// with the code intact and no half-answer beside it. SOURCE_DIFF_UNAVAILABLE is
// the one this tool added, and it is a fact about this server's capability
// rather than about the file.
func TestCompareCodeTypedErrorsRoundTrip(t *testing.T) {
	diffing := func(source provider.SourceProvider) *engine.Engine {
		return engine.New(
			compareContexts{compareV1.ID: single(compareV1), compareV2.ID: single(compareV2), compareOther.ID: single(compareOther)},
			nil, nil, source,
		)
	}

	for name, tc := range map[string]struct {
		eng            *engine.Engine
		from, to, path string
		want           vacerr.Code
	}{
		"source cannot diff": {
			diffing(compareReadOnlySource{}), compareV1.ID, compareV2.ID, comparedPath, vacerr.SourceDiffUnavailable,
		},
		"cross repository": {
			diffing(&compareDiffSource{diff: modifiedDiff()}), compareV1.ID, compareOther.ID, comparedPath, vacerr.InvalidArgument,
		},
		"unknown context": {
			diffing(&compareDiffSource{diff: modifiedDiff()}), compareV1.ID, "demo-v9", comparedPath, vacerr.ContextNotFound,
		},
		"provider failure passes through": {
			diffing(&compareDiffSource{err: vacerr.New(vacerr.RevisionNotFound, "no such revision", map[string]any{"revision": "deadbeef"})}),
			compareV1.ID, compareV2.ID, comparedPath, vacerr.RevisionNotFound,
		},
	} {
		t.Run(name, func(t *testing.T) {
			vErr, raw := compareError(t, compareSession(t, tc.eng), "compare_code", map[string]any{
				"from_context": tc.from, "to_context": tc.to, "path": tc.path,
			})
			t.Logf("%s -> %s", name, raw)

			if vErr.Code != tc.want {
				t.Errorf("code = %q, want %q", vErr.Code, tc.want)
			}
			// Fail closed all the way to the wire: nothing a client could read as
			// half a comparison.
			for _, leak := range []string{`"from"`, `"to"`, `"change"`, `"hunks"`} {
				if strings.Contains(raw, leak) {
					t.Errorf("error result carries %s: %s", leak, raw)
				}
			}
		})
	}
}

// AC #5: the two tool files hold no comparison logic. The claim is checked where
// it is decidable rather than by grepping for words: what a file can do is
// bounded by what it calls and what it branches on.
//
// A file that compared anything itself would have to compare two values — two
// revisions, two repository names, two file contents, a symbol against a set —
// or reach for a package that can. Neither file does either: the only binary
// expressions in them are error checks against nil and the string concatenation
// building a tool description, the only package functions they call are
// "register a tool", "build a single-context envelope" and "name a direction",
// and the engine is reached through exactly one query method each. Every value
// on the wire therefore came out of the result that method returned.
func TestComparisonToolsHoldNoComparisonLogic(t *testing.T) {
	// Anything that could compare content, match repositories or decide presence
	// would need one of these, or an adapter. The whitelist is what the two files
	// are allowed to import, and it is checked exactly rather than as a subset.
	wantImports := map[string][]string{
		"compare_code.go": {
			"context",
			"github.com/modelcontextprotocol/go-sdk/mcp",
			"github.com/tc3oliver/version-aware-code-mcp/engine",
			"github.com/tc3oliver/version-aware-code-mcp/evidence",
		},
		"compare_calls.go": {
			"context",
			"github.com/modelcontextprotocol/go-sdk/mcp",
			"github.com/tc3oliver/version-aware-code-mcp/engine",
			"github.com/tc3oliver/version-aware-code-mcp/evidence",
			"github.com/tc3oliver/version-aware-code-mcp/provider",
		},
	}
	wantQueries := map[string]string{"compare_code.go": "CompareCode", "compare_calls.go": "CompareCalls"}
	// mcp.AddTool registers, evidence.NewWorkspace builds one side's envelope
	// from the workspace that side was answered in, and provider.Direction is a
	// string conversion. None of the three can compare anything.
	allowedPackageCalls := map[string]bool{"mcp.AddTool": true, "evidence.NewWorkspace": true, "provider.Direction": true}

	for file, wantImport := range wantImports {
		t.Run(file, func(t *testing.T) {
			parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse %s: %v", file, err)
			}

			var imported []string
			// The identifier each import is referred to by, which is what a call
			// expression names: the last element of its path, none of these being
			// renamed or named differently from their directory.
			packages := map[string]bool{}
			for _, spec := range parsed.Imports {
				path := strings.Trim(spec.Path.Value, `"`)
				imported = append(imported, path)
				packages[path[strings.LastIndex(path, "/")+1:]] = true
			}
			slices.Sort(imported)
			if !slices.Equal(imported, wantImport) {
				t.Errorf("%s imports %v, want exactly %v", file, imported, wantImport)
			}

			var packageCalls, engineCalls []string
			ast.Inspect(parsed, func(node ast.Node) bool {
				switch node := node.(type) {
				case *ast.CallExpr:
					selector, ok := node.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					receiver, ok := selector.X.(*ast.Ident)
					if !ok {
						return true
					}
					switch {
					case receiver.Name == "eng":
						engineCalls = append(engineCalls, selector.Sel.Name)
					case packages[receiver.Name]:
						packageCalls = append(packageCalls, receiver.Name+"."+selector.Sel.Name)
					}
				case *ast.BinaryExpr:
					// The only decisions these files make: an error check, a nil
					// list, and the + building a description string.
					if node.Op == token.ADD {
						return true
					}
					if right, ok := node.Y.(*ast.Ident); ok && right.Name == "nil" {
						return true
					}
					t.Errorf("%s compares two values at %v, which is a decision the engine has already made", file, node.OpPos)
				}
				return true
			})

			for _, call := range packageCalls {
				if !allowedPackageCalls[call] {
					t.Errorf("%s calls %s, which is outside decode-call-encode", file, call)
				}
			}
			if got := slices.Compact(slices.Sorted(slices.Values(engineCalls))); !slices.Equal(got, []string{wantQueries[file]}) {
				t.Errorf("%s reaches the engine through %v, want only %s", file, got, wantQueries[file])
			}
		})
	}
}

// comparisonTool returns the registered tool named name, as a client discovers
// it before ever calling anything.
func comparisonTool(t *testing.T, name string) *mcp.Tool {
	t.Helper()

	session := compareCodeSession(t, &compareDiffSource{diff: modifiedDiff()})
	listed, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	for _, candidate := range listed.Tools {
		if candidate.Name == name {
			return candidate
		}
	}
	t.Fatalf("tools/list returned %d tools, none named %s", len(listed.Tools), name)
	return nil
}

// compareCodeSession serves the comparison tools over an engine whose only
// provider is source: comparing code reaches neither a graph nor an index, and
// building it with those two nil is what keeps it that way.
func compareCodeSession(t *testing.T, source provider.SourceProvider) *mcp.ClientSession {
	t.Helper()
	return compareSession(t, engine.New(
		compareContexts{compareV1.ID: single(compareV1), compareV2.ID: single(compareV2), compareOther.ID: single(compareOther)},
		nil, nil, source,
	))
}

// compareSession serves both comparison tools over stateless Streamable HTTP and
// connects a client to it, so every assertion is made on what came back over a
// real wire rather than on a Go value. Both are registered on one server because
// that is how they are served, and a client discovers them together.
func compareSession(t *testing.T, eng *engine.Engine) *mcp.ClientSession {
	t.Helper()

	srv := server.New(testVersion)
	AddCompareCode(srv, eng)
	AddCompareCalls(srv, eng)

	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true},
	))
	t.Cleanup(httpServer.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "vacmcp-test", Version: testVersion}, nil)
	clientSession, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: httpServer.URL}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	return clientSession
}

// compareRaw calls a comparison tool and returns the result together with the
// JSON text the client received. The text is read rather than the decoded
// structured content because the assertions are about what is and is not on the
// wire — null and a missing key decode the same way and are not the same answer.
func compareRaw(t *testing.T, session *mcp.ClientSession, tool string, args map[string]any) (*mcp.CallToolResult, string) {
	t.Helper()

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("tools/call %s: %v", tool, err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("result carries %d content blocks, want 1", len(res.Content))
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("result content = %#v, want text", res.Content[0])
	}
	return res, text.Text
}

// compareCodeCall calls compare_code and requires it to have succeeded.
func compareCodeCall(t *testing.T, session *mcp.ClientSession, from, to, path string) (compareCodeWire, string) {
	t.Helper()

	res, raw := compareRaw(t, session, "compare_code", map[string]any{"from_context": from, "to_context": to, "path": path})
	if res.IsError {
		t.Fatalf("compare_code(%s, %s, %s) failed: %s", from, to, path, raw)
	}
	var got compareCodeWire
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return got, raw
}

// compareError calls a comparison tool and requires it to have failed with the
// unified error envelope, carrying nothing else.
func compareError(t *testing.T, session *mcp.ClientSession, tool string, args map[string]any) (*vacerr.Error, string) {
	t.Helper()

	res, raw := compareRaw(t, session, tool, args)
	if !res.IsError {
		t.Fatalf("%s(%v) succeeded, want an error: %s", tool, args, raw)
	}
	if res.StructuredContent != nil {
		t.Errorf("error result carries structured content: %v", res.StructuredContent)
	}

	var envelope struct {
		Error struct {
			Code    vacerr.Code    `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("error result is not the unified envelope: %s (%v)", raw, err)
	}
	if envelope.Error.Code == "" || strings.TrimSpace(envelope.Error.Message) == "" {
		t.Fatalf("error result is missing its code or message: %s", raw)
	}
	return vacerr.New(envelope.Error.Code, envelope.Error.Message, envelope.Error.Details), raw
}

// topLevelKeys reports the keys of the result object, sorted.
func topLevelKeys(t *testing.T, raw string) []string {
	t.Helper()

	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return slices.Sorted(maps.Keys(fields))
}

// assertSideContext checks one side carries the whole version context it was
// answered in, which is what makes its half of the answer checkable.
func assertSideContext(t *testing.T, name string, got *comparisonSideWire, want vacctx.CodeContext) {
	t.Helper()

	if got.Context.ID != want.ID {
		t.Errorf("%s.context.id = %q, want %q", name, got.Context.ID, want.ID)
	}
	if got.Context.Repository != want.Repository {
		t.Errorf("%s.context.repository = %q, want %q", name, got.Context.Repository, want.Repository)
	}
	if got.Context.Branch != want.Branch {
		t.Errorf("%s.context.branch = %q, want %q", name, got.Context.Branch, want.Branch)
	}
	if got.Context.Revision != want.Revision {
		t.Errorf("%s.context.revision = %q, want %q", name, got.Context.Revision, want.Revision)
	}
}
