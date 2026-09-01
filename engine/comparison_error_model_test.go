package engine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// The error model of the two comparisons, asked of both of them at once.
//
// compare_code and compare_calls each have their own tests, and some of what is
// here is checked there too. The duplication is deliberate: a taxonomy is a set
// of rules that hold together, and a rule checked only inside one method's own
// file is one the other method can quietly stop obeying without anything going
// red. Every test below asks the same question of both comparisons, so a future
// change that answers it differently in one of them fails here whichever one it
// was.
//
// The model in full:
//
//	what happened                 compare_code             compare_calls
//	two repositories              INVALID_ARGUMENT         INVALID_ARGUMENT
//	only one side has it          ADDED / REMOVED          TO_ONLY / FROM_ONLY
//	neither side has it           the provider's           SYMBOL_NOT_FOUND
//	                              INVALID_ARGUMENT
//	the request is ambiguous      —                        SYMBOL_AMBIGUOUS
//	a side could not be asked     the provider's own error the provider's own error
//	this server cannot compare    SOURCE_DIFF_UNAVAILABLE  GRAPH_PROVIDER_UNAVAILABLE
//	no provider of that kind      REPOSITORY_NOT_FOUND     GRAPH_PROVIDER_UNAVAILABLE
//
// Two rules of the model are about what is *not* an error, and they are the
// reason the rest of it is worth pinning: a thing only one version has is an
// answer, and only a thing neither version has leaves nothing to compare.

// otherRepository is a third version context, complete and valid in every way
// except the one that matters here: it belongs to another repository.
var otherRepository = vacctx.CodeContext{
	ID: "other@v1", Repository: "other", Branch: "v1",
	Revision: "3333333333333333333333333333333333333333", GraphRef: "other-v1",
}

// Acceptance criterion 2: neither comparison answers across two repositories,
// and both refuse with the same code — INVALID_ARGUMENT, with no dedicated code
// of its own — naming the two repositories so the caller can see which pair it
// asked for.
//
// The two refusals are not the same check for the same reason, which is why
// having them both matters. compare_code could leave it to the git adapter,
// which has the check too, because one diff of two revisions in two object
// databases is not an operation that exists. compare_calls has no such
// backstop: each side is walked in its own graph, so two unrelated repositories
// would compare perfectly happily and report one project's whole call graph as
// removed and the other's as added. The engine is the only layer that sees both
// contexts at once, so it is the layer that has to say no.
//
// Duplicates TestCompareCodeRefusesContextsInDifferentRepositories, on purpose:
// the rule under test here is that this holds for every comparison, not that it
// holds for compare_code.
//
// What is compared is the resolved member's repository, because that is the only
// place a repository is written down — a context is a workspace and has none of
// its own. Both comparisons are asked the same question with two contexts over
// one repository elsewhere in this file and answer it, so a check that had
// become true of any two distinct contexts would show up there.
func TestNoComparisonAnswersAcrossTwoRepositories(t *testing.T) {
	contexts := mapContexts{compareV1.ID: single(compareV1), otherRepository.ID: single(otherRepository)}

	t.Run("compare_code", func(t *testing.T) {
		source := &diffSource{files: map[string]string{
			compareV1.ID:       "package demo\n",
			otherRepository.ID: "package other\n",
		}}
		req := compareCodeRequest()
		req.ToContext = otherRepository.ID

		out, err := engine.New(contexts, nil, nil, source).CompareCode(context.Background(), req)
		if err == nil {
			t.Fatal("compare_code answered across two repositories")
		}
		assertCode(t, err, vacerr.InvalidArgument)
		assertNotACodeComparison(t, out)
		assertNamesBothRepositories(t, err, otherRepository.ID)
		if source.diffs != 0 {
			t.Fatalf("a cross-repository comparison reached the source provider %d times", source.diffs)
		}
	})

	t.Run("compare_calls", func(t *testing.T) {
		graph := &versionedGraph{graphs: map[string]provider.CallGraph{
			compareV1.ID: {Symbol: "demo.Process", Edges: []provider.CallEdge{
				{Caller: "demo.Main", Callee: "demo.Process", Path: "main.go", Line: 4},
			}},
			otherRepository.ID: {Symbol: "other.Process", Edges: []provider.CallEdge{
				{Caller: "other.Main", Callee: "other.Process", Path: "main.go", Line: 4},
			}},
		}}
		req := compareRequest()
		req.ToContext = otherRepository.ID

		out, err := engine.New(contexts, nil, graph, nil).CompareCalls(context.Background(), req)
		if err == nil {
			t.Fatal("compare_calls answered across two repositories")
		}
		assertCode(t, err, vacerr.InvalidArgument)
		assertNotAComparison(t, out)
		assertNamesBothRepositories(t, err, otherRepository.ID)
		if len(graph.asked) != 0 {
			t.Fatalf("a cross-repository comparison reached the graph provider: %v", graph.asked)
		}
	})
}

