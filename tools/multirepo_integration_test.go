//go:build integration

package tools

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	cbmadapter "github.com/tc3oliver/version-aware-code-mcp/adapters/cbm"
	gitadapter "github.com/tc3oliver/version-aware-code-mcp/adapters/git"
	zoektadapter "github.com/tc3oliver/version-aware-code-mcp/adapters/zoekt"
	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/internal/demorepo"
	"github.com/tc3oliver/version-aware-code-mcp/resolver"
	"github.com/tc3oliver/version-aware-code-mcp/server"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// TASK-84: demorepo.MultiContext is the prepared fixture's multi-member
// workspace, versioned-demo-repo's release/v1 paired with the second
// repository — chosen because they collide on purpose, same path
// (handler.go), same symbol name (LegacyHandler), different content and a
// different caller. That collision is what these tests ask real Zoekt and
// real CBM to keep apart: a fake engine could only prove this package agrees
// with its own idea of a collision.
const (
	multiRepo1 = "versioned-demo-repo"
	multiRepo2 = demorepo.Repo2
)

// AC #7: a search over the whole workspace, with no repository argument,
// reaches both members and each match says which repository it came from.
func TestSearchCodeOverAMultiMemberWorkspaceReachesBothRepositories(t *testing.T) {
	cfg := fixtureConfig(t)
	session := multiRepoSession(t, cfg)

	raw, isError := callSearchCode(t, session, demorepo.MultiContext, "LegacyHandler")
	if isError {
		t.Fatalf("search_code(%s, LegacyHandler) failed: %s", demorepo.MultiContext, raw)
	}
	var out struct {
		Context listedContext `json:"context"`
		Matches []searchMatch `json:"matches"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}

	if out.Context.ID != demorepo.MultiContext || len(out.Context.Members) != 2 {
		t.Fatalf("context = %+v, want the two-member %s", out.Context, demorepo.MultiContext)
	}

	found := map[string]bool{}
	for _, match := range out.Matches {
		if match.Repository == "" || match.Revision == "" {
			t.Errorf("match %+v carries no repository or revision, want the member it was found in", match)
		}
		found[match.Repository] = true
	}
	for _, repository := range []string{multiRepo1, multiRepo2} {
		if !found[repository] {
			t.Errorf("search_code(%s, LegacyHandler) matches = %+v, want at least one from %s", demorepo.MultiContext, out.Matches, repository)
		}
	}
	t.Logf("search_code(%s, LegacyHandler) = %s", demorepo.MultiContext, raw)
}

// AC #8: the same path, read from each member in turn, is each repository's
// own content — handler.go exists in both and says something different in
// each.
func TestGetCodeOverAMultiMemberWorkspaceReadsEachRepositorysOwnContent(t *testing.T) {
	cfg := fixtureConfig(t)
	session := multiRepoSession(t, cfg)

	// End lines are each repository's own handler.go line count: get_code
	// refuses a range past the end of the file rather than clamping it, so a
	// number wide enough for one repository's file is out of range for the
	// other's.
	tests := map[string]struct {
		want, absent string
		endLine      int
	}{
		multiRepo1: {"legacy: ", "second: ", 6},
		multiRepo2: {"second: ", "legacy: ", 9},
	}
	for repository, tc := range tests {
		t.Run(repository, func(t *testing.T) {
			res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
				Name: "get_code",
				Arguments: map[string]any{
					"context": demorepo.MultiContext, "repository": repository,
					"path": "handler.go", "start_line": 1, "end_line": tc.endLine,
				},
			})
			if err != nil {
				t.Fatalf("tools/call get_code: %v", err)
			}
			text, ok := res.Content[0].(*mcp.TextContent)
			if !ok {
				t.Fatalf("result content = %#v, want text", res.Content[0])
			}
			if res.IsError {
				t.Fatalf("get_code(%s, repository=%s, handler.go) failed: %s", demorepo.MultiContext, repository, text.Text)
			}

			var out struct {
				Context listedContext `json:"context"`
				Content string        `json:"content"`
			}
			if err := json.Unmarshal([]byte(text.Text), &out); err != nil {
				t.Fatalf("decode %s: %v", text.Text, err)
			}
			if out.Context.Repository != repository {
				t.Errorf("context.repository = %q, want %q: %s", out.Context.Repository, repository, text.Text)
			}
			if !strings.Contains(out.Content, tc.want) {
				t.Errorf("get_code(%s, repository=%s, handler.go) = %q, want it to contain %q", demorepo.MultiContext, repository, out.Content, tc.want)
			}
			if strings.Contains(out.Content, tc.absent) {
				t.Errorf("get_code(%s, repository=%s, handler.go) = %q, want no trace of the other repository's %q", demorepo.MultiContext, repository, out.Content, tc.absent)
			}
			t.Logf("get_code(%s, repository=%s, handler.go) = %s", demorepo.MultiContext, repository, text.Text)
		})
	}

	// The argument is required, not merely useful: with it left out there are
	// two handler.go files and nothing else in the request says which one was
	// meant, so the whole read is refused rather than answered from whichever
	// member happened to be first.
	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "get_code",
		Arguments: map[string]any{
			"context": demorepo.MultiContext, "path": "handler.go", "start_line": 1, "end_line": 20,
		},
	})
	if err != nil {
		t.Fatalf("tools/call get_code: %v", err)
	}
	if !res.IsError {
		t.Fatalf("get_code(%s, handler.go) with no repository succeeded, want it refused: %v", demorepo.MultiContext, res.Content)
	}
}

// AC #9: the same symbol name, LegacyHandler, is declared in both members.
// Tracing its callers is answered from one member's own graph and never the
// other's — versioned-demo-repo's Process calls a LegacyHandler of its own,
// and the second repository's Invoke calls a different function that merely
// shares the name.
func TestTraceCallsOverAMultiMemberWorkspaceWalksOneRepositorysOwnGraph(t *testing.T) {
	cfg := traceFixture(t)
	session := multiRepoTraceSession(t, cfg)

	tests := map[string]struct{ wantCaller, absentCaller string }{
		multiRepo1: {"Process", "Invoke"},
		multiRepo2: {"Invoke", "Process"},
	}
	for repository, tc := range tests {
		t.Run(repository, func(t *testing.T) {
			res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
				Name: "trace_calls",
				Arguments: map[string]any{
					"context": demorepo.MultiContext, "repository": repository,
					"symbol": "LegacyHandler", "direction": "callers", "depth": 2,
				},
			})
			if err != nil {
				t.Fatalf("tools/call trace_calls: %v", err)
			}
			text, ok := res.Content[0].(*mcp.TextContent)
			if !ok {
				t.Fatalf("result content = %#v, want text", res.Content[0])
			}
			if res.IsError {
				t.Fatalf("trace_calls(%s, repository=%s, LegacyHandler) failed: %s", demorepo.MultiContext, repository, text.Text)
			}

			var out traceCallsOutputWire
			if err := json.Unmarshal([]byte(text.Text), &out); err != nil {
				t.Fatalf("decode %s: %v", text.Text, err)
			}
			if out.Context.Repository != repository {
				t.Errorf("context.repository = %q, want %q: %s", out.Context.Repository, repository, text.Text)
			}

			callers := callersOf(out.Calls, "LegacyHandler")
			if !contains(callers, tc.wantCaller) {
				t.Errorf("LegacyHandler's callers in %s = %v, want it to include %s", repository, callers, tc.wantCaller)
			}
			if contains(callers, tc.absentCaller) {
				t.Errorf("LegacyHandler's callers in %s = %v, which includes %s from the other repository: the trace crossed graphs", repository, callers, tc.absentCaller)
			}
			t.Logf("trace_calls(%s, repository=%s, LegacyHandler, callers) = %s", demorepo.MultiContext, repository, text.Text)
		})
	}

	// A call graph is one repository's own; with the argument left out there is
	// no walk at all, not a walk of whichever member came first.
	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "trace_calls",
		Arguments: map[string]any{
			"context": demorepo.MultiContext, "symbol": "LegacyHandler", "direction": "callers", "depth": 2,
		},
	})
	if err != nil {
		t.Fatalf("tools/call trace_calls: %v", err)
	}
	if !res.IsError {
		t.Fatalf("trace_calls(%s, LegacyHandler) with no repository succeeded, want it refused: %v", demorepo.MultiContext, res.Content)
	}
	var body traceCallsErrorWire
	text := res.Content[0].(*mcp.TextContent).Text
	if err := json.Unmarshal([]byte(text), &body); err != nil {
		t.Fatalf("decode %s: %v", text, err)
	}
	if body.Error.Code != vacerr.InvalidArgument {
		t.Errorf("code = %q, want %q: %s", body.Error.Code, vacerr.InvalidArgument, text)
	}
	if repos, _ := body.Error.Details["repositories"].([]any); len(repos) != 2 {
		t.Errorf("details[repositories] = %v, want the two members to choose between", body.Error.Details["repositories"])
	}
}

// TestListContextsReportsTheMultiMemberWorkspace is the discovery path an
// agent actually takes: it has to see demo-multi's two members, and the
// repository names it reports are exactly the ones the tests above pass as
// the repository argument.
func TestListContextsReportsTheMultiMemberWorkspace(t *testing.T) {
	cfg := fixtureConfig(t)
	session := multiRepoSession(t, cfg)

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_contexts", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("tools/call list_contexts: %v", err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if res.IsError {
		t.Fatalf("list_contexts failed: %s", text)
	}

	var out struct {
		Contexts []listedContext `json:"contexts"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("decode %s: %v", text, err)
	}

	var multi *listedContext
	for i, c := range out.Contexts {
		if c.ID == demorepo.MultiContext {
			multi = &out.Contexts[i]
		}
	}
	if multi == nil {
		t.Fatalf("list_contexts = %+v, missing %s", out.Contexts, demorepo.MultiContext)
	}
	if multi.Repository != "" || multi.Branch != "" || multi.Revision != "" {
		t.Errorf("%s carries flat repository/branch/revision fields %+v, want only members: a workspace of several repositories has no single one of any of them", demorepo.MultiContext, multi)
	}
	got := make([]string, 0, len(multi.Members))
	for _, m := range multi.Members {
		got = append(got, m.Repository)
	}
	slices.Sort(got)
	want := []string{multiRepo1, multiRepo2}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("%s members = %v, want %v", demorepo.MultiContext, got, want)
	}
}

