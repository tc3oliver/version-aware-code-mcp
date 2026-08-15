//go:build integration

package tools

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	cbmadapter "github.com/tc3oliver/version-aware-code-mcp/adapters/cbm"
	gitadapter "github.com/tc3oliver/version-aware-code-mcp/adapters/git"
	zoektadapter "github.com/tc3oliver/version-aware-code-mcp/adapters/zoekt"
	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/evidence"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/resolver"
	"github.com/tc3oliver/version-aware-code-mcp/server"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// The engine is what vacmcp answers and MCP is one way of asking. This file is
// the evidence for the second half of that sentence: over one fixture, an
// engine call and the matching tool call return the same context, the same
// evidence, the same payload, the same error code and the same version
// isolation.
//
// Both sides are real and independent — two engines over one *config.Config,
// each with its own Zoekt, CBM and git providers — so agreement here is two
// separately built stacks reaching the same answer rather than one value being
// read twice. Nothing is faked: what a fake would prove is that this package
// agrees with our own idea of an engine.

// parityWire is a tool result as a client receives it, wide enough for all
// three tools. Fields a given tool does not emit stay at their zero value on
// both sides of the comparison, so one type serves for all of them rather than
// three near-identical ones. Decoding into it is also what pins the field names:
// a wrong json tag leaves the field it should have filled empty.
type parityWire struct {
	// doc-1's Tool Contract half, on every successful result.
	Context  listedContext       `json:"context"`
	Evidence []evidence.Evidence `json:"evidence"`

	// search_code's payload.
	Matches []searchMatch `json:"matches"`

	// trace_calls's payload. Direction and depth are deliberately absent: the
	// tool echoes back what was asked, which is a property of the request rather
	// than an answer the engine produced, so there is nothing to compare them to.
	Symbol string `json:"symbol"`
	Calls  []call `json:"calls"`

	// get_code's payload.
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Content   string `json:"content"`
}

// The two versions of the demo repository, and the handler that exists in each.
// Process calls LegacyHandler on release/v1 and NewHandler on release/v2, so
// every "other" below is a symbol that really exists — in the other version.
// An answer that crossed contexts shows up as the wrong handler rather than as
// a difference in a number.
var parityVersions = []struct{ id, own, other string }{
	{v1, "LegacyHandler", "NewHandler"},
	{v2, "NewHandler", "LegacyHandler"},
}

// TestEngineAndMCPAnswerIdentically is decision-8's release gate: the protocol
// adapter adds no answer of its own and drops none of the engine's.
//
// It matters because every version guarantee this project makes is tested at
// the engine, where it needs no server and no wire. That is only worth
// something if the tool an agent actually calls returns what the engine
// decided — including the negative answers, which is where a leak would hide:
// an empty result and a wrong-version result look alike until you ask both
// sides the same question.
func TestEngineAndMCPAnswerIdentically(t *testing.T) {
	cfg := parityFixture(t)
	eng := parityEngine(t, cfg)
	session := paritySession(t, cfg)

	// list_contexts first, for the reason an agent calls it first: it is the
	// only tool whose answer is the set of versions rather than something inside
	// one, so if the two sides disagree here they are not even talking about the
	// same configuration.
	t.Run("list_contexts", func(t *testing.T) {
		direct := list(eng.ListContexts(t.Context()))

		raw, isError := parityCall(t, session, "list_contexts", map[string]any{})
		if isError {
			t.Fatalf("list_contexts failed: %s", raw)
		}
		var wire listContextsOutput
		if err := json.Unmarshal([]byte(raw), &wire); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}

		if !reflect.DeepEqual(direct, wire.Contexts) {
			t.Errorf("the engine lists %+v and list_contexts lists %+v", direct, wire.Contexts)
		}
		if len(direct) == 0 {
			t.Fatal("the fixture configured no contexts, so nothing below compares anything")
		}
		t.Logf("both sides list %+v (wire: %s)", direct, raw)
	})

	for _, version := range parityVersions {
		t.Run(version.id, func(t *testing.T) {
			t.Run("search_code", func(t *testing.T) { paritySearchCode(t, eng, session, version.id, version.own, version.other) })
			t.Run("trace_calls", func(t *testing.T) { parityTraceCalls(t, eng, session, version.id, version.own, version.other) })
			t.Run("get_code", func(t *testing.T) { parityGetCode(t, eng, session, version.id, version.own, version.other) })
		})
	}
}

