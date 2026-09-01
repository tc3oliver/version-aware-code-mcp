package engine_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// Two workspaces over the same two repositories, one at each side's revisions.
// Every member is filed under its workspace's ID, as a resolver files them,
// which is the whole difficulty these tests are about: the ID no longer says
// which repository anything came from, so only the member does.
//
// beta is here to be the member that must not be compared. It is deliberately
// identical in both workspaces where alpha is not, so narrowing to the wrong one
// does not merely answer differently — it answers "nothing changed", which is
// the confident wrong answer rather than a visible mistake.
var (
	alphaV1 = vacctx.CodeContext{
		ID: "stack@v1", Repository: "alpha", Branch: "v1",
		Revision: "a111111111111111111111111111111111111111", GraphRef: "alpha-v1",
	}
	alphaV2 = vacctx.CodeContext{
		ID: "stack@v2", Repository: "alpha", Branch: "v2",
		Revision: "a222222222222222222222222222222222222222", GraphRef: "alpha-v2",
	}
	betaV1 = vacctx.CodeContext{
		ID: "stack@v1", Repository: "beta", Branch: "v1",
		Revision: "b111111111111111111111111111111111111111", GraphRef: "beta-v1",
	}
	betaV2 = vacctx.CodeContext{
		ID: "stack@v2", Repository: "beta", Branch: "v2",
		Revision: "b222222222222222222222222222222222222222", GraphRef: "beta-v2",
	}
)

// atRevision is what a fake here keys its answers on: the repository and the
// revision, which is the one pair that names a version. Two members of one
// workspace share the workspace's ID, so a fake keyed on that could not tell the
// member the request selected from the member it did not — which is exactly what
// these tests exist to check.
func atRevision(codeCtx vacctx.CodeContext) string {
	return codeCtx.Repository + "@" + codeCtx.Revision
}

// memberSource is [diffSource] keyed by member instead of by context ID.
type memberSource struct {
	fakeSource
	files    map[string]string
	from, to vacctx.CodeContext
	diffs    int
}

func (d *memberSource) Diff(_ context.Context, from, to vacctx.CodeContext, req provider.SourceDiffRequest) (*provider.SourceDiff, error) {
	d.diffs++
	d.from, d.to = from, to

	fromText, inFrom := d.files[atRevision(from)]
	toText, inTo := d.files[atRevision(to)]
	diff := &provider.SourceDiff{Path: req.Path}
	switch {
	case !inFrom && !inTo:
		return nil, vacerr.New(
			vacerr.InvalidArgument,
			"compare_code: neither version has file "+req.Path,
			map[string]any{"path": req.Path},
		)
	case !inFrom:
		diff.Change = provider.ChangeAdded
	case !inTo:
		diff.Change = provider.ChangeRemoved
	case fromText == toText:
		diff.Change = provider.ChangeUnchanged
	default:
		diff.Change = provider.ChangeModified
		diff.Hunks = []provider.DiffHunk{{
			OldStart: 1, OldLines: 1, NewStart: 1, NewLines: 1,
			Lines: []provider.DiffLine{
				{Kind: provider.LineRemoved, Content: fromText},
				{Kind: provider.LineAdded, Content: toText},
			},
		}}
	}
	return diff, nil
}

// memberGraph is [versionedGraph] keyed by graph_ref, which is one member's own
// where a context ID is a whole workspace's.
type memberGraph struct {
	graphs map[string]provider.CallGraph
	asked  []string
}

func (g *memberGraph) TraceCalls(_ context.Context, codeCtx vacctx.CodeContext, req provider.TraceRequest) (*provider.CallGraph, error) {
	g.asked = append(g.asked, codeCtx.GraphRef)
	graph, ok := g.graphs[codeCtx.GraphRef]
	if !ok {
		return nil, vacerr.New(
			vacerr.SymbolNotFound,
			"trace_calls: graph "+codeCtx.GraphRef+" has no such symbol",
			map[string]any{"context": codeCtx.ID, "symbol": req.Symbol, "graph_ref": codeCtx.GraphRef},
		)
	}
	return &graph, nil
}

