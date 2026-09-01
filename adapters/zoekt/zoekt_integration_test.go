//go:build integration

package zoekt_test

// The searches run against a real Zoekt web server serving the index
// testdata/prepare-fixture.sh built from the versioned demo repository. A fake
// engine could not show what these tests are for — that this adapter and Zoekt
// agree on how a branch is selected.

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	zoektadapter "github.com/tc3oliver/version-aware-code-mcp/adapters/zoekt"
	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/internal/demorepo"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// searchProvider returns an adapter pointed at a Zoekt web server started for
// this test over the fixture's index, together with the fixture's contexts.
func searchProvider(t *testing.T) (*zoektadapter.Provider, map[string]vacctx.CodeContext) {
	t.Helper()
	fixture := demorepo.Prepared(t)

	cfg, err := config.Load(fixture.Config)
	if err != nil {
		t.Fatalf("config.Load(%s) error = %v", fixture.Config, err)
	}
	// The fixture's URL names the port a long-running Zoekt would listen on.
	// This test brings its own server up, so it supplies the address.
	cfg.Providers.Zoekt.URL = startZoekt(t, fixture.ZoektIndex)

	// One member per single-repository fixture context, which is what this
	// adapter is handed: a search runs in one repository at one revision
	// whatever the context it came out of holds. This package's own tests only
	// ever ask about v1 and v2, so a multi-member context elsewhere in the
	// fixture (demo-multi, colliding with the first repository on purpose for
	// the engine-level multi-repository tests) has nothing here to flatten it
	// into and is skipped rather than failing the whole provider construction.
	contexts := map[string]vacctx.CodeContext{}
	for id, workspace := range cfg.Contexts {
		if len(workspace.Members) == 1 {
			contexts[id] = workspace.Members[0]
		}
	}
	return zoektadapter.New(cfg), contexts
}

// search runs one query in the named context and fails the test if it errors.
func search(t *testing.T, p *zoektadapter.Provider, contexts map[string]vacctx.CodeContext, id, query string) []provider.SearchResult {
	t.Helper()
	results, err := p.Search(t.Context(), contexts[id], provider.SearchQuery{Query: query})
	if err != nil {
		t.Fatalf("Search(%s, %q) error = %v", id, query, err)
	}
	return results
}

// TestSearchIsolatesBranches is AC #1 and AC #2 together, and doc-1's first two
// release gates: the same symbol, the same repository, the same index, and the
// only thing that differs is which context asked.
func TestSearchIsolatesBranches(t *testing.T) {
	p, contexts := searchProvider(t)

	absent := search(t, p, contexts, v1, "NewHandler")
	if len(absent) != 0 {
		t.Errorf("%s NewHandler = %+v, want no results: %s does not have that symbol", v1, absent, contexts[v1].Branch)
	}

	present := search(t, p, contexts, v2, "NewHandler")
	if len(present) == 0 {
		t.Fatalf("%s NewHandler = no results, want at least one on %s", v2, contexts[v2].Branch)
	}

	// The reverse direction too, so the isolation cannot be one branch simply
	// returning nothing for everything.
	legacy := search(t, p, contexts, v1, "LegacyHandler")
	if len(legacy) == 0 {
		t.Errorf("%s LegacyHandler = no results, want at least one on %s", v1, contexts[v1].Branch)
	}
	if leaked := search(t, p, contexts, v2, "LegacyHandler"); len(leaked) != 0 {
		t.Errorf("%s LegacyHandler = %+v, want no results", v2, leaked)
	}

	t.Logf("%s (%s) NewHandler = %d results", v1, contexts[v1].Branch, len(absent))
	for _, r := range present {
		t.Logf("%s (%s) NewHandler = %s:%d:%s", v2, contexts[v2].Branch, r.Path, r.Line, r.Snippet)
	}
	for _, r := range legacy {
		t.Logf("%s (%s) LegacyHandler = %s:%d:%s", v1, contexts[v1].Branch, r.Path, r.Line, r.Snippet)
	}
}

