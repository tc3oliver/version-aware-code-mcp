package engine_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
)

// How long a call gets to reach the provider, and then how long it gets to come
// back once its context is cancelled. It is not a performance assertion — a
// propagated cancellation returns in microseconds — but the only way an engine
// that dropped the caller's context can fail this test in seconds instead of
// hanging the package until go test's own timeout fires with no idea which call
// it was waiting on.
const cancelDeadline = 10 * time.Second

// blockingProvider is all three provider contracts, and every one of them waits
// for the context it was handed to be cancelled before returning. That is the
// whole instrument: it cannot answer, so the only way a call through it ever
// returns is the caller's cancellation reaching this far down.
//
// entered is closed on the way in, so the test can cancel *after* the provider
// is known to be blocked. Cancelling before the call would prove much less: the
// engine's own resolve step could have refused it, and the provider would never
// have been reached at all.
type blockingProvider struct{ entered chan struct{} }

func (b *blockingProvider) block(ctx context.Context) error {
	close(b.entered)
	<-ctx.Done()
	return ctx.Err()
}

func (b *blockingProvider) Search(ctx context.Context, _ vacctx.CodeContext, _ provider.SearchQuery) ([]provider.SearchResult, error) {
	return nil, b.block(ctx)
}

func (b *blockingProvider) TraceCalls(ctx context.Context, _ vacctx.CodeContext, _ provider.TraceRequest) (*provider.CallGraph, error) {
	return nil, b.block(ctx)
}

func (b *blockingProvider) Read(ctx context.Context, _ vacctx.CodeContext, _ string, _, _ int) (*provider.SourceContent, error) {
	return nil, b.block(ctx)
}

// A [context.Context] handed to an engine method reaches the provider that
// method calls, so cancelling it stops work that is already running.
//
// This is not a property the type system gives: every provider method takes a
// ctx, but nothing stops [engine.Engine] from passing [context.Background] down
// instead of the caller's, and the ordinary tests would not notice — a provider
// that answers immediately answers whatever context it was given. So the
// provider here refuses to answer at all, and the call is required to come back
// anyway once the caller says stop. An engine that dropped the context would
// leave the goroutine below blocked forever.
func TestCancellationReachesTheProvider(t *testing.T) {
	codeCtx := vacctx.CodeContext{
		ID: "demo@v1", Repository: "demo", Branch: "v1",
		Revision: "1111111111111111111111111111111111111111", GraphRef: "demo-v1",
	}

	// ListContexts is absent on purpose: it calls no provider, so it has no
	// cancellation to propagate and a test of it here would assert nothing.
	calls := map[string]func(context.Context, *engine.Engine) error{
		"SearchCode": func(ctx context.Context, eng *engine.Engine) error {
			_, err := eng.SearchCode(ctx, engine.SearchCodeRequest{Context: codeCtx.ID, Query: "Process"})
			return err
		},
		"TraceCalls": func(ctx context.Context, eng *engine.Engine) error {
			_, err := eng.TraceCalls(ctx, engine.TraceCallsRequest{
				Context: codeCtx.ID, Symbol: "Process", Direction: provider.Callees, Depth: 2,
			})
			return err
		},
		"GetCode": func(ctx context.Context, eng *engine.Engine) error {
			_, err := eng.GetCode(ctx, engine.GetCodeRequest{
				Context: codeCtx.ID, Path: "processor.go", StartLine: 1, EndLine: 3,
			})
			return err
		},
	}

	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			blocked := &blockingProvider{entered: make(chan struct{})}
			eng := engine.New(mapContexts{codeCtx.ID: codeCtx}, blocked, blocked, blocked)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			done := make(chan error, 1)
			go func() { done <- call(ctx, eng) }()

			select {
			case <-blocked.entered:
			case err := <-done:
				t.Fatalf("%s returned %v without reaching the provider, so there is no propagation to test", name, err)
			case <-time.After(cancelDeadline):
				t.Fatalf("%s did not reach the provider within %s", name, cancelDeadline)
			}

			// The provider is blocked inside the call at this point, which makes
			// what happens next the propagation itself and not a request that was
			// refused before it started.
			cancel()

			select {
			case err := <-done:
				// The provider's own ctx.Err(), returned by the engine unchanged:
				// the cancellation is reported as the reason rather than being
				// swallowed and reshaped into a failure of the query.
				if !errors.Is(err, context.Canceled) {
					t.Errorf("%s returned %v, want the cancellation the provider observed", name, err)
				}
			case <-time.After(cancelDeadline):
				t.Fatalf("%s did not return within %s of its context being cancelled: the caller's context does not reach the provider", name, cancelDeadline)
			}
		})
	}
}