// assertNamesBothRepositories checks the refusal says which pair of versions was
// asked for. A caller told only "invalid argument" cannot tell which of the two
// context IDs it should have written differently.
func assertNamesBothRepositories(t *testing.T, err error, toContext string) {
	t.Helper()
	var vErr *vacerr.Error
	if !errors.As(err, &vErr) {
		t.Fatalf("error is %v, want a *vacerr.Error", err)
	}
	for _, want := range []struct{ key, value string }{
		{"from_context", compareV1.ID},
		{"to_context", toContext},
		{"from_repository", compareV1.Repository},
		{"to_repository", otherRepository.Repository},
	} {
		if vErr.Details[want.key] != want.value {
			t.Errorf("details say %s=%v, want %q", want.key, vErr.Details[want.key], want.value)
		}
	}
}

// Acceptance criterion 3: a thing only one version has is an answer and not a
// failure, in both comparisons. This is the half of the model that says what is
// not an error, and it is the half a taxonomy is most likely to lose: reporting
// a deleted file or a deleted function as a failure would make every real
// version difference look like a broken query.
//
// The version that has it is present and reports its own scope; the version
// that does not is absent rather than empty, because it has nothing to cite on
// account of having had nothing.
//
// Duplicates TestCompareCodeReportsAFileFoundInOnlyOneVersion and
// TestCompareCallsReportsASymbolFoundInOnlyOneVersion, which check the same
// rule one method at a time and in more detail.
func TestEveryComparisonAnswersWhenOnlyOneVersionHasIt(t *testing.T) {
	t.Run("compare_code: a file only the to version has", func(t *testing.T) {
		source := &diffSource{files: map[string]string{compareV2.ID: "package demo\n"}}

		out, err := compareCodeEngine(source).CompareCode(context.Background(), compareCodeRequest())
		if err != nil {
			t.Fatalf("a file only the to version has is an answer, got error: %v", err)
		}
		if out.Change() != engine.CodeAdded {
			t.Fatalf("change is %q, want %q", out.Change(), engine.CodeAdded)
		}
		assertOneSidedAnswer(t, out.From(), out.To(), compareV2)
	})

	t.Run("compare_calls: a symbol only the to version has", func(t *testing.T) {
		graph := &versionedGraph{graphs: map[string]provider.CallGraph{compareV2.ID: {
			Symbol: "demo.Process",
			Edges:  []provider.CallEdge{{Caller: "demo.Main", Callee: "demo.Process", Path: "main.go", Line: 4}},
		}}}

		out, err := compareEngine(graph).CompareCalls(context.Background(), compareRequest())
		if err != nil {
			t.Fatalf("a symbol only the to version has is an answer, got error: %v", err)
		}
		if out.Presence() != engine.PresenceToOnly {
			t.Fatalf("presence is %q, want %q", out.Presence(), engine.PresenceToOnly)
		}
		assertOneSidedAnswer(t, out.From(), out.To(), compareV2)
		if len(out.Added()) != 1 || len(out.Removed()) != 0 {
			t.Fatalf("a one-sided comparison reported added %v and removed %v, want the surviving version's relations on its own side",
				names(out.Added()), names(out.Removed()))
		}
	})
}

// assertOneSidedAnswer holds what both comparisons say when one version has the
// thing and the other does not: the surviving side carries its version, and the
// absent side carries none and cites nothing.
func assertOneSidedAnswer(t *testing.T, absent, present engine.ComparisonSide, presentIn vacctx.CodeContext) {
	t.Helper()
	if !present.Present() || answeredIn(t, present) != presentIn {
		t.Fatalf("the surviving side reports context %+v and Present %v, want the version that has it",
			present.Context(), present.Present())
	}
	if absent.Present() {
		t.Fatalf("the version without it reports Present true and context %+v", absent.Context())
	}
	if absent.Evidence() != nil {
		t.Errorf("the version without it cites %+v, want nothing", absent.Evidence())
	}
}