// Acceptance criterion 3: naming a repository selects the member and changes
// nothing else. The same comparison is made twice — once between two workspaces
// of two repositories with the repository named, once between two workspaces
// holding only that member and nothing named — and the two results are equal
// field by field, unexported ones included.
//
// That equality is the claim worth making. It is not that member selection
// produces a plausible answer, it is that it produces the same answer the
// v0.4.0 path produces: everything after the selection — the differ, the
// classification, the two sides, their citations — is handed one
// [vacctx.CodeContext] per side and cannot tell which of the two ways it got
// there. Narrowing is a front end, not a second comparison.
//
// The workspace IDs are the same on both runs on purpose, so the two results
// are comparable in every field rather than in every field but the one this
// test would have had to exclude.
func TestCompareCodeNarrowedToOneMemberMatchesTheSingleContextComparison(t *testing.T) {
	files := map[string]string{
		atRevision(alphaV1): "package alpha\n",
		atRevision(alphaV2): "package alpha // v2\n",
		// beta is the same file in both versions: a comparison that selected it
		// would answer UNCHANGED and look like an answer.
		atRevision(betaV1): "package beta\n",
		atRevision(betaV2): "package beta\n",
	}

	narrowedSource := &memberSource{files: files}
	narrowed, err := engine.New(mapContexts{
		"stack@v1": over("stack@v1", alphaV1, betaV1),
		"stack@v2": over("stack@v2", alphaV2, betaV2),
	}, nil, nil, narrowedSource).CompareCode(context.Background(), engine.CompareCodeRequest{
		FromContext: "stack@v1", ToContext: "stack@v2", Repository: "alpha", Path: comparedPath,
	})
	if err != nil {
		t.Fatalf("CompareCode narrowed to alpha: %v", err)
	}

	directSource := &memberSource{files: files}
	direct, err := engine.New(mapContexts{
		"stack@v1": over("stack@v1", alphaV1),
		"stack@v2": over("stack@v2", alphaV2),
	}, nil, nil, directSource).CompareCode(context.Background(), engine.CompareCodeRequest{
		FromContext: "stack@v1", ToContext: "stack@v2", Path: comparedPath,
	})
	if err != nil {
		t.Fatalf("CompareCode between two single-member contexts: %v", err)
	}

	if !reflect.DeepEqual(narrowed, direct) {
		t.Fatalf("narrowing to alpha answered\n%+v\nwhere the same two members compared directly answered\n%+v", narrowed, direct)
	}

	// The revisions that were compared are alpha's own, on both sides. Read on
	// its own this says the selection happened; read beside the equality above it
	// says the selection is the only thing that happened.
	if narrowedSource.from != alphaV1 || narrowedSource.to != alphaV2 {
		t.Fatalf("the differ was handed from=%+v to=%+v, want alpha's two revisions", narrowedSource.from, narrowedSource.to)
	}
	if narrowedSource.diffs != 1 {
		t.Fatalf("the differ was asked %d times, want once: the member not selected is not compared and discarded", narrowedSource.diffs)
	}
	if narrowed.Change() != engine.CodeModified {
		t.Fatalf("change is %q, want %q: alpha's file differs between the two revisions", narrowed.Change(), engine.CodeModified)
	}
	if answeredIn(t, narrowed.From()) != alphaV1 || answeredIn(t, narrowed.To()) != alphaV2 {
		t.Fatalf("the sides report from=%+v to=%+v, want the two alpha members", narrowed.From().Context(), narrowed.To().Context())
	}
}

