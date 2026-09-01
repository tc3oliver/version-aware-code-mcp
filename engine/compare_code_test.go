package engine_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// The one path every code comparison here is about.
const comparedPath = "process.go"

// diffSource is a source backend that also has the optional
// [provider.SourceDiffer] capability, which fakeSource deliberately does not:
// what compare_code does with a backend that can compare two versions and what
// it does with one that cannot are two different answers, and only a second fake
// can tell them apart.
//
// It classifies from the two versions' content the way a real differ does,
// rather than from a canned answer, because most of what is under test only
// means something when the outcome is derived: comparing a version with itself
// has to come out unchanged because the same content is on both sides, not
// because a fake was told to say so. A version missing from files is one that
// does not have the file; a whole file is treated as one line, which is enough
// for a hunk to be checked against.
type diffSource struct {
	fakeSource
	files   map[string]string
	diffErr error
	from    vacctx.CodeContext
	to      vacctx.CodeContext
	req     provider.SourceDiffRequest
	diffs   int
}

func (d *diffSource) Diff(_ context.Context, from, to vacctx.CodeContext, req provider.SourceDiffRequest) (*provider.SourceDiff, error) {
	d.diffs++
	d.from, d.to, d.req = from, to, req
	if d.diffErr != nil {
		return nil, d.diffErr
	}

	fromText, inFrom := d.files[from.ID]
	toText, inTo := d.files[to.ID]
	diff := &provider.SourceDiff{Path: req.Path}
	switch {
	case !inFrom && !inTo:
		// What the git adapter answers for a path neither revision has: a
		// comparison of a file that was never there is not a reassuring
		// "nothing changed".
		return nil, vacerr.New(
			vacerr.InvalidArgument,
			"compare_code: neither version has file "+req.Path,
			map[string]any{"path": req.Path, "from_context": from.ID, "to_context": to.ID},
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

// compareCodeEngine is an Engine over the same two versions the call-graph
// comparison is tested against, answering through the source provider handed in
// and no other provider at all: comparing code reads neither a graph nor an
// index, and building it with those two nil is what keeps it that way.
func compareCodeEngine(source provider.SourceProvider) *engine.Engine {
	return engine.New(mapContexts{compareV1.ID: single(compareV1), compareV2.ID: single(compareV2)}, nil, nil, source)
}

func compareCodeRequest() engine.CompareCodeRequest {
	return engine.CompareCodeRequest{
		FromContext: compareV1.ID,
		ToContext:   compareV2.ID,
		Path:        comparedPath,
	}
}

// Acceptance criterion 1: a code comparison names its scope with two context IDs
// and the file, and nothing else. A repository, branch or revision field here
// would let a caller compare a version the configuration never granted it, which
// is the whole guarantee given away in a struct literal.
func TestCompareCodeRequestIsScopedByContextIDsAlone(t *testing.T) {
	typ := reflect.TypeOf(engine.CompareCodeRequest{})
	var got []string
	for i := range typ.NumField() {
		got = append(got, typ.Field(i).Name)
	}
	want := []string{"FromContext", "ToContext", "Path"}
	if !slices.Equal(got, want) {
		t.Fatalf("CompareCodeRequest has the fields %v, want exactly %v", got, want)
	}
}

// Acceptance criterion 2: an ID naming no configured version is refused on
// either side, before any file is read. Guessing one, or comparing against an
// empty file, would report another version's code — or its absence — as this
// one's.
func TestCompareCodeUnknownContextIsContextNotFound(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*engine.CompareCodeRequest)
	}{
		{"from", func(r *engine.CompareCodeRequest) { r.FromContext = "demo@v9" }},
		{"to", func(r *engine.CompareCodeRequest) { r.ToContext = "demo@v9" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := &diffSource{files: map[string]string{
				compareV1.ID: "package demo\n",
				compareV2.ID: "package demo\n",
			}}
			req := compareCodeRequest()
			tc.mutate(&req)

			out, err := compareCodeEngine(source).CompareCode(context.Background(), req)
			if err == nil {
				t.Fatalf("a comparison answered with %s naming an unconfigured version", tc.name)
			}
			assertCode(t, err, vacerr.ContextNotFound)
			assertNotACodeComparison(t, out)
			if source.diffs != 0 {
				t.Fatalf("an unconfigured version reached the source provider %d times", source.diffs)
			}
		})
	}
}

// Acceptance criterion 3: two repositories have no shared history, so there is
// no comparison to make between them. It is refused here, before any provider is
// reached, rather than left to whichever backend would happen to notice: a
// caller holding only this API must not have to rely on the adapter behind it
// for the refusal.
//
// The repository being compared is the resolved member's, which is the only
// place a repository is written down: a context is a workspace and has none of
// its own. That is what keeps this check from being satisfied by two contexts
// merely having different IDs — every other test in this file compares two
// contexts whose members share a repository, and they answer.
func TestCompareCodeRefusesContextsInDifferentRepositories(t *testing.T) {
	other := vacctx.CodeContext{
		ID: "other@v1", Repository: "other", Branch: "v1",
		Revision: "3333333333333333333333333333333333333333", GraphRef: "other-v1",
	}
	source := &diffSource{files: map[string]string{
		compareV1.ID: "package demo\n",
		other.ID:     "package other\n",
	}}
	eng := engine.New(mapContexts{compareV1.ID: single(compareV1), other.ID: single(other)}, nil, nil, source)
	req := compareCodeRequest()
	req.ToContext = other.ID

	out, err := eng.CompareCode(context.Background(), req)
	if err == nil {
		t.Fatal("a comparison answered across two repositories")
	}
	assertCode(t, err, vacerr.InvalidArgument)
	assertNotACodeComparison(t, out)

	var vErr *vacerr.Error
	if !errors.As(err, &vErr) {
		t.Fatalf("error is %v, want a *vacerr.Error", err)
	}
	if vErr.Details["from_repository"] != compareV1.Repository || vErr.Details["to_repository"] != other.Repository {
		t.Errorf("details name repositories from=%v to=%v, want %q and %q",
			vErr.Details["from_repository"], vErr.Details["to_repository"], compareV1.Repository, other.Repository)
	}
	if vErr.Details["from_context"] != compareV1.ID || vErr.Details["to_context"] != other.ID {
		t.Errorf("details name contexts from=%v to=%v, want %q and %q",
			vErr.Details["from_context"], vErr.Details["to_context"], compareV1.ID, other.ID)
	}
	if source.diffs != 0 {
		t.Fatalf("a cross-repository comparison reached the source provider %d times", source.diffs)
	}
}

// Acceptance criterion 4: comparing code needs a source provider that can
// compare, and the two ways of not having one are different facts. No source
// provider at all is no repository to read, which is what get_code already says
// for want of a source-unavailable code; a source provider that reads one
// version at a time is a capability this server does not have, which is what
// SOURCE_DIFF_UNAVAILABLE was added to say.
func TestCompareCodeNeedsASourceProviderThatCanDiff(t *testing.T) {
	t.Run("no source provider", func(t *testing.T) {
		out, err := compareCodeEngine(nil).CompareCode(context.Background(), compareCodeRequest())
		if err == nil {
			t.Fatal("a comparison answered with no source provider behind it")
		}
		assertCode(t, err, vacerr.RepositoryNotFound)
		assertNotACodeComparison(t, out)
	})

	t.Run("a source provider that cannot diff", func(t *testing.T) {
		// fakeSource reads and nothing else, which is what a SourceProvider
		// without the optional capability looks like from here.
		source := &fakeSource{}
		if _, canDiff := any(source).(provider.SourceDiffer); canDiff {
			t.Fatal("fakeSource implements SourceDiffer, so this test no longer tests a backend that cannot")
		}

		out, err := compareCodeEngine(source).CompareCode(context.Background(), compareCodeRequest())
		if err == nil {
			t.Fatal("a comparison answered through a source provider that cannot compare versions")
		}
		assertCode(t, err, vacerr.SourceDiffUnavailable)
		assertNotACodeComparison(t, out)
		if source.called {
			t.Error("a backend that cannot compare was asked to read instead, which answers a different question")
		}
	})
}

// Acceptance criterion 5: a file only one version has is an answer, not a
// failure — it is the answer "this was added" or "this was removed". The version
// that has it is present and reports its own scope; the other is absent rather
// than empty, because it cites nothing on account of having had nothing, which
// is not the same as having had nothing to cite.
func TestCompareCodeReportsAFileFoundInOnlyOneVersion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		present vacctx.CodeContext
		absent  vacctx.CodeContext
		change  engine.CodeChange
	}{
		{"only the to version has it", compareV2, compareV1, engine.CodeAdded},
		{"only the from version has it", compareV1, compareV2, engine.CodeRemoved},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := &diffSource{files: map[string]string{tc.present.ID: "package demo\n"}}

			out, err := compareCodeEngine(source).CompareCode(context.Background(), compareCodeRequest())
			if err != nil {
				t.Fatalf("CompareCode: %v", err)
			}
			if out.Change() != tc.change {
				t.Fatalf("change is %q, want %q", out.Change(), tc.change)
			}

			side, absent := out.From(), out.To()
			if tc.change == engine.CodeAdded {
				side, absent = out.To(), out.From()
			}
			if !side.Present() || answeredIn(t, side) != tc.present {
				t.Fatalf("the surviving side reports context %+v and Present %v, want the version that has the file",
					side.Context(), side.Present())
			}
			if absent.Present() {
				t.Fatalf("the version without the file reports Present true and context %+v", absent.Context())
			}
			if absent.Evidence() != nil {
				t.Errorf("the version without the file cites %+v, want nothing", absent.Evidence())
			}
			// Present with nothing worth citing is not absent: a whole file's
			// diff has no one line to point at, and an empty list says so where
			// nil would say "this is not an answer".
			if citedIn(t, side) == nil {
				t.Error("the surviving side cites nil, want an empty list: it is present, not absent")
			}
			if out.Path() != comparedPath {
				t.Errorf("path is %q, want the file compared %q", out.Path(), comparedPath)
			}
		})
	}
}