// Acceptance criterion 4: a thing neither version has leaves nothing to
// compare, and both comparisons fail rather than answer. An empty result would
// read as "it is in both versions and is identical", which is a confident
// statement about code that does not exist.
//
// The two codes differ because the two failures are found in different places,
// and the model keeps it that way on purpose. compare_calls resolves the symbol
// itself, one side at a time, and so knows both sides came back empty:
// SYMBOL_NOT_FOUND. compare_code asks one question of the differ and the
// backend is the one that finds the path in neither revision — the git adapter
// returns INVALID_ARGUMENT for it (see adapters/git.Provider.emptyDiff), and
// what is checked here is that the engine hands that back untouched rather than
// inventing a code of its own or, worse, reporting it as UNCHANGED.
func TestEveryComparisonFailsWhenNeitherVersionHasIt(t *testing.T) {
	t.Run("compare_code: a file in neither revision", func(t *testing.T) {
		// The error the git adapter returns for a path neither revision has.
		neither := vacerr.New(
			vacerr.InvalidArgument,
			"compare_code: neither revision 1111111 nor 2222222 has file "+comparedPath,
			map[string]any{"path": comparedPath, "from_revision": compareV1.Revision, "to_revision": compareV2.Revision},
		)
		source := &diffSource{diffErr: neither}

		out, err := compareCodeEngine(source).CompareCode(context.Background(), compareCodeRequest())
		if err == nil {
			t.Fatal("a comparison of a file neither version has answered successfully")
		}
		assertCode(t, err, vacerr.InvalidArgument)
		if !errors.Is(err, neither) {
			t.Fatalf("error is %v, want the provider's own error unchanged", err)
		}
		assertNotACodeComparison(t, out)
	})

	t.Run("compare_calls: a symbol in neither version", func(t *testing.T) {
		graph := &versionedGraph{}

		out, err := compareEngine(graph).CompareCalls(context.Background(), compareRequest())
		if err == nil {
			t.Fatal("a comparison of a symbol neither version has answered successfully")
		}
		assertCode(t, err, vacerr.SymbolNotFound)
		assertNotAComparison(t, out)
		if len(graph.asked) != 2 {
			t.Fatalf("the graph provider was asked %v, want both versions: one absent side is not an outcome on its own", graph.asked)
		}
	})
}

// Acceptance criterion 5: an ambiguous request fails the whole comparison and
// says which side was ambiguous, so the caller knows which version to name a
// candidate in.
//
// Only compare_calls has this failure, and the model says so rather than
// leaving a hole: compare_code is asked for a path, which names at most one
// file in a revision, so there is no candidate set for it to choose between and
// nothing for it to be ambiguous about. SYMBOL_AMBIGUOUS therefore has one
// producer in a comparison and CONTEXT_AMBIGUOUS still has none — a context is
// named by a unique ID here as everywhere else.
//
// Duplicates TestCompareCallsReportsWhichSideIsAmbiguous, which also checks the
// provider's candidates survive.
func TestOnlyCompareCallsHasASideToCallAmbiguous(t *testing.T) {
	for _, tc := range []struct {
		side      string
		ambiguous vacctx.CodeContext
		other     vacctx.CodeContext
	}{
		{"from", compareV1, compareV2},
		{"to", compareV2, compareV1},
	} {
		t.Run("compare_calls: the "+tc.side+" side", func(t *testing.T) {
			graph := &versionedGraph{
				graphs: map[string]provider.CallGraph{tc.other.ID: {Symbol: "demo.Process"}},
				errs: map[string]error{tc.ambiguous.ID: vacerr.New(
					vacerr.SymbolAmbiguous,
					"trace_calls: 2 symbols match",
					map[string]any{"context": tc.ambiguous.ID, "symbol": "Process"},
				)},
			}

			out, err := compareEngine(graph).CompareCalls(context.Background(), compareRequest())
			if err == nil {
				t.Fatalf("an ambiguous %s side answered successfully", tc.side)
			}
			assertCode(t, err, vacerr.SymbolAmbiguous)
			assertNotAComparison(t, out)

			var vErr *vacerr.Error
			if !errors.As(err, &vErr) {
				t.Fatalf("error is %v, want a *vacerr.Error", err)
			}
			if vErr.Details["side"] != tc.side {
				t.Errorf("details say side %v, want %q", vErr.Details["side"], tc.side)
			}
		})
	}
}

