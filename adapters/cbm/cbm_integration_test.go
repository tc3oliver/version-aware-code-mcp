//go:build integration

package cbm_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/internal/demorepo"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// fixture is the prepared configuration: the CBM binary to run and the contexts
// whose graph_refs are the graphs it built.
func fixture(t *testing.T) *config.Config {
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

// contextNamed returns the prepared context with that ID.
func contextNamed(t *testing.T, cfg *config.Config, id string) vacctx.CodeContext {
	t.Helper()
	codeCtx, ok := cfg.Contexts[id]
	if !ok {
		t.Fatalf("context %q is missing from the fixture configuration", id)
	}
	return codeCtx
}

// callees returns the names the graph says symbol calls.
func callees(graph *provider.CallGraph, symbol string) []string {
	var names []string
	for _, edge := range graph.Edges {
		if edge.Caller == symbol {
			names = append(names, edge.Callee)
		}
	}
	return names
}

// TestTraceCallsUsesTheGraphOfItsOwnVersion is AC #1 and AC #2, and doc-1's
// third and fourth release-gate checks at the same time: one repository, one
// symbol, two contexts, and each one's call chain is its own version's. The two
// graphs are separate CBM projects, so a trace that reached the other project
// would show up here as the wrong handler.
func TestTraceCallsUsesTheGraphOfItsOwnVersion(t *testing.T) {
	cfg := fixture(t)
	p := newProvider(t, cfg)

	tests := map[string]struct{ context, want, absent string }{
		"v1 calls LegacyHandler": {"demo-v1", "LegacyHandler", "NewHandler"},
		"v2 calls NewHandler":    {"demo-v2", "NewHandler", "LegacyHandler"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			codeCtx := contextNamed(t, cfg, tc.context)
			graph, err := p.TraceCalls(t.Context(), codeCtx, provider.TraceRequest{
				Symbol:    "Process",
				Direction: provider.Callees,
				Depth:     3,
			})
			if err != nil {
				t.Fatalf("TraceCalls(%s, Process) error = %v", tc.context, err)
			}

			if graph.Symbol != "Process" {
				t.Errorf("Symbol = %q, want %q", graph.Symbol, "Process")
			}
			called := callees(graph, "Process")
			if !contains(called, tc.want) {
				t.Errorf("Process calls %v in %s, want it to include %s", called, codeCtx.GraphRef, tc.want)
			}
			if contains(called, tc.absent) {
				t.Errorf("Process calls %v in %s, which is %s's version: the graphs are contaminated", called, codeCtx.GraphRef, tc.absent)
			}

			// A call graph that cannot be cited is not a result this server
			// returns, so every edge has to locate itself.
			for _, edge := range graph.Edges {
				if edge.Path == "" || edge.Line < 1 {
					t.Errorf("edge %s -> %s has no location (%q:%d)", edge.Caller, edge.Callee, edge.Path, edge.Line)
				}
				t.Logf("%s graph_ref=%s edge %s -> %s at %s:%d",
					codeCtx.ID, codeCtx.GraphRef, edge.Caller, edge.Callee, edge.Path, edge.Line)
			}
		})
	}
}

// TestTraceCallsWalksBothDirections covers the direction mapping: the same edge
// has to be reachable from either end, from the caller looking down and from
// the callee looking up.
func TestTraceCallsWalksBothDirections(t *testing.T) {
	cfg := fixture(t)
	codeCtx := contextNamed(t, cfg, "demo-v1")

	graph, err := newProvider(t, cfg).TraceCalls(t.Context(), codeCtx, provider.TraceRequest{
		Symbol:    "LegacyHandler",
		Direction: provider.Callers,
		Depth:     3,
	})
	if err != nil {
		t.Fatalf("TraceCalls(LegacyHandler, callers) error = %v", err)
	}

	want := provider.CallEdge{Caller: "Process", Callee: "LegacyHandler", Path: "processor.go", Line: 4}
	if len(graph.Edges) != 1 || graph.Edges[0] != want {
		t.Fatalf("Edges = %+v, want exactly %+v", graph.Edges, want)
	}
	t.Logf("%s callers of LegacyHandler: %+v", codeCtx.GraphRef, graph.Edges)
}

// TestTraceCallsAmbiguousSymbol is AC #3. The demo repository has no duplicated
// name, so the ambiguity is built as its own real CBM project: two packages,
// one function name. The adapter must report both candidates, because returning
// the call graph of whichever one CBM listed first is a wrong answer that looks
// exactly like a right one.
func TestTraceCallsAmbiguousSymbol(t *testing.T) {
	cfg := fixture(t)
	graphRef := indexAmbiguousProject(t, cfg.Providers.CBM.Command)

	codeCtx := vacctx.CodeContext{
		ID:         "ambiguous",
		Repository: "versioned-demo-repo",
		Branch:     demorepo.V1,
		Revision:   "HEAD",
		GraphRef:   graphRef,
	}
	graph, err := newProvider(t, cfg).TraceCalls(t.Context(), codeCtx, provider.TraceRequest{
		Symbol:    "Duplicated",
		Direction: provider.Callees,
		Depth:     2,
	})
	if err == nil {
		t.Fatalf("TraceCalls() = %+v, want SYMBOL_AMBIGUOUS", graph)
	}
	if graph != nil {
		t.Errorf("TraceCalls() = %+v, want no graph alongside the error", graph)
	}

	vErr := errorOf(t, err)
	if vErr.Code != vacerr.SymbolAmbiguous {
		t.Fatalf("code = %q, want SYMBOL_AMBIGUOUS (error: %v)", vErr.Code, err)
	}
	candidates, ok := vErr.Details["candidates"].([]map[string]any)
	if !ok || len(candidates) != 2 {
		t.Fatalf("details[candidates] = %v, want the two matches", vErr.Details["candidates"])
	}
	for _, candidate := range candidates {
		if qn, _ := candidate["qualified_name"].(string); qn == "" {
			t.Errorf("candidate %v has no qualified_name to disambiguate with", candidate)
		}
	}

	wire, marshalErr := vErr.MarshalJSON()
	if marshalErr != nil {
		t.Fatalf("MarshalJSON() error = %v", marshalErr)
	}
	t.Logf("SYMBOL_AMBIGUOUS wire = %s", wire)
}