// multiRepoSession serves search_code, get_code and trace_calls together, over
// the real Zoekt, CBM and git the prepared fixture names — every provider
// AC #7 through #9 need in one server, the way cmd/vacmcp wires them.
func multiRepoSession(t *testing.T, cfg *config.Config) *mcp.ClientSession {
	t.Helper()
	if _, err := exec.LookPath(cfg.Providers.CBM.Command); err != nil {
		t.Skipf("codebase-memory-mcp is not runnable at %q: %v", cfg.Providers.CBM.Command, err)
	}

	srv := server.New(testVersion)
	eng := engine.New(resolver.New(cfg), zoektadapter.New(cfg), cbmadapter.New(cfg), gitadapter.New(cfg))
	t.Cleanup(func() { _ = eng.Close() })
	AddListContexts(srv, eng)
	AddSearchCode(srv, eng)
	AddGetCode(srv, eng)
	AddTraceCalls(srv, eng)

	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true},
	))
	t.Cleanup(httpServer.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "vacmcp-test", Version: testVersion}, nil)
	clientSession, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: httpServer.URL}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	return clientSession
}

// multiRepoTraceSession is [multiRepoSession] over cfg's trace_calls
// dependencies alone, for the tests that only ever call that one tool: no
// search or source provider is registered, so a request that reached either
// would fail the test rather than answer from a stub it was never meant to
// have.
func multiRepoTraceSession(t *testing.T, cfg *config.Config) *mcp.ClientSession {
	t.Helper()

	srv := server.New(testVersion)
	AddTraceCalls(srv, engine.New(resolver.New(cfg), nil, cbmadapter.New(cfg), nil))

	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true},
	))
	t.Cleanup(httpServer.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "vacmcp-test", Version: testVersion}, nil)
	clientSession, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: httpServer.URL}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	return clientSession
}

// callersOf returns the names the result says call symbol.
func callersOf(calls []call, symbol string) []string {
	var names []string
	for _, c := range calls {
		if c.Callee == symbol {
			names = append(names, c.Caller)
		}
	}
	return names
}
