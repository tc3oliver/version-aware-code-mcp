//go:build integration

package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
)

// This test is about [engine.Engine] rather than about a tool, but it lives in
// this package because the real fixture does: fixtureConfig and demorepo's
// prepared Zoekt index and CBM graphs are here, and a second copy of that
// wiring under engine/ would be a second thing to keep correct for no extra
// coverage. engine/'s own test files stay tag-free and fake-backed, which is
// the line extension_test.go already draws.

// How long the whole concurrent run gets. It is not a performance assertion —
// the calls below take milliseconds each once the engines are up — but the only
// way a deadlock fails this test instead of hanging the package until go test's
// own timeout fires with no idea which call it was waiting on.
const concurrencyDeadline = 5 * time.Minute

// Enough goroutines that the providers are genuinely contended, and enough
// rounds that a leak has more than one chance to appear. Every provider behind
// the engine is shared: one CBM process serving every trace, one Zoekt server
// answering every search, one git repository read at two revisions at once.
//
// Six engine calls per worker per round — two searches, a trace, a read, and
// the two comparisons — so 1080 calls in all, of which 360 span both versions
// at once.
const (
	concurrentWorkers = 60
	concurrentRounds  = 3
)

// concurrentVersion is one version scope and the handler that identifies it.
// Resolved before any goroutine starts, because a test may not be failed from a
// goroutine it did not start.
type concurrentVersion struct {
	codeCtx    vacctx.CodeContext
	own, other string
}

// TestConcurrentEngineCallsStayInTheirOwnVersion is the regression this project
// cannot ship without: two versions asked at once, and one of them getting the
// other's answer back.
//
// One Engine serves every call here, as one does in a running server — an agent
// asking about two versions at once, or two agents sharing a server, are
// concurrent calls on the same providers. The version scope travels as an
// argument rather than as state, which is what should make that safe; this is
// the test that says so rather than assuming it. Contamination has more than one
// shape — a response delivered to the wrong caller, a provider caching the last
// context it saw, a CBM project leaking between requests — and all of them look
// the same from here: an answer carrying the other release's handler.
//
// It is run under `-race` by full-gate.yml's Tier 3, which is the tier that
// combines the detector with the real engines.
func TestConcurrentEngineCallsStayInTheirOwnVersion(t *testing.T) {
	cfg := parityFixture(t)
	eng := parityEngine(t, cfg)

	versions := make([]concurrentVersion, 0, len(parityVersions))
	for _, version := range parityVersions {
		workspace, ok := cfg.Contexts[version.id]
		if !ok {
			t.Fatalf("the fixture configures no context %q", version.id)
		}
		// One repository per fixture context, which is what the tools under test
		// can be asked about at all.
		versions = append(versions, concurrentVersion{codeCtx: workspace.Members[0], own: version.own, other: version.other})
	}

	ctx, cancel := context.WithTimeout(t.Context(), concurrencyDeadline)
	defer cancel()

	var wg sync.WaitGroup
	for worker := range concurrentWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := range concurrentRounds {
				// Interleaved both ways: neighbouring workers are on different
				// versions at the same moment, and each worker changes version
				// between rounds. A scope held anywhere but in the argument —
				// in the engine, in a provider, in the CBM session — has a
				// request in the other version right beside it to be picked up
				// by.
				version := versions[(worker+round)%len(versions)]
				where := fmt.Sprintf("worker %d round %d, context %s", worker, round, version.codeCtx.ID)

				concurrentSearch(ctx, t, eng, where, version)
				concurrentTrace(ctx, t, eng, where, version)
				concurrentGet(ctx, t, eng, where, version)

				// The same worker then asks a question that spans both versions
				// at once, beside the single-context calls its neighbours are
				// making in each of them. A comparison is where a leak has the
				// most room: it holds two contexts in one call, so it can pick
				// up the wrong one, hand the wrong one to a provider, or report
				// one version's answer on the other's side — and the direction
				// alternates with the round, so neither side is always the
				// first.
				other := versions[(worker+round+1)%len(versions)]
				pair := fmt.Sprintf("worker %d round %d, %s -> %s", worker, round, version.codeCtx.ID, other.codeCtx.ID)
				concurrentCompareCode(ctx, t, eng, pair, version, other)
				concurrentCompareCalls(ctx, t, eng, pair, version, other)
			}
		}()
	}
	wg.Wait()
}