// Acceptance criterion 3 for compare_calls, and acceptance criterion 7 through
// the same door: the same comparison made through two multi-member workspaces
// with the repository named, and through two workspaces holding only that
// member, are equal field by field.
//
// The graphs are built so the relation identity is exercised on the narrowed
// path and not only on the v0.4.0 one. One relation is written further down the
// file — the same relation, because the line is not part of what a call is
// compared by, and burying real additions under every edit above them is what
// including it would cost. One is written in another file, which is one removed
// relation and one added one, because the path is part of it and "which file to
// look in" is a fact worth keeping.
func TestCompareCallsNarrowedToOneMemberMatchesTheSingleContextComparison(t *testing.T) {
	graphs := map[string]provider.CallGraph{
		"alpha-v1": {Symbol: "alpha.Process", Edges: []provider.CallEdge{
			{Caller: "alpha.Main", Callee: "alpha.Process", Path: "main.go", Line: 4},
			{Caller: "alpha.Serve", Callee: "alpha.Process", Path: "serve.go", Line: 9},
		}},
		"alpha-v2": {Symbol: "alpha.Process", Edges: []provider.CallEdge{
			// Moved down main.go: one relation, not one added and one removed.
			{Caller: "alpha.Main", Callee: "alpha.Process", Path: "main.go", Line: 40},
			// Moved to another file: one removed and one added.
			{Caller: "alpha.Serve", Callee: "alpha.Process", Path: "cmd/serve.go", Line: 9},
		}},
		// beta's graph is the same in both versions, so narrowing to the wrong
		// member would answer "nothing changed" rather than fail.
		"beta-v1": {Symbol: "beta.Process", Edges: []provider.CallEdge{
			{Caller: "beta.Main", Callee: "beta.Process", Path: "main.go", Line: 1},
		}},
		"beta-v2": {Symbol: "beta.Process", Edges: []provider.CallEdge{
			{Caller: "beta.Main", Callee: "beta.Process", Path: "main.go", Line: 1},
		}},
	}
	request := func(repository string) engine.CompareCallsRequest {
		return engine.CompareCallsRequest{
			FromContext: "stack@v1", ToContext: "stack@v2", Repository: repository,
			Symbol: "Process", Direction: provider.Callers, Depth: 2,
		}
	}

	narrowedGraph := &memberGraph{graphs: graphs}
	narrowed, err := engine.New(mapContexts{
		"stack@v1": over("stack@v1", alphaV1, betaV1),
		"stack@v2": over("stack@v2", alphaV2, betaV2),
	}, nil, narrowedGraph, nil).CompareCalls(context.Background(), request("alpha"))
	if err != nil {
		t.Fatalf("CompareCalls narrowed to alpha: %v", err)
	}

	directGraph := &memberGraph{graphs: graphs}
	direct, err := engine.New(mapContexts{
		"stack@v1": over("stack@v1", alphaV1),
		"stack@v2": over("stack@v2", alphaV2),
	}, nil, directGraph, nil).CompareCalls(context.Background(), request(""))
	if err != nil {
		t.Fatalf("CompareCalls between two single-member contexts: %v", err)
	}

	if !reflect.DeepEqual(narrowed, direct) {
		t.Fatalf("narrowing to alpha answered\n%+v\nwhere the same two members compared directly answered\n%+v", narrowed, direct)
	}

	// alpha's two graphs and no others: the member not selected is not walked at
	// all, rather than walked and discarded, which would read the same in the
	// result and be a walk in a version the caller narrowed away from.
	if !slices.Equal(narrowedGraph.asked, []string{"alpha-v1", "alpha-v2"}) {
		t.Fatalf("the graph provider was asked %v, want alpha's two graphs", narrowedGraph.asked)
	}
	if narrowed.Presence() != engine.PresenceBoth {
		t.Fatalf("presence is %q, want %q", narrowed.Presence(), engine.PresenceBoth)
	}

	// Acceptance criterion 7 on the narrowed path: caller, callee and path decide
	// what one relation is, and the line does not.
	assertRelations(t, narrowed.Unchanged(), "unchanged", "alpha.Main->alpha.Process@main.go")
	assertRelations(t, narrowed.Removed(), "removed", "alpha.Serve->alpha.Process@serve.go")
	assertRelations(t, narrowed.Added(), "added", "alpha.Serve->alpha.Process@cmd/serve.go")
	if answeredIn(t, narrowed.From()) != alphaV1 || answeredIn(t, narrowed.To()) != alphaV2 {
		t.Fatalf("the sides report from=%+v to=%+v, want the two alpha members", narrowed.From().Context(), narrowed.To().Context())
	}
}

