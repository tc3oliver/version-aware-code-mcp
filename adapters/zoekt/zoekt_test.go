// Package zoekt_test is the evidence for the Zoekt search adapter: that a
// search answers from the branch its context names and from no other, and that
// it fails with a code instead of a panic when the engine is not there.
//
// The searches run against a real Zoekt web server serving the index
// testdata/prepare-fixture.sh built from the versioned demo repository. A fake
// engine could not show what these tests are for — that this adapter and Zoekt
// agree on how a branch is selected.
package zoekt_test

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
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

// The adapter satisfies the contract it is written against.
var _ provider.SearchProvider = (*zoektadapter.Provider)(nil)

// The contexts the fixture configures, one per release branch of the demo
// repository. Process() calls LegacyHandler on the first and NewHandler on the
// second, so a symbol of one is absent from the other.
const (
	v1 = "demo-v1"
	v2 = "demo-v2"
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

	return zoektadapter.New(cfg), cfg.Contexts
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

// errorOf fails the test unless err is a *vacerr.Error, and returns it.
func errorOf(t *testing.T, err error) *vacerr.Error {
	t.Helper()
	var vErr *vacerr.Error
	if !errors.As(err, &vErr) {
		t.Fatalf("error = %v (%T), want *vacerr.Error", err, err)
	}
	return vErr
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

// TestSearchDropsMatchesOutsideTheContext is the last line of defence, and the
// only test here that does not use Zoekt: the point is a response no real
// engine should send. Whatever reaches the adapter, a match is only returned if
// it says it is in this repository and on this branch, so version isolation
// does not rest on the query having been built correctly.
func TestSearchDropsMatchesOutsideTheContext(t *testing.T) {
	const body = `{"Result":{"Files":[
		{"FileName":"handler.go","Repository":"versioned-demo-repo","Branches":["release/v2"],
		 "LineMatches":[{"Line":"ZnVuYyBOZXdIYW5kbGVyKCkgew==","LineNumber":4,"FileName":false}]},
		{"FileName":"other.go","Repository":"someone-elses-repo","Branches":["release/v1"],
		 "LineMatches":[{"Line":"ZnVuYyBOZXdIYW5kbGVyKCkgew==","LineNumber":9,"FileName":false}]},
		{"FileName":"processor.go","Repository":"versioned-demo-repo","Branches":["main","release/v1"],
		 "LineMatches":[{"Line":"ZnVuYyBQcm9jZXNzKCkgew==","LineNumber":4,"FileName":false},
		                {"Line":"cHJvY2Vzc29yLmdv","LineNumber":0,"FileName":true}]}]}}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	p := zoektadapter.New(&config.Config{Providers: config.Providers{Zoekt: config.Zoekt{URL: server.URL}}})
	codeCtx := vacctx.CodeContext{ID: v1, Repository: "versioned-demo-repo", Branch: "release/v1", Revision: "HEAD", GraphRef: "vacmcp-demo-v1"}

	got, err := p.Search(t.Context(), codeCtx, provider.SearchQuery{Query: "NewHandler"})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	// Only the content match in this repository on this branch survives: the
	// other branch, the other repository, and the match on a file name that
	// has no line to cite are all left out.
	want := []provider.SearchResult{{Path: "processor.go", Line: 4, Snippet: "func Process() {"}}
	if len(got) != len(want) || got[0] != want[0] {
		t.Errorf("Search() = %+v, want %+v", got, want)
	}
}

// TestSearchProviderUnavailable is AC #4. The condition is real rather than
// simulated: the address is one nobody is listening on, so the adapter meets
// the same refused connection an operator does when Zoekt is not running.
func TestSearchProviderUnavailable(t *testing.T) {
	p := zoektadapter.New(&config.Config{
		Providers: config.Providers{Zoekt: config.Zoekt{URL: "http://" + closedAddress(t)}},
	})
	codeCtx := vacctx.CodeContext{ID: v1, Repository: "versioned-demo-repo", Branch: "release/v1", Revision: "HEAD", GraphRef: "vacmcp-demo-v1"}

	got, err := p.Search(t.Context(), codeCtx, provider.SearchQuery{Query: "NewHandler"})
	if err == nil {
		t.Fatalf("Search() = %+v, want SEARCH_PROVIDER_UNAVAILABLE", got)
	}
	if got != nil {
		t.Errorf("Search() = %+v, want no results alongside the error", got)
	}

	vErr := errorOf(t, err)
	if vErr.Code != vacerr.SearchProviderUnavailable {
		t.Fatalf("code = %q, want SEARCH_PROVIDER_UNAVAILABLE (error: %v)", vErr.Code, err)
	}

	wire, marshalErr := vErr.MarshalJSON()
	if marshalErr != nil {
		t.Fatalf("MarshalJSON() error = %v", marshalErr)
	}
	if !strings.HasPrefix(string(wire), `{"error":{"code":"SEARCH_PROVIDER_UNAVAILABLE"`) {
		t.Errorf("wire = %s, want a SEARCH_PROVIDER_UNAVAILABLE error envelope", wire)
	}
	t.Logf("SEARCH_PROVIDER_UNAVAILABLE wire = %s", wire)
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

// closedAddress returns a loopback address that was free and is now unbound: a
// port to start a server on, or one guaranteed to refuse a connection.
func closedAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("releasing %s: %v", address, err)
	}
	return address
}
