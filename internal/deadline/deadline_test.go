package deadline_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/tc3oliver/version-aware-code-mcp/internal/deadline"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// The three cases OPERATION_TIMEOUT's contract turns on, and the fourth that is
// none of them. They are one table because the point is that the same code
// distinguishes them — a helper that got any one right in isolation would still
// be wrong if it could not tell it from the others.

const budget = 50 * time.Millisecond

// TestEndedTellsWhoseDeadlineItWas is the whole contract.
func TestEndedTellsWhoseDeadlineItWas(t *testing.T) {
	t.Run("server budget expired", func(t *testing.T) {
		ctx, cancel := deadline.With(context.Background(), budget, deadline.Git, "read")
		defer cancel()
		<-ctx.Done()

		err := deadline.Ended(ctx)
		var vErr *vacerr.Error
		if !errors.As(err, &vErr) {
			t.Fatalf("Ended = %v (%T), want a *vacerr.Error", err, err)
		}
		if vErr.Code != vacerr.OperationTimeout {
			t.Fatalf("code = %s, want %s", vErr.Code, vacerr.OperationTimeout)
		}
		assertDetails(t, vErr, deadline.Git, "read")
	})

	// The caller stopped waiting. It knows why, so it gets its own error back
	// rather than a code describing a budget that never expired.
	t.Run("caller cancelled", func(t *testing.T) {
		parent, stop := context.WithCancel(context.Background())
		ctx, cancel := deadline.With(parent, time.Hour, deadline.CBM, "trace")
		defer cancel()

		stop()
		<-ctx.Done()

		err := deadline.Ended(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Ended = %v, want context.Canceled", err)
		}
		assertNotClassified(t, err)
	})

	// The one inspecting the error cannot get right: the context reports
	// DeadlineExceeded here exactly as it does when the budget above expires.
	t.Run("caller deadline expired", func(t *testing.T) {
		parent, stop := context.WithTimeout(context.Background(), budget)
		defer stop()
		ctx, cancel := deadline.With(parent, time.Hour, deadline.Zoekt, "search")
		defer cancel()
		<-ctx.Done()

		err := deadline.Ended(ctx)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Ended = %v, want context.DeadlineExceeded", err)
		}
		assertNotClassified(t, err)
	})

	// Nothing ended: whatever went wrong is the provider's own failure and the
	// adapter keeps the classification it already had.
	t.Run("still running", func(t *testing.T) {
		ctx, cancel := deadline.With(context.Background(), time.Hour, deadline.Git, "diff")
		defer cancel()

		if err := deadline.Ended(ctx); err != nil {
			t.Fatalf("Ended on a live context = %v, want nil so the caller keeps its own error", err)
		}
	})
}

// TestTheCallersDeadlineWinsEvenWhenItIsTheTighterOne is the race the cause
// exists for. Both deadlines are live and the caller's is shorter, so the
// caller's fires first — and the answer must be the caller's whatever this
// server's budget would have done a moment later.
func TestTheCallersDeadlineWinsEvenWhenItIsTheTighterOne(t *testing.T) {
	parent, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	ctx, cancel := deadline.With(parent, 10*time.Second, deadline.Git, "history")
	defer cancel()
	<-ctx.Done()

	if err := deadline.Ended(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Ended = %v, want the caller's context.DeadlineExceeded", err)
	} else {
		assertNotClassified(t, err)
	}
}

// TestTheServerBudgetWinsWhenItIsTheTighterOne is the same race from the other
// side: a caller with a generous deadline, a budget that expires first.
func TestTheServerBudgetWinsWhenItIsTheTighterOne(t *testing.T) {
	parent, stop := context.WithTimeout(context.Background(), time.Hour)
	defer stop()
	ctx, cancel := deadline.With(parent, budget, deadline.CBM, "trace")
	defer cancel()
	<-ctx.Done()

	var vErr *vacerr.Error
	if err := deadline.Ended(ctx); !errors.As(err, &vErr) || vErr.Code != vacerr.OperationTimeout {
		t.Fatalf("Ended = %v, want OPERATION_TIMEOUT", err)
	}
}

