package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	gitadapter "github.com/tc3oliver/version-aware-code-mcp/adapters/git"
	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/evidence"
	"github.com/tc3oliver/version-aware-code-mcp/internal/demorepo"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/resolver"
	"github.com/tc3oliver/version-aware-code-mcp/server"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// These run against a real git repository rather than a fake source provider,
// exactly as get_code_test.go does and for its reason: git is a Tier 1
// dependency, always present, so there is nothing to gain from a stand-in that
// would only prove this tool agrees with a fake. Zoekt and CBM are the engines a
// tag keeps out of this tier, and search_history reaches neither.

// historyWire is a successful result as a client receives it: doc-1's context
// block and evidence array, plus this tool's payload. Decoding into it is what
// pins the field names — a wrong json tag leaves its field empty.
type historyWire struct {
	Context struct {
		ID         string `json:"id"`
		Repository string `json:"repository"`
		Branch     string `json:"branch"`
		Revision   string `json:"revision"`
		Members    []struct {
			Repository string `json:"repository"`
			Branch     string `json:"branch"`
			Revision   string `json:"revision"`
		} `json:"members"`
	} `json:"context"`
	Evidence []evidence.Evidence `json:"evidence"`
	Commits  []struct {
		Commit     string `json:"commit"`
		Path       string `json:"path"`
		Author     string `json:"author"`
		Timestamp  string `json:"timestamp"`
		Message    string `json:"message"`
		Repository string `json:"repository"`
	} `json:"commits"`
}

// The commits the generator makes, named here so an assertion says which one it
// means. v1 and v2 diverge after "Add Keep delegating to SharedHandler": each
// branch has two commits the other has never seen.
const (
	sharedCommit = "Add Keep delegating to SharedHandler"
	onlyOnV1     = "Cut release/v1"
	alsoOnlyOnV1 = "Add OldOnly to release/v1"
	onlyOnV2     = "Switch Process to the v2 handler"
	alsoOnlyOnV2 = "Add NewOnly to release/v2"
)

// TestSearchHistoryIsBoundedByTheVersionItIsAskedIn is the whole point of the
// tool: the walk starts at the commit the context pins, so two contexts over one
// repository give two different histories, and neither can see the other's
// commits.
func TestSearchHistoryIsBoundedByTheVersionItIsAskedIn(t *testing.T) {
	session := historySession(t, historyConfig(t))

	v1, rawV1 := historyCall(t, session, map[string]any{"context": "demo-v1"})
	v2, rawV2 := historyCall(t, session, map[string]any{"context": "demo-v2"})
	t.Logf("demo-v1 -> %s", rawV1)
	t.Logf("demo-v2 -> %s", rawV2)

	v1Messages := messagesOf(v1)
	v2Messages := messagesOf(v2)

	for _, want := range []string{onlyOnV1, alsoOnlyOnV1, sharedCommit} {
		if !mentions(v1Messages, want) {
			t.Errorf("demo-v1 history %v is missing %q", v1Messages, want)
		}
	}
	for _, unwanted := range []string{onlyOnV2, alsoOnlyOnV2} {
		if mentions(v1Messages, unwanted) {
			t.Errorf("demo-v1 history %v contains %q, which is only on release/v2 — the walk is not bounded by the pinned commit", v1Messages, unwanted)
		}
	}

	for _, want := range []string{onlyOnV2, alsoOnlyOnV2, sharedCommit} {
		if !mentions(v2Messages, want) {
			t.Errorf("demo-v2 history %v is missing %q", v2Messages, want)
		}
	}
	for _, unwanted := range []string{onlyOnV1, alsoOnlyOnV1} {
		if mentions(v2Messages, unwanted) {
			t.Errorf("demo-v2 history %v contains %q, which is only on release/v1", v2Messages, unwanted)
		}
	}

	// A context pinned earlier than both sees neither branch's later work, which
	// is the same claim from the other end: the pin bounds the walk, the branch
	// name does not.
	main, _ := historyCall(t, session, map[string]any{"context": "demo-main"})
	mainMessages := messagesOf(main)
	for _, unwanted := range []string{onlyOnV1, onlyOnV2, alsoOnlyOnV1, alsoOnlyOnV2} {
		if mentions(mainMessages, unwanted) {
			t.Errorf("demo-main history %v contains %q, a commit made after the commit it pins", mainMessages, unwanted)
		}
	}
}

