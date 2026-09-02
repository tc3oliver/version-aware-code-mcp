package engine_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gitadapter "github.com/tc3oliver/version-aware-code-mcp/adapters/git"
	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/resolver"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// History at the engine layer: how a workspace of several repositories is
// searched, and what happens when one of them cannot answer.
//
// The repositories are real, because the version scope is enforced by git and a
// fake source provider would only prove that the engine passes a struct along.

const (
	histRepoA = "repo-a"
	histRepoB = "repo-b"
	histWS    = "workspace-x"
)

// histRepo is one two-commit repository whose worktree stays on the newer
// commit.
type histRepo struct{ path, first, head string }

func newHistRepo(t *testing.T, name, marker string) histRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	r := histRepo{path: t.TempDir()}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", r.path}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(r.path, "main.c"), []byte(body), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	commit := func(message string) string {
		t.Helper()
		git("add", ".")
		git("-c", "user.name=vacmcp", "-c", "user.email=vacmcp@example.invalid",
			"commit", "--no-verify", "-m", message)
		return git("rev-parse", "HEAD")
	}

	git("init")
	write("void " + marker + "(void) {}\n")
	r.first = commit("initial commit in " + name)
	write("void " + marker + "(void) { changed(); }\n")
	r.head = commit("second commit in " + name)
	return r
}

// histEngine builds an engine over a two-repository workspace, each member
// pinned to that repository's FIRST commit — so the newer commits exist but are
// outside the version being asked about.
func histEngine(t *testing.T) (*engine.Engine, histRepo, histRepo) {
	t.Helper()
	a := newHistRepo(t, histRepoA, "AlphaSymbol")
	b := newHistRepo(t, histRepoB, "BetaSymbol")

	cfg := &config.Config{
		Repositories: map[string]config.Repository{
			histRepoA: {Path: a.path},
			histRepoB: {Path: b.path},
		},
		Contexts: map[string]vacctx.Workspace{
			histWS: {ID: histWS, Members: []vacctx.CodeContext{
				{ID: histWS, Repository: histRepoA, Branch: "main", Revision: a.first, GraphRef: "g-a"},
				{ID: histWS, Repository: histRepoB, Branch: "main", Revision: b.first, GraphRef: "g-b"},
			}},
		},
	}
	src := gitadapter.New(cfg)
	return engine.New(resolver.New(cfg), nil, nil, src), a, b
}

// --- Multi-repo ---------------------------------------------------------------

// TestSearchHistorySpansEveryMember proves an unnarrowed history search covers
// the whole workspace, and that each commit keeps the repository it came from.
func TestSearchHistorySpansEveryMember(t *testing.T) {
	eng, a, b := histEngine(t)

	res, err := eng.SearchHistory(context.Background(), engine.SearchHistoryRequest{Context: histWS})
	if err != nil {
		t.Fatalf("SearchHistory: %v", err)
	}
	byRepo := map[string][]engine.Commit{}
	for _, c := range res.Commits() {
		byRepo[c.Repository] = append(byRepo[c.Repository], c)
	}
	if len(byRepo[histRepoA]) == 0 {
		t.Errorf("no commits reported from %s", histRepoA)
	}
	if len(byRepo[histRepoB]) == 0 {
		t.Errorf("no commits reported from %s", histRepoB)
	}
	// Each member is pinned to its own first commit, so neither repository's
	// second commit may appear.
	for _, c := range res.Commits() {
		if c.Commit == a.head || c.Commit == b.head {
			t.Errorf("a commit after the pinned revision leaked in: %+v", c)
		}
	}
	// The answer reports the members it searched.
	if got := len(res.Context().Members); got != 2 {
		t.Errorf("the searched workspace should carry both members, got %d", got)
	}
	// One citation list per member, in the same order.
	if got := len(res.Evidence()); got != 2 {
		t.Errorf("want one citation list per member, got %d", got)
	}
}

// TestSearchHistoryNarrowsToOneRepository proves the selector answers in that
// repository only.
func TestSearchHistoryNarrowsToOneRepository(t *testing.T) {
	eng, _, _ := histEngine(t)

	res, err := eng.SearchHistory(context.Background(), engine.SearchHistoryRequest{
		Context: histWS, Repository: histRepoA})
	if err != nil {
		t.Fatalf("SearchHistory: %v", err)
	}
	if len(res.Commits()) == 0 {
		t.Fatal("repo-a has history and should report it")
	}
	for _, c := range res.Commits() {
		if c.Repository != histRepoA {
			t.Errorf("a narrowed search answered out of %s: %+v", c.Repository, c)
		}
	}
	// The workspace reported is the one that was searched, not the whole context.
	if got := len(res.Context().Members); got != 1 {
		t.Errorf("a narrowed search is answered in one member, got %d", got)
	}
}

// TestSearchHistoryUnknownRepositoryFailsClosed proves a repository the context
// does not name is refused rather than guessed at.
func TestSearchHistoryUnknownRepositoryFailsClosed(t *testing.T) {
	eng, _, _ := histEngine(t)

	_, err := eng.SearchHistory(context.Background(), engine.SearchHistoryRequest{
		Context: histWS, Repository: "repo-not-in-workspace"})
	if err == nil {
		t.Fatal("an unknown repository must fail closed, not fall back to a member")
	}
	assertCode(t, err, vacerr.InvalidArgument)
}