// Acceptance criterion 6: a version that could not be asked has said nothing
// about the thing being compared, in either comparison. Reading a backend
// failure as an absence would report a whole call graph, or a whole file, as
// added or removed on the strength of a timeout — the one mistake in this model
// that produces a confident wrong answer instead of an error.
//
// compare_calls is checked on each side separately because it asks its provider
// twice; compare_code asks once, with both contexts, so it has one failure to
// pass on. Either way the provider's own error is what comes back: the engine
// adds no second opinion about a version nobody managed to read.
func TestEveryComparisonFailsWhenAVersionCannotBeAsked(t *testing.T) {
	for _, tc := range []struct {
		side    string
		failing vacctx.CodeContext
		other   vacctx.CodeContext
	}{
		{"from", compareV1, compareV2},
		{"to", compareV2, compareV1},
	} {
		t.Run("compare_calls: the "+tc.side+" side", func(t *testing.T) {
			failure := vacerr.New(
				vacerr.GraphProviderUnavailable,
				"the graph backend would not answer",
				map[string]any{"context": tc.failing.ID},
			)
			graph := &versionedGraph{
				graphs: map[string]provider.CallGraph{tc.other.ID: {
					Symbol: "demo.Process",
					Edges:  []provider.CallEdge{{Caller: "demo.Main", Callee: "demo.Process", Path: "main.go", Line: 4}},
				}},
				errs: map[string]error{tc.failing.ID: failure},
			}

			out, err := compareEngine(graph).CompareCalls(context.Background(), compareRequest())
			if err == nil {
				t.Fatalf("a comparison answered with the %s version unreadable", tc.side)
			}
			assertCode(t, err, vacerr.GraphProviderUnavailable)
			if !errors.Is(err, failure) {
				t.Fatalf("error is %v, want the provider's own error", err)
			}
			// Not a one-sided answer: the half that did answer is not the whole
			// truth, and the half that did not has not said the symbol is gone.
			assertNotAComparison(t, out)
		})
	}

	t.Run("compare_code: the revisions could not be diffed", func(t *testing.T) {
		failure := vacerr.New(vacerr.RevisionNotFound, "the revision is not in this repository", map[string]any{"path": comparedPath})
		source := &diffSource{diffErr: failure}

		out, err := compareCodeEngine(source).CompareCode(context.Background(), compareCodeRequest())
		if err == nil {
			t.Fatal("a comparison answered with the source provider refusing")
		}
		assertCode(t, err, vacerr.RevisionNotFound)
		if !errors.Is(err, failure) {
			t.Fatalf("error is %v, want the provider's own error", err)
		}
		assertNotACodeComparison(t, out)
	})
}

// Acceptance criteria 6 and 7, side by side: "this server cannot make that
// comparison" is one fact with two codes, because it is two different providers
// that are missing. compare_calls needs a graph and says
// GRAPH_PROVIDER_UNAVAILABLE; compare_code needs a source backend that can
// compare two revisions and says SOURCE_DIFF_UNAVAILABLE, the eleventh code and
// the only one added after v0.1.0.
//
// The third case is here because it is what makes the second one legible: with
// no source provider at all, compare_code reports REPOSITORY_NOT_FOUND, the same
// compromise get_code documents for want of a source-unavailable code. So the
// two facts are kept apart — there is no repository to read at all, against the
// source this server does have cannot diff — rather than folded into one code
// that would answer neither question.
//
// Whichever it is, it is a fact about this server and not about the code asked
// for: nothing is claimed about the file, the symbol or the versions.
func TestEachComparisonNamesTheCapabilityItLacks(t *testing.T) {
	contexts := mapContexts{compareV1.ID: single(compareV1), compareV2.ID: single(compareV2)}

	t.Run("compare_calls: no graph provider", func(t *testing.T) {
		eng := engine.New(contexts, &fakeSearch{}, nil, &fakeSource{})

		out, err := eng.CompareCalls(context.Background(), compareRequest())
		if err == nil {
			t.Fatal("a comparison answered with no graph provider behind it")
		}
		assertCode(t, err, vacerr.GraphProviderUnavailable)
		assertNotAComparison(t, out)
	})

	t.Run("compare_code: a source provider that cannot diff", func(t *testing.T) {
		// fakeSource reads and nothing else, which is what a SourceProvider
		// without the optional capability looks like from here.
		source := &fakeSource{}
		if _, canDiff := any(source).(provider.SourceDiffer); canDiff {
			t.Fatal("fakeSource implements SourceDiffer, so this no longer tests a backend that cannot")
		}

		out, err := engine.New(contexts, nil, nil, source).CompareCode(context.Background(), compareCodeRequest())
		if err == nil {
			t.Fatal("a comparison answered through a source provider that cannot compare versions")
		}
		assertCode(t, err, vacerr.SourceDiffUnavailable)
		assertNotACodeComparison(t, out)
		if source.called {
			t.Error("a backend that cannot compare was asked to read one version instead, which answers a different question")
		}
	})

	t.Run("compare_code: no source provider at all", func(t *testing.T) {
		out, err := engine.New(contexts, nil, nil, nil).CompareCode(context.Background(), compareCodeRequest())
		if err == nil {
			t.Fatal("a comparison answered with no source provider behind it")
		}
		assertCode(t, err, vacerr.RepositoryNotFound)
		assertNotACodeComparison(t, out)
	})
}
