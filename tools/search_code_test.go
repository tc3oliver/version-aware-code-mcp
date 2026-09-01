package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/server"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
)

// The two fixtures below are fakes rather than the prepared demo repository,
// which every other search_code test uses. That is not a shortcut around the
// real thing: a workspace of several repositories has to be searched through a
// server, and the point of these tests is the shape that reaches a client rather
// than what Zoekt finds, so a fixture needing an index and a running engine
// would make a wire-shape test wait on a search backend to agree with it.
var (
	soloSearch = single(vacctx.CodeContext{
		ID: "solo", Repository: "alpha", Branch: "main",
		Revision: "1111111111111111111111111111111111111111", GraphRef: "alpha-main",
	})
	stackedSearch = vacctx.Workspace{ID: "stack", Members: []vacctx.CodeContext{
		{
			ID: "stack", Repository: "alpha", Branch: "main",
			Revision: "1111111111111111111111111111111111111111", GraphRef: "alpha-main",
		},
		{
			ID: "stack", Repository: "beta", Branch: "release/2.x",
			Revision: "2222222222222222222222222222222222222222", GraphRef: "beta-v2",
		},
	}}
)

// wiredContexts serves the two workspaces above and nothing else.
type wiredContexts map[string]vacctx.Workspace

func (w wiredContexts) Contexts(context.Context) ([]vacctx.Workspace, error) { return nil, nil }

func (w wiredContexts) Resolve(_ context.Context, id string) (vacctx.Workspace, error) {
	return w[id], nil
}

// wiredSearch answers one match per repository, named after it, so a result says
// which member each match came out of without the test having to arrange for two
// repositories to differ.
type wiredSearch struct{}

func (wiredSearch) Search(_ context.Context, codeCtx vacctx.CodeContext, _ provider.SearchQuery) ([]provider.SearchResult, error) {
	return []provider.SearchResult{{Path: codeCtx.Repository + ".go", Line: 3, Snippet: "func Process()"}}, nil
}

// searchWireSession serves search_code over a real MCP server, client and all,
// because what is under test is what survives the SDK: a result the server is
// happy with and the protocol rejects is not a result a client ever sees.
func searchWireSession(t *testing.T) *mcp.ClientSession {
	t.Helper()

	srv := server.New(testVersion)
	AddSearchCode(srv, engine.New(
		wiredContexts{soloSearch.ID: soloSearch, stackedSearch.ID: stackedSearch},
		wiredSearch{}, nil, nil,
	))

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

// searched calls search_code and returns both halves of what the client
// received: the JSON text block, and the structured content re-marshalled.
//
// Both, because they are not the same bytes. The text block is the result
// marshalled straight from the Go values, so its object keys are in declaration
// order; structured content is a decoded JSON object, so re-marshalling it here
// sorts them. Neither is more true than the other, and a test that read only the
// decoded form could not see a key that arrived as null instead of [], which is
// a different answer to the agent reading it.
func searched(t *testing.T, session *mcp.ClientSession, contextID string) (text, structured string, isError bool) {
	t.Helper()
	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "search_code",
		Arguments: map[string]any{"context": contextID, "query": "Process"},
	})
	if err != nil {
		// A call that does not come back at all is the failure this file exists
		// for: it is what a declared output schema that has gone stale looks like
		// from a client — a protocol fault where an answer or a typed error
		// should be.
		t.Fatalf("search_code(%s) never returned a result: %v", contextID, err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("search_code(%s) returned %d content blocks, want one", contextID, len(res.Content))
	}
	block, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("search_code(%s) returned %T, want text", contextID, res.Content[0])
	}
	// Structured content is what an SDK client decodes, so a tool that stopped
	// emitting it would have changed its answer however right the text looked.
	if res.StructuredContent == nil {
		t.Fatalf("search_code(%s) returned no structured content: %s", contextID, block.Text)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	return block.Text, string(raw), res.IsError
}

// A search in a context naming one repository emits the document it has emitted
// since v0.4.0: the flat context block, one citation per match, and no per-item
// repository or revision.
//
// The whole document is compared rather than a field at a time. What this is
// guarding is a client that predates workspaces, and such a client is broken by
// any key that appears, moves or changes shape — including ones nobody thought
// to assert on.
//
// The two forms are pinned separately because only one of them survived this
// tool losing its declared output schema unchanged. Structured content — what an
// SDK client decodes — is byte for byte what it was. The text block's object
// keys are now in declaration order rather than sorted: with a schema declared
// the SDK validated the result through a map and re-marshalled it from there,
// and that sorting was the only thing search_code did differently from get_code
// and the two comparison tools, which have never declared one. Declaration order
// is also the order evidence/testdata/v0.4.0/*.json freezes for this same
// document, so the sorted form was this tool's deviation from the v0.4.0
// contract rather than the contract itself.
func TestSearchCodeOverOneRepositoryEmitsTheFlatShape(t *testing.T) {
	text, structured, isError := searched(t, searchWireSession(t), soloSearch.ID)
	if isError {
		t.Fatalf("search_code(%s) failed: %s", soloSearch.ID, text)
	}

	const wantStructured = `{"context":{"branch":"main","id":"solo","repository":"alpha",` +
		`"revision":"1111111111111111111111111111111111111111"},` +
		`"evidence":[{"location":{"end_line":3,"path":"alpha.go","start_line":3},"snippet":"func Process()"}],` +
		`"matches":[{"line":3,"path":"alpha.go","snippet":"func Process()"}]}`
	if structured != wantStructured {
		t.Errorf("search_code(%s) structured content is\n%s\nwant\n%s", soloSearch.ID, structured, wantStructured)
	}

	const wantText = `{"context":{"id":"solo","repository":"alpha","branch":"main",` +
		`"revision":"1111111111111111111111111111111111111111"},` +
		`"evidence":[{"location":{"path":"alpha.go","start_line":3,"end_line":3},"snippet":"func Process()"}],` +
		`"matches":[{"path":"alpha.go","line":3,"snippet":"func Process()"}]}`
	if text != wantText {
		t.Errorf("search_code(%s) emitted\n%s\nwant\n%s", soloSearch.ID, text, wantText)
	}
}