// TestTraceCallsSymbolNotFound is AC #4, asked in the way that matters:
// NewHandler is a real symbol of the demo repository, present in the v2 graph
// and absent from the v1 one. Tracing it in v1 has to come back empty-handed
// rather than reach across to the version that has it.
func TestTraceCallsSymbolNotFound(t *testing.T) {
	cfg := fixture(t)
	p := newProvider(t, cfg)
	req := provider.TraceRequest{Symbol: "NewHandler", Direction: provider.Callers, Depth: 2}

	graph, err := p.TraceCalls(t.Context(), contextNamed(t, cfg, "demo-v1"), req)
	if err == nil {
		t.Fatalf("TraceCalls(demo-v1, NewHandler) = %+v, want SYMBOL_NOT_FOUND", graph)
	}
	if graph != nil {
		t.Errorf("TraceCalls() = %+v, want no graph alongside the error", graph)
	}
	if code := errorOf(t, err).Code; code != vacerr.SymbolNotFound {
		t.Errorf("code = %q, want SYMBOL_NOT_FOUND", code)
	}
	t.Logf("demo-v1 NewHandler -> %v", err)

	// The same request against the version that does have the symbol answers,
	// which is what makes the refusal above version isolation rather than a
	// broken query.
	if _, err := p.TraceCalls(t.Context(), contextNamed(t, cfg, "demo-v2"), req); err != nil {
		t.Fatalf("TraceCalls(demo-v2, NewHandler) error = %v, want it to resolve there", err)
	}
}

// TestTraceCallsGraphProviderUnavailable is AC #5, covering both ways CBM is
// not there to answer: a binary that cannot be run at all, and a running CBM
// that has never indexed the graph the context names. Neither may surface as a
// panic or a raw exec error.
func TestTraceCallsGraphProviderUnavailable(t *testing.T) {
	req := provider.TraceRequest{Symbol: "Process", Direction: provider.Callees, Depth: 2}

	t.Run("binary is not installed", func(t *testing.T) {
		p := newProvider(t, &config.Config{
			Providers: config.Providers{CBM: config.CBM{Command: filepath.Join(t.TempDir(), "codebase-memory-mcp")}},
		})
		codeCtx := vacctx.CodeContext{ID: "demo-v1", Repository: "r", Branch: "b", Revision: "c", GraphRef: "vacmcp-demo-v1"}

		graph, err := p.TraceCalls(t.Context(), codeCtx, req)
		if err == nil {
			t.Fatalf("TraceCalls() = %+v, want GRAPH_PROVIDER_UNAVAILABLE", graph)
		}
		if code := errorOf(t, err).Code; code != vacerr.GraphProviderUnavailable {
			t.Errorf("code = %q, want GRAPH_PROVIDER_UNAVAILABLE", code)
		}
		t.Logf("absent binary -> %v", err)
	})

	t.Run("graph is not indexed", func(t *testing.T) {
		cfg := fixture(t)
		codeCtx := contextNamed(t, cfg, "demo-v1")
		codeCtx.GraphRef = "vacmcp-never-indexed"

		graph, err := newProvider(t, cfg).TraceCalls(t.Context(), codeCtx, req)
		if err == nil {
			t.Fatalf("TraceCalls() = %+v, want GRAPH_PROVIDER_UNAVAILABLE", graph)
		}
		if code := errorOf(t, err).Code; code != vacerr.GraphProviderUnavailable {
			t.Errorf("code = %q, want GRAPH_PROVIDER_UNAVAILABLE", code)
		}
		t.Logf("unindexed graph_ref -> %v", err)
	})
}

// indexAmbiguousProject builds a real CBM project holding one function name in
// two packages, and returns its graph_ref. The project is deleted afterwards so
// a test run leaves nothing behind in CBM's store.
func indexAmbiguousProject(t *testing.T, command string) string {
	t.Helper()
	const graphRef = "vacmcp-cbm-adapter-ambiguous"

	dir := t.TempDir()
	files := map[string]string{
		"go.mod":       "module ambiguous\n\ngo 1.26\n",
		"alpha/dup.go": "package alpha\n\nfunc Duplicated() {}\n",
		"beta/dup.go":  "package beta\n\nfunc Duplicated() {}\n",
		"only/once.go": "package only\n\nfunc Unique() {}\n",
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

func contains(names []string, want string) bool {
	for _, name := range names {
		if strings.EqualFold(name, want) {
			return true
		}
	}
	return false
}
