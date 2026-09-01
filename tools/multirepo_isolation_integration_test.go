//go:build integration

package tools

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
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
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// TASK-86 is the adversarial isolation gate multi-repo introduces: an agent
// that tries a malformed query, a repository outside the workspace, a
// same-named path or symbol, a member whose provider is down, or a path
// escaping the repository must never get back a plausible-looking answer from
// the wrong repository or revision. Every test here runs against the real
// Zoekt and CBM engines testdata/prepare-fixture.sh built and demo-multi's
// real collision (versioned-demo-repo's release/v1 paired with
// second-demo-repo, both declaring a handler.go with a LegacyHandler of their
// own) — a fake provider could agree with its own idea of the isolation and
// prove nothing about whether a query actually reached the wrong engine.

// memberOf returns the member of cfg's demo-multi context named repository, so
// an assertion can be made against its own configured revision rather than a
// value copied by hand.
func memberOf(t *testing.T, cfg *config.Config, repository string) vacctx.CodeContext {
	t.Helper()
	for _, member := range cfg.Contexts[demorepo.MultiContext].Members {
		if member.Repository == repository {
			return member
		}
	}
	t.Fatalf("%s has no member named %q", demorepo.MultiContext, repository)
	return vacctx.CodeContext{}
}

// errorBody decodes a tool's error result into doc-1's error envelope, the
// same shape every tool fails with regardless of which one was called.
func errorBody(t *testing.T, raw string) traceCallsErrorWire {
	t.Helper()
	var body traceCallsErrorWire
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode error envelope %s: %v", raw, err)
	}
	return body
}