// Acceptance criterion 6: two versions holding the same file is a comparison
// that ran and found nothing, and two versions holding different ones is a
// comparison that found the difference and can show it. Both are answers with
// both sides present — the file is in both versions either way.
func TestCompareCodeReportsWhetherTheContentDiffers(t *testing.T) {
	t.Run("identical content", func(t *testing.T) {
		source := &diffSource{files: map[string]string{
			compareV1.ID: "package demo\n",
			compareV2.ID: "package demo\n",
		}}

		out, err := compareCodeEngine(source).CompareCode(context.Background(), compareCodeRequest())
		if err != nil {
			t.Fatalf("CompareCode: %v", err)
		}
		if out.Change() != engine.CodeUnchanged {
			t.Fatalf("change is %q, want %q", out.Change(), engine.CodeUnchanged)
		}
		assertBothSidesPresent(t, out)
		// Empty rather than nil: the comparison ran and had nothing to show,
		// which is not the zero result nobody built.
		if out.Hunks() == nil {
			t.Error("an unchanged file reports nil hunks, want an empty list")
		}
		if len(out.Hunks()) != 0 {
			t.Errorf("an unchanged file reports hunks %+v", out.Hunks())
		}
		if out.Binary() {
			t.Error("a text file is reported as binary")
		}
	})

	t.Run("different content", func(t *testing.T) {
		source := &diffSource{files: map[string]string{
			compareV1.ID: "package demo\n",
			compareV2.ID: "package demo // v2\n",
		}}

		out, err := compareCodeEngine(source).CompareCode(context.Background(), compareCodeRequest())
		if err != nil {
			t.Fatalf("CompareCode: %v", err)
		}
		if out.Change() != engine.CodeModified {
			t.Fatalf("change is %q, want %q", out.Change(), engine.CodeModified)
		}
		assertBothSidesPresent(t, out)

		// The provider's own hunks, line numbers and all: what changed is the
		// backend's answer, and reshaping it here would put numbers on the
		// result that the caller would go on to cite.
		want := []provider.DiffHunk{{
			OldStart: 1, OldLines: 1, NewStart: 1, NewLines: 1,
			Lines: []provider.DiffLine{
				{Kind: provider.LineRemoved, Content: "package demo\n"},
				{Kind: provider.LineAdded, Content: "package demo // v2\n"},
			},
		}}
		if !reflect.DeepEqual(out.Hunks(), want) {
			t.Fatalf("hunks are %+v, want the provider's %+v", out.Hunks(), want)
		}
	})
}

