package tools

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	cbmadapter "github.com/tc3oliver/version-aware-code-mcp/adapters/cbm"
	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/internal/demorepo"
	"github.com/tc3oliver/version-aware-code-mcp/resolver"
	"github.com/tc3oliver/version-aware-code-mcp/server"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// Everything here calls trace_calls over a real MCP session against a real
// codebase-memory-mcp and the graphs testdata/prepare-fixture.sh built from the
// versioned demo repository. There is no fake graph engine: a mock would only
// prove the tool agrees with our own idea of CBM, and the property under test —
// that a trace answers from the graph its context names and no other — is a
// property of the two real projects.

// traceCallsOutputWire is the whole of a successful result as a client reads it:
// doc-1's Tool Contract half plus the tool's own.
type traceCallsOutputWire struct {
	Context struct {
		ID         string `json:"id"`
		Repository string `json:"repository"`
		Branch     string `json:"branch"`
		Revision   string `json:"revision"`
	} `json:"context"`
	Evidence []struct {
		Location struct {
			Path      string `json:"path"`
			StartLine int    `json:"start_line"`
			EndLine   int    `json:"end_line"`
		} `json:"location"`
	} `json:"evidence"`
	Symbol    string `json:"symbol"`
	Direction string `json:"direction"`
	Depth     int    `json:"depth"`
	Calls     []call `json:"calls"`
}