// TestSearchCodeQuerySyntaxCannotEscapeAMembersOrTheWorkspacesScope is AC #1
// through #3: a query that tries to break out of the parentheses the adapter
// wraps it in is refused before it reaches Zoekt, whether it is scoped to one
// member or to the whole workspace, and a query that legitimately groups and
// ors is still confined to the member it was asked about.
func TestSearchCodeQuerySyntaxCannotEscapeAMembersOrTheWorkspacesScope(t *testing.T) {
	cfg := fixtureConfig(t)
	session := multiRepoSession(t, cfg)

	escapes := []string{
		"LegacyHandler) or (LegacyHandler",
		"LegacyHandler) or (second:",
		") or (LegacyHandler",
		"LegacyHandler)",
	}

	// AC #1 and #2: scoped to one member, an escaping query is INVALID_ARGUMENT
	// and returns nothing — not a fallback and not a partial answer.
	for _, query := range escapes {
		t.Run("member-scoped/"+query, func(t *testing.T) {
			res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
				Name: "search_code",
				Arguments: map[string]any{
					"context": demorepo.MultiContext, "repository": multiRepo1, "query": query,
				},
			})
			if err != nil {
				t.Fatalf("tools/call search_code: %v", err)
			}
			text := res.Content[0].(*mcp.TextContent).Text
			if !res.IsError {
				t.Fatalf("search_code(%s, repository=%s, %q) succeeded, want INVALID_ARGUMENT: %s", demorepo.MultiContext, multiRepo1, query, text)
			}
			if code := errorBody(t, text).Error.Code; code != vacerr.InvalidArgument {
				t.Errorf("code = %q, want INVALID_ARGUMENT: %s", code, text)
			}
			if strings.Contains(text, `"matches"`) {
				t.Errorf("error result carries matches: %s", text)
			}
		})
	}

	// AC #3: the same escape attempts, asked of the whole workspace with no
	// repository argument, fail exactly the same way. A caller narrowing to one
	// member is not what stops the escape; nothing here may answer with a
	// non-member repository's content either.
	for _, query := range escapes {
		t.Run("workspace-scoped/"+query, func(t *testing.T) {
			res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
				Name:      "search_code",
				Arguments: map[string]any{"context": demorepo.MultiContext, "query": query},
			})
			if err != nil {
				t.Fatalf("tools/call search_code: %v", err)
			}
			text := res.Content[0].(*mcp.TextContent).Text
			if !res.IsError {
				t.Fatalf("search_code(%s, %q) with no repository succeeded, want INVALID_ARGUMENT: %s", demorepo.MultiContext, query, text)
			}
			if code := errorBody(t, text).Error.Code; code != vacerr.InvalidArgument {
				t.Errorf("code = %q, want INVALID_ARGUMENT: %s", code, text)
			}
		})
	}

	// A query that legitimately groups and ors is not refused, and is still
	// confined to the member it was scoped to: the or can reach nothing
	// second-demo-repo has, even though its LegacyHandler is in the same index
	// under the same file name.
	raw, isError := callSearchCode(t, session, demorepo.MultiContext, "LegacyHandler or LegacyHandler")
	if isError {
		t.Fatalf("search_code(%s, LegacyHandler or LegacyHandler) failed: %s", demorepo.MultiContext, raw)
	}
	var out struct {
		Matches []searchMatch `json:"matches"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if len(out.Matches) == 0 {
		t.Fatalf("search_code(%s, LegacyHandler or LegacyHandler) = no matches, want both members' own LegacyHandler", demorepo.MultiContext)
	}
	for _, match := range out.Matches {
		if strings.Contains(match.Snippet, "second: ") && match.Repository != multiRepo2 {
			t.Errorf("match %+v carries second-demo-repo's content but is attributed to %s", match, match.Repository)
		}
	}
}

// TestRepositoryOutsideTheWorkspaceIsRefusedByEveryTool is AC #4: naming a
// repository the workspace does not have is INVALID_ARGUMENT for every tool
// that takes the argument, and none of them falls back to a member the
// workspace does name. list_contexts takes no repository argument and has
// nothing here to narrow, so it is not one of the tools below — see
// AddListContexts.
func TestRepositoryOutsideTheWorkspaceIsRefusedByEveryTool(t *testing.T) {
	cfg := fixtureConfig(t)
	session := allToolsSession(t, cfg)
	const stranger = "someone-elses-repository"

	calls := map[string]map[string]any{
		"search_code": {"context": demorepo.MultiContext, "repository": stranger, "query": "LegacyHandler"},
		"get_code":    {"context": demorepo.MultiContext, "repository": stranger, "path": "handler.go", "start_line": 1, "end_line": 1},
		"trace_calls": {"context": demorepo.MultiContext, "repository": stranger, "symbol": "LegacyHandler", "direction": "callers", "depth": 1},
		"compare_code": {
			"from_context": demorepo.MultiContext, "to_context": demorepo.MultiContext,
			"repository": stranger, "path": "handler.go",
		},
		"compare_calls": {
			"from_context": demorepo.MultiContext, "to_context": demorepo.MultiContext,
			"repository": stranger, "symbol": "LegacyHandler", "direction": "callers", "depth": 1,
		},
	}
	for tool, args := range calls {
		t.Run(tool, func(t *testing.T) {
			res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})
			if err != nil {
				t.Fatalf("tools/call %s: %v", tool, err)
			}
			text := res.Content[0].(*mcp.TextContent).Text
			if !res.IsError {
				t.Fatalf("%s(repository=%s) succeeded, want INVALID_ARGUMENT: %s", tool, stranger, text)
			}
			body := errorBody(t, text)
			if body.Error.Code != vacerr.InvalidArgument {
				t.Errorf("code = %q, want INVALID_ARGUMENT: %s", body.Error.Code, text)
			}
			// No fallback: the repositories offered are exactly the workspace's
			// own two members, never the stranger and never just one of them.
			offered, _ := body.Error.Details["repositories"].([]any)
			got := map[string]bool{}
			for _, r := range offered {
				got[r.(string)] = true
			}
			if len(got) != 2 || !got[multiRepo1] || !got[multiRepo2] {
				t.Errorf("%s: details[repositories] = %v, want exactly [%s %s]", tool, offered, multiRepo1, multiRepo2)
			}
			if got[stranger] {
				t.Errorf("%s: details[repositories] offers the stranger repository %q", tool, stranger)
			}
		})
	}
}

// TestGetCodeRejectsPathEscapesForEveryMember is AC #12: a path outside the
// repository is refused for whichever member it is asked of, member by
// member — the check is the git adapter's own and runs once per read, so
// this holds it to that for both real repositories rather than trusting that
// one proves the other.
func TestGetCodeRejectsPathEscapesForEveryMember(t *testing.T) {
	cfg := fixtureConfig(t)
	session := multiRepoSession(t, cfg)

	for _, repository := range []string{multiRepo1, multiRepo2} {
		for name, path := range map[string]string{
			"absolute path":   "/etc/passwd",
			"parent escape":   "../../etc/passwd",
			"embedded escape": "handler.go/../../../etc/passwd",
		} {
			t.Run(repository+"/"+name, func(t *testing.T) {
				res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
					Name: "get_code",
					Arguments: map[string]any{
						"context": demorepo.MultiContext, "repository": repository,
						"path": path, "start_line": 1, "end_line": 1,
					},
				})
				if err != nil {
					t.Fatalf("tools/call get_code: %v", err)
				}
				text := res.Content[0].(*mcp.TextContent).Text
				if !res.IsError {
					t.Fatalf("get_code(%s, repository=%s, %q) succeeded, want INVALID_ARGUMENT: %s", demorepo.MultiContext, repository, path, text)
				}
				if code := errorBody(t, text).Error.Code; code != vacerr.InvalidArgument {
					t.Errorf("code = %q, want INVALID_ARGUMENT: %s", code, text)
				}
				if strings.Contains(text, `"content"`) {
					t.Errorf("error result carries content: %s", text)
				}
			})
		}
	}
}

// TestGetCodeAndTraceCallsRequireRepositoryEvenWhenOnlyOneMemberHasTheAnswer
// is AC #5: decision-11 §3 forbids auto-resolving to the one member that
// happens to hold a path or symbol today, because that would make the same
// request answer differently the moment a second member happens to gain one
// of its own. processor.go and the symbol Process exist only in multiRepo1 —
// second-demo-repo has neither — so there is no collision here to hide
// behind: this is the case decision-11 names explicitly, not the ambiguous
// one [TestGetCodeOverAMultiMemberWorkspaceReadsEachRepositorysOwnContent]
// and [TestTraceCallsOverAMultiMemberWorkspaceWalksOneRepositorysOwnGraph]
// (tools/multirepo_integration_test.go) already cover with handler.go and
// LegacyHandler.
func TestGetCodeAndTraceCallsRequireRepositoryEvenWhenOnlyOneMemberHasTheAnswer(t *testing.T) {
	cfg := traceFixture(t)
	full := fixtureConfig(t)
	full.Providers.CBM = cfg.Providers.CBM

	t.Run("get_code/processor.go", func(t *testing.T) {
		session := multiRepoSession(t, full)
		res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
			Name: "get_code",
			Arguments: map[string]any{
				"context": demorepo.MultiContext, "path": "processor.go", "start_line": 1, "end_line": 1,
			},
		})
		if err != nil {
			t.Fatalf("tools/call get_code: %v", err)
		}
		text := res.Content[0].(*mcp.TextContent).Text
		if !res.IsError {
			t.Fatalf("get_code(%s, processor.go) with no repository succeeded even though only %s has that path, want INVALID_ARGUMENT: %s", demorepo.MultiContext, multiRepo1, text)
		}
		body := errorBody(t, text)
		if body.Error.Code != vacerr.InvalidArgument {
			t.Errorf("code = %q, want INVALID_ARGUMENT: %s", body.Error.Code, text)
		}
		offered, _ := body.Error.Details["repositories"].([]any)
		if len(offered) != 2 {
			t.Errorf("details[repositories] = %v, want both members offered even though only one has processor.go", offered)
		}
	})

	t.Run("trace_calls/Process", func(t *testing.T) {
		session := multiRepoTraceSession(t, full)
		res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
			Name: "trace_calls",
			Arguments: map[string]any{
				"context": demorepo.MultiContext, "symbol": "Process", "direction": "callers", "depth": 1,
			},
		})
		if err != nil {
			t.Fatalf("tools/call trace_calls: %v", err)
		}
		text := res.Content[0].(*mcp.TextContent).Text
		if !res.IsError {
			t.Fatalf("trace_calls(%s, Process) with no repository succeeded even though only %s declares Process, want INVALID_ARGUMENT: %s", demorepo.MultiContext, multiRepo1, text)
		}
		body := errorBody(t, text)
		if body.Error.Code != vacerr.InvalidArgument {
			t.Errorf("code = %q, want INVALID_ARGUMENT: %s", body.Error.Code, text)
		}
		offered, _ := body.Error.Details["repositories"].([]any)
		if len(offered) != 2 {
			t.Errorf("details[repositories] = %v, want both members offered even though only one declares Process", offered)
		}
	})
}

// TestEveryResultInAMultiMemberWorkspaceCarriesItsRepositoryAndRevision is
// AC #6 and AC #13 together: a read scoped to one repository comes back with
// that repository's own configured revision, not merely a non-empty one, and
// every tool that can answer inside demo-multi does the same — there is no
// result in a multi-member workspace that cannot be attributed to a
// repository and a revision.
func TestEveryResultInAMultiMemberWorkspaceCarriesItsRepositoryAndRevision(t *testing.T) {
	cfg := traceFixture(t)
	full := fixtureConfig(t)
	full.Providers.CBM = cfg.Providers.CBM
	session := multiRepoSession(t, full)

	for _, repository := range []string{multiRepo1, multiRepo2} {
		want := memberOf(t, full, repository)

		t.Run("get_code/"+repository, func(t *testing.T) {
			res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
				Name: "get_code",
				Arguments: map[string]any{
					"context": demorepo.MultiContext, "repository": repository,
					"path": "handler.go", "start_line": 1, "end_line": 1,
				},
			})
			if err != nil {
				t.Fatalf("tools/call get_code: %v", err)
			}
			text := res.Content[0].(*mcp.TextContent).Text
			if res.IsError {
				t.Fatalf("get_code(%s, repository=%s) failed: %s", demorepo.MultiContext, repository, text)
			}
			var out struct {
				Context listedContext `json:"context"`
			}
			if err := json.Unmarshal([]byte(text), &out); err != nil {
				t.Fatalf("decode %s: %v", text, err)
			}
			if out.Context.Repository != repository || out.Context.Revision != want.Revision {
				t.Errorf("get_code context = %+v, want repository %s at revision %s: %s", out.Context, repository, want.Revision, text)
			}
		})

		t.Run("trace_calls/"+repository, func(t *testing.T) {
			res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
				Name: "trace_calls",
				Arguments: map[string]any{
					"context": demorepo.MultiContext, "repository": repository,
					"symbol": "LegacyHandler", "direction": "callers", "depth": 1,
				},
			})
			if err != nil {
				t.Fatalf("tools/call trace_calls: %v", err)
			}
			text := res.Content[0].(*mcp.TextContent).Text
			if res.IsError {
				t.Fatalf("trace_calls(%s, repository=%s) failed: %s", demorepo.MultiContext, repository, text)
			}
			var out traceCallsOutputWire
			if err := json.Unmarshal([]byte(text), &out); err != nil {
				t.Fatalf("decode %s: %v", text, err)
			}
			if out.Context.Repository != repository || out.Context.Revision != want.Revision {
				t.Errorf("trace_calls context = %+v, want repository %s at revision %s: %s", out.Context, repository, want.Revision, text)
			}
		})
	}

	// search_code over the whole workspace: every match names the repository
	// and revision it was actually found in, matching that member's own
	// configured revision exactly.
	raw, isError := callSearchCode(t, session, demorepo.MultiContext, "LegacyHandler")
	if isError {
		t.Fatalf("search_code(%s, LegacyHandler) failed: %s", demorepo.MultiContext, raw)
	}
	var out struct {
		Matches []searchMatch `json:"matches"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if len(out.Matches) == 0 {
		t.Fatalf("search_code(%s, LegacyHandler) = no matches", demorepo.MultiContext)
	}
	for _, match := range out.Matches {
		want := memberOf(t, full, match.Repository)
		if match.Repository == "" || match.Revision != want.Revision {
			t.Errorf("match %+v does not carry its own member's revision %s", match, want.Revision)
		}
	}
}

