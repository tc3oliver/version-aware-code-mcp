// Package cbm_test is the evidence for the CBM graph adapter: that a trace
// answers from the graph the context names and no other, and that a symbol it
// cannot resolve to exactly one node is refused rather than guessed at.
//
// The request validation is here, because it is decided before any subprocess
// starts and a CBM on the machine could only hide a check that let something
// through. Everything that claims to exercise CBM is in
// cbm_integration_test.go, against a real codebase-memory-mcp >=0.10.1 and the
// graphs testdata/prepare-fixture.sh built from the versioned demo repository:
// there is no fake CBM anywhere here, because a mock would only prove that the
// adapter agrees with our own idea of CBM.
package cbm_test

import (
	"errors"
	"path/filepath"
	"testing"

	cbmadapter "github.com/tc3oliver/version-aware-code-mcp/adapters/cbm"
	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// The adapter satisfies the contract it is written against.
var _ provider.GraphProvider = (*cbmadapter.Provider)(nil)

// newProvider returns the adapter under test, shut down when the test ends.
//
// Each Provider holds a codebase-memory-mcp process of its own, started on
// first use and kept for the life of the Provider. That is the point — it is
// what a trace no longer has to pay for — but a test binary that leaves one
// running per test is a test binary that competes with itself for memory.
func newProvider(t *testing.T, cfg *config.Config) *cbmadapter.Provider {
	t.Helper()
	p := cbmadapter.New(cfg)
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// errorOf fails the test unless err is a *vacerr.Error, and returns it.
func errorOf(t *testing.T, err error) *vacerr.Error {
	t.Helper()
	var vErr *vacerr.Error
	if !errors.As(err, &vErr) {
		t.Fatalf("error = %v (%T), want *vacerr.Error", err, err)
	}
	return vErr
}

// TestTraceCallsRejectsIllegalRequests is the trust boundary. The symbol,
// direction and depth arrive from a tool caller, so a nonsensical one is
// answered with INVALID_ARGUMENT before any subprocess is started. A context
// with no graph_ref belongs here too: there is no graph to trace in, and
// guessing one is the cross-version answer this server refuses to give.
func TestTraceCallsRejectsIllegalRequests(t *testing.T) {
	// The command is deliberately absent: nothing here may reach CBM, so a
	// GRAPH_PROVIDER_UNAVAILABLE would mean validation let the request through.
	p := newProvider(t, &config.Config{
		Providers: config.Providers{CBM: config.CBM{Command: filepath.Join(t.TempDir(), "codebase-memory-mcp")}},
	})
	full := vacctx.CodeContext{ID: "demo-v1", Repository: "r", Branch: "b", Revision: "c", GraphRef: "vacmcp-demo-v1"}
	noGraph := full
	noGraph.GraphRef = " "

	tests := map[string]struct {
		codeCtx vacctx.CodeContext
		req     provider.TraceRequest
	}{
		"no graph_ref":      {noGraph, provider.TraceRequest{Symbol: "Process", Direction: provider.Callees, Depth: 1}},
		"empty symbol":      {full, provider.TraceRequest{Symbol: "  ", Direction: provider.Callees, Depth: 1}},
		"zero depth":        {full, provider.TraceRequest{Symbol: "Process", Direction: provider.Callees, Depth: 0}},
		"negative depth":    {full, provider.TraceRequest{Symbol: "Process", Direction: provider.Callees, Depth: -3}},
		"unknown direction": {full, provider.TraceRequest{Symbol: "Process", Direction: "sideways", Depth: 1}},
		"empty direction":   {full, provider.TraceRequest{Symbol: "Process", Depth: 1}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			graph, err := p.TraceCalls(t.Context(), tc.codeCtx, tc.req)
			if err == nil {
				t.Fatalf("TraceCalls() = %+v, want INVALID_ARGUMENT", graph)
			}
			if code := errorOf(t, err).Code; code != vacerr.InvalidArgument {
				t.Errorf("code = %q, want INVALID_ARGUMENT", code)
			}
		})
	}
}