// TestSearchHistoryReportsTheWholeRecord is doc-1's Tool Contract for this tool:
// the answer says which version it is about and cites what it is made of, and
// every commit carries the fields a caller is promised.
func TestSearchHistoryReportsTheWholeRecord(t *testing.T) {
	cfg := historyConfig(t)
	got, raw := historyCall(t, historySession(t, cfg), map[string]any{"context": "demo-v2"})

	codeCtx := only(cfg, "demo-v2")
	if got.Context.ID != "demo-v2" || got.Context.Repository != codeCtx.Repository ||
		got.Context.Branch != codeCtx.Branch || got.Context.Revision != codeCtx.Revision {
		t.Errorf("context block = %+v, want the version it was asked in (%+v)", got.Context, codeCtx)
	}
	if len(got.Commits) == 0 {
		t.Fatalf("demo-v2 returned no commits: %s", raw)
	}

	for _, commit := range got.Commits {
		// Never a branch, a tag, HEAD or an abbreviation.
		if len(commit.Commit) != 40 || strings.TrimLeft(commit.Commit, "0123456789abcdef") != "" {
			t.Errorf("commit id = %q, want a full 40-character hex id", commit.Commit)
		}
		if commit.Path == "" {
			t.Errorf("commit %s has no path; every entry is a commit-path occurrence", commit.Commit)
		}
		if commit.Author == "" {
			t.Errorf("commit %s has no author", commit.Commit)
		}
		if commit.Message == "" {
			t.Errorf("commit %s has no message", commit.Commit)
		}
		// RFC3339 in UTC, so the same commit reports the same bytes whatever the
		// reader's timezone is.
		parsed, err := time.Parse(time.RFC3339, commit.Timestamp)
		if err != nil {
			t.Errorf("commit %s timestamp %q is not RFC3339: %v", commit.Commit, commit.Timestamp, err)
		} else if _, offset := parsed.Zone(); offset != 0 {
			t.Errorf("commit %s timestamp %q is not UTC", commit.Commit, commit.Timestamp)
		}
		// One repository, so the context block already says which — repeating it
		// per commit would be the same fact in two places.
		if commit.Repository != "" {
			t.Errorf("commit %s carries repository %q in a context naming one repository", commit.Commit, commit.Repository)
		}
	}

	// One citation per commit-path occurrence, naming the path the commit
	// touched. There is no line range to cite: the change is the whole file's
	// difference at that commit.
	if len(got.Evidence) != len(got.Commits) {
		t.Errorf("%d commits but %d citations, want one each", len(got.Commits), len(got.Evidence))
	}
	for i, cited := range got.Evidence {
		if i < len(got.Commits) && cited.Location.Path != got.Commits[i].Path {
			t.Errorf("citation %d is for %q, want the commit's own path %q", i, cited.Location.Path, got.Commits[i].Path)
		}
	}

	// GraphRef is the CBM project backing a context: internal, and a tool's
	// output is the only place it could leak from.
	if strings.Contains(raw, "graph") {
		t.Errorf("search_history leaked the graph reference: %s", raw)
	}
}

// TestSearchHistoryFiltersCombineWithAnd covers the three filters and the rule
// that a filter matching nothing narrows to nothing rather than being dropped —
// widening a search nobody asked to widen answers a different question.
func TestSearchHistoryFiltersCombineWithAnd(t *testing.T) {
	session := historySession(t, historyConfig(t))

	byMessage, _ := historyCall(t, session, map[string]any{"context": "demo-v2", "query": "v2 handler"})
	if got := messagesOf(byMessage); len(got) != 1 || got[0] != onlyOnV2 {
		t.Errorf("query=%q matched %v, want exactly [%q]", "v2 handler", got, onlyOnV2)
	}

	// The pickaxe: commits that changed the number of occurrences of the string.
	bySymbol, _ := historyCall(t, session, map[string]any{"context": "demo-v2", "symbol": "NewOnly"})
	if got := messagesOf(bySymbol); !mentions(got, alsoOnlyOnV2) {
		t.Errorf("symbol=NewOnly matched %v, want it to include %q", got, alsoOnlyOnV2)
	}

	byPath, _ := historyCall(t, session, map[string]any{"context": "demo-v2", "path": "processor.go"})
	if len(byPath.Commits) == 0 {
		t.Error("path=processor.go matched nothing, want the commits that touched it")
	}
	for _, commit := range byPath.Commits {
		if commit.Path != "processor.go" {
			t.Errorf("path=processor.go returned an entry for %q", commit.Path)
		}
	}

	// AND, not OR: a query that matches on its own, narrowed by a path it never
	// touched, matches nothing.
	narrowed, raw := historyCall(t, session, map[string]any{
		"context": "demo-v2", "query": "v2 handler", "path": "does/not/exist.go",
	})
	if len(narrowed.Commits) != 0 {
		t.Errorf("query and a non-matching path returned %d commits, want none: %s", len(narrowed.Commits), raw)
	}
	// An empty result is an answer, and [] and null are different answers.
	if !strings.Contains(raw, `"commits":[]`) {
		t.Errorf("an empty history is %s, want \"commits\":[]", raw)
	}
}