// TestTraceCallsWithOneMembersGraphUnavailableFailsClosedIndependently is
// AC #8: one member's graph being unreachable is GRAPH_PROVIDER_UNAVAILABLE
// for that member and never SYMBOL_NOT_FOUND — which would read as "this
// version has no such symbol" when the truth is the engine could not be
// asked — and it does not mask the other member, whose own graph is
// untouched, into failing or answering the wrong thing either.
func TestTraceCallsWithOneMembersGraphUnavailableFailsClosedIndependently(t *testing.T) {
	cfg := traceFixture(t)
	workspace := cfg.Contexts[demorepo.MultiContext]
	if len(workspace.Members) != 2 {
		t.Fatalf("%s has %d members, want 2", demorepo.MultiContext, len(workspace.Members))
	}

	members := make([]vacctx.CodeContext, len(workspace.Members))
	copy(members, workspace.Members)
	for i, m := range members {
		if m.Repository == multiRepo2 {
			// A graph_ref naming no real CBM project, so this member's graph is
			// unreachable while multiRepo1's graph_ref is untouched.
			members[i].GraphRef = "vacmcp-demo2-does-not-exist"
		}
	}
	cfg.Contexts[demorepo.MultiContext] = vacctx.Workspace{ID: workspace.ID, Members: members}
	session := multiRepoTraceSession(t, cfg)

	broken, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "trace_calls",
		Arguments: map[string]any{
			"context": demorepo.MultiContext, "repository": multiRepo2,
			"symbol": "LegacyHandler", "direction": "callers", "depth": 1,
		},
	})
	if err != nil {
		t.Fatalf("tools/call trace_calls: %v", err)
	}
	brokenText := broken.Content[0].(*mcp.TextContent).Text
	if !broken.IsError {
		t.Fatalf("trace_calls(%s, repository=%s) with an unreachable graph succeeded, want GRAPH_PROVIDER_UNAVAILABLE: %s", demorepo.MultiContext, multiRepo2, brokenText)
	}
	if code := errorBody(t, brokenText).Error.Code; code != vacerr.GraphProviderUnavailable {
		t.Errorf("code = %q, want GRAPH_PROVIDER_UNAVAILABLE (and never SYMBOL_NOT_FOUND): %s", code, brokenText)
	}

	// The other member's own graph is untouched by this, in the same config and
	// the same session: providers degrade per member, not per workspace.
	fine, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "trace_calls",
		Arguments: map[string]any{
			"context": demorepo.MultiContext, "repository": multiRepo1,
			"symbol": "LegacyHandler", "direction": "callers", "depth": 1,
		},
	})
	if err != nil {
		t.Fatalf("tools/call trace_calls: %v", err)
	}
	fineText := fine.Content[0].(*mcp.TextContent).Text
	if fine.IsError {
		t.Fatalf("trace_calls(%s, repository=%s) failed while only the other member's graph is broken: %s", demorepo.MultiContext, multiRepo1, fineText)
	}
}

