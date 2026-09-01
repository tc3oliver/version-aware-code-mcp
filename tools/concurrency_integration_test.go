//go:build integration

package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/internal/demorepo"
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
// answering every search, one git repository read at several revisions at once,
// and — since TASK-85 — a second repository read alongside the first.
//
// Each worker makes five single-context calls per round — two searches, a
// trace and a read confined to one version, plus a compare_code and a
// compare_calls whenever the round's two versions share a repository to
// compare — so every round exercises both the revision-isolation and the
// repository-isolation failure this project exists to prevent.
const (
	concurrentWorkers = 60
	concurrentRounds  = 3
)

// concurrentVersion is one version scope, the repository-narrowed query it is
// asked with, and the handful of facts a query answered from the wrong version
// or the wrong repository would get wrong.
//
// A single struct serves every entry — same repository at two revisions, and
// two different repositories under the multi-member demo-multi context alike
// — rather than one shape for "the same repository" and another for "a
// different one": those are not two kinds of version scope, only two ways to
// end up with one, and a second shape would be a second thing this test has to
// keep correct.
//
// Resolved before any goroutine starts, because a test may not be failed from a
// goroutine it did not start.
type concurrentVersion struct {
	codeCtx vacctx.CodeContext

	// own is found by a search or a read confined to this repository; other is
	// a real symbol that belongs to a different version and must not be.
	own, other string

	// The symbol trace_calls walks in this repository's own graph, and the
	// name expected — or forbidden — among the relations that walk reports.
	// direction differs across the four entries below because the two
	// repositories model the collision from opposite ends: versioned-demo-repo
	// is walked from its caller towards its handler, second-demo-repo from its
	// handler towards its caller.
	tracedSymbol               string
	direction                  provider.Direction
	wantRelated, absentRelated string

	// The file get_code and compare_code read, and the substrings that say
	// whose body came back. processor.go does not exist in second-demo-repo,
	// so a bare "processor.go" is exactly the ambiguous path decision-11
	// warns about — every read below names its own file instead.
	path                       string
	startLine, endLine         int
	wantContent, absentContent string
}

// TestConcurrentEngineCallsStayInTheirOwnVersion is the regression this project
// cannot ship without: two versions asked at once, and one of them getting the
// other's answer back — whether "the other version" is a different revision of
// the same repository or a different repository entirely.
//
// One Engine serves every call here, as one does in a running server — an agent
// asking about two versions at once, or two agents sharing a server, are
// concurrent calls on the same providers. The version scope travels as an
// argument rather than as state, which is what should make that safe; this is
// the test that says so rather than assuming it. Contamination has more than one
// shape — a response delivered to the wrong caller, a provider caching the last
// context it saw, a CBM project or a Zoekt repository filter leaking between
// requests — and all of them look the same from here: an answer carrying the
// other version's handler.
//
// It is run under `-race` by full-gate.yml's Tier 3, which is the tier that
// combines the detector with the real engines.
func TestConcurrentEngineCallsStayInTheirOwnVersion(t *testing.T) {
	cfg := parityFixture(t)
	eng := parityEngine(t, cfg)

	versions := concurrentVersions(t, cfg)

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
				// request in another version or another repository right
				// beside it to be picked up by.
				version := versions[(worker+round)%len(versions)]
				where := fmt.Sprintf("worker %d round %d, context %s repository %s",
					worker, round, version.codeCtx.ID, version.codeCtx.Repository)

				concurrentSearch(ctx, t, eng, where, version)
				concurrentTrace(ctx, t, eng, where, version)
				concurrentGet(ctx, t, eng, where, version)

				// The same worker then asks a question that spans both
				// versions at once, beside the single-context calls its
				// neighbours are making in each of them — but only between
				// two versions of the *same* repository: compare_code and
				// compare_calls have no cross-repository comparison to make
				// (decision-11 §4), and pairing versioned-demo-repo with
				// second-demo-repo here would be asking for the
				// INVALID_ARGUMENT that is neither this test's contamination
				// nor a bug. A comparison is where a leak has the most room
				// when it is available: it holds two contexts in one call,
				// so it can pick up the wrong one, hand the wrong one to a
				// provider, or report one version's answer on the other's
				// side — and the direction alternates with the round, so
				// neither side is always the first.
				other := versions[(worker+round+1)%len(versions)]
				if other.codeCtx.Repository != version.codeCtx.Repository || other.codeCtx.Revision == version.codeCtx.Revision {
					continue
				}
				pair := fmt.Sprintf("worker %d round %d, %s -> %s", worker, round, version.codeCtx.ID, other.codeCtx.ID)
				concurrentCompareCode(ctx, t, eng, pair, version, other)
				concurrentCompareCalls(ctx, t, eng, pair, version, other)
			}
		}()
	}
	wg.Wait()
}