// concurrentSearch asks both halves of the isolation question: this version's
// handler is found, and the other version's is not. The second half is the one
// that catches a leak — the first would still pass if every search were
// answered against a union of both branches.
func concurrentSearch(ctx context.Context, t *testing.T, eng *engine.Engine, where string, version concurrentVersion) {
	found, err := eng.SearchCode(ctx, engine.SearchCodeRequest{Context: version.codeCtx.ID, Query: version.own})
	if err != nil {
		t.Errorf("%s: SearchCode(%s) error = %v", where, version.own, err)
		return
	}
	if len(found.Matches()) == 0 {
		t.Errorf("%s: SearchCode(%s) found nothing, want this version's own handler", where, version.own)
	}
	if got := found.Context().ID; got != version.codeCtx.ID {
		t.Errorf("%s: SearchCode answered in context %q", where, got)
	}

	absent, err := eng.SearchCode(ctx, engine.SearchCodeRequest{Context: version.codeCtx.ID, Query: version.other})
	if err != nil {
		t.Errorf("%s: SearchCode(%s) error = %v", where, version.other, err)
		return
	}
	if len(absent.Matches()) != 0 {
		t.Errorf("%s: SearchCode(%s) = %+v, which is the other version's handler: concurrent searches are crossing branches",
			where, version.other, absent.Matches())
	}
}

// concurrentTrace walks the call graph the context names. Process calls a
// different handler per release, so a trace answered from the other version's
// CBM project reports the wrong callee rather than merely a different count.
func concurrentTrace(ctx context.Context, t *testing.T, eng *engine.Engine, where string, version concurrentVersion) {
	traced, err := eng.TraceCalls(ctx, engine.TraceCallsRequest{
		Context: version.codeCtx.ID, Symbol: "Process", Direction: provider.Callees, Depth: 3,
	})
	if err != nil {
		t.Errorf("%s: TraceCalls(Process) error = %v", where, err)
		return
	}
	if got := traced.Context().ID; got != version.codeCtx.ID {
		t.Errorf("%s: TraceCalls answered in context %q", where, got)
	}

	called := calleesOf(callsOf(traced), "Process")
	if !contains(called, version.own) {
		t.Errorf("%s: Process calls %v, want it to include %s", where, called, version.own)
	}
	if contains(called, version.other) {
		t.Errorf("%s: Process calls %v, which includes the other version's %s: concurrent traces are crossing graphs",
			where, called, version.other)
	}
}

// concurrentGet reads the lines the two releases differ on, and checks the
// revision the bytes came from is this context's. Content and revision are
// checked together because either one alone can be right while the other is
// wrong: a stale revision with correct bytes is still an answer nobody can cite.
func concurrentGet(ctx context.Context, t *testing.T, eng *engine.Engine, where string, version concurrentVersion) {
	read, err := eng.GetCode(ctx, engine.GetCodeRequest{
		Context: version.codeCtx.ID, Path: "processor.go", StartLine: 4, EndLine: 6,
	})
	if err != nil {
		t.Errorf("%s: GetCode(processor.go) error = %v", where, err)
		return
	}
	// One member, because every context in this fixture names one repository, and
	// the revision the bytes came from is that member's. Reported rather than
	// fatal: this runs in a goroutine of its own.
	members := read.Context().Members
	if len(members) != 1 {
		t.Errorf("%s: GetCode answered in %d repositories, want the one this context names", where, len(members))
		return
	}
	if got := members[0].Revision; got != version.codeCtx.Revision {
		t.Errorf("%s: GetCode read at revision %s, want %s", where, got, version.codeCtx.Revision)
	}

	content := read.Source().Content
	if !strings.Contains(content, version.own) {
		t.Errorf("%s: GetCode returned %q, want the %s body", where, content, version.own)
	}
	if strings.Contains(content, version.other) {
		t.Errorf("%s: GetCode returned %q, which is the other version's body: concurrent reads are crossing revisions",
			where, content)
	}
}

// concurrentCompareCode compares the file the two releases differ in, in
// whichever direction this round runs it.
//
// The answer is checked from both ends: each side reports the revision its own
// context declares, and the hunk takes out the from version's handler and puts
// in the to version's. Either check alone can pass on a contaminated answer — a
// diff of one revision against itself still labels its sides correctly, and a
// diff of the right two revisions reported under the wrong contexts still shows
// the right lines — so the pair is what says the comparison ran between the two
// versions this call named.
func concurrentCompareCode(ctx context.Context, t *testing.T, eng *engine.Engine, where string, from, to concurrentVersion) {
	compared, err := eng.CompareCode(ctx, engine.CompareCodeRequest{
		FromContext: from.codeCtx.ID, ToContext: to.codeCtx.ID, Path: "processor.go",
	})
	if err != nil {
		t.Errorf("%s: CompareCode(processor.go) error = %v", where, err)
		return
	}
	if compared.Change() != engine.CodeModified {
		t.Errorf("%s: CompareCode(processor.go) = %s, want MODIFIED: the two releases delegate to different handlers",
			where, compared.Change())
	}
	assertComparedAt(t, where, "from", compared.From(), from)
	assertComparedAt(t, where, "to", compared.To(), to)

	hunks := compared.Hunks()
	if !diffShows(hunks, provider.LineRemoved, from.own) || diffShows(hunks, provider.LineRemoved, to.own) {
		t.Errorf("%s: the removed lines of processor.go are %+v, want the %s call and not the %s one",
			where, hunks, from.own, to.own)
	}
	if !diffShows(hunks, provider.LineAdded, to.own) || diffShows(hunks, provider.LineAdded, from.own) {
		t.Errorf("%s: the added lines of processor.go are %+v, want the %s call and not the %s one",
			where, hunks, to.own, from.own)
	}
}