// TestSearchCodeWithSearchEngineUnavailableFailsTheWholeWorkspaceSearch is
// AC #9. Every repository in a workspace is indexed into the one Zoekt shard
// this server talks to (decision-11 §1), so there is no per-member search
// engine to take down independently of the others — the realistic failure
// this AC describes is Zoekt itself being unreachable, and the whole
// multi-member search must fail rather than quietly answer with whichever
// member happened to be queried first.
func TestSearchCodeWithSearchEngineUnavailableFailsTheWholeWorkspaceSearch(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Providers.Zoekt.URL = "http://" + unreachableAddress(t)
	session := multiRepoSession(t, cfg)

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "search_code",
		Arguments: map[string]any{"context": demorepo.MultiContext, "query": "LegacyHandler"},
	})
	if err != nil {
		t.Fatalf("tools/call search_code: %v", err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !res.IsError {
		t.Fatalf("search_code(%s) with Zoekt unreachable succeeded, want SEARCH_PROVIDER_UNAVAILABLE: %s", demorepo.MultiContext, text)
	}
	if code := errorBody(t, text).Error.Code; code != vacerr.SearchProviderUnavailable {
		t.Errorf("code = %q, want SEARCH_PROVIDER_UNAVAILABLE: %s", code, text)
	}
	if strings.Contains(text, `"matches"`) {
		t.Errorf("a failed multi-member search carries matches, want no partial results: %s", text)
	}
}

// unreachableAddress returns a loopback address nobody is listening on: a
// port reserved and released, guaranteed to refuse the next connection.
func unreachableAddress(t *testing.T) string {
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

// allToolsSession is [multiRepoSession] with compare_code and compare_calls
// added, for the tests that need the full six-tool surface (list_contexts
// takes no repository argument, so it is registered but not exercised by
// AC #4).
func allToolsSession(t *testing.T, cfg *config.Config) *mcp.ClientSession {
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
	AddCompareCode(srv, eng)
	AddCompareCalls(srv, eng)

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