// concurrentVersions is the four version scopes exercised above: the two
// single-repository release contexts, one revision apart on
// versioned-demo-repo, and demo-multi's two members, one repository apart at
// its own revision. Deriving both pairs from the one prepared fixture — rather
// than adding a fixture of its own — is decision-11's "沿用 TASK-84 的共用
// fixture" out of scope line held to.
func concurrentVersions(t *testing.T, cfg *config.Config) []concurrentVersion {
	t.Helper()

	v1Ctx, v2Ctx := only(cfg, v1), only(cfg, v2)
	multi, ok := cfg.Contexts[demorepo.MultiContext]
	if !ok || len(multi.Members) != 2 {
		t.Fatalf("the fixture configures no two-member %s", demorepo.MultiContext)
	}
	repo1, repo2 := multi.Members[0], multi.Members[1]
	if repo1.Repository != multiRepo1 || repo2.Repository != multiRepo2 {
		t.Fatalf("%s members = %+v, want %s then %s", demorepo.MultiContext, multi.Members, multiRepo1, multiRepo2)
	}

	return []concurrentVersion{
		{
			codeCtx: v1Ctx, own: "LegacyHandler", other: "NewHandler",
			tracedSymbol: "Process", direction: provider.Callees,
			wantRelated: "LegacyHandler", absentRelated: "NewHandler",
			path: "processor.go", startLine: 4, endLine: 6,
			wantContent: "LegacyHandler", absentContent: "NewHandler",
		},
		{
			codeCtx: v2Ctx, own: "NewHandler", other: "LegacyHandler",
			tracedSymbol: "Process", direction: provider.Callees,
			wantRelated: "NewHandler", absentRelated: "LegacyHandler",
			path: "processor.go", startLine: 4, endLine: 6,
			wantContent: "NewHandler", absentContent: "LegacyHandler",
		},
		// demo-multi's own copy of release/v1: the same repository, the same
		// revision and the same graph_ref as demo-v1, reached through a
		// second, multi-member context and repository selection instead. It
		// proves selectMembers picks the same version concurrently that a
		// single-repository context names directly.
		{
			codeCtx: repo1, own: "LegacyHandler", other: "NewHandler",
			tracedSymbol: "Process", direction: provider.Callees,
			wantRelated: "LegacyHandler", absentRelated: "NewHandler",
			path: "processor.go", startLine: 4, endLine: 6,
			wantContent: "LegacyHandler", absentContent: "NewHandler",
		},
		// second-demo-repo, demo-multi's other member: not a revision of
		// versioned-demo-repo, a different repository that happens to declare
		// a LegacyHandler of its own at the same path. It is walked from its
		// caller Invoke rather than from a Process it does not have, and
		// "the other version" it must never answer with is
		// versioned-demo-repo's Process.
		//
		// other is NewHandler rather than Process: gen-second-demo-repo.sh's
		// own invoke.go names Process in a doc comment ("must never be
		// versioned-demo-repo's Process"), and Zoekt's full-text search finds
		// it there — a collision in this fixture's own prose, not the leak
		// concurrentSearch checks for. absentRelated stays Process, because
		// that one is checked against the call graph's declared symbols, which
		// a comment cannot add a node to.
		{
			codeCtx: repo2, own: "LegacyHandler", other: "NewHandler",
			tracedSymbol: "LegacyHandler", direction: provider.Callers,
			wantRelated: "Invoke", absentRelated: "Process",
			path: "handler.go", startLine: 1, endLine: 9,
			wantContent: "second: ", absentContent: "legacy: ",
		},
	}
}