// TestSearchResultsAreLocatable is AC #3: every match says where it is, because
// a result the caller cannot go and check is not evidence.
func TestSearchResultsAreLocatable(t *testing.T) {
	p, contexts := searchProvider(t)

	results := search(t, p, contexts, v2, "NewHandler")
	if len(results) == 0 {
		t.Fatalf("Search(%s, NewHandler) = no results, want matches to inspect", v2)
	}
	for _, r := range results {
		if strings.TrimSpace(r.Path) == "" {
			t.Errorf("result %+v has no path", r)
		}
		if r.Line < 1 {
			t.Errorf("result %+v has line %d, want a 1-based line number", r, r.Line)
		}
		if strings.TrimSpace(r.Snippet) == "" {
			t.Errorf("result %+v has no snippet", r)
		}
	}

	// The line numbers are the file's own, not the result's position in the
	// list: NewHandler is declared on line 4 of handler.go and called on line 5
	// of processor.go on this branch.
	want := map[string]int{"handler.go": 4, "processor.go": 5}
	got := map[string]int{}
	for _, r := range results {
		if _, seen := got[r.Path]; !seen || strings.Contains(r.Snippet, "func ") || strings.Contains(r.Snippet, "return ") {
			got[r.Path] = r.Line
		}
	}
	for path, line := range want {
		if got[path] != line {
			t.Errorf("declaration/call of NewHandler in %s reported at line %d, want %d (results: %+v)", path, got[path], line, results)
		}
	}
	t.Logf("Search(%s, NewHandler) = %+v", v2, results)
}

// TestSearchStaysInsideItsBranch is the trust boundary. The query is the
// caller's and Zoekt's language can group and or; the adapter wraps the query
// in parentheses of its own to keep it under the repo: and branch: filters, so
// a query carrying a ")" it never opened would close that group early and
// search every branch of every repository from there on.
func TestSearchStaysInsideItsBranch(t *testing.T) {
	p, contexts := searchProvider(t)

	// `NewHandler) or (NewHandler` reaches Zoekt as
	// `repo:… branch:release/v1 (NewHandler) or (NewHandler)`, whose second
	// arm has no branch filter at all and matches release/v2's handler.go.
	for _, query := range []string{
		"NewHandler) or (NewHandler",
		"NewHandler) or (LegacyHandler",
		") or (NewHandler",
		"NewHandler)",
	} {
		t.Run(query, func(t *testing.T) {
			got, err := p.Search(t.Context(), contexts[v1], provider.SearchQuery{Query: query})
			if err == nil {
				t.Fatalf("Search(%s, %q) = %+v, want INVALID_ARGUMENT", v1, query, got)
			}
			if code := errorOf(t, err).Code; code != vacerr.InvalidArgument {
				t.Errorf("code = %q, want INVALID_ARGUMENT", code)
			}
			if got != nil {
				t.Errorf("Search() = %+v, want no results alongside the error", got)
			}
		})
	}

	// A query that groups and ors legitimately is still confined: on
	// release/v1 the or can reach LegacyHandler and cannot reach NewHandler.
	both := search(t, p, contexts, v1, "NewHandler or LegacyHandler")
	if len(both) == 0 {
		t.Fatalf("Search(%s, NewHandler or LegacyHandler) = no results, want LegacyHandler's", v1)
	}
	for _, r := range both {
		if strings.Contains(r.Snippet, "NewHandler") {
			t.Errorf("%s returned %s:%d:%s, which is release/v2 content", v1, r.Path, r.Line, r.Snippet)
		}
	}
	t.Logf("%s NewHandler or LegacyHandler = %+v", v1, both)
}

