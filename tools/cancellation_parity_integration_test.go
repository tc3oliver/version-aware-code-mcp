//go:build integration

package tools

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/resolver"
	"github.com/tc3oliver/version-aware-code-mcp/server"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
)

// The other half of decision-8's release gate, which
// TestEngineAndMCPAnswerIdentically cannot reach: an answer that never comes.
// Everything there is compared once a call has returned, so a client that gives
// up mid-call is outside all of it — and that is the case where the two paths
// can differ without any result looking wrong, because there is no result.
//
// engine/'s TestCancellationReachesTheProvider already proves the engine alone
// propagates a cancellation. What is proved here is that asking through MCP
// does not lose that: the tool adapter, the JSON-RPC layer and the HTTP carrier
// between a client and the engine each get a chance to swallow the caller's
// stop, and the transport this server runs has no session for the protocol's
// own notifications/cancelled to arrive on.

// How long a call gets to reach the provider, and then to come back once it is
// cancelled. Not a performance assertion — a propagated cancellation returns in
// microseconds — but what turns a lost cancellation into a failed assertion
// naming the call instead of a package that hangs until go test's own timeout.
const cancelParityDeadline = 10 * time.Second

// cancelBlocker is all three provider contracts plus [provider.SourceDiffer],
// and none of them can answer: every one blocks until the context it was handed
// is cancelled. That is the instrument. A provider that answers would answer
// whatever context it was given, which is exactly what a dropped cancellation
// looks like from outside.
//
// entered is closed on the way in, so a test can cancel *after* the provider is
// known to be blocked: cancelling earlier would prove much less, because the
// call could have been refused before it ever reached this far. blocked counts
// every entry rather than only the first, which is what a comparison needs: its
// query has two sides, and a cancellation that was dropped shows up as the
// second one being reached.
//
// observed carries what the provider saw, which is the difference between a
// caller who stopped waiting and work that stopped. release is the way out for
// the second case: httptest's Close waits for its handlers, so a server-side
// call that ignored the cancellation would otherwise hang the package at
// cleanup instead of failing the assertion that noticed it.
type cancelBlocker struct {
	entered  chan struct{}
	observed chan error
	release  chan struct{}
	blocked  atomic.Int64

	// answerFirst lets the first graph query through, so a comparison's *to*
	// side is the one caught blocked: the other place a two-sided query can be
	// cancelled, and the one where a from side has already completed. Zero
	// value, so every other test here blocks on the first call as before.
	answerFirst bool
	answered    atomic.Bool
}

func newCancelBlocker() *cancelBlocker {
	return &cancelBlocker{
		entered: make(chan struct{}),
		// Room for both sides of a comparison. Only one of them should ever get
		// here, and the assertion that says so cannot be the thing that deadlocks
		// when it is about to fail.
		observed: make(chan error, 2),
		release:  make(chan struct{}),
	}
}

func (b *cancelBlocker) block(ctx context.Context) error {
	if b.blocked.Add(1) == 1 {
		close(b.entered)
	}
	select {
	case <-ctx.Done():
	case <-b.release:
	}
	b.observed <- ctx.Err()
	if err := ctx.Err(); err != nil {
		return err
	}
	// Released at cleanup with the context still live, which is the failing case:
	// the caller left and this call kept running. It still has to fail rather
	// than return nothing, because a provider that answers with neither a result
	// nor an error is not something the engine has to survive.
	return errors.New("the provider was released without ever being cancelled")
}

func (b *cancelBlocker) Search(ctx context.Context, _ vacctx.CodeContext, _ provider.SearchQuery) ([]provider.SearchResult, error) {
	return nil, b.block(ctx)
}

func (b *cancelBlocker) TraceCalls(ctx context.Context, _ vacctx.CodeContext, req provider.TraceRequest) (*provider.CallGraph, error) {
	if b.answerFirst && b.answered.CompareAndSwap(false, true) {
		// A graph with no edges is still a graph: [engine.Engine.CompareCalls]
		// reads it as "this version has the symbol", which is what puts the
		// comparison on its second side.
		return &provider.CallGraph{Symbol: req.Symbol}, nil
	}
	return nil, b.block(ctx)
}

func (b *cancelBlocker) Read(ctx context.Context, _ vacctx.CodeContext, _ string, _, _ int) (*provider.SourceContent, error) {
	return nil, b.block(ctx)
}

