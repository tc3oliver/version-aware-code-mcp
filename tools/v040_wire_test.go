package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/server"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
)

// v040Source is a source backend that both reads and diffs, so one server can
// serve all six tools at once. The two other fakes this file needs already exist
// — [wiredSearch] answers one match per repository and [compareGraph] holds one
// call graph per version — and reusing them is what keeps the answers below
// fixed strings rather than whatever a repository on the machine happens to say.
type v040Source struct{}

func (v040Source) Read(_ context.Context, codeCtx vacctx.CodeContext, path string, start, end int) (*provider.SourceContent, error) {
	return &provider.SourceContent{
		Path:      path,
		StartLine: start,
		EndLine:   end,
		Content:   "func Process() {\n\tLegacyHandler()\n}\n",
		Revision:  codeCtx.Revision,
	}, nil
}

func (v040Source) Diff(context.Context, vacctx.CodeContext, vacctx.CodeContext, provider.SourceDiffRequest) (*provider.SourceDiff, error) {
	return modifiedDiff(), nil
}

// v040Calls is one successful call to each of the six tools, all inside contexts
// naming one repository — which is every context v0.4.0 could be configured
// with. None of them passes a repository argument, because none of them could
// have: that argument did not exist in v0.4.0, and a document produced with it
// would not be the document being compared against.
var v040Calls = map[string]mcp.CallToolParams{
	"list_contexts": {Name: "list_contexts"},
	"search_code": {Name: "search_code", Arguments: map[string]any{
		"context": compareV1.ID, "query": "Process",
	}},
	"get_code": {Name: "get_code", Arguments: map[string]any{
		"context": compareV1.ID, "path": comparedPath, "start_line": 4, "end_line": 6,
	}},
	"trace_calls": {Name: "trace_calls", Arguments: map[string]any{
		"context": compareV1.ID, "symbol": comparedSymbol, "direction": "callees", "depth": 2,
	}},
	"compare_code": {Name: "compare_code", Arguments: map[string]any{
		"from_context": compareV1.ID, "to_context": compareV2.ID, "path": comparedPath,
	}},
	"compare_calls": {Name: "compare_calls", Arguments: map[string]any{
		"from_context": compareV1.ID, "to_context": compareV2.ID, "symbol": comparedSymbol,
		"direction": "callees", "depth": 2,
	}},
}

