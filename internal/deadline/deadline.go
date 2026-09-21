// Package deadline puts a vacmcp-owned budget on one provider call and tells
// afterwards whose deadline it was that ended it.
//
// The whole difficulty is that a context cannot be asked that question. A
// context derived from a caller's context reports [context.DeadlineExceeded]
// when its own timer fires and when the caller's does, and reports
// [context.Canceled] when the caller gives up — so an adapter inspecting
// ctx.Err() after a failed call can see *that* time ran out but not *whose*
// time it was. Checking the parent separately is a race: between this server's
// timer firing and the parent being read, the caller's deadline can pass, and
// the call would be misattributed to the caller.
//
// So the budget carries its own cause. [With] attaches a value only this package
// can produce, and [Ended] reads it back through [context.Cause], which reports
// the reason of the *first* cancellation and settles the question at the moment
// it happened rather than when it is asked about.
package deadline

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// Provider names the external thing a budget is spent talking to. They are the
// three this server drives, and the value goes on the wire in the details of an
// OPERATION_TIMEOUT, so the set is closed rather than free text.
const (
	Git   = "git"
	CBM   = "cbm"
	Zoekt = "zoekt"
)

// exceeded is the cause [With] attaches. It is unexported and carries no
// constructor, so a cause matching it can only have come from this package —
// which is what makes reading it back a proof rather than a guess.
type exceeded struct {
	provider  string
	operation string
	budget    time.Duration
}

func (e *exceeded) Error() string {
	return fmt.Sprintf("%s %s exceeded its %s budget", e.provider, e.operation, e.budget)
}

// With returns ctx bounded by budget, and the cancel function its caller must
// defer.
//
// A budget of zero or less returns the context unchanged with a no-op cancel:
// "no budget" is a legitimate configuration, and a zero duration must not become
// a deadline that has already passed.
//
// operation is the call being made — "read", "diff", "history", "search",
// "trace", "resolve" — and is the operation reported in the details of an
// OPERATION_TIMEOUT. It names what this server was doing, never what it was
// asked about: no path, no query, no revision, no context id.
func With(ctx context.Context, budget time.Duration, provider, operation string) (context.Context, context.CancelFunc) {
	if budget <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeoutCause(ctx, budget, &exceeded{
		provider:  provider,
		operation: operation,
		budget:    budget,
	})
}

// Ended reports how a context returned by [With] ended, or nil if it has not
// ended — in which case whatever went wrong is the provider's own failure and
// the caller keeps its existing classification.
//
// The three answers are the three in OPERATION_TIMEOUT's contract:
//
//   - this server's budget expired: a *[vacerr.Error] with
//     [vacerr.OperationTimeout], naming the operation, the provider and the
//     budget.
//   - the caller cancelled: [context.Canceled], unwrapped and unclassified.
//   - the caller's own deadline expired: [context.DeadlineExceeded], likewise.
//
// A caller that cancelled with a cause of its own gets that cause back, which is
// the same rule: the reason the caller's context ended is the caller's to state.
func Ended(ctx context.Context) error {
	if ctx.Err() == nil {
		return nil
	}

	cause := context.Cause(ctx)
	var ours *exceeded
	if errors.As(cause, &ours) {
		return vacerr.New(
			vacerr.OperationTimeout,
			fmt.Sprintf("%s %s did not finish within %s, so vacmcp stopped waiting for it",
				ours.provider, ours.operation, ours.budget),
			map[string]any{
				"operation":  ours.operation,
				"provider":   ours.provider,
				"timeout_ms": ours.budget.Milliseconds(),
			},
		)
	}
	return cause
}

// Override returns how ctx ended if it ended, and err otherwise.
//
// It is [Ended] at the call sites that need it most: the ones that answer a
// failed operation by running *another* external command to find out why it
// failed. `git rev-parse` failing is classified by asking git for the git
// directory; `git show` failing is classified by asking git for the tree; an
// empty diff is classified by asking git whether the path was ever there. Every
// one of those diagnostics can itself be the thing that runs out of time, and
// when it does, the classification it produced describes a command that never
// answered — a repository reported as missing because the question about it was
// never returned.
//
// So the rule is that a classification built after an external command is
// returned through here rather than directly. Whose clock ran out outranks what
// the diagnostic concluded, because the diagnostic did not conclude anything.
func Override(ctx context.Context, err error) error {
	if ended := Ended(ctx); ended != nil {
		return ended
	}
	return err
}

// Positive returns d, and panics unless it is greater than zero. option is the
// exported option's name, so the panic says which call was wrong.
//
// It is what every budget option runs its argument through, and the reason it
// panics rather than returning an error is the shape of the mistake it catches.
// The argument comes from an embedder's own source — very nearly always a
// literal — so a non-positive one is a bug in that source, not a condition the
// running server can find itself in. The constructors it guards return no error
// and must keep not returning one, because every existing call site depends on
// that; recording the fault and failing at the first call instead would report
// a construction-time mistake at a moment that could be hours later, in a stack
// that has nothing to do with it.
//
// Zero is rejected along with negatives, and deliberately so: [With] treats a
// budget of zero or less as no budget at all, so accepting one here would make
// WithReadBudget(0) the documented way to run an unbounded git — the exact
// thing these budgets exist to prevent, spelled the way this kind of API is
// most often misread.
func Positive(option string, d time.Duration) time.Duration {
	if d <= 0 {
		panic(fmt.Sprintf("%s(%s): a budget must be greater than zero; zero does not mean unlimited", option, d))
	}
	return d
}
