//go:build integration

package tools

import (
	"context"
	"errors"
	"net/http/httptest"
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

// cancelBlocker is all three provider contracts, and none of them can answer:
// every one blocks until the context it was handed is cancelled. That is the
// instrument. A provider that answers would answer whatever context it was
// given, which is exactly what a dropped cancellation looks like from outside.
//
// entered is closed on the way in, so a test can cancel *after* the provider is
// known to be blocked: cancelling earlier would prove much less, because the
// call could have been refused before it ever reached this far.
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
}

func newCancelBlocker() *cancelBlocker {
	return &cancelBlocker{
		entered:  make(chan struct{}),
		observed: make(chan error, 1),
		release:  make(chan struct{}),
	}
}

func (b *cancelBlocker) block(ctx context.Context) error {
	close(b.entered)
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

func (b *cancelBlocker) TraceCalls(ctx context.Context, _ vacctx.CodeContext, _ provider.TraceRequest) (*provider.CallGraph, error) {
	return nil, b.block(ctx)
}

func (b *cancelBlocker) Read(ctx context.Context, _ vacctx.CodeContext, _ string, _, _ int) (*provider.SourceContent, error) {
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