// paritySearchCode compares the two paths through search_code: an answer, the
// empty answer that is version isolation, and a refusal.
func paritySearchCode(t *testing.T, eng *engine.Engine, session *mcp.ClientSession, contextID, own, other string) {
	t.Helper()

	search := func(query string) parityWire {
		t.Helper()
		result, err := eng.SearchCode(t.Context(), engine.SearchCodeRequest{Context: contextID, Query: query})
		if err != nil {
			t.Fatalf("engine.SearchCode(%s, %s): %v", contextID, query, err)
		}
		assertScoped(t, result.Context(), contextID)

		matches := make([]searchMatch, 0, len(result.Matches()))
		for _, match := range result.Matches() {
			matches = append(matches, searchMatch{Path: match.Path, Line: match.Line, Snippet: match.Snippet})
		}
		direct := parityWire{
			Context:  onTheWire(result.Context()),
			Evidence: result.Evidence(),
			Matches:  matches,
		}
		wire, raw := parityResult(t, session, "search_code", map[string]any{"context": contextID, "query": query})
		return assertParity(t, "search_code("+contextID+", "+query+")", direct, wire, raw)
	}

	// The symbol this version has. Both sides find it, and find the same ones.
	if found := search(own); len(found.Matches) == 0 {
		t.Errorf("search_code(%s, %s) found nothing on either side, so agreeing on it proves nothing", contextID, own)
	}

	// decision-8's case: the symbol the *other* version has. The answer is empty
	// rather than an error — that branch simply does not contain it — and it has
	// to be equally empty through both paths. A tool that widened its scope, or
	// an engine that did, would answer this one.
	if absent := search(other); len(absent.Matches) != 0 {
		t.Errorf("search_code(%s, %s) = %+v, want no matches: that symbol belongs to the other version", contextID, other, absent.Matches)
	}

	// A context that does not exist, refused identically. An empty result here
	// would read as "this version has no such code", which is a claim about code
	// the server has no version to make.
	_, err := eng.SearchCode(t.Context(), engine.SearchCodeRequest{Context: "demo-v3", Query: own})
	assertCodeParity(t, "search_code(demo-v3)", directCode(t, err),
		parityErrorCode(t, session, "search_code", map[string]any{"context": "demo-v3", "query": own}),
		vacerr.ContextNotFound)
}

// parityTraceCalls compares the two paths through trace_calls: a walk, and the
// refusal that is version isolation — the other version's symbol is not in this
// version's graph, so tracing it is SYMBOL_NOT_FOUND on both sides rather than
// a reach across to the graph that has it.
func parityTraceCalls(t *testing.T, eng *engine.Engine, session *mcp.ClientSession, contextID, own, other string) {
	t.Helper()

	args := map[string]any{"context": contextID, "symbol": "Process", "direction": "callees", "depth": 3}
	result, err := eng.TraceCalls(t.Context(), engine.TraceCallsRequest{
		Context: contextID, Symbol: "Process", Direction: provider.Callees, Depth: 3,
	})
	if err != nil {
		t.Fatalf("engine.TraceCalls(%s, Process): %v", contextID, err)
	}
	assertScoped(t, result.Context(), contextID)

	graph := result.Graph()
	calls := make([]call, 0, len(graph.Edges))
	for _, edge := range graph.Edges {
		calls = append(calls, call{Caller: edge.Caller, Callee: edge.Callee, Path: edge.Path, Line: edge.Line})
	}
	direct := parityWire{
		Context:  onTheWire(result.Context()),
		Evidence: result.Evidence(),
		Symbol:   graph.Symbol,
		Calls:    calls,
	}
	wire, raw := parityResult(t, session, "trace_calls", args)
	traced := assertParity(t, "trace_calls("+contextID+", Process)", direct, wire, raw)

	// Agreement is only worth something if both sides walked a real graph, and
	// the graph they walked has to be this version's.
	called := calleesOf(traced.Calls, "Process")
	if !contains(called, own) {
		t.Errorf("Process calls %v in %s, want it to include %s", called, contextID, own)
	}
	if contains(called, other) {
		t.Errorf("Process calls %v in %s, which is the other version's handler: the trace crossed graphs", called, contextID)
	}

	// The other version's symbol, refused by both. Not an empty graph: a walk
	// that found nothing and a symbol that is not there are different answers.
	_, err = eng.TraceCalls(t.Context(), engine.TraceCallsRequest{
		Context: contextID, Symbol: other, Direction: provider.Callers, Depth: 2,
	})
	assertCodeParity(t, "trace_calls("+contextID+", "+other+")", directCode(t, err),
		parityErrorCode(t, session, "trace_calls", map[string]any{
			"context": contextID, "symbol": other, "direction": "callers", "depth": 2,
		}),
		vacerr.SymbolNotFound)
}