// TestSingleMemberToolsMatchV040Bytes is the compatibility test the repository
// argument and the members shape are added under: a context naming one
// repository must reach a client as the document v0.4.0 sent, byte for byte, so
// an agent written against that release reads the same answer from this one.
//
// The golden files are not hand-written and were not copied out of a passing run
// of this tree. They were produced by running this same harness at the v0.4.0
// tag, so what they hold is what that release actually put on the wire:
//
//	git worktree add /tmp/v040 v0.4.0
//	cp tools/v040_wire_test.go /tmp/v040/tools/
//	# plus a shim defining single() as the identity and copying wiredSearch,
//	# neither of which exists at v0.4.0
//	cd /tmp/v040 && VACMCP_WRITE_V040_GOLDEN=1 go test -run TestSingleMember ./tools/
//
// They were first generated against an intermediate commit of this series
// instead, which was wrong in exactly one way and worth recording: search_code
// had already stopped declaring an output schema by then, and that declaration
// is what decides the key order below. Five goldens were unaffected and are
// byte-identical either way; search_code's was the post-change document being
// compared against itself, so the one tool whose bytes changed was the one the
// gate could not have caught.
//
// The text block is compared rather than the decoded structured content, and the
// whole of it rather than a field at a time. A decoded comparison would pass an
// added key, a dropped omitempty, a null where a [] was and a reordered object,
// and the first three of those are changes to what a client receives.
//
// search_code is the documented exception, and [v040KeyOrderOnly] says why.
func TestSingleMemberToolsMatchV040Bytes(t *testing.T) {
	session := v040Session(t)

	for name, params := range v040Calls {
		t.Run(name, func(t *testing.T) {
			res, err := session.CallTool(t.Context(), &params)
			if err != nil {
				t.Fatalf("tools/call %s: %v", name, err)
			}
			if len(res.Content) != 1 {
				t.Fatalf("%s returned %d content blocks, want one", name, len(res.Content))
			}
			block, ok := res.Content[0].(*mcp.TextContent)
			if !ok {
				t.Fatalf("%s returned %T, want text", name, res.Content[0])
			}
			if res.IsError {
				t.Fatalf("%s failed: %s", name, block.Text)
			}

			// The graph reference is internal in every shape, and this is the one
			// document where all six tools can be checked for it at once.
			if strings.Contains(block.Text, "graph") {
				t.Errorf("%s leaked a graph reference: %s", name, block.Text)
			}

			golden := filepath.Join("testdata", "v0.4.0", name+".json")
			if os.Getenv("VACMCP_WRITE_V040_GOLDEN") != "" {
				// Only ever run at the v0.4.0 tag. Writing these from the tree they
				// are meant to constrain would turn the test into a record of what
				// this code does, which is what it already is without them — and
				// writing them from anywhere in between is the mistake described
				// above, which cost the search_code golden its meaning.
				if err := os.MkdirAll(filepath.Dir(golden), 0o750); err != nil {
					t.Fatalf("MkdirAll: %v", err)
				}
				if err := os.WriteFile(golden, append([]byte(block.Text), '\n'), 0o600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
			}

			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			want = bytes.TrimSuffix(want, []byte("\n"))
			if name == v040KeyOrderOnly {
				assertSameDocument(t, name, []byte(block.Text), want)
				return
			}
			if !bytes.Equal([]byte(block.Text), want) {
				t.Errorf("%s emitted\n%s\nwant the v0.4.0 bytes\n%s", name, block.Text, want)
			}
		})
	}
}

// v040KeyOrderOnly is the one tool whose bytes this release changed, and the
// only one this file does not hold to byte equality.
//
// v0.4.0's search_code declared an output schema. The SDK validates a result
// against a resolved schema by decoding it into a map and marshalling it again,
// and that round trip sorts every object's keys — so v0.4.0 sent
// {"context":{"branch":…,"id":…}} where this release sends the declaration order
// {"context":{"id":…,"branch":…}}. Dropping that schema was deliberate: it was
// inferred from the flat single-member shape and rejected a valid several-member
// result as a protocol error. See the CHANGELOG entry for it.
//
// So the guarantee for this one tool is weaker by exactly one property, and
// naming it here is the point — a client that decodes the JSON reads the same
// document, and object key order is not semantic in JSON. Everything else the
// byte comparison would have caught, [assertSameDocument] still catches: an
// added key, a dropped omitempty, and a null where a [] was all survive
// decoding as a difference.
const v040KeyOrderOnly = "search_code"

// assertSameDocument fails unless got and want decode to the same JSON document.
// It is byte equality minus object key order, and nothing else: encoding/json
// decodes null to nil and [] to an empty slice, so those stay distinguishable.
func assertSameDocument(t *testing.T, name string, got, want []byte) {
	t.Helper()

	var gotDoc, wantDoc any
	if err := json.Unmarshal(got, &gotDoc); err != nil {
		t.Fatalf("%s emitted undecodable JSON: %v\n%s", name, err, got)
	}
	if err := json.Unmarshal(want, &wantDoc); err != nil {
		t.Fatalf("%s golden is undecodable: %v", name, err)
	}
	if !reflect.DeepEqual(gotDoc, wantDoc) {
		t.Errorf("%s emitted\n%s\nwhich is a different document from v0.4.0's\n%s", name, got, want)
	}
}

// v040Session serves the six tools over three contexts naming one repository
// each.
func v040Session(t *testing.T) *mcp.ClientSession {
	t.Helper()

	return serveTools(t, engine.New(
		compareContexts{compareV1.ID: single(compareV1), compareV2.ID: single(compareV2), compareOther.ID: single(compareOther)},
		wiredSearch{},
		compareGraph{compareV1.ID: compareFromGraph, compareV2.ID: compareToGraph},
		v040Source{},
	))
}

// serveTools serves all six tools off one engine over stateless Streamable HTTP
// and connects a client to it, so every assertion is made on what came back over
// a real wire rather than on a Go value.
//
// All six on one server, because that is how vacmcp serves them: a client
// discovers them together, and a document that only holds its shape when its
// tool is registered alone would not be the one anybody receives.
func serveTools(t *testing.T, eng *engine.Engine) *mcp.ClientSession {
	t.Helper()

	srv := server.New(testVersion)
	AddListContexts(srv, eng)
	AddSearchCode(srv, eng)
	AddGetCode(srv, eng)
	AddTraceCalls(srv, eng)
	AddCompareCode(srv, eng)
	AddCompareCalls(srv, eng)

	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true},
	))
	t.Cleanup(httpServer.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "vacmcp-test", Version: testVersion}, nil)
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: httpServer.URL}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}
