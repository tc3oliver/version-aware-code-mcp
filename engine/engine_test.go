package engine_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/resolver"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// The providers are fakes: what an engine test is about is which version scope
// reaches a provider, and a real Zoekt or CBM would answer that question no
// differently while making the test need one.
type fakeSearch struct {
	called  bool
	codeCtx vacctx.CodeContext
	query   provider.SearchQuery
	results []provider.SearchResult
	err     error
}

func (f *fakeSearch) Search(_ context.Context, codeCtx vacctx.CodeContext, query provider.SearchQuery) ([]provider.SearchResult, error) {
	f.called, f.codeCtx, f.query = true, codeCtx, query
	return f.results, f.err
}

type fakeGraph struct {
	called  bool
	codeCtx vacctx.CodeContext
	req     provider.TraceRequest
	graph   provider.CallGraph
	err     error
}

func (f *fakeGraph) TraceCalls(_ context.Context, codeCtx vacctx.CodeContext, req provider.TraceRequest) (*provider.CallGraph, error) {
	f.called, f.codeCtx, f.req = true, codeCtx, req
	if f.err != nil {
		return nil, f.err
	}
	return &f.graph, nil
}

type fakeSource struct {
	called  bool
	codeCtx vacctx.CodeContext
	path    string
	start   int
	end     int
	content provider.SourceContent
	err     error
}

func (f *fakeSource) Read(_ context.Context, codeCtx vacctx.CodeContext, path string, start, end int) (*provider.SourceContent, error) {
	f.called, f.codeCtx, f.path, f.start, f.end = true, codeCtx, path, start, end
	if f.err != nil {
		return nil, f.err
	}
	return &f.content, nil
}