// traceCallsErrorWire is the error shape of doc-1's error model.
type traceCallsErrorWire struct {
	Error struct {
		Code    vacerr.Code    `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

// AC #1 and AC #4, which are doc-1's third and fourth release-gate checks asked
// through the tool: one repository, one symbol, two contexts, and each answer
// is its own version's. The graphs are separate CBM projects, so a query that
// reached the other one shows up here as the wrong handler.
func TestTraceCallsAnswersFromTheContextOfTheRequest(t *testing.T) {
	cfg := traceFixture(t)

	tests := map[string]struct{ context, branch, want, absent string }{
		"v1 calls LegacyHandler": {"demo-v1", demorepo.V1, "LegacyHandler", "NewHandler"},
		"v2 calls NewHandler":    {"demo-v2", demorepo.V2, "NewHandler", "LegacyHandler"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			raw := traceCallsOK(t, cfg, map[string]any{
				"context": tc.context, "symbol": "Process", "direction": "callees", "depth": 3,
			})

			var got traceCallsOutputWire
			if err := json.Unmarshal([]byte(raw), &got); err != nil {
				t.Fatalf("decode %s: %v", raw, err)
			}

			// AC #1: the context of the answer is the context of the question.
			configured := cfg.Contexts[tc.context]
			if got.Context.ID != tc.context || got.Context.Repository != configured.Repository ||
				got.Context.Branch != tc.branch || got.Context.Revision != configured.Revision {
				t.Errorf("context = %+v, want the configured %s (%+v)", got.Context, tc.context, configured)
			}
			if got.Symbol != "Process" || got.Direction != "callees" || got.Depth != 3 {
				t.Errorf("result describes %q/%s/depth %d, want Process/callees/depth 3", got.Symbol, got.Direction, got.Depth)
			}

			called := calleesOf(got.Calls, "Process")
			if !contains(called, tc.want) {
				t.Errorf("Process calls %v in %s, want it to include %s", called, tc.context, tc.want)
			}
			if contains(called, tc.absent) {
				t.Errorf("Process calls %v in %s, which is the other version's: the answer crossed contexts", called, tc.absent)
			}

			// AC #4: every call is located, and the locations are listed as the
			// evidence backing the result. A call graph that cannot be cited is
			// not a result this server returns.
			if len(got.Evidence) == 0 {
				t.Errorf("result carries %d calls and no evidence: %s", len(got.Calls), raw)
			}
			for _, item := range got.Evidence {
				if item.Location.Path == "" || item.Location.StartLine < 1 {
					t.Errorf("evidence %+v does not locate anything", item.Location)
				}
			}
			for _, c := range got.Calls {
				if c.Path == "" || c.Line < 1 {
					t.Errorf("call %s -> %s has no location (%q:%d)", c.Caller, c.Callee, c.Path, c.Line)
				}
			}

			// The graph reference is the CBM project backing the context. It is
			// internal, and a tool's output is the only place it could leak from.
			if strings.Contains(raw, configured.GraphRef) {
				t.Errorf("output leaked the graph reference %q: %s", configured.GraphRef, raw)
			}
			t.Logf("%s -> %s", tc.context, raw)
		})
	}
}

// AC #5. An unconfigured context is refused by name; the server does not fall
// back to the context that does exist, which is the whole point of asking.
func TestTraceCallsUnknownContext(t *testing.T) {
	cfg := traceFixture(t)

	body := traceCallsError(t, cfg, map[string]any{
		"context": "demo-v3", "symbol": "Process", "direction": "callees", "depth": 3,
	})
	if body.Error.Code != vacerr.ContextNotFound {
		t.Errorf("code = %q, want CONTEXT_NOT_FOUND", body.Error.Code)
	}
	if strings.Contains(body.Error.Message, "demo-v1") || strings.Contains(body.Error.Message, "demo-v2") {
		t.Errorf("the error suggests another context: %s", body.Error.Message)
	}
	t.Logf("unknown context -> %+v", body.Error)
}

// AC #3, asked in the way that matters: NewHandler is a real symbol of the demo
// repository, present in the v2 graph and absent from the v1 one. Tracing it in
// v1 has to come back empty-handed rather than reach across to the version that
// has it.
func TestTraceCallsSymbolNotFound(t *testing.T) {
	cfg := traceFixture(t)
	args := map[string]any{"context": "demo-v1", "symbol": "NewHandler", "direction": "callers", "depth": 2}

	body := traceCallsError(t, cfg, args)
	if body.Error.Code != vacerr.SymbolNotFound {
		t.Errorf("code = %q, want SYMBOL_NOT_FOUND", body.Error.Code)
	}
	t.Logf("demo-v1 NewHandler -> %+v", body.Error)

	// The same request against the version that does have the symbol answers,
	// which is what makes the refusal above version isolation rather than a
	// broken query.
	args["context"] = "demo-v2"
	t.Logf("demo-v2 NewHandler -> %s", traceCallsOK(t, cfg, args))
}

// AC #2. The demo repository duplicates no name, so the ambiguity is built as
// its own real CBM project: two packages, one function name. Both candidates
// have to reach the client, because returning the call graph of whichever one
// CBM listed first is a wrong answer that looks exactly like a right one.
func TestTraceCallsAmbiguousSymbol(t *testing.T) {
	cfg := traceFixture(t)
	demo := cfg.Contexts["demo-v1"]
	cfg.Contexts["ambiguous"] = vacctx.CodeContext{
		ID:         "ambiguous",
		Repository: demo.Repository,
		Branch:     demo.Branch,
		Revision:   demo.Revision,
		GraphRef:   indexAmbiguousProject(t, cfg.Providers.CBM.Command),
	}

	body := traceCallsError(t, cfg, map[string]any{
		"context": "ambiguous", "symbol": "Duplicated", "direction": "callees", "depth": 2,
	})
	if body.Error.Code != vacerr.SymbolAmbiguous {
		t.Fatalf("code = %q, want SYMBOL_AMBIGUOUS", body.Error.Code)
	}

	candidates, ok := body.Error.Details["candidates"].([]any)
	if !ok || len(candidates) != 2 {
		t.Fatalf("details[candidates] = %v, want the two matches", body.Error.Details["candidates"])
	}
	for _, candidate := range candidates {
		named, _ := candidate.(map[string]any)
		if qualified, _ := named["qualified_name"].(string); qualified == "" {
			t.Errorf("candidate %v has no qualified_name to disambiguate with", candidate)
		}
	}
	t.Logf("ambiguous symbol -> %+v", body.Error)
}

// AC #6. depth crosses a trust boundary, so a value outside 1-5 is refused
// here: not clamped into range, and not handed to CBM.
//
// The graph engine is deliberately absent from this configuration. Anything
// that reached it would come back as GRAPH_PROVIDER_UNAVAILABLE, which is what
// the last case shows: the same request with a legal depth does reach CBM, so
// the refusals above are the depth check and not some earlier failure.
func TestTraceCallsRejectsDepthOutsideTheRange(t *testing.T) {
	cfg := traceFixture(t)
	cfg.Providers.CBM.Command = filepath.Join(t.TempDir(), "codebase-memory-mcp")

	for _, depth := range []any{0, -1, 6, 100} {
		body := traceCallsError(t, cfg, map[string]any{
			"context": "demo-v1", "symbol": "Process", "direction": "callees", "depth": depth,
		})
		if body.Error.Code != vacerr.InvalidArgument {
			t.Errorf("depth %v: code = %q, want INVALID_ARGUMENT", depth, body.Error.Code)
		}
		if got := body.Error.Details["depth"]; got != float64(depth.(int)) {
			t.Errorf("depth %v: details[depth] = %v, want the depth that was asked for", depth, got)
		}
		t.Logf("depth %v -> %+v", depth, body.Error)
	}

	// A missing depth is out of range too: there is no default level to walk.
	if code := traceCallsError(t, cfg, map[string]any{
		"context": "demo-v1", "symbol": "Process", "direction": "callees",
	}).Error.Code; code != vacerr.InvalidArgument {
		t.Errorf("omitted depth: code = %q, want INVALID_ARGUMENT", code)
	}

	body := traceCallsError(t, cfg, map[string]any{
		"context": "demo-v1", "symbol": "Process", "direction": "callees", "depth": 3,
	})
	if body.Error.Code != vacerr.GraphProviderUnavailable {
		t.Fatalf("depth 3: code = %q, want it to have reached CBM (GRAPH_PROVIDER_UNAVAILABLE)", body.Error.Code)
	}
	t.Logf("depth 3 with no CBM -> %+v", body.Error)
}

// A CBM that cannot be run degrades trace_calls and says so. Only the graph
// feature is affected: search_code does not come through here.
func TestTraceCallsGraphProviderUnavailable(t *testing.T) {
	cfg := traceFixture(t)
	cfg.Providers.CBM.Command = filepath.Join(t.TempDir(), "codebase-memory-mcp")

	body := traceCallsError(t, cfg, map[string]any{
		"context": "demo-v1", "symbol": "Process", "direction": "callees", "depth": 2,
	})
	if body.Error.Code != vacerr.GraphProviderUnavailable {
		t.Errorf("code = %q, want GRAPH_PROVIDER_UNAVAILABLE", body.Error.Code)
	}
	t.Logf("absent CBM binary -> %+v", body.Error)
}

// TestTraceCallsIsDiscoverable checks what an agent sees before it ever calls
// the tool: the four arguments, and a description that names the direction
// values and the depth range it will be held to.
func TestTraceCallsIsDiscoverable(t *testing.T) {
	tools, err := traceCallsSession(t, traceFixture(t)).ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}

	var tool *mcp.Tool
	for _, candidate := range tools.Tools {
		if candidate.Name == "trace_calls" {
			tool = candidate
		}
	}
	if tool == nil {
		t.Fatalf("tools/list returned %d tools, none named trace_calls", len(tools.Tools))
	}

	schema, ok := tool.InputSchema.(map[string]any)
	if !ok {
		t.Fatalf("input schema = %#v, want a JSON object", tool.InputSchema)
	}
	properties, _ := schema["properties"].(map[string]any)
	for _, field := range []string{"context", "symbol", "direction", "depth"} {
		if _, declared := properties[field]; !declared {
			t.Errorf("input schema does not declare %q: %v", field, properties)
		}
	}
	for _, mention := range []string{"callers", "callees", "1 to 5"} {
		if !strings.Contains(tool.Description, mention) {
			t.Errorf("description does not mention %q: %s", mention, tool.Description)
		}
	}
}

// traceFixture is the prepared configuration: the CBM binary to run, and the
// contexts whose graph_refs are the graphs it built.
func traceFixture(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(demorepo.Prepared(t).Config)
	if err != nil {
		t.Fatalf("load fixture config: %v", err)
	}
	if _, err := exec.LookPath(cfg.Providers.CBM.Command); err != nil {
		t.Skipf("codebase-memory-mcp is not runnable at %q: %v", cfg.Providers.CBM.Command, err)
	}
	return cfg
}

// traceCallsSession serves a vacmcp server carrying trace_calls over stateless
// Streamable HTTP and connects a client to it, so every assertion is made on
// what came back over a real wire rather than on a Go value.
func traceCallsSession(t *testing.T, cfg *config.Config) *mcp.ClientSession {
	t.Helper()

	srv := server.New(testVersion)
	AddTraceCalls(srv, resolver.New(cfg), cbmadapter.New(cfg))

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

// traceCallsResultText calls the tool and returns the text content the client
// received, together with whether the call was reported as an error.
func traceCallsResultText(t *testing.T, cfg *config.Config, args map[string]any) (string, bool) {
	t.Helper()

	res, err := traceCallsSession(t, cfg).CallTool(t.Context(), &mcp.CallToolParams{Name: "trace_calls", Arguments: args})
	if err != nil {
		t.Fatalf("tools/call trace_calls(%v): %v", args, err)
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

// traceCallsOK calls the tool and fails unless it answered.
func traceCallsOK(t *testing.T, cfg *config.Config, args map[string]any) string {
	t.Helper()
	raw, isError := traceCallsResultText(t, cfg, args)
	if isError {
		t.Fatalf("trace_calls(%v) reported an error: %s", args, raw)
	}
	return raw
}

// traceCallsError calls the tool and fails unless it refused, in doc-1's error
// shape. A refusal that is not a coded error is not an answer a client can act
// on, so the decode is part of the assertion.
func traceCallsError(t *testing.T, cfg *config.Config, args map[string]any) traceCallsErrorWire {
	t.Helper()
	raw, isError := traceCallsResultText(t, cfg, args)
	if !isError {
		t.Fatalf("trace_calls(%v) = %s, want an error result", args, raw)
	}

	var body traceCallsErrorWire
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if body.Error.Code == "" || body.Error.Message == "" {
		t.Fatalf("error result is not the documented shape: %s", raw)
	}
	return body
}

// indexAmbiguousProject builds a real CBM project holding one function name in
// two packages, and returns its graph_ref. The project is deleted afterwards so
// a test run leaves nothing behind in CBM's store.
func indexAmbiguousProject(t *testing.T, command string) string {
	t.Helper()
	const graphRef = "vacmcp-trace-calls-ambiguous"

	dir := t.TempDir()
	files := map[string]string{
		"go.mod":       "module ambiguous\n\ngo 1.26\n",
		"alpha/dup.go": "package alpha\n\nfunc Duplicated() {}\n",
		"beta/dup.go":  "package beta\n\nfunc Duplicated() {}\n",
	}
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("WriteFile(%s): %v", path, err)
		}
	}

	if out, err := exec.Command(command, "cli", "index_repository", "--repo-path", dir, "--name", graphRef).CombinedOutput(); err != nil {
		t.Fatalf("cbm index_repository: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		if out, err := exec.Command(command, "cli", "delete_project", "--project", graphRef).CombinedOutput(); err != nil {
			t.Logf("cbm delete_project %s: %v\n%s", graphRef, err, out)
		}
	})
	return graphRef
}

// calleesOf returns the names the result says symbol calls.
func calleesOf(calls []call, symbol string) []string {
	var names []string
	for _, c := range calls {
		if c.Caller == symbol {
			names = append(names, c.Callee)
		}
	}
	return names
}

func contains(names []string, want string) bool {
	for _, name := range names {
		if strings.EqualFold(name, want) {
			return true
		}
	}
	return false
}
