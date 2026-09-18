//go:build unix

package resolver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// Resolution is the step every tool takes, so a git that never returns here
// hangs all seven rather than one. It is also the step most likely to be blamed
// for something it did not do: without a budget, a wedged rev-parse eventually
// surfaced as REVISION_NOT_FOUND or REPOSITORY_NOT_FOUND — a claim about the
// caller's context when the truth was that git stopped answering.

func TestResolveBudgetProducesOperationTimeout(t *testing.T) {
	hangingGit(t)
	shrinkResolveBudget(t, 500*time.Millisecond)

	_, err := resolverFor(t).Resolve(context.Background(), "app")

	var vErr *vacerr.Error
	if !errors.As(err, &vErr) {
		t.Fatalf("Resolve = %v, want a *vacerr.Error", err)
	}
	if vErr.Code != vacerr.OperationTimeout {
		t.Fatalf("a git that never answers = %s, want %s", vErr.Code, vacerr.OperationTimeout)
	}
	for key, want := range map[string]any{"provider": "git", "operation": "resolve"} {
		if vErr.Details[key] != want {
			t.Errorf("details[%s] = %v, want %v", key, vErr.Details[key], want)
		}
	}
}

func TestResolveReportsTheCallersOwnClock(t *testing.T) {
	t.Run("caller cancelled", func(t *testing.T) {
		hangingGit(t)
		shrinkResolveBudget(t, time.Hour)

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(300 * time.Millisecond)
			cancel()
		}()

		_, err := resolverFor(t).Resolve(ctx, "app")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Resolve = %v, want context.Canceled", err)
		}
		assertUnclassified(t, err)
	})

	t.Run("caller deadline expired", func(t *testing.T) {
		hangingGit(t)
		shrinkResolveBudget(t, time.Hour)

		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()

		_, err := resolverFor(t).Resolve(ctx, "app")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Resolve = %v, want context.DeadlineExceeded", err)
		}
		assertUnclassified(t, err)
	})
}

// TestAnUnconfiguredContextIsStillNotFound: resolution's own refusals happen
// before any git runs and are unaffected by the budget.
func TestAnUnconfiguredContextIsStillNotFound(t *testing.T) {
	_, err := resolverFor(t).Resolve(context.Background(), "no-such-context")

	var vErr *vacerr.Error
	if !errors.As(err, &vErr) || vErr.Code != vacerr.ContextNotFound {
		t.Fatalf("Resolve of an unconfigured context = %v, want %s", err, vacerr.ContextNotFound)
	}
}

func resolverFor(t *testing.T) *Resolver {
	t.Helper()

	return New(&config.Config{
		Repositories: map[string]config.Repository{"demo": {Path: t.TempDir()}},
		Contexts: map[string]vacctx.Workspace{
			"app": {ID: "app", Members: []vacctx.CodeContext{{
				Repository: "demo", Branch: "main",
				Revision: "1212121212121212121212121212121212121212",
				GraphRef: "demo-main",
			}}},
		},
	})
}

func hangingGit(t *testing.T) {
	t.Helper()

	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\necho $$ >>%q\nexec sleep 300\n", filepath.Join(dir, "git.pid"))
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700); err != nil {
		t.Fatalf("write the git stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func shrinkResolveBudget(t *testing.T, d time.Duration) {
	t.Helper()
	previous := resolveBudget
	resolveBudget = d
	t.Cleanup(func() { resolveBudget = previous })
}

func assertUnclassified(t *testing.T, err error) {
	t.Helper()

	var vErr *vacerr.Error
	if errors.As(err, &vErr) {
		t.Errorf("the caller's own context error was classified as %s, want it propagated unchanged", vErr.Code)
	}
}