// TestSearchHistoryUnknownContextFailsClosed keeps context resolution ahead of
// provider availability, as every other query does.
func TestSearchHistoryUnknownContextFailsClosed(t *testing.T) {
	eng, _, _ := histEngine(t)

	_, err := eng.SearchHistory(context.Background(), engine.SearchHistoryRequest{Context: "no-such-context"})
	if err == nil {
		t.Fatal("an unconfigured context must fail closed")
	}
	assertCode(t, err, vacerr.ContextNotFound)
}

// TestSearchHistoryOneBadMemberFailsWholeRequest proves there is no partial
// history: if one member of the workspace cannot answer, the request fails
// rather than returning the members that could.
//
// A history that quietly omitted a repository would look like a complete answer
// for the context while part of it was never looked at.
//
// The failure is injected at the HISTORY PROVIDER rather than by configuring an
// unresolvable revision: the resolver rejects an unknown revision before the
// engine's per-member loop is reached, so that fixture would pass whether or not
// the loop skipped failures — it would prove the resolver works, not this.
func TestSearchHistoryOneBadMemberFailsWholeRequest(t *testing.T) {
	a := newHistRepo(t, histRepoA, "AlphaSymbol")
	b := newHistRepo(t, histRepoB, "BetaSymbol")

	cfg := &config.Config{
		Repositories: map[string]config.Repository{
			histRepoA: {Path: a.path},
			histRepoB: {Path: b.path},
		},
		Contexts: map[string]vacctx.Workspace{
			histWS: {ID: histWS, Members: []vacctx.CodeContext{
				{ID: histWS, Repository: histRepoA, Branch: "main", Revision: a.first, GraphRef: "g-a"},
				{ID: histWS, Repository: histRepoB, Branch: "main", Revision: b.first, GraphRef: "g-b"},
			}},
		},
	}
	// repo-a answers; repo-b fails. Both members resolve, so the engine really
	// does reach the second one.
	failing := &partlyFailingHistory{ok: gitadapter.New(cfg), failFor: histRepoB}
	eng := engine.New(resolver.New(cfg), nil, nil, failing)

	res, err := eng.SearchHistory(context.Background(), engine.SearchHistoryRequest{Context: histWS})
	if err == nil {
		t.Fatalf("one unreadable member must fail the whole request, got %d commits", len(res.Commits()))
	}
	if !strings.Contains(err.Error(), "repo-b is unreadable") {
		t.Errorf("the member's own error should surface, got %v", err)
	}
	if len(res.Commits()) != 0 {
		t.Errorf("a failed request must carry no commits, got %+v", res.Commits())
	}
	// repo-a WAS asked and could have answered: the point is that its answer is
	// withheld rather than returned as a complete-looking history.
	if !failing.askedOK {
		t.Error("the healthy member should have been asked, so this proves suppression rather than short-circuiting")
	}
}

// partlyFailingHistory answers out of a real provider for every repository
// except one, which always fails.
type partlyFailingHistory struct {
	ok      *gitadapter.Provider
	failFor string
	askedOK bool
}

func (p *partlyFailingHistory) Read(ctx context.Context, codeCtx vacctx.CodeContext, path string, start, end int) (*provider.SourceContent, error) {
	return p.ok.Read(ctx, codeCtx, path, start, end)
}

func (p *partlyFailingHistory) SearchHistory(ctx context.Context, codeCtx vacctx.CodeContext, req provider.HistoryQuery) ([]provider.HistoryEntry, error) {
	if codeCtx.Repository == p.failFor {
		return nil, errors.New("repo-b is unreadable")
	}
	p.askedOK = true
	return p.ok.SearchHistory(ctx, codeCtx, req)
}

// TestSearchHistoryWithoutAHistoryProviderIsRefused proves a source provider
// that cannot walk history says so, rather than answering "no commits".
func TestSearchHistoryWithoutAHistoryProviderIsRefused(t *testing.T) {
	a := newHistRepo(t, histRepoA, "AlphaSymbol")
	cfg := &config.Config{
		Repositories: map[string]config.Repository{histRepoA: {Path: a.path}},
		Contexts: map[string]vacctx.Workspace{
			histWS: {ID: histWS, Members: []vacctx.CodeContext{
				{ID: histWS, Repository: histRepoA, Branch: "main", Revision: a.first, GraphRef: "g-a"},
			}},
		},
	}
	// A source provider that reads bytes but implements no HistoryProvider.
	eng := engine.New(resolver.New(cfg), nil, nil, sourceWithoutHistory{})

	_, err := eng.SearchHistory(context.Background(), engine.SearchHistoryRequest{Context: histWS})
	if err == nil {
		t.Fatal("a server with no history capability must refuse rather than answer empty")
	}
	assertCode(t, err, vacerr.SourceHistoryUnavailable)
}

// sourceWithoutHistory reads revisions but cannot walk history, which is the
// shape a caller sees when its backend predates the capability.
type sourceWithoutHistory struct{}

func (sourceWithoutHistory) Read(context.Context, vacctx.CodeContext, string, int, int) (*provider.SourceContent, error) {
	return &provider.SourceContent{}, nil
}