// Diff is the capability compare_code needs. Without it the source provider
// here is not a [provider.SourceDiffer], and compare_code would refuse with
// SOURCE_DIFF_UNAVAILABLE before reaching anything there was to cancel.
func (b *cancelBlocker) Diff(ctx context.Context, _, _ vacctx.CodeContext, _ provider.SourceDiffRequest) (*provider.SourceDiff, error) {
	return nil, b.block(ctx)
}

// cancelOutcome is everything one side of the comparison can be judged on when
// the caller gives up: what the caller was told, whether an answer arrived
// anyway, and whether the work itself stopped.
//
// The last one is the one a wire cannot show. A client that stops waiting sees
// the same thing either way; only the provider knows whether the query it was
// running was abandoned or run to completion for nobody.
type cancelOutcome struct {
	Cancelled bool // the caller was told the call was cancelled
	Answered  bool // an answer came back regardless
	Stopped   bool // the provider saw the cancellation and stopped working
}

// TestEngineAndMCPCancelIdentically is the cancellation half of the release
// gate: a client that gives up mid-call gets the same treatment whether it
// asked the engine directly or through the MCP tool.
//
// It is the same fixture the rest of the parity suite runs on, and the same two
// independently built stacks, with one substitution: the providers cannot
// answer. Context resolution stays real, so the cancellation lands where a real
// query spends its time — inside the provider, after the version was resolved.
func TestEngineAndMCPCancelIdentically(t *testing.T) {
	cfg := parityFixture(t)

	// One case per tool that reaches a provider. list_contexts is absent for the
	// reason engine/'s own cancellation test leaves it out: it calls no provider,
	// so it has no cancellation to propagate.
	cases := []struct {
		name   string
		direct func(context.Context, *engine.Engine) error
		args   map[string]any
	}{
		{
			name: "search_code",
			direct: func(ctx context.Context, eng *engine.Engine) error {
				_, err := eng.SearchCode(ctx, engine.SearchCodeRequest{Context: v1, Query: "Process"})
				return err
			},
			args: map[string]any{"context": v1, "query": "Process"},
		},
		{
			name: "trace_calls",
			direct: func(ctx context.Context, eng *engine.Engine) error {
				_, err := eng.TraceCalls(ctx, engine.TraceCallsRequest{
					Context: v1, Symbol: "Process", Direction: provider.Callees, Depth: 3,
				})
				return err
			},
			args: map[string]any{"context": v1, "symbol": "Process", "direction": "callees", "depth": 3},
		},
		{
			name: "get_code",
			direct: func(ctx context.Context, eng *engine.Engine) error {
				_, err := eng.GetCode(ctx, engine.GetCodeRequest{
					Context: v1, Path: "processor.go", StartLine: 4, EndLine: 6,
				})
				return err
			},
			args: map[string]any{"context": v1, "path": "processor.go", "start_line": 4, "end_line": 6},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			directBlocked := newCancelBlocker()
			t.Cleanup(func() { close(directBlocked.release) })
			eng := engine.New(resolver.New(cfg), directBlocked, directBlocked, directBlocked)
			direct := cancelMidCall(t, "the engine's "+testCase.name, directBlocked, func(ctx context.Context) error {
				return testCase.direct(ctx, eng)
			})

			mcpBlocked := newCancelBlocker()
			session := cancelSession(t, cfg, mcpBlocked)
			viaMCP := cancelMidCall(t, "the "+testCase.name+" tool", mcpBlocked, func(ctx context.Context) error {
				_, err := session.CallTool(ctx, &mcp.CallToolParams{Name: testCase.name, Arguments: testCase.args})
				return err
			})

			// What both sides have to be. Comparing them only to each other would
			// be satisfied by both being wrong in the same way — two paths that
			// both keep working after their caller left agree perfectly.
			want := cancelOutcome{Cancelled: true, Answered: false, Stopped: true}
			if direct != want {
				t.Errorf("cancelling the engine's %s gave %+v, want %+v", testCase.name, direct, want)
			}
			if viaMCP != direct {
				t.Errorf("cancelling %s gave %+v through the engine and %+v through MCP", testCase.name, direct, viaMCP)
			}
			t.Logf("%s: both sides answered a mid-call cancellation with %+v", testCase.name, direct)
		})
	}
}