// newRepo builds a real one-commit git repository and returns its path and the
// SHA of that commit. The resolver runs git, so a context can only resolve
// against a repository that exists.
func newRepo(t *testing.T) (path, head string) {
	t.Helper()
	path = t.TempDir()

	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", path}, args...)...)
		// The machine's own git settings — signing, hooks, a default branch
		// name — must not change what the test observes.
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}

	git("init")
	if err := os.WriteFile(filepath.Join(path, "process.go"), []byte("package demo\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	git("add", ".")
	git("-c", "user.name=vacmcp", "-c", "user.email=vacmcp@example.invalid", "commit", "-m", "v1")
	return path, git("rev-parse", "HEAD")
}

// newEngine returns an engine serving one context, "demo@main", over a real
// repository, plus the fakes it answers through and the context as configured.
func newEngine(t *testing.T) (*engine.Engine, *fakeSearch, *fakeGraph, *fakeSource, vacctx.CodeContext) {
	t.Helper()
	path, head := newRepo(t)
	configured := vacctx.CodeContext{
		ID:         "demo@main",
		Repository: "demo",
		Branch:     "main",
		Revision:   head,
		GraphRef:   "demo-main",
	}
	cfg := &config.Config{
		Repositories: map[string]config.Repository{"demo": {Path: path}},
		Contexts:     map[string]vacctx.CodeContext{configured.ID: configured},
	}
	search, graph, source := &fakeSearch{}, &fakeGraph{}, &fakeSource{}
	return engine.New(resolver.New(cfg), search, graph, source), search, graph, source, configured
}

func assertCode(t *testing.T, err error, want vacerr.Code) {
	t.Helper()
	var vErr *vacerr.Error
	if !errors.As(err, &vErr) {
		t.Fatalf("error is %v, want a *vacerr.Error", err)
	}
	if vErr.Code != want {
		t.Fatalf("code is %s, want %s", vErr.Code, want)
	}
}

// An ID that names no configured version is refused by every query, before any
// provider is reached. Guessing a context, or answering with an empty result,
// would report another version's code — or its absence — as this one's.
func TestUnknownContextIsContextNotFound(t *testing.T) {
	eng, search, graph, source, _ := newEngine(t)
	ctx := context.Background()

	if _, err := eng.SearchCode(ctx, engine.SearchCodeRequest{Context: "nope", Query: "handler"}); err != nil {
		assertCode(t, err, vacerr.ContextNotFound)
	} else {
		t.Fatal("SearchCode answered for an unconfigured context")
	}

	if _, err := eng.TraceCalls(ctx, engine.TraceCallsRequest{Context: "nope", Symbol: "Process", Direction: provider.Callers, Depth: 1}); err != nil {
		assertCode(t, err, vacerr.ContextNotFound)
	} else {
		t.Fatal("TraceCalls answered for an unconfigured context")
	}

	if _, err := eng.GetCode(ctx, engine.GetCodeRequest{Context: "nope", Path: "process.go", StartLine: 1, EndLine: 1}); err != nil {
		assertCode(t, err, vacerr.ContextNotFound)
	} else {
		t.Fatal("GetCode answered for an unconfigured context")
	}

	if search.called || graph.called || source.called {
		t.Fatalf("a provider was reached for an unconfigured context: search=%v graph=%v source=%v", search.called, graph.called, source.called)
	}
}

// The provider is handed the resolved context, graph reference included, and
// the result says which version it was answered in.
func TestSearchCodeAnswersInTheResolvedContext(t *testing.T) {
	eng, search, _, _, configured := newEngine(t)
	search.results = []provider.SearchResult{{Path: "process.go", Line: 1, Snippet: "package demo"}}

	out, err := eng.SearchCode(context.Background(), engine.SearchCodeRequest{Context: configured.ID, Query: "demo"})
	if err != nil {
		t.Fatalf("SearchCode: %v", err)
	}
	if search.codeCtx != configured {
		t.Fatalf("provider got context %+v, want %+v", search.codeCtx, configured)
	}
	if search.query.Query != "demo" {
		t.Fatalf("provider got query %q, want %q", search.query.Query, "demo")
	}
	if out.Context != configured {
		t.Fatalf("result context is %+v, want %+v", out.Context, configured)
	}
	if len(out.Matches) != 1 || out.Matches[0].Path != "process.go" {
		t.Fatalf("matches are %+v, want the provider's one match", out.Matches)
	}
}

// doc-1 bounds depth at 1 to 5. Out of range is refused rather than clamped: a
// clamped walk answers a question nobody asked and looks like the answer to the
// one they did.
func TestTraceCallsRefusesDepthOutsideTheRange(t *testing.T) {
	for _, depth := range []int{0, -1, 6} {
		eng, _, graph, _, configured := newEngine(t)

		_, err := eng.TraceCalls(context.Background(), engine.TraceCallsRequest{
			Context: configured.ID, Symbol: "Process", Direction: provider.Callers, Depth: depth,
		})
		if err == nil {
			t.Fatalf("depth %d was accepted", depth)
		}
		assertCode(t, err, vacerr.InvalidArgument)
		if graph.called {
			t.Fatalf("depth %d reached the graph provider", depth)
		}
	}
}

func TestTraceCallsAnswersInTheResolvedContext(t *testing.T) {
	eng, _, graph, _, configured := newEngine(t)
	graph.graph = provider.CallGraph{
		Symbol: "demo.Process",
		Edges:  []provider.CallEdge{{Caller: "demo.Main", Callee: "demo.Process", Path: "process.go", Line: 1}},
	}

	out, err := eng.TraceCalls(context.Background(), engine.TraceCallsRequest{
		Context: configured.ID, Symbol: "Process", Direction: provider.Callers, Depth: 2,
	})
	if err != nil {
		t.Fatalf("TraceCalls: %v", err)
	}
	if graph.codeCtx != configured {
		t.Fatalf("provider got context %+v, want %+v", graph.codeCtx, configured)
	}
	if graph.req != (provider.TraceRequest{Symbol: "Process", Direction: provider.Callers, Depth: 2}) {
		t.Fatalf("provider got request %+v", graph.req)
	}
	if out.Context != configured {
		t.Fatalf("result context is %+v, want %+v", out.Context, configured)
	}
	// What the graph resolved the request to, not what was asked for.
	if out.Graph.Symbol != "demo.Process" || len(out.Graph.Edges) != 1 {
		t.Fatalf("graph is %+v, want the provider's", out.Graph)
	}
}

// The revision reported is the one the bytes came from, not the spelling the
// configuration used, so a caller can check the claim instead of trusting it.
func TestGetCodeReportsTheRevisionActuallyRead(t *testing.T) {
	eng, _, _, source, configured := newEngine(t)
	const readAt = "0123456789abcdef0123456789abcdef01234567"
	source.content = provider.SourceContent{
		Path: "process.go", StartLine: 1, EndLine: 1, Content: "package demo\n", Revision: readAt,
	}

	out, err := eng.GetCode(context.Background(), engine.GetCodeRequest{
		Context: configured.ID, Path: "process.go", StartLine: 1, EndLine: 1,
	})
	if err != nil {
		t.Fatalf("GetCode: %v", err)
	}
	if source.codeCtx != configured || source.path != "process.go" || source.start != 1 || source.end != 1 {
		t.Fatalf("provider got %+v %q %d-%d", source.codeCtx, source.path, source.start, source.end)
	}
	if out.Context.Revision != readAt {
		t.Fatalf("result revision is %q, want the revision read %q", out.Context.Revision, readAt)
	}
	if out.Source.Content != "package demo\n" {
		t.Fatalf("content is %q, want the provider's", out.Source.Content)
	}
}

// A failing provider fails the query with its own error, with no result beside
// it: half an answer from an unknown version is the failure this server exists
// to prevent.
func TestProviderFailuresAreReturnedUnchanged(t *testing.T) {
	eng, _, _, source, configured := newEngine(t)
	source.err = vacerr.NewSourceMismatch("aaa", "bbb", nil)

	out, err := eng.GetCode(context.Background(), engine.GetCodeRequest{
		Context: configured.ID, Path: "process.go", StartLine: 1, EndLine: 1,
	})
	assertCode(t, err, vacerr.SourceMismatch)
	if out.Source.Content != "" || out.Context.ID != "" {
		t.Fatalf("a failed read returned %+v", out)
	}
}

// ListContexts is what a caller asks before it knows which versions exist, so
// it needs no context of its own and cannot fail. The IDs are filled in and the
// order is stable.
func TestListContexts(t *testing.T) {
	path, head := newRepo(t)
	cfg := &config.Config{
		Repositories: map[string]config.Repository{"demo": {Path: path}},
		Contexts: map[string]vacctx.CodeContext{
			"demo@v2": {Repository: "demo", Branch: "v2", Revision: head},
			"demo@v1": {Repository: "demo", Branch: "v1", Revision: head},
		},
	}
	eng := engine.New(resolver.New(cfg), &fakeSearch{}, &fakeGraph{}, &fakeSource{})

	listed := eng.ListContexts(context.Background())
	if len(listed) != 2 {
		t.Fatalf("listed %d contexts, want 2", len(listed))
	}
	if listed[0].ID != "demo@v1" || listed[1].ID != "demo@v2" {
		t.Fatalf("listed %v, want them sorted by id with the ids filled in", listed)
	}

	empty := engine.New(resolver.New(&config.Config{}), &fakeSearch{}, &fakeGraph{}, &fakeSource{})
	if got := empty.ListContexts(context.Background()); got == nil || len(got) != 0 {
		t.Fatalf("an empty configuration listed %v, want an empty non-nil list", got)
	}
}
