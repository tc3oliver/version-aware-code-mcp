package engine_test

import (
	"context"
	"errors"
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

// recordingGraph answers per repository and keeps every context it was handed,
// in order — the counterpart of [recordingSearch] for the one query that reads a
// graph.
//
// It records the whole [vacctx.CodeContext] and not just the repository name,
// because what decides which CBM project is queried is the member's GraphRef: a
// backend handed the right repository with another member's graph reference
// would walk the wrong graph and report it under the right name.
//
// Every call is kept rather than the last one. "The other member's graph was not
// queried" is a claim about calls that did not happen, and a fake remembering
// only the most recent one could not tell a walk that asked both members from
// one that asked only the named member.
type recordingGraph struct {
	graphs map[string]provider.CallGraph
	fail   map[string]error
	calls  []vacctx.CodeContext
}

func (r *recordingGraph) TraceCalls(_ context.Context, codeCtx vacctx.CodeContext, _ provider.TraceRequest) (*provider.CallGraph, error) {
	r.calls = append(r.calls, codeCtx)
	if err := r.fail[codeCtx.Repository]; err != nil {
		return nil, err
	}
	graph, ok := r.graphs[codeCtx.Repository]
	if !ok {
		return nil, vacerr.New(
			vacerr.SymbolNotFound,
			"no such symbol in "+codeCtx.Repository,
			map[string]any{"context": codeCtx.ID},
		)
	}
	return &graph, nil
}

// The two repositories of [stack] declare a symbol of the same name and nothing
// else in common: different call sites, different files, different callers. A
// walk that answered from the wrong member could not be mistaken for one that
// answered from the right one, and a walk that somehow merged the two would
// carry edges from both.
//
// Each symbol is spelled with its own repository as the first component, which
// is what makes "no edge spans two repositories" checkable at all: the
// repository an edge belongs to can be read off the edge.
var stackGraphs = map[string]provider.CallGraph{
	"alpha": {
		Symbol: "alpha.Process",
		Edges: []provider.CallEdge{
			{Caller: "alpha.Main", Callee: "alpha.Process", Path: "alpha/main.go", Line: 4},
			{Caller: "alpha.Retry", Callee: "alpha.Process", Path: "alpha/retry.go", Line: 18},
		},
	},
	"beta": {
		Symbol: "beta.Process",
		Edges: []provider.CallEdge{
			{Caller: "beta.Serve", Callee: "beta.Process", Path: "beta/serve.go", Line: 9},
		},
	},
}

// repositoryOf is the repository a symbol in [stackGraphs] belongs to.
func repositoryOf(symbol string) string {
	repository, _, _ := strings.Cut(symbol, ".")
	return repository
}

// A context naming several repositories has no walk until the caller says which
// one, so it is refused — and the refusal has to be one the caller can act on.
//
// The two failures being ruled out are the ones that look like an answer:
// walking the first member, which would report one repository's call graph under
// the name of a context covering two, and an empty graph, which would read as
// "this version calls nothing". The third thing being ruled out is a true
// refusal that does not help: told only that this server answers about one
// repository, a caller cannot tell that the fix is a field it left blank, and
// cannot guess a name it is unable to see the configuration for.
func TestTraceCallsRefusesAWorkspaceOfSeveralRepositoriesWithoutOne(t *testing.T) {
	graph := &recordingGraph{graphs: stackGraphs}
	eng := engine.New(fakeContexts{workspace: stack}, &fakeSearch{}, graph, &fakeSource{})

	out, err := eng.TraceCalls(context.Background(), engine.TraceCallsRequest{
		Context: stack.ID, Symbol: "Process", Direction: provider.Callers, Depth: 2,
	})
	if err == nil {
		t.Fatal("TraceCalls walked a context naming two repositories with no repository named")
	}
	assertCode(t, err, vacerr.InvalidArgument)
	assertNotAnAnswer(t, out)
	if len(graph.calls) != 0 {
		t.Fatalf("a context naming two repositories reached the graph provider as %+v", graph.calls)
	}

	// The message says which argument is missing, because that is the only thing
	// the caller can do about it.
	if !strings.Contains(err.Error(), "repository is required") {
		t.Errorf("the refusal reads %q, want it to say that repository is required", err)
	}
	if !strings.Contains(err.Error(), stack.ID) || !strings.Contains(err.Error(), "2 repositories") {
		t.Errorf("the refusal reads %q, want it to name the context and its two repositories", err)
	}

	// And it carries the names to choose between: the caller cannot see the
	// configuration, so a refusal without them leaves it with nothing to ask
	// again with.
	var vErr *vacerr.Error
	if !errors.As(err, &vErr) {
		t.Fatalf("TraceCalls failed with %v, want a *vacerr.Error", err)
	}
	if got, ok := vErr.Details["repositories"].([]string); !ok || !slices.Equal(got, []string{"alpha", "beta"}) {
		t.Errorf("the refusal offers %v, want the repositories this context names", vErr.Details["repositories"])
	}
}

// Repository names one member and the graph of that member is the only one
// queried. Not queried and discarded, which would read the same in the result
// and would still be a walk of a version the caller narrowed away from — and,
// against a real backend, a CBM project loaded for nothing.
//
// The member named is deliberately the second one. Narrowing to the first is the
// one case a selection that quietly ignores the argument still gets right, so it
// is the case that proves the least.
func TestTraceCallsNarrowedToOneMemberWalksNoOther(t *testing.T) {
	beta := stack.Members[1]
	graph := &recordingGraph{graphs: stackGraphs}
	eng := engine.New(fakeContexts{workspace: stack}, &fakeSearch{}, graph, &fakeSource{})

	if _, err := eng.TraceCalls(context.Background(), engine.TraceCallsRequest{
		Context: stack.ID, Repository: "beta", Symbol: "Process", Direction: provider.Callers, Depth: 2,
	}); err != nil {
		t.Fatalf("TraceCalls: %v", err)
	}

	// Exactly one call, with beta's own context: its graph reference is what
	// scopes the query, and alpha's never left the workspace.
	if !slices.Equal(graph.calls, []vacctx.CodeContext{beta}) {
		t.Fatalf("the graph provider was called with %+v, want one call carrying only beta's context %+v", graph.calls, beta)
	}
}

// The heart of it: both repositories have a symbol spelled the same way, and the
// walk follows the one the request named. The two graphs are never mixed, and
// nothing tries to decide that alpha.Process and beta.Process are the same
// function — a same-name match across repositories is the semantic symbol
// identity this server does not claim, so the repository is what the answer
// follows.
//
// The second half is the guarantee that falls out of it: every edge of the
// result has its caller and its callee in the repository that was walked. There
// is no cross-repository edge to be found because there was one graph in the
// question, and an edge joining two repositories could only have been invented
// by matching names.
func TestTraceCallsFollowsTheNamedRepositoryAndSpansNoOther(t *testing.T) {
	for _, want := range stack.Members {
		t.Run(want.Repository, func(t *testing.T) {
			graph := &recordingGraph{graphs: stackGraphs}
			eng := engine.New(fakeContexts{workspace: stack}, &fakeSearch{}, graph, &fakeSource{})

			out, err := eng.TraceCalls(context.Background(), engine.TraceCallsRequest{
				Context: stack.ID, Repository: want.Repository, Symbol: "Process",
				Direction: provider.Callers, Depth: 2,
			})
			if err != nil {
				t.Fatalf("TraceCalls: %v", err)
			}

			// The named member's graph, whole and unmixed: its resolved symbol and
			// its edges, and none of the other member's.
			wanted := stackGraphs[want.Repository]
			if out.Graph().Symbol != wanted.Symbol || !slices.Equal(out.Graph().Edges, wanted.Edges) {
				t.Fatalf("the walk answered %+v, want %q's own graph %+v", out.Graph(), want.Repository, wanted)
			}

			// Answered in the member that was walked, so the version on the answer
			// is the version the answer came from.
			if got := answeredIn(t, out); got != want {
				t.Fatalf("result context is %+v, want a workspace of only %+v", out.Context(), want)
			}

			// No edge joins two repositories. Both ends of every edge, because an
			// edge is only inside one repository if both of them are.
			for _, edge := range out.Graph().Edges {
				if repositoryOf(edge.Caller) != want.Repository || repositoryOf(edge.Callee) != want.Repository {
					t.Errorf("edge %s -> %s spans repositories, in a walk of %q alone",
						edge.Caller, edge.Callee, want.Repository)
				}
			}

			// The citations are the walked member's call sites and nothing else:
			// evidence from the other repository would send a caller to check the
			// answer in a version it was not answered in.
			citations := make([]evidence.Evidence, 0, len(wanted.Edges))
			for _, edge := range wanted.Edges {
				citations = append(citations, evidence.At(edge.Path, edge.Line, edge.Line, ""))
			}
			if got := citedIn(t, out); !slices.Equal(got, citations) {
				t.Fatalf("evidence is %+v, want the walked member's own call sites %+v", got, citations)
			}
		})
	}
}

// A repository the context does not name is refused, and no graph is walked.
//
// Not an empty graph, which would read as "that repository has no such symbol"
// about a repository this context never covered, and not a fallback to a member
// the caller did not name.
func TestTraceCallsRefusesARepositoryTheContextDoesNotName(t *testing.T) {
	graph := &recordingGraph{graphs: stackGraphs}
	eng := engine.New(fakeContexts{workspace: stack}, &fakeSearch{}, graph, &fakeSource{})

	out, err := eng.TraceCalls(context.Background(), engine.TraceCallsRequest{
		Context: stack.ID, Repository: "gamma", Symbol: "Process", Direction: provider.Callers, Depth: 2,
	})
	if err == nil {
		t.Fatal("TraceCalls walked a repository outside the workspace")
	}
	assertCode(t, err, vacerr.InvalidArgument)
	assertNotAnAnswer(t, out)
	if len(graph.calls) != 0 {
		t.Fatalf("a repository outside the workspace reached the graph provider as %+v", graph.calls)
	}

	var vErr *vacerr.Error
	if !errors.As(err, &vErr) {
		t.Fatalf("TraceCalls failed with %v, want a *vacerr.Error", err)
	}
	if vErr.Details["repository"] != "gamma" {
		t.Errorf("the refusal says repository %v, want the one that was asked for", vErr.Details["repository"])
	}
	if got, ok := vErr.Details["repositories"].([]string); !ok || !slices.Equal(got, []string{"alpha", "beta"}) {
		t.Errorf("the refusal offers %v, want the repositories this context does name", vErr.Details["repositories"])
	}
}

// A symbol matching several functions inside the walked member is still
// SYMBOL_AMBIGUOUS with its candidates, exactly as it was before a context could
// name two repositories: the ambiguity is one graph's, and narrowing to that
// graph is what already happened.
//
// The repository is added to the details because it is what the caller has to
// put in the next request. The candidates are what it names one of, and the
// repository is where it names it — an ambiguity reported without the second
// half is one a caller in a multi-repository context cannot act on.
func TestTraceCallsReportsAnAmbiguousSymbolWithTheRepositoryItWalked(t *testing.T) {
	candidates := []map[string]any{
		{"qualified_name": "alpha/inner.Process", "name": "Process"},
		{"qualified_name": "alpha/outer.Process", "name": "Process"},
	}
	ambiguous := vacerr.New(
		vacerr.SymbolAmbiguous,
		`trace_calls: context "stack@v1" has 2 symbols matching "Process"; name one of the candidates`,
		map[string]any{"context": stack.ID, "symbol": "Process", "graph_ref": "alpha-v1", "candidates": candidates},
	)
	graph := &recordingGraph{
		graphs: stackGraphs,
		fail:   map[string]error{"alpha": ambiguous},
	}
	eng := engine.New(fakeContexts{workspace: stack}, &fakeSearch{}, graph, &fakeSource{})

	out, err := eng.TraceCalls(context.Background(), engine.TraceCallsRequest{
		Context: stack.ID, Repository: "alpha", Symbol: "Process", Direction: provider.Callers, Depth: 2,
	})
	assertCode(t, err, vacerr.SymbolAmbiguous)
	assertNotAnAnswer(t, out)

	var vErr *vacerr.Error
	if !errors.As(err, &vErr) {
		t.Fatalf("TraceCalls failed with %v, want a *vacerr.Error", err)
	}
	if vErr.Message != ambiguous.Message {
		t.Errorf("the message is %q, want the provider's own %q", vErr.Message, ambiguous.Message)
	}
	if got, ok := vErr.Details["candidates"].([]map[string]any); !ok || !reflect.DeepEqual(got, candidates) {
		t.Errorf("the candidates are %v, want the provider's own %v", vErr.Details["candidates"], candidates)
	}
	if vErr.Details["repository"] != "alpha" {
		t.Errorf("the ambiguity says repository %v, want the one that was walked", vErr.Details["repository"])
	}
	// The rest of the provider's details survive: adding where the ambiguity was
	// is not the same as replacing what it said.
	if vErr.Details["context"] != stack.ID || vErr.Details["graph_ref"] != "alpha-v1" {
		t.Errorf("the details are %v, want the provider's own kept", vErr.Details)
	}

	// The other member is not consulted for a second opinion: an ambiguity in one
	// repository is that repository's, and a walk of beta looking for a tie-break
	// would be answering about a version the caller narrowed away from.
	if len(graph.calls) != 1 || graph.calls[0].Repository != "alpha" {
		t.Fatalf("the graph provider was called with %+v, want only the named member", graph.calls)
	}
}

// A member whose graph cannot be reached is GRAPH_PROVIDER_UNAVAILABLE, and the
// provider's own error at that.
//
// SYMBOL_NOT_FOUND is the answer this must never be turned into. The two are one
// character apart to read and opposite in meaning: one says this version does not
// have the symbol, which a caller comparing two versions records as a difference,
// and the other says nobody could ask. Missing data is not a real difference, and
// an unavailable backend reported as an absence is how it becomes one.
func TestTraceCallsReportsAnUnreachableGraphRatherThanAnAbsentSymbol(t *testing.T) {
	unavailable := vacerr.New(
		vacerr.GraphProviderUnavailable,
		"cbm: graph alpha-v1 is not indexed",
		map[string]any{"context": stack.ID, "graph_ref": "alpha-v1"},
	)
	graph := &recordingGraph{
		graphs: stackGraphs,
		fail:   map[string]error{"alpha": unavailable},
	}
	eng := engine.New(fakeContexts{workspace: stack}, &fakeSearch{}, graph, &fakeSource{})

	out, err := eng.TraceCalls(context.Background(), engine.TraceCallsRequest{
		Context: stack.ID, Repository: "alpha", Symbol: "Process", Direction: provider.Callers, Depth: 2,
	})
	assertCode(t, err, vacerr.GraphProviderUnavailable)
	if !errors.Is(err, unavailable) {
		t.Fatalf("TraceCalls failed with %v, want the provider's own error", err)
	}
	assertNotAnAnswer(t, out)
	if len(out.Graph().Edges) != 0 {
		t.Errorf("a failed walk returned edges %+v", out.Graph().Edges)
	}
}