// Acceptance criterion 7: comparing a version with itself is a question with an
// answer, and the answer is "nothing changed". It is not special-cased anywhere:
// both sides resolve to the one version, so the backend is asked to compare a
// revision with itself and finds the same content on both sides.
func TestCompareCodeOfOneVersionWithItselfReportsNoChange(t *testing.T) {
	source := &diffSource{files: map[string]string{compareV1.ID: "package demo\n"}}
	req := compareCodeRequest()
	req.ToContext = req.FromContext

	out, err := compareCodeEngine(source).CompareCode(context.Background(), req)
	if err != nil {
		t.Fatalf("CompareCode: %v", err)
	}
	if out.Change() != engine.CodeUnchanged {
		t.Fatalf("change is %q, want %q", out.Change(), engine.CodeUnchanged)
	}
	if len(out.Hunks()) != 0 {
		t.Errorf("comparing a version with itself reported hunks %+v", out.Hunks())
	}
	if answeredIn(t, out.From()) != compareV1 || answeredIn(t, out.To()) != compareV1 {
		t.Fatalf("the sides report from=%+v to=%+v, want both in the one version compared",
			out.From().Context(), out.To().Context())
	}
	// The backend was asked, rather than the engine answering for it: an
	// equality shortcut here would be this server deciding two revisions are the
	// same file without reading either.
	if source.diffs != 1 || source.from.Revision != source.to.Revision {
		t.Fatalf("the source provider was asked %d times with from=%q to=%q, want one comparison of a revision with itself",
			source.diffs, source.from.Revision, source.to.Revision)
	}
}

