package engine_test

import (
	"context"
	"maps"
	"os/exec"
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
type mapContexts map[string]vacctx.CodeContext

func (m mapContexts) Contexts() []vacctx.CodeContext {
	out := slices.Collect(maps.Values(m))
	slices.SortFunc(out, func(a, b vacctx.CodeContext) int { return strings.Compare(a.ID, b.ID) })
	return out
}

func (m mapContexts) Resolve(_ context.Context, id string) (vacctx.CodeContext, error) {
	codeCtx, ok := m[id]
	if !ok {
		return vacctx.CodeContext{}, vacerr.New(
			vacerr.ContextNotFound,
			"no context named "+id,
			map[string]any{"context": id},
		)
	}
	return codeCtx, nil
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
	eng := engine.New(mapContexts{v1.ID: v1, v2.ID: v2}, search, graph, source)
	ctx := context.Background()

	if listed := eng.ListContexts(ctx); !slices.Equal(listed, []vacctx.CodeContext{v1, v2}) {
		t.Fatalf("ListContexts returned %+v, want the source's own contexts %+v", listed, []vacctx.CodeContext{v1, v2})
	}

	searched, err := eng.SearchCode(ctx, engine.SearchCodeRequest{Context: v2.ID, Query: "Process"})
	if err != nil {
		t.Fatalf("SearchCode: %v", err)
	}
	if search.codeCtx != v2 {
		t.Fatalf("search provider got context %+v, want %+v", search.codeCtx, v2)
	}
	if !slices.Equal(searched.Matches(), search.results) {
		t.Fatalf("matches are %+v, want the provider's %+v", searched.Matches(), search.results)
	}
	if searched.Context() != v2 {
		t.Fatalf("result context is %+v, want %+v", searched.Context(), v2)
	}
	if want := []evidence.Evidence{evidence.At("process.go", 12, 12, "func Process()")}; !slices.Equal(searched.Evidence(), want) {
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
	if traced.Context() != v2 {
		t.Fatalf("result context is %+v, want %+v", traced.Context(), v2)
	}
	if want := []evidence.Evidence{evidence.At("main.go", 4, 4, "")}; !slices.Equal(traced.Evidence(), want) {
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
	if read.Context() != v2 {
		t.Fatalf("result context is %+v, want %+v", read.Context(), v2)
	}
	if want := []evidence.Evidence{evidence.At("process.go", 12, 12, "")}; !slices.Equal(read.Evidence(), want) {
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
// backend, it cannot see one. An import of an adapter or of the resolver would
// make the interfaces above decoration — the package would still compile, and
// the next change would quietly reach for the concrete type.
//
// `go list -deps` rather than a grep over the source, for two reasons: it is
// the toolchain's own answer to "what does this package pull in", so it cannot
// disagree with the build, and it is transitive, so an adapter reached through
// a dependency fails this too. Its default is the non-test package — the fakes
// in this file are not in its output, which is what makes them legal and a real
// import not.
func TestEngineDependsOnNoBackend(t *testing.T) {
	const module = "github.com/tc3oliver/version-aware-code-mcp"

	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps .: %v", err)
	}

	for _, dep := range strings.Fields(string(out)) {
		if strings.HasPrefix(dep, module+"/adapters/") || dep == module+"/resolver" {
			t.Errorf("engine depends on %s, so it is written against a backend rather than against provider and ContextSource", dep)
		}
	}
}