// concurrentCompareCalls compares Process's callees across the same pair.
//
// Process calls exactly one handler in each release, so the classification is
// the whole answer: the from version's call is removed, the to version's is
// added, and neither is unchanged. A comparison answered from one graph twice
// reports nothing removed or nothing added; one answered from the wrong pair
// reports them the wrong way round.
func concurrentCompareCalls(ctx context.Context, t *testing.T, eng *engine.Engine, where string, from, to concurrentVersion) {
	compared, err := eng.CompareCalls(ctx, engine.CompareCallsRequest{
		FromContext: from.codeCtx.ID, ToContext: to.codeCtx.ID,
		Symbol: "Process", Direction: provider.Callees, Depth: 1,
	})
	if err != nil {
		t.Errorf("%s: CompareCalls(Process) error = %v", where, err)
		return
	}
	if compared.Presence() != engine.PresenceBoth {
		t.Errorf("%s: CompareCalls(Process) presence = %s, want BOTH: both releases declare Process",
			where, compared.Presence())
	}
	assertComparedAt(t, where, "from", compared.From(), from)
	assertComparedAt(t, where, "to", compared.To(), to)

	if !relates(compared.Removed(), "Process", from.own) || relates(compared.Removed(), "Process", to.own) {
		t.Errorf("%s: removed = %+v, want only the %s call the from version makes", where, compared.Removed(), from.own)
	}
	if !relates(compared.Added(), "Process", to.own) || relates(compared.Added(), "Process", from.own) {
		t.Errorf("%s: added = %+v, want only the %s call the to version makes", where, compared.Added(), to.own)
	}
	if relates(compared.Unchanged(), "Process", from.own) || relates(compared.Unchanged(), "Process", to.own) {
		t.Errorf("%s: unchanged = %+v, want neither handler: each release calls only its own", where, compared.Unchanged())
	}
}

// assertComparedAt holds one half of a comparison to the version it was supposed
// to be answered in. An absent side is a failure here rather than an answer:
// both releases have processor.go and both declare Process, so the only way a
// side goes missing is a comparison that lost one of the two versions it named.
func assertComparedAt(t *testing.T, where, which string, side engine.ComparisonSide, want concurrentVersion) {
	if !side.Present() {
		t.Errorf("%s: the %s side is absent, want it answered in %s", where, which, want.codeCtx.ID)
		return
	}
	if got := side.Context().ID; got != want.codeCtx.ID {
		t.Errorf("%s: the %s side was answered in context %q, want %q", where, which, got, want.codeCtx.ID)
	}
	members := side.Context().Members
	if len(members) != 1 {
		t.Errorf("%s: the %s side was answered in %d repositories, want the one this context names", where, which, len(members))
		return
	}
	if got := members[0].Revision; got != want.codeCtx.Revision {
		t.Errorf("%s: the %s side was answered at revision %s, want %s", where, which, got, want.codeCtx.Revision)
	}
}

// diffShows reports whether any hunk line of that kind mentions the text.
func diffShows(hunks []provider.DiffHunk, kind provider.DiffLineKind, text string) bool {
	for _, h := range hunks {
		for _, line := range h.Lines {
			if line.Kind == kind && strings.Contains(line.Content, text) {
				return true
			}
		}
	}
	return false
}

// relates reports whether one of the classified relations is caller -> callee.
func relates(list []engine.CallRelation, caller, callee string) bool {
	for _, rel := range list {
		if strings.EqualFold(rel.Caller, caller) && strings.EqualFold(rel.Callee, callee) {
			return true
		}
	}
	return false
}

// callsOf projects a traversal onto the wire shape calleesOf reads, which is
// the one the trace_calls tests already share.
func callsOf(traced engine.TraceCallsResult) []call {
	edges := traced.Graph().Edges
	calls := make([]call, 0, len(edges))
	for _, edge := range edges {
		calls = append(calls, call{Caller: edge.Caller, Callee: edge.Callee, Path: edge.Path, Line: edge.Line})
	}
	return calls
}
