package engine_test

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/evidence"
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

// *resolver.Resolver is what an Engine is built with outside tests. The engine
// names only the method set it needs, so this is the one place the two have to
// be kept agreeing.
var _ engine.ContextSource = (*resolver.Resolver)(nil)

// The two method signatures, pinned from both sides. The first assignment holds
// only if ContextSource's methods are at least these; the second holds only if
// they are at most these, and together they say the interface is exactly the
// pair below — a context and an ID in, a workspace and an error out.
//
// It is spelled out here rather than left to the implementations above because
// the error on Contexts is the part most easily lost: a source that decides
// which contexts a caller may see and cannot report a failure has only the empty
// list to answer with, which says there are no versions rather than that it
// could not tell.
type contextSourceSignature interface {
	Contexts(ctx context.Context) ([]vacctx.Workspace, error)
	Resolve(ctx context.Context, id string) (vacctx.Workspace, error)
}

var (
	_ contextSourceSignature = engine.ContextSource(nil)
	_ engine.ContextSource   = contextSourceSignature(nil)
)

// single is the workspace a version context is unless it says otherwise: one
// repository, filed under the context's own ID.
//
// Every test in this package that wants today's behaviour writes it out, rather
// than handing a bare [vacctx.CodeContext] to a fake that would wrap it. What
// the tests are about is which member reached which provider, and a wrapper
// would hide exactly that while keeping everything compiling.
func single(codeCtx vacctx.CodeContext) vacctx.Workspace {
	return vacctx.Workspace{ID: codeCtx.ID, Members: []vacctx.CodeContext{codeCtx}}
}

// over is a workspace of several repositories under one ID: the shape a
// configuration can declare and no query can yet be answered in. Every member is
// filed under the workspace ID, as a resolver files them.
func over(id string, members ...vacctx.CodeContext) vacctx.Workspace {
	filed := make([]vacctx.CodeContext, 0, len(members))
	for _, member := range members {
		member.ID = id
		filed = append(filed, member)
	}
	return vacctx.Workspace{ID: id, Members: filed}
}

// fakeContexts resolves every ID to one workspace, whether or not that workspace
// is usable. A *resolver.Resolver cannot answer with an incomplete one; the
// point of the check under test is that the engine does not rely on that.
type fakeContexts struct {
	workspace vacctx.Workspace
}

func (f fakeContexts) Contexts(context.Context) ([]vacctx.Workspace, error) {
	return []vacctx.Workspace{f.workspace}, nil
}