// A version that could not be compared has not said anything about the file.
// The backend's own error is what comes back, unchanged: the adapter already
// tells a path neither revision has from a repository it cannot read, and
// reclassifying either here would be this layer inventing a second opinion.
func TestCompareCodeReturnsTheProviderFailureUnchanged(t *testing.T) {
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
}

// Acceptance criterion 8: the contract is structural. Every field of
// CompareCodeResult is unexported, so no composite literal outside the package
// can fill one in, and the only value a caller can write for itself is the zero
// one — which reports a change that is none of the four, cites nothing on either
// side and shows no hunks.
//
// It answers no Context and no Evidence of its own either: a comparison is
// answered in two versions, and one merged pair would be a cross-version answer
// wearing the clothes of a version-scoped one.
func TestACompareCodeResultCannotBeBuiltOutsideTheEngine(t *testing.T) {
	typ := reflect.TypeOf(engine.CompareCodeResult{})
	for i := range typ.NumField() {
		if field := typ.Field(i); field.IsExported() {
			t.Errorf("CompareCodeResult.%s is exported: a caller can build a comparison claiming versions it never queried",
				field.Name)
		}
	}

	if _, isContextual := any(engine.CompareCodeResult{}).(contextual); isContextual {
		t.Error("CompareCodeResult answers one Context and one Evidence, which cannot say which of the two versions they came from")
	}

	assertNotACodeComparison(t, engine.CompareCodeResult{})
	for _, change := range []engine.CodeChange{engine.CodeUnchanged, engine.CodeModified, engine.CodeAdded, engine.CodeRemoved} {
		if (engine.CompareCodeResult{}).Change() == change {
			t.Errorf("the zero CompareCodeResult reports change %q, so a caller can claim one by writing one", change)
		}
	}
}

