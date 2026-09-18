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
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// trace_calls was the one tool with no tag-free test file. Its wire shape and
// all five of its error codes were proven only under `-tags=integration` against
// a real codebase-memory-mcp, which ci-fast.yml's tier does not build — so the
// tier whose whole purpose is to answer a pull request in a minute or two never
// checked either. The asymmetry was also the wrong way round from the usual one:
// real engine yes, mocks no.
//
// Nothing here needs an engine. The graph provider is a stub that answers, or
// fails with the code being asserted, which is exactly what the tool adapter's
// job is to carry unchanged.

// tracedContext is the one version these tests trace in.
var tracedContext = vacctx.Workspace{ID: "traced", Members: []vacctx.CodeContext{{
	ID: "traced", Repository: "alpha", Branch: "main",
	Revision: "6666666666666666666666666666666666666666", GraphRef: "alpha-main",
}}}

// tracedContexts resolves the one version above and refuses anything else with
// CONTEXT_NOT_FOUND, which is what the real resolver does. wiredContexts, which
// the search_code tests share, answers an unknown id with a zero workspace
// instead — fine for what those assert, and the wrong thing to test a refusal
// against.
type tracedContexts struct{}

func (tracedContexts) Contexts(context.Context) ([]vacctx.Workspace, error) { return nil, nil }

func (tracedContexts) Resolve(_ context.Context, id string) (vacctx.Workspace, error) {
	if id != tracedContext.ID {
		return vacctx.Workspace{}, vacerr.New(
			vacerr.ContextNotFound,
			"no context with id "+id+" is configured",
			map[string]any{"context": id},
		)
	}
	return tracedContext, nil
}

// stubGraph answers with one call edge, or with whatever error it was built to
// fail with — the graph provider is where every code below is produced, and the
// tool's job is to put it on the wire unchanged.
type stubGraph struct {
	err error
}

func (g stubGraph) TraceCalls(_ context.Context, codeCtx vacctx.CodeContext, req provider.TraceRequest) (*provider.CallGraph, error) {
	if g.err != nil {
		return nil, g.err
	}
	return &provider.CallGraph{
		Symbol: req.Symbol,
		Edges: []provider.CallEdge{{
			Caller: req.Symbol, Callee: "Handle",
			Path: codeCtx.Repository + ".go", Line: 12,
		}},
	}, nil
}

// TestTraceCallsWireShape pins what a client receives on a successful walk: the
// context it was confined to, the symbol, the edges, and the evidence citing
// where each call is written.
func TestTraceCallsWireShape(t *testing.T) {
	session := traceSession(t, stubGraph{})

	res, raw := tracedRaw(t, session, map[string]any{
		"context": "traced", "symbol": "Process", "direction": "callees", "depth": 3,
	})
	if res.IsError {
		t.Fatalf("trace_calls failed: %s", raw)
	}

	var got traceCallsWire
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}

	member := tracedContext.Members[0]
	if got.Context.ID != "traced" || got.Context.Repository != member.Repository ||
		got.Context.Branch != member.Branch || got.Context.Revision != member.Revision {
		t.Errorf("context block = %+v, want the version the walk ran in (%+v)", got.Context, member)
	}
	if got.Symbol != "Process" {
		t.Errorf("symbol = %q, want the symbol that was traced", got.Symbol)
	}
	if len(got.Calls) != 1 || got.Calls[0].Caller != "Process" || got.Calls[0].Callee != "Handle" {
		t.Errorf("calls = %+v, want the one edge the graph holds", got.Calls)
	}
	if len(got.Evidence) != 1 {
		t.Errorf("evidence = %+v, want one citation for the one edge", got.Evidence)
	}

	// GraphRef is the CBM project behind the context: internal, and a tool's
	// output is the only place it could leak from.
	if strings.Contains(raw, "graph") {
		t.Errorf("trace_calls leaked the graph reference: %s", raw)
	}
}