// Acceptance criterion 2: a side naming several repositories with no repository
// to choose between them is refused, and the refusal says which side is at fault
// and which repositories that side offers.
//
// Both halves are what the caller needs and neither is enough alone. A
// comparison names two contexts, so a caller told only "several repositories"
// cannot tell which of the two to narrow; and told only "narrow one", with no
// list, it cannot see the configuration to know what to narrow it to. Picking a
// member for it is the thing being ruled out: that would compare a repository
// the request named nothing of, under the name of the context it did name.
//
// The two sides offer different repositories here, so a refusal that reported
// the wrong side's list would be visible rather than accidentally right.
func TestNoComparisonPicksAMemberForTheCaller(t *testing.T) {
	gammaV2 := vacctx.CodeContext{
		ID: "stack@v2", Repository: "gamma", Branch: "v2",
		Revision: "c222222222222222222222222222222222222222", GraphRef: "gamma-v2",
	}
	solo := vacctx.CodeContext{
		ID: "solo@v1", Repository: "alpha", Branch: "v1",
		Revision: "a111111111111111111111111111111111111111", GraphRef: "alpha-v1",
	}
	contexts := mapContexts{
		"stack@v1": over("stack@v1", alphaV1, betaV1),
		"stack@v2": over("stack@v2", alphaV2, gammaV2),
		solo.ID:    single(solo),
	}

	for _, tc := range []struct {
		side         string
		from, to     string
		repositories []string
	}{
		{"from", "stack@v1", solo.ID, []string{"alpha", "beta"}},
		{"to", solo.ID, "stack@v2", []string{"alpha", "gamma"}},
	} {
		t.Run("compare_code: the "+tc.side+" side", func(t *testing.T) {
			source := &memberSource{files: map[string]string{atRevision(alphaV1): "package alpha\n"}}
			out, err := engine.New(contexts, nil, nil, source).CompareCode(context.Background(), engine.CompareCodeRequest{
				FromContext: tc.from, ToContext: tc.to, Path: comparedPath,
			})
			if err == nil {
				t.Fatalf("compare_code picked a member for a %s side naming several repositories", tc.side)
			}
			assertNotACodeComparison(t, out)
			assertRepositoryRequired(t, err, tc.side, tc.repositories)
			if source.diffs != 0 {
				t.Fatalf("an unnarrowed comparison reached the source provider %d times", source.diffs)
			}
		})

		t.Run("compare_calls: the "+tc.side+" side", func(t *testing.T) {
			graph := &memberGraph{graphs: map[string]provider.CallGraph{
				"alpha-v1": {Symbol: "alpha.Process"},
				"alpha-v2": {Symbol: "alpha.Process"},
			}}
			out, err := engine.New(contexts, nil, graph, nil).CompareCalls(context.Background(), engine.CompareCallsRequest{
				FromContext: tc.from, ToContext: tc.to,
				Symbol: "Process", Direction: provider.Callers, Depth: 2,
			})
			if err == nil {
				t.Fatalf("compare_calls picked a member for a %s side naming several repositories", tc.side)
			}
			assertNotAComparison(t, out)
			assertRepositoryRequired(t, err, tc.side, tc.repositories)
			if len(graph.asked) != 0 {
				t.Fatalf("an unnarrowed comparison reached the graph provider: %v", graph.asked)
			}
		})
	}
}

// assertRepositoryRequired checks the refusal a caller has to act on: which side
// needs narrowing, and what that side can be narrowed to.
func assertRepositoryRequired(t *testing.T, err error, side string, repositories []string) {
	t.Helper()
	assertCode(t, err, vacerr.InvalidArgument)

	var vErr *vacerr.Error
	if !errors.As(err, &vErr) {
		t.Fatalf("error is %v, want a *vacerr.Error", err)
	}
	if vErr.Details["side"] != side {
		t.Errorf("details say side %v, want the side at fault %q", vErr.Details["side"], side)
	}
	if got, ok := vErr.Details["repositories"].([]string); !ok || !slices.Equal(got, repositories) {
		t.Errorf("details offer %v, want the repositories that side names %v", vErr.Details["repositories"], repositories)
	}
	// The message says what is missing, not only that something is: this is the
	// one refusal in the comparison model the caller fixes by adding an argument
	// rather than by choosing a different context.
	if !strings.Contains(vErr.Message, "repository is required") {
		t.Errorf("the refusal reads %q, want it to say repository is required", vErr.Message)
	}
}