// TestSearchHistoryLimitBoundsEachRepository covers the bound and the one value
// that is an error rather than a meaning.
func TestSearchHistoryLimitBoundsEachRepository(t *testing.T) {
	session := historySession(t, historyConfig(t))

	unbounded, _ := historyCall(t, session, map[string]any{"context": "demo-v2"})
	if len(unbounded.Commits) < 2 {
		t.Fatalf("demo-v2 has %d commits, too few to test a limit against", len(unbounded.Commits))
	}

	bounded, _ := historyCall(t, session, map[string]any{"context": "demo-v2", "limit": 1})
	if len(bounded.Commits) != 1 {
		t.Errorf("limit=1 returned %d commits, want 1", len(bounded.Commits))
	}

	// Not "unbounded": a negative limit is a caller's mistake and is said so.
	failure, _ := historyError(t, session, map[string]any{"context": "demo-v2", "limit": -1})
	if failure.Code != vacerr.InvalidArgument {
		t.Errorf("limit=-1 failed with %s, want %s", failure.Code, vacerr.InvalidArgument)
	}
}

// TestSearchHistorySpansAWorkspaceAndNarrowsToOne is the multi-repo contract, and
// it is search_code's: history spans every member by default, because "what
// changed in this version" is a question about the whole workspace, and
// repository selects one of the members rather than scoping outside them.
func TestSearchHistorySpansAWorkspaceAndNarrowsToOne(t *testing.T) {
	cfg, first, second := historyWorkspaceConfig(t)
	session := historySession(t, cfg)

	spanned, raw := historyCall(t, session, map[string]any{"context": "demo-multi"})
	seen := map[string]bool{}
	for _, commit := range spanned.Commits {
		if commit.Repository == "" {
			t.Errorf("commit %s carries no repository in a context naming several: %s", commit.Commit, raw)
		}
		seen[commit.Repository] = true
	}
	if !seen[first] || !seen[second] {
		t.Errorf("a spanning history covered %v, want both %s and %s", seen, first, second)
	}
	// The context block reports the members, which is how a client learns what
	// the repository argument will accept.
	if len(spanned.Context.Members) != 2 {
		t.Errorf("context block = %+v, want the two members", spanned.Context)
	}

	narrowed, narrowedRaw := historyCall(t, session, map[string]any{"context": "demo-multi", "repository": second})
	if len(narrowed.Commits) == 0 {
		t.Fatalf("narrowing to %s returned nothing: %s", second, narrowedRaw)
	}
	for _, commit := range narrowed.Commits {
		// Narrowed to one member, so the answer is reported in the flat shape
		// naming it — the members array belongs to answers that span them.
		if commit.Repository != "" {
			t.Errorf("a narrowed commit carries repository %q, want the flat shape", commit.Repository)
		}
	}
	if narrowed.Context.Repository != second || len(narrowed.Context.Members) != 0 {
		t.Errorf("narrowed context block = %+v, want the flat shape naming %s", narrowed.Context, second)
	}

	// A repository the context does not name is refused rather than searched:
	// the argument selects a member, it is not a way out of the context.
	failure, _ := historyError(t, session, map[string]any{"context": "demo-multi", "repository": "example/not-a-member"})
	if failure.Code != vacerr.InvalidArgument {
		t.Errorf("an unknown repository failed with %s, want %s", failure.Code, vacerr.InvalidArgument)
	}
}

// TestSearchHistoryReportsAnUnconfiguredContext: the tool does not fall back to
// another context and does not answer an unanswerable question with an empty
// result, which would read as "this version has no such commit".
func TestSearchHistoryReportsAnUnconfiguredContext(t *testing.T) {
	session := historySession(t, historyConfig(t))

	failure, raw := historyError(t, session, map[string]any{"context": "no-such-context"})
	if failure.Code != vacerr.ContextNotFound {
		t.Errorf("an unconfigured context failed with %s, want %s: %s", failure.Code, vacerr.ContextNotFound, raw)
	}
}

// TestSearchHistoryReportsASourceProviderThatCannotWalkHistory is the capability
// refusal: history is discovered by type assertion on the source provider, so a
// backend that reads a revision's bytes but cannot walk its history has to say
// so rather than return an empty history that reads as "this version has no
// commits".
func TestSearchHistoryReportsASourceProviderThatCannotWalkHistory(t *testing.T) {
	cfg := historyConfig(t)
	session := historySessionWithSource(t, cfg, readOnlySource{})

	failure, raw := historyError(t, session, map[string]any{"context": "demo-v2"})
	if failure.Code != vacerr.SourceHistoryUnavailable {
		t.Errorf("a source provider without history failed with %s, want %s: %s", failure.Code, vacerr.SourceHistoryUnavailable, raw)
	}
}

// readOnlySource reads revisions and nothing else: no Diff, no SearchHistory. It
// is what makes the capability refusal observable, because the git adapter has
// every capability and could never produce it.
type readOnlySource struct{}

