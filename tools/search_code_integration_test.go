//go:build integration

package tools

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	cbmadapter "github.com/tc3oliver/version-aware-code-mcp/adapters/cbm"
	zoektadapter "github.com/tc3oliver/version-aware-code-mcp/adapters/zoekt"
	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/internal/demorepo"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/resolver"
	"github.com/tc3oliver/version-aware-code-mcp/server"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// The contexts the prepared fixture configures, one per release branch of the
// versioned demo repository. Process() calls LegacyHandler on the first and
// NewHandler on the second, so a symbol of one is absent from the other.
//
// Every search below runs against a real Zoekt over the index
// testdata/prepare-fixture.sh built (decision-3). A fake engine would only
// prove this package can read its own JSON.
const (
	v1 = "demo-v1"
	v2 = "demo-v2"
)

// AC #1: a legal context and query return matches, and the context on the
// output is the one that was asked for, whole and unchanged.
//
// The two halves are doc-1's first two release gates. The repository, the
// index, the query and the tool are identical in both; the only thing that
// differs is which context asked, and that decides whether the symbol exists.
func TestSearchCodeAnswersInsideTheContextItWasGiven(t *testing.T) {
	cfg := fixtureConfig(t)
	session := searchSession(t, cfg)

	found := searchCode(t, session, v2, "NewHandler")
	if len(found.Matches) == 0 {
		t.Fatalf("search_code(%s, NewHandler) returned no matches, want the ones on %s", v2, cfg.Contexts[v2].Branch)
	}
	if got, want := found.Context, listedContextOf(cfg, v2); got != want {
		t.Errorf("context = %+v, want %+v", got, want)
	}
	for _, match := range found.Matches {
		if strings.TrimSpace(match.Path) == "" || match.Line < 1 || strings.TrimSpace(match.Snippet) == "" {
			t.Errorf("match %+v is not locatable: every result carries a path, a 1-based line and the line itself", match)
		}
	}

	// The same query in the other context is not an error and not a guess: it
	// is an empty answer, because that branch does not have the symbol.
	absent := searchCode(t, session, v1, "NewHandler")
	if len(absent.Matches) != 0 {
		t.Errorf("search_code(%s, NewHandler) = %+v, want no matches: %s does not have that symbol", v1, absent.Matches, cfg.Contexts[v1].Branch)
	}
	if got, want := absent.Context, listedContextOf(cfg, v1); got != want {
		t.Errorf("context = %+v, want %+v", got, want)
	}

	// The reverse direction too, so the isolation cannot be one context simply
	// answering nothing for everything.
	legacy := searchCode(t, session, v1, "LegacyHandler")
	if len(legacy.Matches) == 0 {
		t.Errorf("search_code(%s, LegacyHandler) returned no matches, want the ones on %s", v1, cfg.Contexts[v1].Branch)
	}
	if leaked := searchCode(t, session, v2, "LegacyHandler"); len(leaked.Matches) != 0 {
		t.Errorf("search_code(%s, LegacyHandler) = %+v, want no matches", v2, leaked.Matches)
	}

	t.Logf("search_code(%s, NewHandler) context = %+v matches = %+v", v2, found.Context, found.Matches)
	t.Logf("search_code(%s, NewHandler) context = %+v matches = %+v", v1, absent.Context, absent.Matches)
	t.Logf("search_code(%s, LegacyHandler) matches = %+v", v1, legacy.Matches)
}