// parityGetCode compares the two paths through get_code: the same file read at
// each version's revision, and a path no revision has.
func parityGetCode(t *testing.T, eng *engine.Engine, session *mcp.ClientSession, contextID, own, other string) {
	t.Helper()

	// processor.go lines 4 to 6 are the body of Process() on every branch, which
	// is where the versions differ.
	const path = "processor.go"
	args := map[string]any{"context": contextID, "path": path, "start_line": 4, "end_line": 6}

	result, err := eng.GetCode(t.Context(), engine.GetCodeRequest{
		Context: contextID, Path: path, StartLine: 4, EndLine: 6,
	})
	if err != nil {
		t.Fatalf("engine.GetCode(%s, %s): %v", contextID, path, err)
	}
	assertScoped(t, result.Context(), contextID)

	src := result.Source()
	direct := parityWire{
		Context:   onTheWire(result.Context()),
		Evidence:  result.Evidence(),
		Path:      src.Path,
		StartLine: src.StartLine,
		EndLine:   src.EndLine,
		Content:   src.Content,
	}
	wire, raw := parityResult(t, session, "get_code", args)
	read := assertParity(t, "get_code("+contextID+", "+path+")", direct, wire, raw)

	// The bytes both sides agreed on are this version's bytes. Without this the
	// two could agree on the wrong version's content and still pass.
	if !strings.Contains(read.Content, own) || strings.Contains(read.Content, other) {
		t.Errorf("get_code(%s, %s) = %q, want the %s body", contextID, path, read.Content, own)
	}

	// A path no revision has, refused identically. Empty content would be
	// indistinguishable from an empty file.
	_, err = eng.GetCode(t.Context(), engine.GetCodeRequest{
		Context: contextID, Path: "does/not/exist.go", StartLine: 1, EndLine: 1,
	})
	assertCodeParity(t, "get_code("+contextID+", does/not/exist.go)", directCode(t, err),
		parityErrorCode(t, session, "get_code", map[string]any{
			"context": contextID, "path": "does/not/exist.go", "start_line": 1, "end_line": 1,
		}),
		vacerr.InvalidArgument)
}

// assertParity is the comparison itself, and returns the agreed result so the
// caller can go on to check it is not agreement on nothing.
func assertParity(t *testing.T, what string, direct parityWire, wire parityWire, raw string) parityWire {
	t.Helper()
	if !reflect.DeepEqual(direct, wire) {
		t.Errorf("%s:\n  engine: %+v\n     MCP: %+v", what, direct, wire)
	}
	t.Logf("%s: both sides answered %s", what, raw)
	return wire
}

// assertCodeParity requires both paths to have refused with the same code, and
// with the code the contract names. Comparing them to each other alone would be
// satisfied by both being wrong in the same way.
func assertCodeParity(t *testing.T, what string, direct, wire, want vacerr.Code) {
	t.Helper()
	if direct != want {
		t.Errorf("%s: the engine failed with %q, want %q", what, direct, want)
	}
	if wire != direct {
		t.Errorf("%s: the engine failed with %q and the MCP tool with %q", what, direct, wire)
	}
	t.Logf("%s: both sides refused with %s", what, direct)
}

// assertScoped checks the half of the engine's context that MCP does not carry.
//
// GraphRef is the CBM project backing a context: internal, and deliberately not
// on the wire, so the parity comparison cannot see it. That makes this the only
// place it can be checked at all — and it has to be, because an empty one is
// how a trace would end up in no graph in particular.
func assertScoped(t *testing.T, codeCtx vacctx.CodeContext, contextID string) {
	t.Helper()
	if codeCtx.ID != contextID {
		t.Errorf("the engine answered in context %q, want %q", codeCtx.ID, contextID)
	}
	if strings.TrimSpace(codeCtx.GraphRef) == "" {
		t.Errorf("context %q reached a provider with no graph_ref", contextID)
	}
}