// Acceptance criteria 4 and 5: a repository only one side has, and a repository
// neither side has, are both INVALID_ARGUMENT.
//
// Criterion 4 is the one that matters, and it is a rule about what this server
// must not answer. A repository present on the from side and absent on the to
// side is not a file or a symbol that was removed: REMOVED is what a version
// says about code inside a repository both sides have, and reporting a member
// the caller cannot compare as a code change would be a cross-version answer
// wearing the clothes of a version-scoped one. Whole workspaces compared against
// whole workspaces — where a member appearing or disappearing is an outcome
// rather than a mistake — is not a comparison v0.5.0 has.
//
// Either way the refusal carries what that side does name, because the caller
// cannot see the configuration: told only "no", it cannot tell a misspelling
// from the wrong context.
func TestCompareCodeRefusesARepositoryASideDoesNotName(t *testing.T) {
	contexts := mapContexts{
		"stack@v1": over("stack@v1", alphaV1, betaV1),
		"stack@v2": over("stack@v2", alphaV2),
	}

	for _, tc := range []struct {
		name       string
		repository string
	}{
		{"a repository only the from side has", "beta"},
		{"a repository neither side has", "gamma"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := &memberSource{files: map[string]string{
				atRevision(alphaV1): "package alpha\n",
				atRevision(betaV1):  "package beta\n",
			}}

			out, err := engine.New(contexts, nil, nil, source).CompareCode(context.Background(), engine.CompareCodeRequest{
				FromContext: "stack@v1", ToContext: "stack@v2", Repository: tc.repository, Path: comparedPath,
			})
			if err == nil {
				t.Fatalf("compare_code answered for repository %q", tc.repository)
			}
			assertCode(t, err, vacerr.InvalidArgument)
			assertNotACodeComparison(t, out)
			// Not REMOVED, and not ADDED: assertNotACodeComparison already insists
			// the change is blank, which is what says the outcome is a refusal and
			// not one of the four answers.
			if source.diffs != 0 {
				t.Fatalf("a repository a side does not name reached the source provider %d times", source.diffs)
			}
			assertRepositoryOffered(t, err, tc.repository)
		})
	}
}

// The mirror of TestCompareCodeRefusesARepositoryASideDoesNotName for
// compare_calls, which has the same rule for a different reason: nothing would
// stop it comparing, so the refusal has to be here. Each side is walked in its
// own graph, so a repository only one side has would be walked on that side and
// found missing on the other, and the answer would read as "this whole call
// graph was removed" about a repository the other side never had.
func TestCompareCallsRefusesARepositoryASideDoesNotName(t *testing.T) {
	contexts := mapContexts{
		"stack@v1": over("stack@v1", alphaV1, betaV1),
		"stack@v2": over("stack@v2", alphaV2),
	}

	for _, tc := range []struct {
		name       string
		repository string
	}{
		{"a repository only the from side has", "beta"},
		{"a repository neither side has", "gamma"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := &memberGraph{graphs: map[string]provider.CallGraph{
				"beta-v1": {Symbol: "beta.Process", Edges: []provider.CallEdge{
					{Caller: "beta.Main", Callee: "beta.Process", Path: "main.go", Line: 4},
				}},
			}}

			out, err := engine.New(contexts, nil, graph, nil).CompareCalls(context.Background(), engine.CompareCallsRequest{
				FromContext: "stack@v1", ToContext: "stack@v2", Repository: tc.repository,
				Symbol: "Process", Direction: provider.Callers, Depth: 2,
			})
			if err == nil {
				t.Fatalf("compare_calls answered for repository %q", tc.repository)
			}
			assertCode(t, err, vacerr.InvalidArgument)
			// Not FROM_ONLY, and no removed relations: assertNotAComparison insists
			// on both, which is what says beta's call graph was not reported as
			// deleted by a version that never had it.
			assertNotAComparison(t, out)
			if len(graph.asked) != 0 {
				t.Fatalf("a repository a side does not name reached the graph provider: %v", graph.asked)
			}
			assertRepositoryOffered(t, err, tc.repository)
		})
	}
}

// assertRepositoryOffered checks a refused repository comes back named, beside
// the ones the side at fault does offer.
func assertRepositoryOffered(t *testing.T, err error, repository string) {
	t.Helper()
	var vErr *vacerr.Error
	if !errors.As(err, &vErr) {
		t.Fatalf("error is %v, want a *vacerr.Error", err)
	}
	if vErr.Details["repository"] != repository {
		t.Errorf("details say repository %v, want the one that was asked for %q", vErr.Details["repository"], repository)
	}
	if got, ok := vErr.Details["repositories"].([]string); !ok || len(got) == 0 {
		t.Errorf("details offer %v, want the repositories the side does name", vErr.Details["repositories"])
	}
}