// AC #2: a context that is not configured is an error with a code, not an empty
// result and not an answer from a context that does exist.
//
// The distinction matters more than it looks: an empty result reads as "this
// version does not contain that symbol", which is a claim about the code. The
// server has no version to make a claim about here.
func TestSearchCodeRejectsAnUnknownContext(t *testing.T) {
	session := searchSession(t, fixtureConfig(t))

	// A query that has matches in every configured context, so an empty result
	// could not be blamed on the query.
	raw, isError := callSearchCode(t, session, "demo-v3", "Process")
	if !isError {
		t.Fatalf("search_code(demo-v3, Process) succeeded with %s, want an error result", raw)
	}

	var body struct {
		Error struct {
			Code    vacerr.Code    `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if body.Error.Code != vacerr.ContextNotFound {
		t.Errorf("code = %q, want CONTEXT_NOT_FOUND (result: %s)", body.Error.Code, raw)
	}
	if body.Error.Details["context"] != "demo-v3" {
		t.Errorf("details = %v, want the context that was asked for", body.Error.Details)
	}
	// Nothing that looks like an answer travels with the failure: no matches,
	// and no other context substituted for the one that does not exist.
	for _, leaked := range []string{"matches", v1, v2, "release/v"} {
		if strings.Contains(raw, leaked) {
			t.Errorf("error result mentions %q, want no answer alongside the failure: %s", leaked, raw)
		}
	}
	t.Logf("search_code(demo-v3, Process) = %s", raw)
}

// AC #3: doc-1 §6's Tool Contract. A successful output is never a bare answer:
// it carries the version context it was resolved in and the evidence backing
// every match, and it carries them when there is nothing to report too.
func TestSearchCodeOutputCarriesContextAndEvidence(t *testing.T) {
	cfg := fixtureConfig(t)
	session := searchSession(t, cfg)

	raw, isError := callSearchCode(t, session, v2, "NewHandler")
	if isError {
		t.Fatalf("search_code(%s, NewHandler) failed: %s", v2, raw)
	}

	// Decoding into the output type is what pins the field names on the wire:
	// a wrong json tag leaves the field it should have filled empty.
	var out searchCodeOutput
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if out.Context != listedContextOf(cfg, v2) {
		t.Errorf("context = %+v, want %+v", out.Context, listedContextOf(cfg, v2))
	}
	if len(out.Evidence) != len(out.Matches) {
		t.Fatalf("%d matches carry %d citations, want one each: %s", len(out.Matches), len(out.Evidence), raw)
	}
	for i, cited := range out.Evidence {
		match := out.Matches[i]
		if cited.Location.Path != match.Path || cited.Location.StartLine != match.Line || cited.Snippet != match.Snippet {
			t.Errorf("evidence %+v does not cite its match %+v", cited, match)
		}
		if cited.Location.EndLine < cited.Location.StartLine {
			t.Errorf("evidence %+v has no line range", cited)
		}
	}

	// GraphRef is the CBM project backing the context. It is internal, and a
	// tool's output is the only place it could leak from.
	if strings.Contains(raw, cfg.Contexts[v2].GraphRef) || strings.Contains(raw, "graph") {
		t.Errorf("search_code leaked the graph reference: %s", raw)
	}

	// An empty answer is still scoped and still evidence-backed, and both lists
	// reach the client as [] rather than null: to an agent those are different
	// answers.
	empty, isError := callSearchCode(t, session, v1, "NewHandler")
	if isError {
		t.Fatalf("search_code(%s, NewHandler) failed: %s", v1, empty)
	}
	if !strings.Contains(empty, `"evidence":[]`) || !strings.Contains(empty, `"matches":[]`) {
		t.Errorf("empty result = %s, want evidence and matches as []", empty)
	}
	if !strings.Contains(empty, `"context":{`) {
		t.Errorf("empty result = %s, want it scoped to a context", empty)
	}

	t.Logf("search_code(%s, NewHandler) = %s", v2, raw)
	t.Logf("search_code(%s, NewHandler) = %s", v1, empty)
}

// AC #4, and doc-1 §23's eighth success criterion: CBM unavailable degrades the
// graph and nothing else.
//
// The condition is real rather than simulated. The configuration names a CBM
// binary that is not on this machine, which is what an operator has who never
// installed it, and the graph provider is called first to prove it: a test that
// only showed search still working would not have shown that CBM was broken.
func TestSearchCodeWorksWhenTheGraphProviderIsUnavailable(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Providers.CBM.Command = filepath.Join(t.TempDir(), "codebase-memory-mcp-that-is-not-installed")

	graph, err := cbmadapter.New(cfg).TraceCalls(t.Context(), cfg.Contexts[v2], provider.TraceRequest{
		Symbol:    "Process",
		Direction: provider.Callees,
		Depth:     2,
	})
	if err == nil {
		t.Fatalf("trace_calls returned %+v, want the graph provider to be unavailable for this test to mean anything", graph)
	}
	var vErr *vacerr.Error
	if !errors.As(err, &vErr) || vErr.Code != vacerr.GraphProviderUnavailable {
		t.Fatalf("trace_calls error = %v (%T), want GRAPH_PROVIDER_UNAVAILABLE", err, err)
	}
	t.Logf("graph provider is unavailable: %v", err)

	// Same configuration, same contexts, same server. Search does not come
	// through the graph, so this must still answer.
	found := searchCode(t, searchSession(t, cfg), v2, "NewHandler")
	if len(found.Matches) == 0 {
		t.Fatalf("search_code(%s, NewHandler) returned no matches while CBM is unavailable, want the ones on %s", v2, cfg.Contexts[v2].Branch)
	}
	if got, want := found.Context, listedContextOf(cfg, v2); got != want {
		t.Errorf("context = %+v, want %+v", got, want)
	}
	t.Logf("search_code(%s, NewHandler) with CBM unavailable = %+v", v2, found.Matches)
}

// TestSearchCodeIsDiscoverable checks what an agent reads before it ever calls
// the tool: that it is registered, that both arguments are required — a context
// is not something the server may be left to pick — and that the result it
// promises is the contract's, context and evidence included.
func TestSearchCodeIsDiscoverable(t *testing.T) {
	listed, err := searchSession(t, fixtureConfig(t)).ListTools(t.Context(), nil)
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

	in, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatalf("marshal input schema: %v", err)
	}
	var input struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	if err := json.Unmarshal(in, &input); err != nil {
		t.Fatalf("decode input schema %s: %v", in, err)
	}
	for _, field := range []string{"context", "query"} {
		if _, ok := input.Properties[field]; !ok {
			t.Errorf("input schema has no %q property: %s", field, in)
		}
		if !slices.Contains(input.Required, field) {
			t.Errorf("input schema does not require %q: %s", field, in)
		}
	}
	// The context decides the repository and the branch, so a query may not
	// carry its own: that would be a way out of the version it was given.
	for _, field := range []string{"repository", "branch", "revision"} {
		if _, ok := input.Properties[field]; ok {
			t.Errorf("input schema takes %q, want the context to decide it: %s", field, in)
		}
	}

	out, err := json.Marshal(tool.OutputSchema)
	if err != nil {
		t.Fatalf("marshal output schema: %v", err)
	}
	for _, field := range []string{`"context"`, `"evidence"`, `"matches"`} {
		if !strings.Contains(string(out), field) {
			t.Errorf("output schema does not describe %s: %s", field, out)
		}
	}
	// The lists are never absent, only empty, for the same reason list_contexts
	// says so: an agent reads this before it sees a result.
	if strings.Contains(string(out), "null") {
		t.Errorf("output schema admits null: %s", out)
	}
	t.Logf("search_code input schema = %s", in)
	t.Logf("search_code output schema = %s", out)
}

// fixtureConfig loads the prepared fixture's configuration, with Zoekt pointed
// at a server started for this test over the fixture's index. The fixture's own
// URL names the port a long-running Zoekt would listen on, which this test does
// not depend on being up.
func fixtureConfig(t *testing.T) *config.Config {
	t.Helper()
	fixture := demorepo.Prepared(t)

	cfg, err := config.Load(fixture.Config)
	if err != nil {
		t.Fatalf("config.Load(%s) error = %v", fixture.Config, err)
	}
	cfg.Providers.Zoekt.URL = demorepo.StartZoekt(t, fixture.ZoektIndex)
	return cfg
}

// searchSession serves search_code over cfg on a real MCP server and connects a
// client to it, so every assertion is made on what came back over a wire rather
// than on a Go value.
//
// The engine is given the graph provider cfg names even though search never
// reaches one, so doc-1 §23's eighth success criterion is tested on the wiring a
// server really runs: TestSearchCodeWorksWhenTheGraphProviderIsUnavailable
// points that provider at a binary that is not installed.
func searchSession(t *testing.T, cfg *config.Config) *mcp.ClientSession {
	t.Helper()

	srv := server.New(testVersion)
	AddSearchCode(srv, engine.New(resolver.New(cfg), zoektadapter.New(cfg), cbmadapter.New(cfg), nil))

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

// searchCode calls the tool and decodes a successful result, failing the test
// if the call reported an error.
func searchCode(t *testing.T, session *mcp.ClientSession, contextID, query string) searchCodeOutput {
	t.Helper()

	raw, isError := callSearchCode(t, session, contextID, query)
	if isError {
		t.Fatalf("search_code(%s, %q) failed: %s", contextID, query, raw)
	}
	var out searchCodeOutput
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return out
}

// call makes one tools/call and returns the JSON the client received together
// with whether it was an error result. It reads the text content rather than
// the decoded structured content on purpose: the difference between [] and null
// only survives in the bytes.
func callSearchCode(t *testing.T, session *mcp.ClientSession, contextID, query string) (string, bool) {
	t.Helper()

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "search_code",
		Arguments: map[string]any{"context": contextID, "query": query},
	})
	if err != nil {
		t.Fatalf("tools/call search_code: %v", err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("result carries %d content blocks, want 1", len(res.Content))
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("result content = %#v, want text", res.Content[0])
	}
	return text.Text, res.IsError
}

// listedContextOf is the context configuration as it must appear on the wire:
// the four public fields of the context filed under id, and no graph reference.
func listedContextOf(cfg *config.Config, id string) listedContext {
	codeCtx := cfg.Contexts[id]
	return listedContext{
		ID:         id,
		Repository: codeCtx.Repository,
		Branch:     codeCtx.Branch,
		Revision:   codeCtx.Revision,
	}
}