// concurrentSearch asks both halves of the isolation question, confined to
// this version's own repository: this version's handler is found, and the
// other version's is not. The second half is the one that catches a leak —
// the first would still pass if every search were answered against a union of
// every repository and every revision.
//
// repository is always given, even for the two single-repository contexts
// where it changes nothing: a bare Query with no Repository would search every
// member of demo-multi at once, which is exactly the ambiguity this project's
// isolation guarantee is not about — that is search_code's documented "search
// every repository" behaviour, not a leak.
func concurrentSearch(ctx context.Context, t *testing.T, eng *engine.Engine, where string, version concurrentVersion) {
	found, err := eng.SearchCode(ctx, engine.SearchCodeRequest{
		Context: version.codeCtx.ID, Repository: version.codeCtx.Repository, Query: version.own,
	})
	if err != nil {
		t.Errorf("%s: SearchCode(%s) error = %v", where, version.own, err)
		return
	}
	if len(found.Matches()) == 0 {
		t.Errorf("%s: SearchCode(%s) found nothing, want this version's own handler", where, version.own)
	}
	for _, member := range found.Context().Members {
		if member.Repository != version.codeCtx.Repository {
			t.Errorf("%s: SearchCode answered with repository %q among its members", where, member.Repository)
		}
	}

	absent, err := eng.SearchCode(ctx, engine.SearchCodeRequest{
		Context: version.codeCtx.ID, Repository: version.codeCtx.Repository, Query: version.other,
	})
	if err != nil {
		t.Errorf("%s: SearchCode(%s) error = %v", where, version.other, err)
		return
	}
	if len(absent.Matches()) != 0 {
		t.Errorf("%s: SearchCode(%s) = %+v, which is another version's handler: concurrent searches are crossing branches or repositories",
			where, version.other, absent.Matches())
	}
}

// concurrentTrace walks version's own call graph. Both repositories declare a
// symbol named LegacyHandler, so a trace answered from the wrong graph — the
// other revision of this repository, or the other repository entirely —
// reports the wrong related name rather than merely a different count.
func concurrentTrace(ctx context.Context, t *testing.T, eng *engine.Engine, where string, version concurrentVersion) {
	traced, err := eng.TraceCalls(ctx, engine.TraceCallsRequest{
		Context: version.codeCtx.ID, Repository: version.codeCtx.Repository,
		Symbol: version.tracedSymbol, Direction: version.direction, Depth: 2,
	})
	if err != nil {
		t.Errorf("%s: TraceCalls(%s) error = %v", where, version.tracedSymbol, err)
		return
	}
	members := traced.Context().Members
	if len(members) != 1 || members[0].Repository != version.codeCtx.Repository {
		t.Errorf("%s: TraceCalls answered with members %+v, want the one repository this walk named", where, members)
	}

	related := tracedRelated(callsOf(traced), version.tracedSymbol, version.direction)
	if !contains(related, version.wantRelated) {
		t.Errorf("%s: %s's %s = %v, want it to include %s", where, version.tracedSymbol, version.direction, related, version.wantRelated)
	}
	if contains(related, version.absentRelated) {
		t.Errorf("%s: %s's %s = %v, which includes %s from another version or repository: concurrent traces are crossing graphs",
			where, version.tracedSymbol, version.direction, related, version.absentRelated)
	}
}

// tracedRelated projects a traversal onto the names on the other end of
// tracedSymbol's relations, in whichever direction the walk asked for:
// callees when walking towards what the symbol calls, callers when walking
// towards what calls it.
func tracedRelated(calls []call, symbol string, direction provider.Direction) []string {
	if direction == provider.Callers {
		return callersOf(calls, symbol)
	}
	return calleesOf(calls, symbol)
}

// concurrentGet reads the file that says which repository and which revision
// answered, and checks both together: content and revision are checked
// together because either one alone can be right while the other is wrong — a
// stale revision with correct bytes is still an answer nobody can cite, and
// the reverse is a leak wearing the right label.
func concurrentGet(ctx context.Context, t *testing.T, eng *engine.Engine, where string, version concurrentVersion) {
	read, err := eng.GetCode(ctx, engine.GetCodeRequest{
		Context: version.codeCtx.ID, Repository: version.codeCtx.Repository,
		Path: version.path, StartLine: version.startLine, EndLine: version.endLine,
	})
	if err != nil {
		t.Errorf("%s: GetCode(%s) error = %v", where, version.path, err)
		return
	}
	// One member, because every entry here names one repository via
	// Repository, and the revision the bytes came from is that member's.
	// Reported rather than fatal: this runs in a goroutine of its own.
	members := read.Context().Members
	if len(members) != 1 {
		t.Errorf("%s: GetCode answered in %d repositories, want the one this context names", where, len(members))
		return
	}
	if got := members[0].Repository; got != version.codeCtx.Repository {
		t.Errorf("%s: GetCode read repository %s, want %s", where, got, version.codeCtx.Repository)
	}
	if got := members[0].Revision; got != version.codeCtx.Revision {
		t.Errorf("%s: GetCode read at revision %s, want %s", where, got, version.codeCtx.Revision)
	}

	content := read.Source().Content
	if !strings.Contains(content, version.wantContent) {
		t.Errorf("%s: GetCode returned %q, want the %s body", where, content, version.wantContent)
	}
	if strings.Contains(content, version.absentContent) {
		t.Errorf("%s: GetCode returned %q, which is another version's body: concurrent reads are crossing revisions or repositories",
			where, content)
	}
}