// TestEngineAndMCPCancelComparisonsIdentically is the cancellation gate for the
// two comparison tools, and it asks one thing more than the test above.
//
// A comparison is two queries, run one after the other: compare_code hands both
// revisions to one diff, and compare_calls walks the from side's graph and then
// the to side's. So a dropped cancellation has a shape here that no
// single-context tool can show — the caller is let go while the *second* side is
// still to come, and the server goes on to run a query in a second version for
// nobody. The provider counts what it was asked, so "the comparison stopped" is
// checked rather than assumed from the caller having returned.
func TestEngineAndMCPCancelComparisonsIdentically(t *testing.T) {
	cfg := parityFixture(t)

	cases := []struct {
		name   string
		direct func(context.Context, *engine.Engine) error
		args   map[string]any
	}{
		{
			// One provider call, spanning both revisions: git is handed the two
			// contexts and asked for the difference. There is no second side to
			// reach, so what has to stop is the diff itself.
			name: "compare_code",
			direct: func(ctx context.Context, eng *engine.Engine) error {
				_, err := eng.CompareCode(ctx, engine.CompareCodeRequest{
					FromContext: v1, ToContext: v2, Path: "processor.go",
				})
				return err
			},
			args: map[string]any{"from_context": v1, "to_context": v2, "path": "processor.go"},
		},
		{
			// Two provider calls, and the from side is the one blocked here: the
			// to side must never be walked at all.
			name: "compare_calls",
			direct: func(ctx context.Context, eng *engine.Engine) error {
				_, err := eng.CompareCalls(ctx, engine.CompareCallsRequest{
					FromContext: v1, ToContext: v2, Symbol: "Process", Direction: provider.Callees, Depth: 1,
				})
				return err
			},
			args: map[string]any{
				"from_context": v1, "to_context": v2,
				"symbol": "Process", "direction": "callees", "depth": 1,
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			directBlocked := newCancelBlocker()
			t.Cleanup(func() { close(directBlocked.release) })
			eng := engine.New(resolver.New(cfg), directBlocked, directBlocked, directBlocked)
			direct := cancelMidCall(t, "the engine's "+testCase.name, directBlocked, func(ctx context.Context) error {
				return testCase.direct(ctx, eng)
			})

			mcpBlocked := newCancelBlocker()
			session := cancelSession(t, cfg, mcpBlocked)
			viaMCP := cancelMidCall(t, "the "+testCase.name+" tool", mcpBlocked, func(ctx context.Context) error {
				_, err := session.CallTool(ctx, &mcp.CallToolParams{Name: testCase.name, Arguments: testCase.args})
				return err
			})

			want := cancelOutcome{Cancelled: true, Answered: false, Stopped: true}
			if direct != want {
				t.Errorf("cancelling the engine's %s gave %+v, want %+v", testCase.name, direct, want)
			}
			if viaMCP != direct {
				t.Errorf("cancelling %s gave %+v through the engine and %+v through MCP", testCase.name, direct, viaMCP)
			}

			// The whole comparison stopped, not just the side that was blocked.
			// A second query here is the failure this test exists for: one side
			// abandoned and the other still running in a version nobody is
			// waiting on an answer from.
			assertNoSecondSide(t, "the engine's "+testCase.name, directBlocked)
			assertNoSecondSide(t, "the "+testCase.name+" tool", mcpBlocked)
			t.Logf("%s: both sides answered a mid-call cancellation with %+v", testCase.name, direct)
		})
	}
}

// TestEngineAndMCPCancelTheSecondSideIdentically is the other place a two-sided
// query can be cancelled: the from side has already answered and the to side is
// in flight.
//
// It is the case the test above cannot reach, because there the first side never
// returns. It matters for the same reason: the comparison is half-finished, and
// a cancellation dropped here leaves a query running in the second version with
// a from side already in hand — which is exactly the state that would tempt an
// implementation to finish the job anyway.
//
// compare_calls only: compare_code asks its provider once, so it has no second
// side to be caught on.
func TestEngineAndMCPCancelTheSecondSideIdentically(t *testing.T) {
	cfg := parityFixture(t)

	blocker := func(t *testing.T) *cancelBlocker {
		t.Helper()
		blocked := newCancelBlocker()
		blocked.answerFirst = true
		return blocked
	}
	call := func(ctx context.Context, eng *engine.Engine) error {
		_, err := eng.CompareCalls(ctx, engine.CompareCallsRequest{
			FromContext: v1, ToContext: v2, Symbol: "Process", Direction: provider.Callees, Depth: 1,
		})
		return err
	}
	args := map[string]any{
		"from_context": v1, "to_context": v2,
		"symbol": "Process", "direction": "callees", "depth": 1,
	}

	directBlocked := blocker(t)
	t.Cleanup(func() { close(directBlocked.release) })
	eng := engine.New(resolver.New(cfg), directBlocked, directBlocked, directBlocked)
	direct := cancelMidCall(t, "the engine's compare_calls", directBlocked, func(ctx context.Context) error {
		return call(ctx, eng)
	})

	mcpBlocked := blocker(t)
	session := cancelSession(t, cfg, mcpBlocked)
	viaMCP := cancelMidCall(t, "the compare_calls tool", mcpBlocked, func(ctx context.Context) error {
		_, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "compare_calls", Arguments: args})
		return err
	})

	want := cancelOutcome{Cancelled: true, Answered: false, Stopped: true}
	if direct != want {
		t.Errorf("cancelling the engine's compare_calls on its second side gave %+v, want %+v", direct, want)
	}
	if viaMCP != direct {
		t.Errorf("cancelling compare_calls on its second side gave %+v through the engine and %+v through MCP", direct, viaMCP)
	}

	// The instrument itself: unless the from side really was answered, this test
	// is the one above with extra steps.
	for _, side := range []struct {
		what    string
		blocked *cancelBlocker
	}{{"the engine's compare_calls", directBlocked}, {"the compare_calls tool", mcpBlocked}} {
		if !side.blocked.answered.Load() {
			t.Errorf("%s never got past its from side, so it was not the to side that was cancelled", side.what)
		}
		assertNoSecondSide(t, side.what, side.blocked)
	}
	t.Logf("compare_calls: both sides answered a cancellation of the to side with %+v", direct)
}