func (f fakeContexts) Resolve(context.Context, string) (vacctx.Workspace, error) {
	return f.workspace, nil
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
		Contexts:     map[string]vacctx.Workspace{configured.ID: single(configured)},
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

// A ContextSource is not trusted to have validated what it hands back. A member
// missing any field every query is scoped by names no version to answer in, and
// is refused before a provider sees it: a provider given an empty revision reads
// whatever the checkout happens to be on, which is an answer from a version
// nobody asked for.
//
// The ID is one of them. Without it the query would reach its provider, get an
// answer, and only then fail in evidence.New — with an error that is not a
// *vacerr.Error and names no context, which is the one shape doc-1 says a
// failure never takes.
//
// Each case is run twice: with the incomplete member alone, and with it beside a
// complete one. The second is what makes this a check of every member rather
// than of the first: a workspace whose first member is perfectly usable must
// still be refused for the second, and the refusal must name the field that is
// blank rather than the fact that the workspace has two repositories in it.
func TestIncompleteResolvedContextIsRefusedBeforeAnyProvider(t *testing.T) {
	for _, tc := range []struct {
		field string
		blank func(*vacctx.CodeContext)
	}{
		{"id", func(c *vacctx.CodeContext) { c.ID = "" }},
		{"repository", func(c *vacctx.CodeContext) { c.Repository = "" }},
		{"branch", func(c *vacctx.CodeContext) { c.Branch = "" }},
		// Whitespace, not empty: a revision of " " is as unusable as none, and
		// would otherwise pass a bare != "" check.
		{"revision", func(c *vacctx.CodeContext) { c.Revision = "  " }},
	} {
		incomplete := usable
		tc.blank(&incomplete)

		// The complete member is written out with its own repository so the
		// workspace below is one a configuration could really declare: one
		// repository per member.
		complete := usable
		complete.Repository = "other"
		complete.GraphRef = "other-main"

		for _, shape := range []struct {
			name      string
			workspace vacctx.Workspace
		}{
			{"alone", vacctx.Workspace{ID: usable.ID, Members: []vacctx.CodeContext{incomplete}}},
			{"beside a complete member", vacctx.Workspace{ID: usable.ID, Members: []vacctx.CodeContext{complete, incomplete}}},
		} {
			t.Run(tc.field+", "+shape.name, func(t *testing.T) {
				search, graph, source := &fakeSearch{}, &fakeGraph{}, &fakeSource{}
				eng := engine.New(fakeContexts{workspace: shape.workspace}, search, graph, source)
				ctx := context.Background()

				// usable.ID, not incomplete.ID: what is under test is a
				// ContextSource answering a well-formed request with a context it
				// should not have, so the request itself stays well-formed.
				_, searchErr := eng.SearchCode(ctx, engine.SearchCodeRequest{Context: usable.ID, Query: "demo"})
				_, traceErr := eng.TraceCalls(ctx, engine.TraceCallsRequest{
					Context: usable.ID, Symbol: "Process", Direction: provider.Callers, Depth: 1,
				})
				_, getErr := eng.GetCode(ctx, engine.GetCodeRequest{
					Context: usable.ID, Path: "process.go", StartLine: 1, EndLine: 1,
				})

				for name, err := range map[string]error{"SearchCode": searchErr, "TraceCalls": traceErr, "GetCode": getErr} {
					if err == nil {
						t.Fatalf("%s answered in a context with an incomplete member", name)
					}
					assertCode(t, err, vacerr.InvalidArgument)
					// The blank field, not the member count: a refusal that named
					// the second repository would mean the completeness check
					// never ran on it.
					var vErr *vacerr.Error
					if !errors.As(err, &vErr) {
						t.Fatalf("%s failed with %v, want a *vacerr.Error", name, err)
					}
					if vErr.Details["field"] != tc.field {
						t.Errorf("%s says field %v, want the blank %q: %v", name, vErr.Details["field"], tc.field, err)
					}
				}

				if search.called || graph.called || source.called {
					t.Fatalf("a provider was reached with an incomplete context: search=%v graph=%v source=%v", search.called, graph.called, source.called)
				}
			})
		}
	}
}

// A context naming several repositories is refused by every query, with an error
// that says so, and no provider is reached in any of the versions it names.
//
// This is the half of the workspace model that is not implemented yet, pinned so
// it cannot be half-implemented by accident. The two failures it rules out are
// the ones that look like success: answering in the first member, which drops a
// whole repository out of a scope the caller was told it asked in, and answering
// with an empty result, which reads as "none of this code exists here".
func TestAContextOfSeveralRepositoriesIsRefusedRatherThanNarrowed(t *testing.T) {
	second := usable
	second.Repository = "other"
	second.Branch = "release/2.x"
	second.Revision = "2222222222222222222222222222222222222222"
	second.GraphRef = "other-v2"

	search, graph, source := &fakeSearch{}, &fakeGraph{}, &fakeSource{}
	eng := engine.New(fakeContexts{workspace: over(usable.ID, usable, second)}, search, graph, source)
	ctx := context.Background()

	searchOut, searchErr := eng.SearchCode(ctx, engine.SearchCodeRequest{Context: usable.ID, Query: "demo"})
	traceOut, traceErr := eng.TraceCalls(ctx, engine.TraceCallsRequest{
		Context: usable.ID, Symbol: "Process", Direction: provider.Callers, Depth: 1,
	})
	getOut, getErr := eng.GetCode(ctx, engine.GetCodeRequest{
		Context: usable.ID, Path: "process.go", StartLine: 1, EndLine: 1,
	})
	_, compareCodeErr := eng.CompareCode(ctx, engine.CompareCodeRequest{
		FromContext: usable.ID, ToContext: usable.ID, Path: "process.go",
	})
	_, compareCallsErr := eng.CompareCalls(ctx, engine.CompareCallsRequest{
		FromContext: usable.ID, ToContext: usable.ID, Symbol: "Process", Direction: provider.Callers, Depth: 1,
	})

	for name, err := range map[string]error{
		"SearchCode":   searchErr,
		"TraceCalls":   traceErr,
		"GetCode":      getErr,
		"CompareCode":  compareCodeErr,
		"CompareCalls": compareCallsErr,
	} {
		if err == nil {
			t.Fatalf("%s answered in a context naming two repositories", name)
		}
		assertCode(t, err, vacerr.InvalidArgument)
		// The message is what a caller reads, and it has to say which of its
		// contexts it cannot use and why.
		if !strings.Contains(err.Error(), usable.ID) || !strings.Contains(err.Error(), "2 repositories") {
			t.Errorf("%s failed with %q, want it to name the context and its two repositories", name, err)
		}
		var vErr *vacerr.Error
		if !errors.As(err, &vErr) {
			t.Fatalf("%s failed with %v, want a *vacerr.Error", name, err)
		}
		if got, ok := vErr.Details["repositories"].([]string); !ok || !slices.Equal(got, []string{usable.Repository, second.Repository}) {
			t.Errorf("%s says repositories %v, want both of them", vErr.Details["repositories"], []string{usable.Repository, second.Repository})
		}
	}

	// Not one of them, and not the first: a search that had reached the search
	// provider would have answered about one repository under the name of a
	// context covering two.
	if search.called || graph.called || source.called {
		t.Fatalf("a provider was reached for a context naming two repositories: search=%v graph=%v source=%v", search.called, graph.called, source.called)
	}
	for name, out := range map[string]contextual{"SearchCode": searchOut, "TraceCalls": traceOut, "GetCode": getOut} {
		t.Run(name, func(t *testing.T) { assertNotAnAnswer(t, out) })
	}
}

// A workspace with no member at all is refused the same way, and says the same
// thing a context missing its repository says: there is no version to answer in.
// A ContextSource is an interface, so "the configuration rejects this" is not a
// reason for the engine not to check it.
func TestAContextOfNoRepositoryAtAllIsRefused(t *testing.T) {
	search, graph, source := &fakeSearch{}, &fakeGraph{}, &fakeSource{}
	eng := engine.New(fakeContexts{workspace: vacctx.Workspace{ID: usable.ID}}, search, graph, source)

	searchErr, traceErr, getErr := queryAll(t, eng)
	for name, err := range map[string]error{"SearchCode": searchErr, "TraceCalls": traceErr, "GetCode": getErr} {
		if err == nil {
			t.Fatalf("%s answered in a context naming no repository", name)
		}
		assertCode(t, err, vacerr.InvalidArgument)
	}
	if search.called || graph.called || source.called {
		t.Fatalf("a provider was reached for a context naming no repository: search=%v graph=%v source=%v", search.called, graph.called, source.called)
	}
}

// The provider is handed a member of the workspace the request named, and it is
// possible to say which one: the member's own repository, branch, revision and
// graph reference arrive, not the workspace's ID with some other version's
// fields attached to it.
//
// Two workspaces, each over its own repository, are configured at once, so
// "the right member arrived" cannot be satisfied by handing over the only
// context there is. Every field differs between them for the same reason.
func TestTheMemberOfTheNamedWorkspaceIsWhatReachesTheProvider(t *testing.T) {
	v1 := vacctx.CodeContext{
		ID: "demo@v1", Repository: "demo", Branch: "release/1.x",
		Revision: "1111111111111111111111111111111111111111", GraphRef: "demo-v1",
	}
	v2 := vacctx.CodeContext{
		ID: "other@v2", Repository: "other", Branch: "release/2.x",
		Revision: "2222222222222222222222222222222222222222", GraphRef: "other-v2",
	}

	for _, want := range []vacctx.CodeContext{v1, v2} {
		t.Run(want.ID, func(t *testing.T) {
			search, graph, source := &fakeSearch{}, &fakeGraph{}, &fakeSource{}
			eng := engine.New(mapContexts{v1.ID: single(v1), v2.ID: single(v2)}, search, graph, source)
			ctx := context.Background()

			if _, err := eng.SearchCode(ctx, engine.SearchCodeRequest{Context: want.ID, Query: "demo"}); err != nil {
				t.Fatalf("SearchCode: %v", err)
			}
			if _, err := eng.TraceCalls(ctx, engine.TraceCallsRequest{
				Context: want.ID, Symbol: "Process", Direction: provider.Callers, Depth: 1,
			}); err != nil {
				t.Fatalf("TraceCalls: %v", err)
			}
			if _, err := eng.GetCode(ctx, engine.GetCodeRequest{
				Context: want.ID, Path: "process.go", StartLine: 1, EndLine: 1,
			}); err != nil {
				t.Fatalf("GetCode: %v", err)
			}

			for name, got := range map[string]vacctx.CodeContext{
				"Search":     search.codeCtx,
				"TraceCalls": graph.codeCtx,
				"Read":       source.codeCtx,
			} {
				if got != want {
					t.Errorf("%s was handed member %+v, want %+v", name, got, want)
				}
			}
		})
	}
}

// Only trace_calls names a graph, so only trace_calls needs the context to name
// one. A caller with no graph backend at all configures no graph_ref and must
// still be able to search and read code: requiring it of every query would make
// the absent graph provider break the two queries that never wanted it, which
// is the independent failure of doc-1 §23's eighth criterion given up one layer
// down, at the context instead of the provider.
// The graph reference is read off the member, which is where it lives: the
// workspace below is complete in every way a workspace can be — it has its ID
// and it has its one member — and the member is the only thing missing a
// graph_ref. A check that looked at anything but the member would find nothing
// wrong here and let trace_calls through to the graph provider.
func TestOnlyTraceCallsNeedsAGraphRef(t *testing.T) {
	searchOnly := usable
	searchOnly.GraphRef = ""
	contexts := fakeContexts{workspace: single(searchOnly)}

	// With a graph provider present the context is what is incomplete, so the
	// refusal blames the context — and still reaches no provider.
	t.Run("with a graph provider", func(t *testing.T) {
		search, graph, source := &fakeSearch{}, &fakeGraph{}, &fakeSource{}
		eng := engine.New(contexts, search, graph, source)

		searchErr, traceErr, getErr := queryAll(t, eng)
		assertCode(t, traceErr, vacerr.InvalidArgument)
		var vErr *vacerr.Error
		if !errors.As(traceErr, &vErr) {
			t.Fatalf("TraceCalls failed with %v, want a *vacerr.Error", traceErr)
		}
		if vErr.Details["field"] != "graph_ref" {
			t.Errorf("TraceCalls says field %v, want the member's blank graph_ref", vErr.Details["field"])
		}
		if searchErr != nil || getErr != nil {
			t.Fatalf("a context with no graph_ref broke a query that reads no graph: search=%v get=%v", searchErr, getErr)
		}
		if graph.called {
			t.Fatal("a context with no graph_ref reached the graph provider")
		}
		if !search.called || !source.called {
			t.Fatalf("a query that reads no graph was not run: search=%v source=%v", search.called, source.called)
		}
	})

	// The whole shape a search-only embedder has: no graph provider and no
	// graph_ref anywhere. Trace says which of the two is missing; nothing else
	// notices.
	t.Run("with no graph provider", func(t *testing.T) {
		search, source := &fakeSearch{}, &fakeSource{}
		eng := engine.New(contexts, search, nil, source)

		searchErr, traceErr, getErr := queryAll(t, eng)
		assertCode(t, traceErr, vacerr.GraphProviderUnavailable)
		if searchErr != nil || getErr != nil {
			t.Fatalf("a search-only deployment broke a query it can answer: search=%v get=%v", searchErr, getErr)
		}
		if !search.called || !source.called {
			t.Fatalf("a query that reads no graph was not run: search=%v source=%v", search.called, source.called)
		}
	})
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
	if out.Context() != configured {
		t.Fatalf("result context is %+v, want %+v", out.Context(), configured)
	}
	if len(out.Matches()) != 1 || out.Matches()[0].Path != "process.go" {
		t.Fatalf("matches are %+v, want the provider's one match", out.Matches())
	}
	// The match is its own citation: a caller can check the answer at the line
	// it was found on without asking a second question.
	want := []evidence.Evidence{evidence.At("process.go", 1, 1, "package demo")}
	if !slices.Equal(out.Evidence(), want) {
		t.Fatalf("evidence is %+v, want %+v", out.Evidence(), want)
	}
}

// A query that matches nothing still answers with a context and a citation
// list. Empty evidence says "checked, found none"; nil evidence would be
// indistinguishable from a result nobody backed.
func TestSearchCodeCitesNothingRatherThanNil(t *testing.T) {
	eng, search, _, _, configured := newEngine(t)
	search.results = nil

	out, err := eng.SearchCode(context.Background(), engine.SearchCodeRequest{Context: configured.ID, Query: "absent"})
	if err != nil {
		t.Fatalf("SearchCode: %v", err)
	}
	if out.Evidence() == nil {
		t.Fatal("a match-free search returned nil evidence, want an empty list")
	}
	if len(out.Evidence()) != 0 {
		t.Fatalf("a match-free search cited %+v", out.Evidence())
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
	if out.Context() != configured {
		t.Fatalf("result context is %+v, want %+v", out.Context(), configured)
	}
	// What the graph resolved the request to, not what was asked for.
	if out.Graph().Symbol != "demo.Process" || len(out.Graph().Edges) != 1 {
		t.Fatalf("graph is %+v, want the provider's", out.Graph())
	}
	want := []evidence.Evidence{evidence.At("process.go", 1, 1, "")}
	if !slices.Equal(out.Evidence(), want) {
		t.Fatalf("evidence is %+v, want %+v", out.Evidence(), want)
	}
}

// Several calls written on one line are one place to look, so the citation is
// listed once however many edges point at it.
func TestTraceCallsCitesEachLocationOnce(t *testing.T) {
	eng, _, graph, _, configured := newEngine(t)
	graph.graph = provider.CallGraph{
		Symbol: "demo.Process",
		Edges: []provider.CallEdge{
			{Caller: "demo.Main", Callee: "demo.Process", Path: "process.go", Line: 7},
			{Caller: "demo.Main", Callee: "demo.Cleanup", Path: "process.go", Line: 7},
			{Caller: "demo.Serve", Callee: "demo.Process", Path: "serve.go", Line: 3},
		},
	}

	out, err := eng.TraceCalls(context.Background(), engine.TraceCallsRequest{
		Context: configured.ID, Symbol: "Process", Direction: provider.Callers, Depth: 2,
	})
	if err != nil {
		t.Fatalf("TraceCalls: %v", err)
	}
	want := []evidence.Evidence{
		evidence.At("process.go", 7, 7, ""),
		evidence.At("serve.go", 3, 3, ""),
	}
	if !slices.Equal(out.Evidence(), want) {
		t.Fatalf("evidence is %+v, want %+v", out.Evidence(), want)
	}
	if len(out.Graph().Edges) != 3 {
		t.Fatalf("deduplicating citations dropped an edge: %+v", out.Graph().Edges)
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
	if out.Context().Revision != readAt {
		t.Fatalf("result revision is %q, want the revision read %q", out.Context().Revision, readAt)
	}
	if out.Source().Content != "package demo\n" {
		t.Fatalf("content is %q, want the provider's", out.Source().Content)
	}
	// The citation is the range read, and it is scoped by the revision it was
	// read at rather than the one the configuration spelled.
	want := []evidence.Evidence{evidence.At("process.go", 1, 1, "")}
	if !slices.Equal(out.Evidence(), want) {
		t.Fatalf("evidence is %+v, want %+v", out.Evidence(), want)
	}
}

// A failing provider fails the query with its own error, with no result beside
// it: half an answer from an unknown version is the failure this server exists
// to prevent. The code is the provider's own — the engine classifies nothing —
// and the zero result it returns carries neither a context nor evidence, so a
// caller that ignored the error still cannot mistake it for an answer.
func TestProviderFailuresAreReturnedUnchanged(t *testing.T) {
	// One case per provider, each with the code that provider is the source of.
	for _, tc := range []struct {
		name string
		code vacerr.Code
		call func(*engine.Engine, *fakeSearch, *fakeGraph, *fakeSource, vacctx.CodeContext, error) (contextual, error)
	}{
		{
			name: "search provider unavailable",
			code: vacerr.SearchProviderUnavailable,
			call: func(eng *engine.Engine, search *fakeSearch, _ *fakeGraph, _ *fakeSource, cfg vacctx.CodeContext, err error) (contextual, error) {
				search.err = err
				return eng.SearchCode(context.Background(), engine.SearchCodeRequest{Context: cfg.ID, Query: "demo"})
			},
		},
		{
			name: "symbol ambiguous",
			code: vacerr.SymbolAmbiguous,
			call: func(eng *engine.Engine, _ *fakeSearch, graph *fakeGraph, _ *fakeSource, cfg vacctx.CodeContext, err error) (contextual, error) {
				graph.err = err
				return eng.TraceCalls(context.Background(), engine.TraceCallsRequest{
					Context: cfg.ID, Symbol: "Process", Direction: provider.Callers, Depth: 2,
				})
			},
		},
		{
			name: "source mismatch",
			code: vacerr.SourceMismatch,
			call: func(eng *engine.Engine, _ *fakeSearch, _ *fakeGraph, source *fakeSource, cfg vacctx.CodeContext, err error) (contextual, error) {
				source.err = err
				return eng.GetCode(context.Background(), engine.GetCodeRequest{
					Context: cfg.ID, Path: "process.go", StartLine: 1, EndLine: 1,
				})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng, search, graph, source, configured := newEngine(t)
			failure := vacerr.New(tc.code, "the provider said no", map[string]any{"context": configured.ID})

			out, err := tc.call(eng, search, graph, source, configured, failure)
			assertCode(t, err, tc.code)
			if !errors.Is(err, failure) {
				t.Fatalf("error is %v, want the provider's own error", err)
			}
			assertNotAnAnswer(t, out)
		})
	}
}

// contextual is every result type: they answer Context and Evidence and
// nothing else in common, which is the whole of what a failed call must not
// return.
type contextual interface {
	Context() vacctx.CodeContext
	Evidence() []evidence.Evidence
}

var (
	_ contextual = engine.SearchCodeResult{}
	_ contextual = engine.TraceCallsResult{}
	_ contextual = engine.GetCodeResult{}
)

func assertNotAnAnswer(t *testing.T, out contextual) {
	t.Helper()
	if out.Context() != (vacctx.CodeContext{}) {
		t.Errorf("a failed call returned context %+v, want the zero context", out.Context())
	}
	if out.Evidence() != nil {
		t.Errorf("a failed call returned evidence %+v, want none", out.Evidence())
	}
}

// The contract is structural, not a convention each method remembers: every
// field of every result type is unexported, so no composite literal outside
// this package can fill one in. The only value a caller can write for itself is
// the zero one, and that carries no context and no evidence — which is exactly
// how a failed call is already told apart from an answer.
func TestASuccessfulResultCannotBeBuiltOutsideTheEngine(t *testing.T) {
	for _, out := range []contextual{engine.SearchCodeResult{}, engine.TraceCallsResult{}, engine.GetCodeResult{}} {
		typ := reflect.TypeOf(out)
		for i := range typ.NumField() {
			if field := typ.Field(i); field.IsExported() {
				t.Errorf("%s.%s is exported: a caller can build a result claiming success with no evidence behind it",
					typ.Name(), field.Name)
			}
		}
		assertNotAnAnswer(t, out)
	}
}

// ListContexts is what a caller asks before it knows which versions exist, so
// it needs no context of its own and does not fail for want of one. The IDs are
// filled in, each context carries the member it was configured with, and the
// order is stable.
func TestListContexts(t *testing.T) {
	path, head := newRepo(t)
	cfg := &config.Config{
		Repositories: map[string]config.Repository{"demo": {Path: path}},
		Contexts: map[string]vacctx.Workspace{
			"demo@v2": {Members: []vacctx.CodeContext{{Repository: "demo", Branch: "v2", Revision: head}}},
			"demo@v1": {Members: []vacctx.CodeContext{{Repository: "demo", Branch: "v1", Revision: head}}},
		},
	}
	eng := engine.New(resolver.New(cfg), &fakeSearch{}, &fakeGraph{}, &fakeSource{})

	listed, err := eng.ListContexts(context.Background())
	if err != nil {
		t.Fatalf("ListContexts: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("listed %d contexts, want 2", len(listed))
	}
	if listed[0].ID != "demo@v1" || listed[1].ID != "demo@v2" {
		t.Fatalf("listed %v, want them sorted by id with the ids filled in", listed)
	}
	// Down to the member: a listing that reported the IDs and lost what they are
	// scoped to would still pass everything above.
	for _, want := range []struct {
		at     int
		branch string
	}{{0, "v1"}, {1, "v2"}} {
		members := listed[want.at].Members
		if len(members) != 1 || members[0].Branch != want.branch || members[0].ID != listed[want.at].ID {
			t.Fatalf("%s listed members %+v, want the one branch %q it was configured with, filed under its context",
				listed[want.at].ID, members, want.branch)
		}
	}

	empty := engine.New(resolver.New(&config.Config{}), &fakeSearch{}, &fakeGraph{}, &fakeSource{})
	got, err := empty.ListContexts(context.Background())
	if err != nil {
		t.Fatalf("ListContexts of an empty configuration: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("an empty configuration listed %v, want an empty non-nil list", got)
	}
}

// unsortedContexts is a ContextSource answering in the order it was written,
// which a *resolver.Resolver never does. The order of ListContexts is the
// engine's promise, so it must not rest on the one implementation that happens
// to sort for it.
type unsortedContexts []vacctx.Workspace

func (u unsortedContexts) Contexts(context.Context) ([]vacctx.Workspace, error) { return u, nil }

func (u unsortedContexts) Resolve(_ context.Context, id string) (vacctx.Workspace, error) {
	return vacctx.Workspace{}, vacerr.New(vacerr.ContextNotFound, "no", map[string]any{"context": id})
}

func TestListContextsSortsWhateverTheSourceHandsBack(t *testing.T) {
	source := unsortedContexts{
		single(vacctx.CodeContext{ID: "demo@v2"}),
		single(vacctx.CodeContext{ID: "demo@v10"}),
		single(vacctx.CodeContext{ID: "demo@v1"}),
	}
	eng := engine.New(source, &fakeSearch{}, &fakeGraph{}, &fakeSource{})

	listed, err := eng.ListContexts(context.Background())
	if err != nil {
		t.Fatalf("ListContexts: %v", err)
	}
	want := []string{"demo@v1", "demo@v10", "demo@v2"}
	if len(listed) != len(want) {
		t.Fatalf("listed %v, want %d contexts", listed, len(want))
	}
	for i, id := range want {
		if listed[i].ID != id {
			t.Fatalf("listed %v, want them sorted by id as %v", listed, want)
		}
	}

	// The source's own slice is not the engine's to reorder: one that reuses
	// what it hands back would find it shuffled under it.
	if source[0].ID != "demo@v2" {
		t.Fatalf("ListContexts sorted the source's slice in place: %v", source)
	}
}

// A source that could not say which contexts exist has not said there are none.
// The failure is the source's own, returned unchanged and with no list beside
// it: an empty list here would tell an agent there is no version to ask about,
// which is a statement about the configuration rather than about the failure.
func TestListContextsReportsASourceThatCouldNotAnswer(t *testing.T) {
	refused := vacerr.New(vacerr.InvalidArgument, "the context source would not answer", nil)
	eng := engine.New(failingContexts{err: refused}, &fakeSearch{}, &fakeGraph{}, &fakeSource{})

	listed, err := eng.ListContexts(context.Background())
	if !errors.Is(err, refused) {
		t.Fatalf("ListContexts reported %v, want the source's own error", err)
	}
	if listed != nil {
		t.Fatalf("ListContexts returned %v beside its error, want no list at all", listed)
	}
}

// failingContexts is a source that cannot answer either question.
type failingContexts struct{ err error }

func (f failingContexts) Contexts(context.Context) ([]vacctx.Workspace, error) {
	return nil, f.err
}

func (f failingContexts) Resolve(context.Context, string) (vacctx.Workspace, error) {
	return vacctx.Workspace{}, f.err
}

// usable is the complete context the nil-provider tests are answered in. They
// need no repository on disk: what is under test is which provider a query
// reaches, and resolving through fakeContexts gets there without a git
// checkout having a say in it.
var usable = vacctx.CodeContext{
	ID: "demo@main", Repository: "demo", Branch: "main",
	Revision: "0123456789abcdef0123456789abcdef01234567", GraphRef: "demo-main",
}

// queryAll asks all three queries and reports how each one answered, so a test
// can state both halves of "only its own query": the one that failed, and the
// ones that carried on.
func queryAll(t *testing.T, eng *engine.Engine) (searchErr, traceErr, getErr error) {
	t.Helper()
	ctx := context.Background()
	_, searchErr = eng.SearchCode(ctx, engine.SearchCodeRequest{Context: usable.ID, Query: "demo"})
	_, traceErr = eng.TraceCalls(ctx, engine.TraceCallsRequest{
		Context: usable.ID, Symbol: "Process", Direction: provider.Callers, Depth: 1,
	})
	_, getErr = eng.GetCode(ctx, engine.GetCodeRequest{
		Context: usable.ID, Path: "process.go", StartLine: 1, EndLine: 1,
	})
	return searchErr, traceErr, getErr
}

// A provider a caller does not have is a provider it may leave out. The query
// needing the absent one fails with a *vacerr.Error naming what is missing —
// not a nil dereference, and not an empty result, which would read as "this
// version has no such code" — and every other query answers as usual.
func TestAMissingProviderFailsOnlyItsOwnQuery(t *testing.T) {
	contexts := fakeContexts{workspace: single(usable)}

	t.Run("no search provider", func(t *testing.T) {
		eng := engine.New(contexts, nil, &fakeGraph{}, &fakeSource{})

		searchErr, traceErr, getErr := queryAll(t, eng)
		assertCode(t, searchErr, vacerr.SearchProviderUnavailable)
		if traceErr != nil || getErr != nil {
			t.Fatalf("an absent search provider broke another query: trace=%v get=%v", traceErr, getErr)
		}
		if listed, err := eng.ListContexts(context.Background()); err != nil || len(listed) != 1 {
			t.Fatalf("an absent search provider left ListContexts answering %v", listed)
		}
	})

	t.Run("no graph provider", func(t *testing.T) {
		eng := engine.New(contexts, &fakeSearch{}, nil, &fakeSource{})

		searchErr, traceErr, getErr := queryAll(t, eng)
		assertCode(t, traceErr, vacerr.GraphProviderUnavailable)
		// doc-1 §23's eighth success criterion, in the case it was written for:
		// search keeps answering with no CBM behind the server at all.
		if searchErr != nil || getErr != nil {
			t.Fatalf("an absent graph provider broke another query: search=%v get=%v", searchErr, getErr)
		}
		if listed, err := eng.ListContexts(context.Background()); err != nil || len(listed) != 1 {
			t.Fatalf("an absent graph provider left ListContexts answering %v", listed)
		}
	})

	t.Run("no source provider", func(t *testing.T) {
		eng := engine.New(contexts, &fakeSearch{}, &fakeGraph{}, nil)

		searchErr, traceErr, getErr := queryAll(t, eng)
		assertCode(t, getErr, vacerr.RepositoryNotFound)
		if searchErr != nil || traceErr != nil {
			t.Fatalf("an absent source provider broke another query: search=%v trace=%v", searchErr, traceErr)
		}
		if listed, err := eng.ListContexts(context.Background()); err != nil || len(listed) != 1 {
			t.Fatalf("an absent source provider left ListContexts answering %v", listed)
		}
	})
}

// An Engine is a Closer, so a caller can defer its shutdown the way it defers
// any other.
var _ io.Closer = (*engine.Engine)(nil)

// closeableGraph and closeableSource are providers whose lifetime the caller
// handed over, which is to say they have a Close method for [engine.Engine] to
// find.
type closeableGraph struct {
	fakeGraph
	closed int
	err    error
}

func (c *closeableGraph) Close() error {
	c.closed++
	return c.err
}

type closeableSource struct {
	fakeSource
	closed int
}

func (c *closeableSource) Close() error {
	c.closed++
	return nil
}

// ownedElsewhere is a provider its caller keeps: closing it is someone else's
// business, and it says so by failing the test if an Engine closes it.
type ownedElsewhere struct {
	fakeSearch
	t *testing.T
}

func (o *ownedElsewhere) Close() error {
	o.t.Error("Engine.Close closed a provider whose lifetime the caller kept")
	return nil
}

// keepOpen is the documented way to keep that ownership: an embedded interface
// promotes that interface's methods and no others, so this is a SearchProvider
// that is not an io.Closer however closeable the value inside it is.
type keepOpen struct{ provider.SearchProvider }

// Close closes what the caller handed over to be closed, and nothing else. The
// wrapped provider is closeable and is still not closed, which is the half
// that matters for an embedding caller: a provider it manages itself is not
// shut down under it by an Engine that happens to hold a reference.
//
// It also proves a failing Close does not swallow the rest: the graph provider
// refuses, and the source provider after it is still closed and the refusal is
// still reported.
func TestCloseClosesOnlyWhatWasHandedOver(t *testing.T) {
	kept := &ownedElsewhere{t: t}
	refuses := errors.New("the session would not shut down")
	graph := &closeableGraph{err: refuses}
	source := &closeableSource{}

	eng := engine.New(fakeContexts{workspace: single(usable)}, keepOpen{kept}, graph, source)

	err := eng.Close()
	if !errors.Is(err, refuses) {
		t.Fatalf("Close reported %v, want the graph provider's refusal", err)
	}
	if graph.closed != 1 {
		t.Fatalf("the graph provider was closed %d times, want once", graph.closed)
	}
	if source.closed != 1 {
		t.Fatalf("the source provider was closed %d times, want once: a failing Close skipped the providers after it", source.closed)
	}
}

// Nothing to close is not a failure. A provider that cannot be closed is left
// alone, and an absent one is not dereferenced.
func TestCloseWithNothingToClose(t *testing.T) {
	eng := engine.New(fakeContexts{workspace: single(usable)}, &fakeSearch{}, nil, nil)

	if err := eng.Close(); err != nil {
		t.Fatalf("closing an Engine with nothing to close reported %v", err)
	}
}
