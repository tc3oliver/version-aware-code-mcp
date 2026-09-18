//go:build unix

package cbm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

var traceContext = vacctx.CodeContext{
	ID: "timeout", Repository: "demo", Branch: "main",
	Revision: "9999999999999999999999999999999999999999", GraphRef: "demo-main",
}

// TestAHangingCBMIsAnOperationTimeout: a graph engine that never answers is this
// server's budget expiring, not a graph engine that is unavailable — the same
// distinction the Zoekt migration makes, at the other provider.
func TestAHangingCBMIsAnOperationTimeout(t *testing.T) {
	pidFile := hangingCBM(t)
	shrinkTraceBudget(t, 500*time.Millisecond, 500*time.Millisecond)

	p := New(&config.Config{Providers: config.Providers{CBM: config.CBM{Command: "codebase-memory-mcp"}}})
	t.Cleanup(func() { _ = p.Close() })

	_, err := p.TraceCalls(context.Background(), traceContext, provider.TraceRequest{
		Symbol: "Process", Direction: provider.Callees, Depth: 3,
	})

	var vErr *vacerr.Error
	if !errors.As(err, &vErr) || vErr.Code != vacerr.OperationTimeout {
		t.Fatalf("TraceCalls against a CBM that never answers = %v, want %s", err, vacerr.OperationTimeout)
	}
	for key, want := range map[string]any{"provider": "cbm", "operation": "trace"} {
		if vErr.Details[key] != want {
			t.Errorf("details[%s] = %v, want %v", key, vErr.Details[key], want)
		}
	}
	assertReaped(t, pidFile)
}

// TestCBMReportsTheCallersOwnClock: the caller's cancellation stays the caller's.
func TestCBMReportsTheCallersOwnClock(t *testing.T) {
	pidFile := hangingCBM(t)
	// A short session budget so the start gives up quickly and the adapter falls
	// back to `cli`, which is the path a caller's cancellation can reach: the
	// session start is built on context.WithoutCancel on purpose, so a client
	// going away cannot take down the session every later call depends on. The
	// trace budget is an hour, so what ends this call can only be the caller.
	shrinkTraceBudget(t, time.Hour, 300*time.Millisecond)

	p := New(&config.Config{Providers: config.Providers{CBM: config.CBM{Command: "codebase-memory-mcp"}}})
	t.Cleanup(func() { _ = p.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// After the session start has given up, so the cancellation lands on the
		// `cli` call rather than on a start that would ignore it.
		time.Sleep(900 * time.Millisecond)
		cancel()
	}()

	_, err := p.TraceCalls(ctx, traceContext, provider.TraceRequest{
		Symbol: "Process", Direction: provider.Callees, Depth: 3,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("TraceCalls = %v, want context.Canceled", err)
	}
	var vErr *vacerr.Error
	if errors.As(err, &vErr) {
		t.Errorf("the caller's cancellation was classified as %s", vErr.Code)
	}
	assertReaped(t, pidFile)
}

// hangingCBM puts a codebase-memory-mcp on PATH that records its pid and parks,
// whichever way the adapter calls it — as a session over STDIO or as `cli`.
func hangingCBM(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "cbm.pid")
	script := fmt.Sprintf("#!/bin/sh\necho $$ >>%q\nexec sleep 300\n", pidFile)
	if err := os.WriteFile(filepath.Join(dir, "codebase-memory-mcp"), []byte(script), 0o700); err != nil {
		t.Fatalf("write the CBM stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return pidFile
}

// shrinkTraceBudget stands the trace budget at d, and the session-start budget
// with it.
//
// Both, because they are sequential and the session one is deliberately outside
// the trace budget: it is built on context.WithoutCancel so a client going away
// cannot take down the session every later call depends on. Against a CBM that
// never finishes starting, that means the start budget is spent in full before
// the trace budget begins — two minutes of it, which is the right behaviour in
// production and an unusable test.
func shrinkTraceBudget(t *testing.T, trace, connect time.Duration) {
	t.Helper()
	previousTrace, previousConnect := traceBudget, connectTimeout
	traceBudget, connectTimeout = trace, connect
	t.Cleanup(func() { traceBudget, connectTimeout = previousTrace, previousConnect })
}

func assertReaped(t *testing.T, pidFile string) {
	t.Helper()

	var pids []int
	for range 200 {
		if pids = recordedPids(pidFile); len(pids) > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(pids) == 0 {
		t.Fatalf("no CBM stub ever ran, so the call did not reach one")
	}
	for _, pid := range pids {
		alive := true
		for range 300 {
			if err := syscall.Kill(pid, 0); err != nil {
				alive = false
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if alive {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Errorf("the CBM subprocess (pid %d) was still running after the call returned", pid)
		}
	}
}

func recordedPids(pidFile string) []int {
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Fields(string(raw)) {
		if pid, err := strconv.Atoi(line); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids
}
