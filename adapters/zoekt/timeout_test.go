package zoekt

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// The migration, and what it is a migration away from.
//
// Before this, the budget was an http.Client timeout, which produces an error
// indistinguishable from a connection failure — so a Zoekt that accepted the
// request and went quiet was reported as a Zoekt that was unavailable. Both are
// still failures; they are not the same failure, and only one of them means the
// operator should go and look at the search engine.

var searchContext = vacctx.CodeContext{
	ID: "timeout", Repository: "demo", Branch: "main",
	Revision: "8888888888888888888888888888888888888888", GraphRef: "demo-main",
}

// TestAHangingZoektIsAnOperationTimeout is the after side of the migration: the
// server accepts and never answers, and that is now this server's budget saying
// so rather than a claim about reachability.
func TestAHangingZoektIsAnOperationTimeout(t *testing.T) {
	p := providerFor(t, hangingServer(t))
	shrinkBudget(t, 300*time.Millisecond)

	_, err := p.Search(context.Background(), searchContext, provider.SearchQuery{Query: "Process"})

	var vErr *vacerr.Error
	if !errors.As(err, &vErr) {
		t.Fatalf("Search = %v, want a *vacerr.Error", err)
	}
	if vErr.Code != vacerr.OperationTimeout {
		t.Fatalf("a Zoekt that never answered = %s, want %s (this is the migration)", vErr.Code, vacerr.OperationTimeout)
	}
	if vErr.Code == vacerr.SearchProviderUnavailable {
		t.Errorf("still reporting a hanging server as an unavailable one")
	}
	for key, want := range map[string]any{"provider": "zoekt", "operation": "search"} {
		if vErr.Details[key] != want {
			t.Errorf("details[%s] = %v, want %v", key, vErr.Details[key], want)
		}
	}
}

// TestAnUnreachableZoektIsStillUnavailable is the half that must NOT change. A
// server that cannot be reached at all is what SEARCH_PROVIDER_UNAVAILABLE has
// always meant, and it still means it.
func TestAnUnreachableZoektIsStillUnavailable(t *testing.T) {
	// A closed listener: the connection is refused immediately, well inside any
	// budget, so what comes back is about reachability and nothing else.
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()

	p := providerFor(t, url)

	_, err := p.Search(context.Background(), searchContext, provider.SearchQuery{Query: "Process"})
	var vErr *vacerr.Error
	if !errors.As(err, &vErr) {
		t.Fatalf("Search = %v, want a *vacerr.Error", err)
	}
	if vErr.Code != vacerr.SearchProviderUnavailable {
		t.Errorf("an unreachable Zoekt = %s, want %s", vErr.Code, vacerr.SearchProviderUnavailable)
	}
}

// TestZoektReportsTheCallersOwnClock: neither of the caller's two ways of ending
// a call becomes this server's code.
func TestZoektReportsTheCallersOwnClock(t *testing.T) {
	t.Run("caller cancelled", func(t *testing.T) {
		p := providerFor(t, hangingServer(t))
		shrinkBudget(t, time.Hour)

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(200 * time.Millisecond)
			cancel()
		}()

		_, err := p.Search(ctx, searchContext, provider.SearchQuery{Query: "Process"})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Search = %v, want context.Canceled", err)
		}
		assertUnclassified(t, err)
	})

	t.Run("caller deadline expired", func(t *testing.T) {
		p := providerFor(t, hangingServer(t))
		shrinkBudget(t, time.Hour)

		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()

		_, err := p.Search(ctx, searchContext, provider.SearchQuery{Query: "Process"})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Search = %v, want context.DeadlineExceeded", err)
		}
		assertUnclassified(t, err)
	})
}

// hangingServer accepts the request and never answers it, which is the failure
// mode a reachability check cannot see.
func hangingServer(t *testing.T) string {
	t.Helper()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})
	return srv.URL
}

func providerFor(t *testing.T, url string) *Provider {
	t.Helper()
	return New(&config.Config{Providers: config.Providers{Zoekt: config.Zoekt{URL: url}}})
}

func shrinkBudget(t *testing.T, d time.Duration) {
	t.Helper()
	previous := requestBudget
	requestBudget = d
	t.Cleanup(func() { requestBudget = previous })
}

func assertUnclassified(t *testing.T, err error) {
	t.Helper()

	var vErr *vacerr.Error
	if errors.As(err, &vErr) {
		t.Errorf("the caller's own context error was classified as %s, want it propagated unchanged", vErr.Code)
	}
}
