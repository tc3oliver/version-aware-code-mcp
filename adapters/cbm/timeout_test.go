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
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// stubBudget is what a budget is shrunk to here: long enough that starting the
// stub is never what a test races against. See the git adapter's copy for the
// whole reason.
const stubBudget = 5 * time.Second

var traceContext = vacctx.CodeContext{
	ID: "timeout", Repository: "demo", Branch: "main",
	Revision: "9999999999999999999999999999999999999999", GraphRef: "demo-main",
}

// TestAHangingCBMIsAnOperationTimeout: a graph engine that never answers is this
// server's budget expiring, not a graph engine that is unavailable — the same
// distinction the Zoekt migration makes, at the other provider.
func TestAHangingCBMIsAnOperationTimeout(t *testing.T) {
	pidFile := hangingCBM(t)
	// Both, because they are spent in sequence: see timeoutProvider. The
	// length is stubBudget's, for the reason given there.
	p := timeoutProvider(t, stubBudget, stubBudget)
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
	p := timeoutProvider(t, time.Hour, stubBudget)
	t.Cleanup(func() { _ = p.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// After the session start has given up, so the cancellation lands on the
		// `cli` call rather than on a start that would ignore it.
		time.Sleep(stubBudget + time.Second)
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

// timeoutProvider builds the provider under test with both budgets set: the
// trace budget and the session-start one.
//
// Both, because they are sequential and the session one is deliberately outside
// the trace budget: it is built on context.WithoutCancel so a client going away
// cannot take down the session every later call depends on. Against a CBM that
// never finishes starting, that means the start budget is spent in full before
// the trace budget begins — two minutes of it, which is the right behaviour in
// production and an unusable test.
//
// They are passed as options rather than assigned to package state, and that is
// the point: [WithTraceBudget] and [WithConnectTimeout] are the exact calls an
// embedder makes, so every run of this suite exercises the embedder's path
// instead of a test-only one beside it.
func timeoutProvider(t *testing.T, trace, connect time.Duration) *Provider {
	t.Helper()
	return New(
		&config.Config{Providers: config.Providers{CBM: config.CBM{Command: "codebase-memory-mcp"}}},
		WithTraceBudget(trace),
		WithConnectTimeout(connect),
	)
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

// The first session start is the case the two tests above cannot reach. It is
// the only part of a trace that deliberately does not run on the caller's
// context: the session it produces is shared by every later call, so a client
// that walks away must not take it down with it.
//
// That is a statement about the startup's lifetime, not about the caller's. A
// caller still has to be able to stop waiting on its own clock — otherwise the
// first request to arrive at a codebase-memory-mcp that never finishes starting
// is held for the whole connect budget, two minutes, with its own deadline long
// past. These two tests are that boundary, one for each of the caller's clocks.

func TestAHangingFirstStartDoesNotHoldTheCaller(t *testing.T) {
	// Both budgets far out of reach, so nothing but the caller's own clock can
	// end these calls — and so a return proves the wait was not the connect
	// budget quietly expiring.

	for _, testCase := range []struct {
		name string
		// stop ends the caller's context the way this case is about, and says
		// which error the call must come back with.
		caller func(t *testing.T) (context.Context, error)
	}{
		{
			name: "caller deadline expired",
			caller: func(t *testing.T) (context.Context, error) {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				t.Cleanup(cancel)
				return ctx, context.DeadlineExceeded
			},
		},
		{
			name: "caller cancelled",
			caller: func(t *testing.T) (context.Context, error) {
				ctx, cancel := context.WithCancel(context.Background())
				time.AfterFunc(2*time.Second, cancel)
				t.Cleanup(cancel)
				return ctx, context.Canceled
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			pidFile := hangingCBM(t)
			p := timeoutProvider(t, time.Hour, time.Hour)

			ctx, want := testCase.caller(t)
			started := time.Now()
			_, err := p.TraceCalls(ctx, traceContext, provider.TraceRequest{
				Symbol: "Process", Direction: provider.Callees, Depth: 3,
			})
			waited := time.Since(started)

			if !errors.Is(err, want) {
				t.Fatalf("TraceCalls behind a CBM that never finishes starting = %v, want %v", err, want)
			}
			var vErr *vacerr.Error
			if errors.As(err, &vErr) {
				t.Errorf("the caller's own clock was classified as %s", vErr.Code)
			}
			// The point of the test: it came back on the caller's clock, nowhere
			// near the hour the session start is allowed to take.
			if waited > 30*time.Second {
				t.Errorf("the call waited %s, so it was held by the session start rather than by its own context", waited)
			}

			// The start is still running, and this provider has not been
			// condemned to the `cli` mode by a caller that merely left: the next
			// trace gets the session this one paid to begin.
			p.mu.Lock()
			starting, cliOnly := p.starting != nil, p.cliOnly
			p.mu.Unlock()
			if cliOnly {
				t.Error("a caller's cancellation switched the provider to the cli mode for good")
			}
			if !starting {
				t.Error("the session start was abandoned with the caller, so the next trace pays for a cold start again")
			}

			// And Close is what ends it: the CBM that was still starting has to
			// be gone when the provider that spawned it is.
			if err := p.Close(); err != nil {
				t.Errorf("Close() = %v", err)
			}
			assertReaped(t, pidFile)
		})
	}
}

// TestOneHangingStartServesEveryWaiter: the session's lifecycle is shared, so
// arriving while it is being started is waiting for it, not starting another.
// Without that, a burst of traces against a CBM that is slow to come up spawns
// one CBM per trace — the cost the persistent session exists to avoid, paid at
// the worst possible moment.
func TestOneHangingStartServesEveryWaiter(t *testing.T) {
	pidFile := hangingCBM(t)
	p := timeoutProvider(t, time.Hour, time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var waiters sync.WaitGroup
	for range 5 {
		waiters.Add(1)
		go func() {
			defer waiters.Done()
			if _, err := p.TraceCalls(ctx, traceContext, provider.TraceRequest{
				Symbol: "Process", Direction: provider.Callees, Depth: 3,
			}); !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("TraceCalls = %v, want context.DeadlineExceeded", err)
			}
		}()
	}
	waiters.Wait()

	if pids := recordedPids(pidFile); len(pids) != 1 {
		t.Errorf("five concurrent traces started %d CBM processes, want 1", len(pids))
	}
	if err := p.Close(); err != nil {
		t.Errorf("Close() = %v", err)
	}
	assertReaped(t, pidFile)
}