func (readOnlySource) Read(context.Context, vacctx.CodeContext, string, int, int) (*provider.SourceContent, error) {
	return &provider.SourceContent{}, nil
}

func historyConfig(t *testing.T) *config.Config {
	t.Helper()

	repo := demorepo.Generate(t)
	cfg := &config.Config{
		Repositories: map[string]config.Repository{"example/demo": {Path: repo}},
		Contexts:     map[string]vacctx.Workspace{},
	}
	for id, branch := range map[string]string{"demo-main": demorepo.Main, "demo-v1": demorepo.V1, "demo-v2": demorepo.V2} {
		cfg.Contexts[id] = single(vacctx.CodeContext{
			Repository: "example/demo",
			Branch:     branch,
			Revision:   demorepo.Revision(t, repo, branch),
			GraphRef:   id + "-graph",
		})
	}
	return cfg
}

// historyWorkspaceConfig is a context naming two repositories, each pinned to its
// own revision, and returns the two repository names so an assertion can say
// which member it means.
func historyWorkspaceConfig(t *testing.T) (*config.Config, string, string) {
	t.Helper()

	first := demorepo.Generate(t)
	second := demorepo.GenerateSecond(t)
	const firstName, secondName = "example/demo", "example/second"

	return &config.Config{
		Repositories: map[string]config.Repository{
			firstName:  {Path: first},
			secondName: {Path: second},
		},
		Contexts: map[string]vacctx.Workspace{
			"demo-multi": {ID: "demo-multi", Members: []vacctx.CodeContext{
				{
					Repository: firstName,
					Branch:     demorepo.V2,
					Revision:   demorepo.Revision(t, first, demorepo.V2),
					GraphRef:   "demo-multi-first",
				},
				{
					Repository: secondName,
					Branch:     demorepo.Repo2Main,
					Revision:   demorepo.Revision(t, second, demorepo.Repo2Main),
					GraphRef:   "demo-multi-second",
				},
			}},
		},
	}, firstName, secondName
}

// historySession serves search_history over stateless Streamable HTTP with the
// real resolver and the real git source adapter behind it: nothing between the
// client and the repository is a stub. search_history reaches neither of the
// other two providers, so the engine is built without them and a tool that tried
// would fail the test.
func historySession(t *testing.T, cfg *config.Config) *mcp.ClientSession {
	t.Helper()
	return historySessionWithSource(t, cfg, gitadapter.New(cfg))
}

func historySessionWithSource(t *testing.T, cfg *config.Config, source provider.SourceProvider) *mcp.ClientSession {
	t.Helper()

	srv := server.New(testVersion)
	AddSearchHistory(srv, engine.New(resolver.New(cfg), nil, nil, source))

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

// historyRaw calls the tool and returns the result with the JSON text the client
// received. The text is read rather than the decoded structured content because
// the assertions are about what is and is not on the wire.
func historyRaw(t *testing.T, session *mcp.ClientSession, args map[string]any) (*mcp.CallToolResult, string) {
	t.Helper()

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "search_history", Arguments: args})
	if err != nil {
		t.Fatalf("tools/call search_history(%v): %v", args, err)
	}
	if len(res.Content) == 0 {
		t.Fatalf("search_history(%v) returned no content", args)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("result content = %#v, want text", res.Content[0])
	}
	return res, text.Text
}

func historyCall(t *testing.T, session *mcp.ClientSession, args map[string]any) (historyWire, string) {
	t.Helper()

	res, raw := historyRaw(t, session, args)
	if res.IsError {
		t.Fatalf("search_history(%v) failed: %s", args, raw)
	}
	var got historyWire
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return got, raw
}

// historyError calls the tool and requires it to have failed with the unified
// error envelope, carrying nothing else.
func historyError(t *testing.T, session *mcp.ClientSession, args map[string]any) (*vacerr.Error, string) {
	t.Helper()

	res, raw := historyRaw(t, session, args)
	if !res.IsError {
		t.Fatalf("search_history(%v) succeeded, want an error: %s", args, raw)
	}
	if res.StructuredContent != nil {
		t.Errorf("error result carries structured content: %v", res.StructuredContent)
	}
	var envelope struct {
		Error vacerr.Error `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return &envelope.Error, raw
}

func messagesOf(got historyWire) []string {
	seen := map[string]bool{}
	var messages []string
	for _, commit := range got.Commits {
		if !seen[commit.Message] {
			seen[commit.Message] = true
			messages = append(messages, commit.Message)
		}
	}
	return messages
}

// mentions reports whether any of haystack contains needle.
func mentions(haystack []string, needle string) bool {
	for _, straw := range haystack {
		if strings.Contains(straw, needle) {
			return true
		}
	}
	return false
}
