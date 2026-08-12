package cbm_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/provider"
)

// TestFallsBackToTheCLIWhenNoSessionCanBeStarted covers the mode that has to
// keep working when the fast one does not.
//
// The persistent session is an optimisation; answering is not. A CBM that
// cannot be run as an MCP server — an older build, a wrapper script, a sandbox
// that will not let it hold a pipe — must still be asked through
// `codebase-memory-mcp cli`, and must still answer from the graph the context
// names. That second half is the one worth a test: a fallback that answered
// from the wrong project would be a version leak arriving through the error
// path, where nobody is looking.
func TestFallsBackToTheCLIWhenNoSessionCanBeStarted(t *testing.T) {
	cfg := fixture(t)

	// Everything but the command is the fixture's own configuration, contexts
	// and graph_refs included.
	degraded := *cfg
	degraded.Providers.CBM.Command = cliOnlyCBM(t, cfg.Providers.CBM.Command)
	p := newProvider(t, &degraded)

	for _, tc := range []struct{ context, want, absent string }{
		{"demo-v1", "LegacyHandler", "NewHandler"},
		{"demo-v2", "NewHandler", "LegacyHandler"},
	} {
		t.Run(tc.context, func(t *testing.T) {
			codeCtx := contextNamed(t, cfg, tc.context)
			graph, err := p.TraceCalls(t.Context(), codeCtx, provider.TraceRequest{
				Symbol:    "Process",
				Direction: provider.Callees,
				Depth:     3,
			})
			if err != nil {
				t.Fatalf("TraceCalls(%s) on the fallback error = %v", tc.context, err)
			}

			called := callees(graph, "Process")
			if !contains(called, tc.want) {
				t.Errorf("Process calls %v in %s, want it to include %s", called, codeCtx.GraphRef, tc.want)
			}
			if contains(called, tc.absent) {
				t.Errorf("Process calls %v in %s, which is the other version's handler: the fallback lost the project", called, codeCtx.GraphRef)
			}
			t.Logf("%s answered through the cli fallback: %v", codeCtx.ID, graph.Edges)
		})
	}
}

// TestCloseLeavesTheProviderUsable pins what Close means: it retires the
// session, not the Provider. Anything that closes one and keeps using it — a
// test, a long-lived host that idles CBM out — has to get an answer, not a
// dead adapter.
func TestCloseLeavesTheProviderUsable(t *testing.T) {
	cfg := fixture(t)
	p := newProvider(t, cfg)
	codeCtx := contextNamed(t, cfg, "demo-v1")
	req := provider.TraceRequest{Symbol: "Process", Direction: provider.Callees, Depth: 3}

	if _, err := p.TraceCalls(t.Context(), codeCtx, req); err != nil {
		t.Fatalf("first TraceCalls error = %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	graph, err := p.TraceCalls(t.Context(), codeCtx, req)
	if err != nil {
		t.Fatalf("TraceCalls after Close error = %v", err)
	}
	if called := callees(graph, "Process"); !contains(called, "LegacyHandler") {
		t.Errorf("Process calls %v after Close, want it to include LegacyHandler", called)
	}
}

// cliOnlyCBM returns the path of a codebase-memory-mcp that refuses to be an
// MCP server and passes everything else to the real one. It is how the
// fallback is reached on purpose rather than by breaking the installation.
func cliOnlyCBM(t *testing.T, command string) string {
	t.Helper()

	real, err := exec.LookPath(command)
	if err != nil {
		t.Skipf("codebase-memory-mcp is not runnable at %q: %v", command, err)
	}

	path := filepath.Join(t.TempDir(), "codebase-memory-mcp")
	shim := "#!/bin/sh\n" +
		"if [ \"$1\" = cli ]; then exec " + real + " \"$@\"; fi\n" +
		"echo 'this build does not serve MCP' >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(path, []byte(shim), 0o700); err != nil {
		t.Fatalf("writing the cli-only stand-in: %v", err)
	}
	return path
}