// TestSearchKeepsQueryFilters is doc-1 §12's other half: the caller's file:
// and lang: filters have to arrive as filters. Confining the query inside
// parentheses is what could take them away — Zoekt reads "(file:x)" as a search
// for the literal text "file:x" — so this is the check that the confinement
// costs the query nothing.
func TestSearchKeepsQueryFilters(t *testing.T) {
	p, contexts := searchProvider(t)

	plain := search(t, p, contexts, v2, "NewHandler")
	byLang := search(t, p, contexts, v2, "lang:go NewHandler")
	if len(byLang) != len(plain) {
		t.Errorf("lang:go NewHandler = %+v, want the same %d Go matches as NewHandler alone", byLang, len(plain))
	}

	byFile := search(t, p, contexts, v2, "file:processor NewHandler")
	if len(byFile) == 0 {
		t.Fatalf("file:processor NewHandler = no results, want the call in processor.go")
	}
	for _, r := range byFile {
		if !strings.Contains(r.Path, "processor") {
			t.Errorf("file:processor NewHandler returned %s:%d, want only processor.go", r.Path, r.Line)
		}
	}
	if len(byFile) >= len(plain) {
		t.Errorf("file:processor NewHandler returned %d results and NewHandler alone %d; the filter narrowed nothing", len(byFile), len(plain))
	}

	// A filter cannot widen the scope back out: the branch still decides.
	if leaked := search(t, p, contexts, v1, "lang:go NewHandler"); len(leaked) != 0 {
		t.Errorf("%s lang:go NewHandler = %+v, want no results", v1, leaked)
	}
	t.Logf("%s lang:go NewHandler = %+v", v2, byLang)
	t.Logf("%s file:processor NewHandler = %+v", v2, byFile)
}

// TestSearchRejectsUnusableQueries covers the rest of the boundary: a query
// with nothing in it, and one Zoekt itself refuses. Both are the caller's
// mistake, and neither is the engine being unavailable.
func TestSearchRejectsUnusableQueries(t *testing.T) {
	p, contexts := searchProvider(t)

	for name, query := range map[string]string{
		"empty":       "",
		"blank":       "   \t ",
		"unparseable": "[abc",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := p.Search(t.Context(), contexts[v1], provider.SearchQuery{Query: query})
			if err == nil {
				t.Fatalf("Search(%q) = %+v, want INVALID_ARGUMENT", query, got)
			}
			if code := errorOf(t, err).Code; code != vacerr.InvalidArgument {
				t.Errorf("code = %q, want INVALID_ARGUMENT (error: %v)", code, err)
			}
		})
	}
}

// multiRepoProvider is [searchProvider] for demo-multi's two real members
// instead of the single-repository contexts: versioned-demo-repo's
// release/v1 (the same repository and branch v1/v2 above are, indexed
// alongside it in the one Zoekt shard) paired with second-demo-repo, which
// testdata/gen-second-demo-repo.sh makes collide with it on purpose — same
// path, same symbol name, different content. TASK-86 AC #1 through #3 need
// exactly that collision, indexed for real, to prove a query scoped to one of
// them cannot come back stamped as the other's.
func multiRepoProvider(t *testing.T) (p *zoektadapter.Provider, repo1, repo2 vacctx.CodeContext) {
	t.Helper()
	fixture := demorepo.Prepared(t)
	cfg, err := config.Load(fixture.Config)
	if err != nil {
		t.Fatalf("config.Load(%s) error = %v", fixture.Config, err)
	}
	cfg.Providers.Zoekt.URL = startZoekt(t, fixture.ZoektIndex)

	members := cfg.Contexts[demorepo.MultiContext].Members
	for _, m := range members {
		switch m.Repository {
		case "versioned-demo-repo":
			repo1 = m
		case demorepo.Repo2:
			repo2 = m
		}
	}
	if repo1.Repository == "" || repo2.Repository == "" {
		t.Fatalf("%s members = %+v, want versioned-demo-repo and %s", demorepo.MultiContext, members, demorepo.Repo2)
	}
	return zoektadapter.New(cfg), repo1, repo2
}