// A search in a context naming several repositories reaches the client as a
// result.
//
// It is neither of the two things it must not be. Not a refusal: the members
// were searched and what they found is an answer. And not a protocol-level
// failure either — a result the SDK rejects on the way out carries none of this
// server's error model, so a client gets a transport fault where it should get
// either an answer or a typed error. That is what a declared output schema
// mirroring one of the two context shapes produces, and it is why search_code
// declares none.
func TestSearchCodeOverSeveralRepositoriesReachesTheClient(t *testing.T) {
	raw, _, isError := searched(t, searchWireSession(t), stackedSearch.ID)
	if isError {
		t.Fatalf("search_code(%s) failed: %s", stackedSearch.ID, raw)
	}

	var out struct {
		Context struct {
			ID      string `json:"id"`
			Members []struct {
				Repository string `json:"repository"`
				Branch     string `json:"branch"`
				Revision   string `json:"revision"`
			} `json:"members"`
		} `json:"context"`
		Evidence []struct {
			Repository string `json:"repository"`
			Revision   string `json:"revision"`
		} `json:"evidence"`
		Matches []searchMatch `json:"matches"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}

	// Both members, in the workspace's order, and each citation attributed to the
	// one it was found in: a search over several repositories that could not say
	// which one a line came from is the answer this server exists not to give.
	if out.Context.ID != stackedSearch.ID || len(out.Context.Members) != 2 {
		t.Fatalf("context = %+v, want both members of %s: %s", out.Context, stackedSearch.ID, raw)
	}
	if len(out.Evidence) != 2 {
		t.Fatalf("%d citations, want one per member: %s", len(out.Evidence), raw)
	}
	for i, member := range stackedSearch.Members {
		if got := out.Context.Members[i]; got.Repository != member.Repository ||
			got.Branch != member.Branch || got.Revision != member.Revision {
			t.Errorf("member %d = %+v, want %+v", i, got, member)
		}
		if out.Evidence[i].Repository != member.Repository || out.Evidence[i].Revision != member.Revision {
			t.Errorf("citation %d is attributed to %+v, want %s at %s",
				i, out.Evidence[i], member.Repository, member.Revision)
		}
	}
	if len(out.Matches) != 2 {
		t.Fatalf("matches = %+v, want one from each member: %s", out.Matches, raw)
	}

	// The graph reference is internal in both shapes, and a second member is a
	// second chance to leak one.
	if strings.Contains(raw, "beta-v2") || strings.Contains(raw, "graph") {
		t.Errorf("search_code leaked a graph reference: %s", raw)
	}
}

// What tools/list advertises has to be true of both shapes search_code can
// answer in, and the honest thing to advertise for a document whose context
// block has two forms is no output schema at all.
//
// This is the assertion that fails if someone reinstates one. A declared schema
// here is not documentation, it is enforcement: the SDK validates every result
// against it, so a schema describing one shape does not merely under-describe
// the other, it stops it from being returned.
func TestSearchCodeAdvertisesNoOutputSchema(t *testing.T) {
	listed, err := searchWireSession(t).ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	var tool *mcp.Tool
	for _, candidate := range listed.Tools {
		if candidate.Name == "search_code" {
			tool = candidate
		}
	}
	if tool == nil {
		t.Fatalf("tools/list returned %d tools, none named search_code", len(listed.Tools))
	}
	if tool.OutputSchema != nil {
		raw, err := json.Marshal(tool.OutputSchema)
		if err != nil {
			t.Fatalf("marshal output schema: %v", err)
		}
		t.Errorf("search_code advertises an output schema, which the SDK enforces on every result: %s", raw)
	}

	// The input schema is untouched by any of this, and an agent still cannot
	// widen a search's scope past the context it named.
	raw, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatalf("marshal input schema: %v", err)
	}
	var schema struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("decode input schema %s: %v", raw, err)
	}
	for _, field := range []string{"context", "query"} {
		if _, ok := schema.Properties[field]; !ok {
			t.Errorf("input schema has no %s property: %s", field, raw)
		}
	}
	for _, forbidden := range []string{"repository", "branch", "revision"} {
		if _, ok := schema.Properties[forbidden]; ok {
			t.Errorf("input schema accepts a %s override: %s", forbidden, raw)
		}
	}
}
