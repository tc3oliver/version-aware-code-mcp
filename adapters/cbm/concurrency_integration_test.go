//go:build integration

package cbm_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
)

// traceDeadline bounds the whole concurrent run. It is not a performance
// assertion — the traces below take milliseconds once CBM is up — but the only
// way a deadlock can fail this test instead of hanging the package until the
// go test timeout fires ten minutes later with no idea which test it was.
const traceDeadline = 2 * time.Minute

// TestConcurrentTracesStayInTheirOwnGraph is the evidence for the shared CBM
// session.
//
// One Provider now holds one codebase-memory-mcp process, and every trace_calls
// in the server goes through it: an agent asking about two versions at once,
// or two agents on one shared server, are concurrent calls on the same
// connection. That is a new failure surface, and it has three shapes worth
// naming — a crash (CBM before 0.10.1 terminated on seven or more parallel
// requests, which is why doc-1 §9 pins 0.10.1), a deadlock, and the one this
// project cannot ship: two versions asking at once and one of them getting the
// other's graph back.
//
// So the traces are run interleaved rather than in sequence, from one shared
// Provider, and each one is checked against its own version's handler. Process
// calls LegacyHandler on release/v1 and NewHandler on release/v2; a single
// answer carrying the wrong one is the contamination, whether it came from a
// response delivered to the wrong caller or a project leaking between calls.
func TestConcurrentTracesStayInTheirOwnGraph(t *testing.T) {
	cfg := fixture(t)
	p := newProvider(t, cfg)

	// Resolved up front: contextNamed fails the test, and a test may not be
	// failed from a goroutine it did not start.
	versions := []struct {
		codeCtx vacctx.CodeContext
		want    string
		absent  string
	}{
		{contextNamed(t, cfg, "demo-v1"), "LegacyHandler", "NewHandler"},
		{contextNamed(t, cfg, "demo-v2"), "NewHandler", "LegacyHandler"},
	}

	ctx, cancel := context.WithTimeout(t.Context(), traceDeadline)
	defer cancel()

	// Sixteen traces, three CBM calls each: comfortably past the seven parallel
	// requests that used to be fatal, and enough interleaving that a response
	// routed to the wrong caller has somewhere to go.
	const rounds = 8

	var wg sync.WaitGroup
	for round := range rounds {
		for _, version := range versions {
			wg.Add(1)
			go func() {
				defer wg.Done()

				where := fmt.Sprintf("round %d, context %s (graph_ref %s)", round, version.codeCtx.ID, version.codeCtx.GraphRef)
				graph, err := p.TraceCalls(ctx, version.codeCtx, provider.TraceRequest{
					Symbol:    "Process",
					Direction: provider.Callees,
					Depth:     3,
				})
				if err != nil {
					t.Errorf("%s: TraceCalls(Process) error = %v", where, err)
					return
				}

				called := callees(graph, "Process")
				if !contains(called, version.want) {
					t.Errorf("%s: Process calls %v, want it to include %s", where, called, version.want)
				}
				if contains(called, version.absent) {
					t.Errorf("%s: Process calls %v, which is the other version's handler: concurrent traces are crossing graphs", where, called)
				}
			}()
		}
	}
	wg.Wait()
}