// TestTraceCallsErrorMapping is the fast-tier half of what
// trace_calls_integration_test.go proves against a real graph engine: every code
// the tool can answer with reaches the client inside doc-1's error envelope,
// with nothing beside it.
//
// The codes are produced by the provider rather than invented here, which is the
// point — the adapter classifies nothing, so what is under test is that it
// carries a code through rather than reducing it to a string the SDK would send
// in its place.
func TestTraceCallsErrorMapping(t *testing.T) {
	details := map[string]any{"context": "traced"}

	cases := []struct {
		name string
		args map[string]any
		from error
		want vacerr.Code
	}{
		{
			name: "context_not_found",
			args: map[string]any{"context": "no-such-context", "symbol": "Process", "direction": "callees", "depth": 3},
			want: vacerr.ContextNotFound,
		},
		{
			name: "symbol_not_found",
			args: map[string]any{"context": "traced", "symbol": "Missing", "direction": "callees", "depth": 3},
			from: vacerr.New(vacerr.SymbolNotFound, "no such symbol in this version", details),
			want: vacerr.SymbolNotFound,
		},
		{
			// The one whose details are the answer: the candidates are what a
			// caller disambiguates with, so an envelope that dropped them would
			// leave the client unable to act on the failure.
			name: "symbol_ambiguous",
			args: map[string]any{"context": "traced", "symbol": "Process", "direction": "callees", "depth": 3},
			from: vacerr.New(vacerr.SymbolAmbiguous, "several symbols match", map[string]any{
				"context": "traced", "candidates": []any{"a.Process", "b.Process"},
			}),
			want: vacerr.SymbolAmbiguous,
		},
		{
			name: "graph_provider_unavailable",
			args: map[string]any{"context": "traced", "symbol": "Process", "direction": "callees", "depth": 3},
			from: vacerr.New(vacerr.GraphProviderUnavailable, "the graph engine is not reachable", details),
			want: vacerr.GraphProviderUnavailable,
		},
		{
			// depth is refused before any provider is reached, so it is this
			// tier's to check. direction is deliberately not here: the tool
			// constrains neither field, and it is the CBM adapter that rejects an
			// unknown direction — a stub graph provider accepting "sideways"
			// would be testing the stub. That one stays with the real engine, in
			// trace_calls_integration_test.go.
			name: "invalid_depth",
			args: map[string]any{"context": "traced", "symbol": "Process", "direction": "callees", "depth": 99},
			want: vacerr.InvalidArgument,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			session := traceSession(t, stubGraph{err: testCase.from})
			failure, raw := tracedError(t, session, testCase.args)
			if failure.Code != testCase.want {
				t.Errorf("trace_calls(%v) failed with %s, want %s: %s", testCase.args, failure.Code, testCase.want, raw)
			}
			if failure.Message == "" {
				t.Errorf("trace_calls(%v) reported %s with no message: %s", testCase.args, failure.Code, raw)
			}
		})
	}
}

// TestTraceCallsWithNoGraphProvider is the code an engine built without a graph
// answers with, which no provider can produce and which therefore has no other
// way of being reached.
func TestTraceCallsWithNoGraphProvider(t *testing.T) {
	srv := server.New(testVersion)
	AddTraceCalls(srv, engine.New(tracedContexts{}, nil, nil, nil))

	failure, raw := tracedError(t, connectTo(t, srv), map[string]any{
		"context": "traced", "symbol": "Process", "direction": "callees", "depth": 3,
	})
	if failure.Code != vacerr.GraphProviderUnavailable {
		t.Errorf("trace_calls with no graph provider failed with %s, want %s: %s",
			failure.Code, vacerr.GraphProviderUnavailable, raw)
	}
}

func traceSession(t *testing.T, graph provider.GraphProvider) *mcp.ClientSession {
	t.Helper()

	srv := server.New(testVersion)
	AddTraceCalls(srv, engine.New(tracedContexts{}, nil, graph, nil))
	return connectTo(t, srv)
}

// connectTo serves srv over stateless Streamable HTTP and connects a client, so
// every assertion is made on what came back over a real wire rather than on a Go
// value.
func connectTo(t *testing.T, srv *mcp.Server) *mcp.ClientSession {
	t.Helper()

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

func tracedRaw(t *testing.T, session *mcp.ClientSession, args map[string]any) (*mcp.CallToolResult, string) {
	t.Helper()

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "trace_calls", Arguments: args})
	if err != nil {
		t.Fatalf("tools/call trace_calls(%v): %v", args, err)
	}
	if len(res.Content) == 0 {
		t.Fatalf("trace_calls(%v) returned no content", args)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("result content = %#v, want text", res.Content[0])
	}
	return res, text.Text
}

func tracedError(t *testing.T, session *mcp.ClientSession, args map[string]any) (*vacerr.Error, string) {
	t.Helper()

	res, raw := tracedRaw(t, session, args)
	if !res.IsError {
		t.Fatalf("trace_calls(%v) succeeded, want an error: %s", args, raw)
	}
	if res.StructuredContent != nil {
		t.Errorf("error result carries structured content: %v", res.StructuredContent)
	}
	var envelope struct {
		Error vacerr.Error `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return &envelope.Error, raw
}