// TestACallersOwnCauseIsPassedThrough: the reason a caller's context ended is
// the caller's to state, so a cause it attached is not replaced by one of ours.
func TestACallersOwnCauseIsPassedThrough(t *testing.T) {
	mine := errors.New("the embedder gave up")
	parent, stop := context.WithCancelCause(context.Background())
	ctx, cancel := deadline.With(parent, time.Hour, deadline.Zoekt, "search")
	defer cancel()

	stop(mine)
	<-ctx.Done()

	if err := deadline.Ended(ctx); !errors.Is(err, mine) {
		t.Fatalf("Ended = %v, want the caller's own cause", err)
	}
}

// TestZeroBudgetIsNoBudget: a zero duration is "not configured", not "a deadline
// that has already passed".
func TestZeroBudgetIsNoBudget(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		ctx, cancel := deadline.With(context.Background(), d, deadline.Git, "read")
		cancel()
		if _, ok := ctx.Deadline(); ok {
			t.Errorf("With(%s) set a deadline, want the context unchanged", d)
		}
	}

	// And the caller's own cancellation still reaches through an unbounded one.
	parent, stop := context.WithCancel(context.Background())
	ctx, cancel := deadline.With(parent, 0, deadline.Git, "read")
	defer cancel()
	stop()
	<-ctx.Done()
	if err := deadline.Ended(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Ended on an unbounded context = %v, want context.Canceled", err)
	}
}

// TestOperationTimeoutSerialises checks the code reaches the wire in the shape
// every other one does, and pins the details: the three a caller needs to decide
// whether to retry, and nothing about the repository, the query or the machine.
func TestOperationTimeoutSerialises(t *testing.T) {
	ctx, cancel := deadline.With(context.Background(), 1500*time.Millisecond, deadline.Zoekt, "search")
	defer cancel()
	<-ctx.Done()

	var vErr *vacerr.Error
	if !errors.As(deadline.Ended(ctx), &vErr) {
		t.Fatal("the budget did not produce a vacerr")
	}

	raw, err := json.Marshal(vErr)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var envelope struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if envelope.Error.Code != "OPERATION_TIMEOUT" {
		t.Errorf("code = %q, want OPERATION_TIMEOUT: %s", envelope.Error.Code, raw)
	}
	if envelope.Error.Details["timeout_ms"] != float64(1500) {
		t.Errorf("timeout_ms = %v, want 1500: %s", envelope.Error.Details["timeout_ms"], raw)
	}
	if envelope.Error.Details["provider"] != deadline.Zoekt {
		t.Errorf("provider = %v, want %s: %s", envelope.Error.Details["provider"], deadline.Zoekt, raw)
	}
	if envelope.Error.Details["operation"] != "search" {
		t.Errorf("operation = %v, want search: %s", envelope.Error.Details["operation"], raw)
	}
	// Three keys and no more: details are a contract, not a place to put
	// whatever was to hand.
	if len(envelope.Error.Details) != 3 {
		t.Errorf("details = %v, want exactly operation, provider and timeout_ms", envelope.Error.Details)
	}
}

func assertDetails(t *testing.T, vErr *vacerr.Error, provider, operation string) {
	t.Helper()

	if vErr.Details["provider"] != provider {
		t.Errorf("provider = %v, want %s", vErr.Details["provider"], provider)
	}
	if vErr.Details["operation"] != operation {
		t.Errorf("operation = %v, want %s", vErr.Details["operation"], operation)
	}
	if _, ok := vErr.Details["timeout_ms"]; !ok {
		t.Errorf("details = %v, want a timeout_ms", vErr.Details)
	}
}

// assertNotClassified is the half of the contract that is about what must NOT
// happen: a caller's own deadline or cancellation must reach it as the context
// error it is, never wrapped in this server's vocabulary.
func assertNotClassified(t *testing.T, err error) {
	t.Helper()

	var vErr *vacerr.Error
	if errors.As(err, &vErr) {
		t.Errorf("the caller's own context error was classified as %s, want it propagated unchanged", vErr.Code)
	}
}