// TestSearchStaysInsideItsRepositoryEvenWhenTheQueryTriesToEscape is TASK-86
// AC #1 through #3, asked of the real engine that indexes both of demo-multi's
// members in one shard. It is [TestSearchStaysInsideItsBranch] generalized
// from an escape across branches of one repository to an escape across
// repositories of one workspace: the same style of query, that adapter's own
// wrapping parentheses have to stay closed inside, would — if it worked — put
// its second arm outside the repo: filter entirely rather than merely outside
// the branch: one, reaching second-demo-repo's real "second: " content from a
// query scoped to versioned-demo-repo.
func TestSearchStaysInsideItsRepositoryEvenWhenTheQueryTriesToEscape(t *testing.T) {
	p, repo1, repo2 := multiRepoProvider(t)

	for _, query := range []string{
		"LegacyHandler) or (LegacyHandler",
		"LegacyHandler) or (second:",
		") or (LegacyHandler",
		"LegacyHandler)",
	} {
		t.Run(query, func(t *testing.T) {
			got, err := p.Search(t.Context(), repo1, provider.SearchQuery{Query: query})
			if err == nil {
				t.Fatalf("Search(%s, %q) = %+v, want INVALID_ARGUMENT: an escaping query must never reach Zoekt scoped to less than the whole workspace", repo1.Repository, query, got)
			}
			if code := errorOf(t, err).Code; code != vacerr.InvalidArgument {
				t.Errorf("code = %q, want INVALID_ARGUMENT", code)
			}
			if got != nil {
				t.Errorf("Search() = %+v, want no results alongside the error", got)
			}
		})
	}

	// A query that legitimately groups and ors is still confined to repo1: the
	// or can reach nothing repo2 has, even though repo2's LegacyHandler is
	// sitting in the same index under the same file name.
	confined, err := p.Search(t.Context(), repo1, provider.SearchQuery{Query: "LegacyHandler or LegacyHandler"})
	if err != nil {
		t.Fatalf("Search(%s, LegacyHandler or LegacyHandler) error = %v", repo1.Repository, err)
	}
	if len(confined) == 0 {
		t.Fatalf("Search(%s, LegacyHandler or LegacyHandler) = no results, want %s's own LegacyHandler", repo1.Repository, repo1.Repository)
	}
	for _, r := range confined {
		if strings.Contains(r.Snippet, "second: ") {
			t.Errorf("Search(%s, ...) returned %s:%d:%s, which is %s's content: the query reached the wrong member", repo1.Repository, r.Path, r.Line, r.Snippet, repo2.Repository)
		}
	}

	// The mirror query, scoped to repo2, so the isolation is proven both
	// directions and not one member simply answering nothing for everything.
	mirror, err := p.Search(t.Context(), repo2, provider.SearchQuery{Query: "LegacyHandler"})
	if err != nil {
		t.Fatalf("Search(%s, LegacyHandler) error = %v", repo2.Repository, err)
	}
	if len(mirror) == 0 {
		t.Fatalf("Search(%s, LegacyHandler) = no results, want %s's own LegacyHandler", repo2.Repository, repo2.Repository)
	}
	for _, r := range mirror {
		if strings.Contains(r.Snippet, "legacy: ") {
			t.Errorf("Search(%s, LegacyHandler) returned %s:%d:%s, which is %s's content: the query reached the wrong member", repo2.Repository, r.Path, r.Line, r.Snippet, repo1.Repository)
		}
	}

	t.Logf("%s LegacyHandler or LegacyHandler = %+v", repo1.Repository, confined)
	t.Logf("%s LegacyHandler = %+v", repo2.Repository, mirror)
}

// startZoekt runs a Zoekt web server over indexDir for the duration of the
// test and returns its base URL. -rpc turns on the JSON API the adapter uses;
// the HTML interface is off because nothing here reads it.
func startZoekt(t *testing.T, indexDir string) string {
	t.Helper()
	binary, err := exec.LookPath("zoekt-webserver")
	if err != nil {
		t.Skipf("zoekt-webserver is not on PATH, see CONTRIBUTING.md: %v", err)
	}

	// ponytail: the port is picked by closing a listener and handing the number
	// over, so another process could take it in between. Closing that window
	// needs Zoekt to accept an already listening socket.
	address := closedAddress(t)
	logPath := filepath.Join(t.TempDir(), "zoekt-webserver.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("creating %s: %v", logPath, err)
	}

	cmd := exec.Command(binary, "-index", indexDir, "-listen", address, "-rpc", "-html=false")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting zoekt-webserver: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = logFile.Close()
	})

	url := "http://" + address
	waitReady(t, url, logPath)
	return url
}

// waitReady blocks until the server answers its health check. On timeout the
// server's own output is the diagnosis, so it is read back and reported.
func waitReady(t *testing.T, url, logPath string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url+"/healthz", nil)
		if err != nil {
			t.Fatalf("building the health check request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			err = fmt.Errorf("http %s", resp.Status)
		}
		if time.Now().After(deadline) {
			output, _ := os.ReadFile(logPath)
			t.Fatalf("zoekt-webserver at %s never became ready: %v\n%s", url, err, output)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
