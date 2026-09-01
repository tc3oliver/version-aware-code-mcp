package engine_test

import (
	"context"
	"maps"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/evidence"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// mapContexts is a [engine.ContextSource] that is a Go map and nothing else: no
// configuration file, no git, no *resolver.Resolver. It is here because
// "the engine only needs the interface" is a claim that is only worth as much
// as the second implementation proving it.
//
// Its values are workspaces rather than bare contexts, and every test that
// wants one repository writes single(…) for it. That is deliberate: a fake that
// took a [vacctx.CodeContext] and wrapped it into a workspace on the way out
// would keep every test in this package compiling while hiding the very thing
// they are now about — which member of which workspace the engine resolved and
// handed to a provider.
type mapContexts map[string]vacctx.Workspace

func (m mapContexts) Contexts(context.Context) ([]vacctx.Workspace, error) {
	out := slices.Collect(maps.Values(m))
	slices.SortFunc(out, func(a, b vacctx.Workspace) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func (m mapContexts) Resolve(_ context.Context, id string) (vacctx.Workspace, error) {
	workspace, ok := m[id]
	if !ok {
		return vacctx.Workspace{}, vacerr.New(
			vacerr.ContextNotFound,
			"no context named "+id,
			map[string]any{"context": id},
		)
	}
	return workspace, nil
}

// A whole Engine built from implementations that exist only in this file
// answers all four queries. Nothing here knows what Zoekt, CBM or git are:
// mapContexts is a map, and fakeSearch, fakeGraph and fakeSource are canned
// answers, so what is exercised is the engine's own chain — resolve, check,
// call the provider, build the result — over interface values alone.
//
// This is the extension contract of doc-1 stated as a test rather than as
// prose: anyone with an SCIP index, another graph service or a contexts.json
// can reach the same four answers by writing these four small types.
func TestEngineRunsOnImplementationsOfNothingButItsInterfaces(t *testing.T) {
	v1 := vacctx.CodeContext{
		ID: "demo@v1", Repository: "demo", Branch: "v1",
		Revision: "1111111111111111111111111111111111111111", GraphRef: "demo-v1",
	}
	v2 := vacctx.CodeContext{
		ID: "demo@v2", Repository: "demo", Branch: "v2",
		Revision: "2222222222222222222222222222222222222222", GraphRef: "demo-v2",
	}
	search := &fakeSearch{results: []provider.SearchResult{{Path: "process.go", Line: 12, Snippet: "func Process()"}}}
	graph := &fakeGraph{graph: provider.CallGraph{
		Symbol: "demo.Process",
		Edges:  []provider.CallEdge{{Caller: "demo.Main", Callee: "demo.Process", Path: "main.go", Line: 4}},
	}}
	source := &fakeSource{content: provider.SourceContent{
		Path: "process.go", StartLine: 12, EndLine: 12, Content: "func Process() {\n", Revision: v2.Revision,
	}}
	eng := engine.New(mapContexts{v1.ID: single(v1), v2.ID: single(v2)}, search, graph, source)
	ctx := context.Background()

	listed, err := eng.ListContexts(ctx)
	if err != nil {
		t.Fatalf("ListContexts: %v", err)
	}
	if want := []vacctx.Workspace{single(v1), single(v2)}; !reflect.DeepEqual(listed, want) {
		t.Fatalf("ListContexts returned %+v, want the source's own contexts %+v", listed, want)
	}

	searched, err := eng.SearchCode(ctx, engine.SearchCodeRequest{Context: v2.ID, Query: "Process"})
	if err != nil {
		t.Fatalf("SearchCode: %v", err)
	}
	if search.codeCtx != v2 {
		t.Fatalf("search provider got context %+v, want %+v", search.codeCtx, v2)
	}
	// The provider's own match, with the member it was found in written onto it
	// by the engine — a search backend reports where a match is, and which
	// version that is comes from which context was asked.
	wantMatches := []engine.Match{{
		Repository: v2.Repository, Revision: v2.Revision,
		Path: "process.go", Line: 12, Snippet: "func Process()",
	}}
	if !slices.Equal(searched.Matches(), wantMatches) {
		t.Fatalf("matches are %+v, want the provider's %+v attributed to %s", searched.Matches(), search.results, v2.Repository)
	}
	if got := answeredIn(t, searched); got != v2 {
		t.Fatalf("result context is %+v, want a workspace of only %+v", searched.Context(), v2)
	}
	if want := []evidence.Evidence{evidence.At("process.go", 12, 12, "func Process()")}; !slices.Equal(citedIn(t, searched), want) {
		t.Fatalf("evidence is %+v, want %+v", searched.Evidence(), want)
	}

	traced, err := eng.TraceCalls(ctx, engine.TraceCallsRequest{
		Context: v2.ID, Symbol: "Process", Direction: provider.Callers, Depth: 2,
	})
	if err != nil {
		t.Fatalf("TraceCalls: %v", err)
	}
	if graph.codeCtx != v2 {
		t.Fatalf("graph provider got context %+v, want %+v", graph.codeCtx, v2)
	}
	if traced.Graph().Symbol != "demo.Process" || !slices.Equal(traced.Graph().Edges, graph.graph.Edges) {
		t.Fatalf("graph is %+v, want the provider's %+v", traced.Graph(), graph.graph)
	}
	if got := answeredIn(t, traced); got != v2 {
		t.Fatalf("result context is %+v, want a workspace of only %+v", traced.Context(), v2)
	}
	if want := []evidence.Evidence{evidence.At("main.go", 4, 4, "")}; !slices.Equal(citedIn(t, traced), want) {
		t.Fatalf("evidence is %+v, want %+v", traced.Evidence(), want)
	}

	read, err := eng.GetCode(ctx, engine.GetCodeRequest{
		Context: v2.ID, Path: "process.go", StartLine: 12, EndLine: 12,
	})
	if err != nil {
		t.Fatalf("GetCode: %v", err)
	}
	if source.codeCtx != v2 || source.path != "process.go" {
		t.Fatalf("source provider got %+v %q", source.codeCtx, source.path)
	}
	if read.Source() != source.content {
		t.Fatalf("content is %+v, want the provider's %+v", read.Source(), source.content)
	}
	if got := answeredIn(t, read); got != v2 {
		t.Fatalf("result context is %+v, want a workspace of only %+v", read.Context(), v2)
	}
	if want := []evidence.Evidence{evidence.At("process.go", 12, 12, "")}; !slices.Equal(citedIn(t, read), want) {
		t.Fatalf("evidence is %+v, want %+v", read.Evidence(), want)
	}

	// The custom source decides what exists, and the engine reports its refusal
	// as its own: an ID this map does not hold is CONTEXT_NOT_FOUND here for the
	// same reason it is with the real resolver installed.
	if _, err := eng.SearchCode(ctx, engine.SearchCodeRequest{Context: "demo@v3", Query: "Process"}); err != nil {
		assertCode(t, err, vacerr.ContextNotFound)
	} else {
		t.Fatal("SearchCode answered for a context the source does not hold")
	}
}

// The other half of the claim: the engine does not merely work without a
// backend and without MCP, it cannot see either. An import of any of the
// packages below would make the interfaces above decoration — the package would
// still compile, and the next change would quietly reach for the concrete type.
//
// `go list -deps` rather than a grep over the source, for two reasons: it is
// the toolchain's own answer to "what does this package pull in", so it cannot
// disagree with the build, and it is transitive, so an adapter reached through
// a dependency fails this too. Its default is the non-test package — the fakes
// in this file are not in its output, which is what makes them legal and a real
// import not.
//
// This runs in the tag-free build, so ci-fast.yml's Tier 1 already pays for it:
// the gate needs no workflow step of its own, and cannot be one that the
// workflow and the test disagree about.
func TestEngineDependsOnNoBackendOrProtocol(t *testing.T) {
	const module = "github.com/tc3oliver/version-aware-code-mcp"

	// Each entry is a package tree the engine must not reach, and what reaching
	// it would mean. The first two are backends. The rest are the protocol and
	// the transport that sit *above* the engine: doc-1's split is that MCP,
	// JSON-RPC and HTTP ask the engine questions and are never asked from inside
	// it, so an engine that could name them is one no second front end — a CLI,
	// an LSP server, a library caller — could reuse without dragging a server in.
	forbidden := []struct{ tree, why string }{
		{module + "/adapters", "it is written against a backend rather than against provider and ContextSource"},
		{module + "/resolver", "it is written against a backend rather than against ContextSource"},
		{"github.com/modelcontextprotocol/go-sdk", "the answers would be shaped by the protocol that asked for them"},
		{"net/http", "it would carry a transport, and what vacmcp answers does not depend on how it was asked"},
		{module + "/tools", "the engine would depend on the MCP adapters that are supposed to depend on it"},
		{module + "/server", "the engine would depend on the server that is supposed to serve it"},
		{module + "/cmd", "the engine would depend on the binary that is supposed to build it"},
	}

	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps .: %v", err)
	}

	for _, dep := range strings.Fields(string(out)) {
		for _, banned := range forbidden {
			// The tree and everything under it, rather than a bare prefix: the
			// latter would also match a package whose name merely starts the same
			// way, and report an import nobody made.
			if dep == banned.tree || strings.HasPrefix(dep, banned.tree+"/") {
				t.Errorf("engine depends on %s, so %s", dep, banned.why)
			}
		}
	}
}