// onTheWire is the context as MCP publishes it: the four public fields, without
// the graph reference. It mirrors the evidence package's own projection, which
// is what makes it the right shape to compare a tool result against.
func onTheWire(codeCtx vacctx.CodeContext) listedContext {
	return listedContext{
		ID:         codeCtx.ID,
		Repository: codeCtx.Repository,
		Branch:     codeCtx.Branch,
		Revision:   codeCtx.Revision,
	}
}

// parityFixture is the prepared fixture's configuration, with a Zoekt started
// for this test. It also requires CBM to be runnable, because trace_calls is
// half of what is being compared and a missing graph engine would turn that
// half into an unnoticed pass.
func parityFixture(t *testing.T) *config.Config {
	t.Helper()
	cfg := fixtureConfig(t)
	if _, err := exec.LookPath(cfg.Providers.CBM.Command); err != nil {
		t.Skipf("codebase-memory-mcp is not runnable at %q: %v", cfg.Providers.CBM.Command, err)
	}
	return cfg
}

// parityEngine builds the engine with all three providers, which is the wiring
// cmd/vacmcp runs. Every other test in this package builds one with the
// providers its own tool reaches and nil for the rest; here both sides have to
// answer every tool.
func parityEngine(t *testing.T, cfg *config.Config) *engine.Engine {
	t.Helper()
	eng := engine.New(resolver.New(cfg), zoektadapter.New(cfg), cbmadapter.New(cfg), gitadapter.New(cfg))
	t.Cleanup(func() { _ = eng.Close() })
	return eng
}

// paritySession serves every tool over a real MCP server on its own engine, and
// connects a client to it. Its engine is a second one over the same
// configuration rather than the caller's: two stacks agreeing is the claim, and
// sharing one would leave only the encoding tested.
//
// It is the wiring cmd/vacmcp runs, so the comparison tools are registered here
// too — compare_code_integration_test.go and compare_calls_integration_test.go
// ask this session for them rather than standing up a second server that would
// have to be kept the same as this one.
func paritySession(t *testing.T, cfg *config.Config) *mcp.ClientSession {
	t.Helper()

	srv := server.New(testVersion)
	eng := parityEngine(t, cfg)
	AddListContexts(srv, eng)
	AddSearchCode(srv, eng)
	AddTraceCalls(srv, eng)
	AddGetCode(srv, eng)
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

// parityCall makes one tools/call and returns the JSON text the client received
// together with whether it was an error result. The text rather than the
// decoded structured content, for the reason the other integration tests give:
// the difference between [] and null only survives in the bytes.
func parityCall(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("tools/call %s(%v): %v", name, args, err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("%s result carries %d content blocks, want 1", name, len(res.Content))
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("%s result content = %#v, want text", name, res.Content[0])
	}
	return text.Text, res.IsError
}

// parityResult calls a tool, requires it to have answered, and decodes what came
// back. The raw text travels with it so a failed comparison can be read.
func parityResult(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) (parityWire, string) {
	t.Helper()

	raw, isError := parityCall(t, session, name, args)
	if isError {
		t.Fatalf("%s(%v) failed: %s", name, args, raw)
	}
	var wire parityWire
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	// The graph reference is internal, and a tool's output is the only place it
	// could leak from. The comparison above cannot catch it: it is not a field
	// parityWire has.
	if strings.Contains(raw, "graph_ref") || strings.Contains(raw, "vacmcp-demo-") {
		t.Errorf("%s leaked the graph reference: %s", name, raw)
	}
	return wire, raw
}

// parityErrorCode calls a tool, requires it to have refused, and returns the
// code it refused with.
func parityErrorCode(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) vacerr.Code {
	t.Helper()

	raw, isError := parityCall(t, session, name, args)
	if !isError {
		t.Fatalf("%s(%v) answered with %s, want an error result", name, args, raw)
	}
	var body struct {
		Error struct {
			Code vacerr.Code `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return body.Error.Code
}

// directCode is the code an engine error carries, which is the same error model
// the wire shape above is built from.
func directCode(t *testing.T, err error) vacerr.Code {
	t.Helper()
	if err == nil {
		t.Fatal("the engine answered, want it to have refused")
	}
	var vErr *vacerr.Error
	if !errors.As(err, &vErr) {
		t.Fatalf("the engine failed with %v (%T), want a *vacerr.Error", err, err)
	}
	return vErr.Code
}