// Acceptance criterion 9: comparing code reads no graph and no search index, so
// an embedder with neither provider can still do it. The engine here has a
// context source and a source provider and nothing else, and a comparison that
// had grown a dependency on either of the others would not answer at all.
func TestCompareCodeNeedsOnlyContextsAndASourceThatCanDiff(t *testing.T) {
	source := &diffSource{files: map[string]string{
		compareV1.ID: "package demo\n",
		compareV2.ID: "package demo // v2\n",
	}}
	eng := engine.New(mapContexts{compareV1.ID: single(compareV1), compareV2.ID: single(compareV2)}, nil, nil, source)

	out, err := eng.CompareCode(context.Background(), compareCodeRequest())
	if err != nil {
		t.Fatalf("CompareCode with no search or graph provider: %v", err)
	}

	// Each side was handed its own resolved scope, and the file was asked for
	// once and identically: a difference produced by asking about two different
	// files is not a difference between the two versions.
	if source.from != compareV1 || source.to != compareV2 {
		t.Fatalf("the source provider was asked from=%+v to=%+v, want the two versions compared", source.from, source.to)
	}
	if source.req != (provider.SourceDiffRequest{Path: comparedPath}) {
		t.Fatalf("the source provider was asked %+v, want the path that was requested", source.req)
	}
	if source.called {
		t.Error("comparing code read a single version as well, which answers a different question")
	}

	if out.Change() != engine.CodeModified {
		t.Fatalf("change is %q, want %q", out.Change(), engine.CodeModified)
	}
	if answeredIn(t, out.From()) != compareV1 || answeredIn(t, out.To()) != compareV2 {
		t.Fatalf("the sides report from=%+v to=%+v, want the two versions compared", out.From().Context(), out.To().Context())
	}
	assertBothSidesPresent(t, out)
	if len(out.Hunks()) != 1 {
		t.Fatalf("hunks are %+v, want the provider's one", out.Hunks())
	}
}

func assertBothSidesPresent(t *testing.T, out engine.CompareCodeResult) {
	t.Helper()
	if !out.From().Present() || !out.To().Present() {
		t.Fatalf("a file both versions have reports Present from=%v to=%v", out.From().Present(), out.To().Present())
	}
	if citedIn(t, out.From()) == nil || citedIn(t, out.To()) == nil {
		t.Errorf("a present side cites nil, want an empty list: from=%+v to=%+v", out.From().Evidence(), out.To().Evidence())
	}
}

// assertNotACodeComparison holds the other half of the anti-forgery contract:
// what a failed comparison returns beside its error. Neither side claims a
// version, no change is reported and no hunk is shown, so a caller that ignored
// the error still cannot mistake the result for an answer.
func assertNotACodeComparison(t *testing.T, out engine.CompareCodeResult) {
	t.Helper()
	assertNotAnAnswer(t, out.From())
	assertNotAnAnswer(t, out.To())
	if out.From().Present() || out.To().Present() {
		t.Errorf("a result that is not an answer reports Present from=%v to=%v", out.From().Present(), out.To().Present())
	}
	if out.Change() != "" || out.Path() != "" || out.Binary() {
		t.Errorf("a result that is not an answer reports change %q, path %q, binary %v", out.Change(), out.Path(), out.Binary())
	}
	if len(out.Hunks()) != 0 {
		t.Errorf("a result that is not an answer shows hunks %+v", out.Hunks())
	}
}
