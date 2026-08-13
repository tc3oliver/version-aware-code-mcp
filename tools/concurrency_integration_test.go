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
		codeCtx, ok := cfg.Contexts[version.id]
		if !ok {
			t.Fatalf("the fixture configures no context %q", version.id)
		}
		versions = append(versions, concurrentVersion{codeCtx: codeCtx, own: version.own, other: version.other})
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
	if got := read.Context().Revision; got != version.codeCtx.Revision {
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