// assertNoSecondSide requires the provider to have been blocked exactly once.
// More than that is a comparison that went on to its second version after its
// caller had already gone.
func assertNoSecondSide(t *testing.T, what string, blocked *cancelBlocker) {
	t.Helper()
	if got := blocked.blocked.Load(); got != 1 {
		t.Errorf("%s reached the provider %d times, want 1: the cancelled comparison ran a second query", what, got)
	}
}

// cancelMidCall runs call, cancels it once the provider is known to be inside
// it, and reports what that did to the caller and to the work.
func cancelMidCall(t *testing.T, what string, blocked *cancelBlocker, call func(context.Context) error) cancelOutcome {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- call(ctx) }()

	select {
	case <-blocked.entered:
	case err := <-done:
		t.Fatalf("%s returned %v without reaching the provider, so there was nothing to cancel", what, err)
	case <-time.After(cancelParityDeadline):
		t.Fatalf("%s did not reach the provider within %s", what, cancelParityDeadline)
	}

	// The provider is blocked inside the call, which makes everything below the
	// cancellation itself rather than a request refused before it started.
	cancel()

	var outcome cancelOutcome
	select {
	case err := <-done:
		outcome.Answered = err == nil
		outcome.Cancelled = errors.Is(err, context.Canceled)
		t.Logf("%s returned %v", what, err)
	case <-time.After(cancelParityDeadline):
		t.Fatalf("%s did not return within %s of being cancelled", what, cancelParityDeadline)
	}

	select {
	case observed := <-blocked.observed:
		outcome.Stopped = errors.Is(observed, context.Canceled)
	case <-time.After(cancelParityDeadline):
		// Still running: the caller was let go but the query was not, and the
		// answer it is still producing has nowhere to go.
	}
	return outcome
}

// cancelSession is paritySession's shape — a real MCP server over HTTP with a
// client connected to it — with the fixture's providers replaced by one that
// cannot answer, so a call can be caught in flight.
//
// The handler comes from the server package rather than being assembled here:
// whether a cancellation crosses the wire is decided by that wiring, and a copy
// of it in this file would let the deployed server lose what the test proves.
func cancelSession(t *testing.T, cfg *config.Config, blocked *cancelBlocker) *mcp.ClientSession {
	t.Helper()

	srv := server.New(testVersion)
	eng := engine.New(resolver.New(cfg), blocked, blocked, blocked)
	AddSearchCode(srv, eng)
	AddTraceCalls(srv, eng)
	AddGetCode(srv, eng)
	AddCompareCode(srv, eng)
	AddCompareCalls(srv, eng)

	httpServer := httptest.NewServer(server.Handler(srv))
	t.Cleanup(httpServer.Close)
	// After the server's own cleanup, so it runs before it: Close waits for the
	// handlers still in flight, and one that ignored the cancellation is exactly
	// what this test exists to catch.
	t.Cleanup(func() { close(blocked.release) })

	client := mcp.NewClient(&mcp.Implementation{Name: "vacmcp-test", Version: testVersion}, nil)
	clientSession, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: httpServer.URL}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	return clientSession
}
