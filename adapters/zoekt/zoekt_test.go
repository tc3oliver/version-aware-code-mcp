// Package zoekt_test is the evidence for the Zoekt search adapter: that a
// search answers from the branch its context names and from no other, and that
// it fails with a code instead of a panic when the engine is not there.
//
// The two checks that do not need Zoekt are here: the response filter, whose
// point is a response no real engine should send, and the unreachable engine,
// whose condition is a port nobody is listening on. Everything that queries the
// index testdata/prepare-fixture.sh built is in zoekt_integration_test.go — a
// fake engine could not show what those are for, that this adapter and Zoekt
// agree on how a branch is selected.
package zoekt_test

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	zoektadapter "github.com/tc3oliver/version-aware-code-mcp/adapters/zoekt"
	"github.com/tc3oliver/version-aware-code-mcp/config"
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

// errorOf fails the test unless err is a *vacerr.Error, and returns it.
func errorOf(t *testing.T, err error) *vacerr.Error {
	t.Helper()
	var vErr *vacerr.Error
	if !errors.As(err, &vErr) {
		t.Fatalf("error = %v (%T), want *vacerr.Error", err, err)
	}
	return vErr
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

// TestSearchDropsAnotherWorkspaceMembersMatchEvenThoughItIsNotAStranger pins
// TASK-86 AC #2 at the line that decides it. A multi-member workspace's engine
// issues one query per member, each carrying that member's own
// [vacctx.CodeContext] ([github.com/tc3oliver/version-aware-code-mcp/engine.Engine.SearchCode]),
// and this adapter never sees the workspace a member belongs to — Search only
// ever compares a returned match's repository and branch against the one
// member it was asked about.
//
// The regression this pins is decision-11 §3's named risk: if that comparison
// were ever loosened from "is this member's own repository" to "is this a
// repository of the workspace this member belongs to", a match that reached
// the wrong member's query would stop being dropped and would be reported —
// stamped with the *asking* member's own repository and revision by the
// engine, which trusts this adapter completely. second-demo-repo is not a
// stranger to demo-multi the way "someone-elses-repo" above is: it really is a
// member of the same workspace, just not the one this query was scoped to,
// which is exactly what would make the mistake easy to introduce and hard to
// notice.
func TestSearchDropsAnotherWorkspaceMembersMatchEvenThoughItIsNotAStranger(t *testing.T) {
	const body = `{"Result":{"Files":[
		{"FileName":"handler.go","Repository":"versioned-demo-repo","Branches":["release/v1"],
		 "LineMatches":[{"Line":"ZnVuYyBMZWdhY3lIYW5kbGVyKCkgew==","LineNumber":4,"FileName":false}]},
		{"FileName":"handler.go","Repository":"second-demo-repo","Branches":["main"],
		 "LineMatches":[{"Line":"ZnVuYyBMZWdhY3lIYW5kbGVyKCkgew==","LineNumber":6,"FileName":false}]}]}}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	p := zoektadapter.New(&config.Config{Providers: config.Providers{Zoekt: config.Zoekt{URL: server.URL}}})
	// This query was scoped to versioned-demo-repo's release/v1: that is the
	// member it was asked about, and the only one its answer may cite.
	repo1 := vacctx.CodeContext{ID: "demo-multi", Repository: "versioned-demo-repo", Branch: "release/v1", Revision: "HEAD", GraphRef: "vacmcp-demo-v1"}

	got, err := p.Search(t.Context(), repo1, provider.SearchQuery{Query: "LegacyHandler"})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	// Only the match Zoekt reports as versioned-demo-repo's own survives. The
	// second-demo-repo entry is a real member of the same workspace, but not of
	// this query's own repository, and that is what has to keep it out.
	want := []provider.SearchResult{{Path: "handler.go", Line: 4, Snippet: "func LegacyHandler() {"}}
	if len(got) != len(want) || got[0] != want[0] {
		t.Errorf("Search() = %+v, want %+v: a fellow workspace member's match must not be attributed to this one", got, want)
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
