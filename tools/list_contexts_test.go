package tools

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/resolver"
	"github.com/tc3oliver/version-aware-code-mcp/server"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
)

const testVersion = "0.0.0-test"

// twoContexts is doc-1's configuration example: one repository, two versions,
// filed under IDs that are deliberately not in sorted order.
var twoContexts = map[string]vacctx.CodeContext{
	"app-v2": {Repository: "example/backend", Branch: "release/2.x", Revision: "94cb821", GraphRef: "backend-v2"},
	"app-v1": {Repository: "example/backend", Branch: "release/1.x", Revision: "8af31e2", GraphRef: "backend-v1"},
}

// AC #1: list_contexts returns every configured context's id, repository,
// branch and revision.
//
// The decode is what pins the field names: a wrong json tag leaves the field
// it should have filled empty, and the comparison fails.
func TestListContextsReturnsEveryConfiguredContext(t *testing.T) {
	raw := callListContexts(t, twoContexts)

	var got listContextsOutput
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}

	want := listContextsOutput{Contexts: []listedContext{
		{ID: "app-v1", Repository: "example/backend", Branch: "release/1.x", Revision: "8af31e2"},
		{ID: "app-v2", Repository: "example/backend", Branch: "release/2.x", Revision: "94cb821"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("list_contexts returned\n%+v\nwant\n%+v", got, want)
	}

	// GraphRef is the CBM project backing a context. It is internal, and the
	// only place it could leak from is a tool's output.
	if strings.Contains(raw, "backend-v1") || strings.Contains(raw, "graph") {
		t.Errorf("list_contexts leaked the graph reference: %s", raw)
	}
}

// AC #2: a configuration with no contexts is an answer, not a failure. The
// empty list has to reach the client as [], because null is a different answer.
func TestListContextsWithNoContextsReturnsAnEmptyList(t *testing.T) {
	for name, contexts := range map[string]map[string]vacctx.CodeContext{
		"nil map":   nil,
		"empty map": {},
	} {
		t.Run(name, func(t *testing.T) {
			if got, want := callListContexts(t, contexts), `{"contexts":[]}`; got != want {
				t.Errorf("list_contexts returned\n%s\nwant\n%s", got, want)
			}
		})
	}
}

// TestListContextsIsDiscoverable checks the tool is registered and takes no
// arguments, which is what an agent sees before it ever calls it.
func TestListContextsIsDiscoverable(t *testing.T) {
	tools, err := session(t, nil).ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}

	var tool *mcp.Tool
	for _, candidate := range tools.Tools {
		if candidate.Name == "list_contexts" {
			tool = candidate
		}
	}
	if tool == nil {
		t.Fatalf("tools/list returned %d tools, none named list_contexts", len(tools.Tools))
	}

	schema, ok := tool.InputSchema.(map[string]any)
	if !ok {
		t.Fatalf("input schema = %#v, want a JSON object", tool.InputSchema)
	}
	if properties, ok := schema["properties"].(map[string]any); ok && len(properties) > 0 {
		t.Errorf("input schema declares properties %v, want a tool that takes no arguments", properties)
	}

	// The output schema is read before the tool is ever called, so it has to
	// say the same thing the tool does: a list that may be empty, never absent.
	out, err := json.Marshal(tool.OutputSchema)
	if err != nil {
		t.Fatalf("marshal output schema: %v", err)
	}
	if strings.Contains(string(out), "null") {
		t.Errorf("output schema admits null: %s", out)
	}
	if !strings.Contains(string(out), `"contexts"`) {
		t.Errorf("output schema does not describe contexts: %s", out)
	}
}

// callListContexts calls the tool over a real MCP session and returns the JSON
// the client received. It reads the text content rather than the decoded
// structured content on purpose: the difference between [] and null only
// survives in the bytes.
func callListContexts(t *testing.T, contexts map[string]vacctx.CodeContext) string {
	t.Helper()

	res, err := session(t, contexts).CallTool(t.Context(), &mcp.CallToolParams{Name: "list_contexts"})
	if err != nil {
		t.Fatalf("tools/call list_contexts: %v", err)
	}
	if res.IsError {
		t.Fatalf("list_contexts reported an error result: %v", res.Content)
	}
	if len(res.Content) != 1 {
		t.Fatalf("result carries %d content blocks, want 1", len(res.Content))
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("result content = %#v, want text", res.Content[0])
	}
	return text.Text
}

// session serves a vacmcp server carrying the tool over stateless Streamable
// HTTP and connects a client to it, so every assertion is made on what came
// back over a real wire rather than on a Go value.
//
// The engine is built with no providers: list_contexts reaches none, and one
// that tried would fail the test rather than answer from a stub.
func session(t *testing.T, contexts map[string]vacctx.CodeContext) *mcp.ClientSession {
	t.Helper()

	srv := server.New(testVersion)
	AddListContexts(srv, engine.New(resolver.New(&config.Config{Contexts: contexts}), nil, nil, nil))

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