// concurrentCompareCode compares the file the two releases differ in, in
// whichever direction this round runs it. The caller only reaches here when
// from and to share a repository, so req.Repository disambiguates without
// ever being the ambiguous bare path a ready-made "processor.go" would be
// against demo-multi's second member.
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
		FromContext: from.codeCtx.ID, ToContext: to.codeCtx.ID, Repository: from.codeCtx.Repository, Path: from.path,
	})
	if err != nil {
		t.Errorf("%s: CompareCode(%s) error = %v", where, from.path, err)
		return
	}
	if compared.Change() != engine.CodeModified {
		t.Errorf("%s: CompareCode(%s) = %s, want MODIFIED: the two revisions delegate to different handlers",
			where, from.path, compared.Change())
	}
	assertComparedAt(t, where, "from", compared.From(), from)
	assertComparedAt(t, where, "to", compared.To(), to)

	hunks := compared.Hunks()
	if !diffShows(hunks, provider.LineRemoved, from.own) || diffShows(hunks, provider.LineRemoved, to.own) {
		t.Errorf("%s: the removed lines of %s are %+v, want the %s call and not the %s one",
			where, from.path, hunks, from.own, to.own)
	}
	if !diffShows(hunks, provider.LineAdded, to.own) || diffShows(hunks, provider.LineAdded, from.own) {
		t.Errorf("%s: the added lines of %s are %+v, want the %s call and not the %s one",
			where, from.path, hunks, to.own, from.own)
	}
}

// concurrentCompareCalls compares tracedSymbol's callees across the same pair.
//
// Each revision calls exactly one handler, so the classification is the whole
// answer: the from version's call is removed, the to version's is added, and
// neither is unchanged. A comparison answered from one graph twice reports
// nothing removed or nothing added; one answered from the wrong pair reports
// them the wrong way round.
func concurrentCompareCalls(ctx context.Context, t *testing.T, eng *engine.Engine, where string, from, to concurrentVersion) {
	compared, err := eng.CompareCalls(ctx, engine.CompareCallsRequest{
		FromContext: from.codeCtx.ID, ToContext: to.codeCtx.ID, Repository: from.codeCtx.Repository,
		Symbol: from.tracedSymbol, Direction: from.direction, Depth: 1,
	})
	if err != nil {
		t.Errorf("%s: CompareCalls(%s) error = %v", where, from.tracedSymbol, err)
		return
	}
	if compared.Presence() != engine.PresenceBoth {
		t.Errorf("%s: CompareCalls(%s) presence = %s, want BOTH: both revisions declare it",
			where, from.tracedSymbol, compared.Presence())
	}
	assertComparedAt(t, where, "from", compared.From(), from)
	assertComparedAt(t, where, "to", compared.To(), to)

	related := func(list []engine.CallRelation, name string) bool { return relates(list, from.tracedSymbol, name) }
	if from.direction == provider.Callers {
		related = func(list []engine.CallRelation, name string) bool { return relates(list, name, from.tracedSymbol) }
	}
	if !related(compared.Removed(), from.own) || related(compared.Removed(), to.own) {
		t.Errorf("%s: removed = %+v, want only the %s relation the from version makes", where, compared.Removed(), from.own)
	}
	if !related(compared.Added(), to.own) || related(compared.Added(), from.own) {
		t.Errorf("%s: added = %+v, want only the %s relation the to version makes", where, compared.Added(), to.own)
	}
	if related(compared.Unchanged(), from.own) || related(compared.Unchanged(), to.own) {
		t.Errorf("%s: unchanged = %+v, want neither handler: each revision calls only its own", where, compared.Unchanged())
	}
}

// assertComparedAt holds one half of a comparison to the version it was
// supposed to be answered in — the repository and the revision both, since a
// side answered in the right repository at the wrong revision is exactly the
// gap decision-11's "pin 不同 revision" case exists to catch.
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
	if got := members[0].Repository; got != want.codeCtx.Repository {
		t.Errorf("%s: the %s side was answered in repository %s, want %s", where, which, got, want.codeCtx.Repository)
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

// callsOf projects a traversal onto the wire shape calleesOf and callersOf
// read, which is the one the trace_calls tests already share.
func callsOf(traced engine.TraceCallsResult) []call {
	edges := traced.Graph().Edges
	calls := make([]call, 0, len(edges))
	for _, edge := range edges {
		calls = append(calls, call{Caller: edge.Caller, Callee: edge.Callee, Path: edge.Path, Line: edge.Line})
	}
	return calls
}
