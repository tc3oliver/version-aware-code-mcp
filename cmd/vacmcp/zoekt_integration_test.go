//go:build integration

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/adapters/zoekt"
	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/internal/demorepo"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/store"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
)

// The index lifecycle is tested against the real zoekt-git-index and a real
// zoekt-webserver, never against a fake engine. What is being checked is that
// the shard Zoekt actually writes carries the refs vacmcp created and stops
// carrying the ones it deleted, and that the v0.1.0 search adapter finds the
// result through them — none of which a stub could answer, because all of it is
// Zoekt's behaviour rather than vacmcp's.

// indexer is the Zoekt indexing binary the managed lifecycle runs. The tests
// here need the name to know whether it is installed, and to rebuild a shard
// the way an interrupted removal leaves one.
const indexer = "zoekt-git-index"

// searchRefIndexed reports whether the Zoekt server at zoektURL has c's search
// ref in the index it serves.
//
// It asks the server rather than reading the shard because a context is only
// searchable once the engine answering the queries has loaded it: a shard on
// disk that nothing has picked up yet answers nothing. Asking also keeps the
// shard format out of these tests — it goes through the same client, with the
// same timeout, that the search adapter queries with.
func searchRefIndexed(ctx context.Context, zoektURL string, c store.Context) (bool, error) {
	cfg := &config.Config{Providers: config.Providers{Zoekt: config.Zoekt{URL: zoektURL}}}
	branches, err := zoekt.New(cfg).IndexedBranches(ctx, c.Repository)
	if err != nil {
		return false, err
	}
	return slices.Contains(branches, c.Branch), nil
}

// requireIndexer skips when the indexing binary is not installed. Every skip in
// this repository means an engine is missing, which is right for a developer
// without one and refused in CI, where the workflow installs it.
func requireIndexer(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath(indexer); err != nil {
		t.Skipf("%s is not on PATH, see CONTRIBUTING.md: %v", indexer, err)
	}
}

// indexed is a repository with two contexts of it: one pinning main, where only
// alphaToken is defined, and one pinning a second branch, where only betaToken
// is. Two tokens on two branches are what tell an index that holds both refs
// from one that answers out of the wrong version.
const (
	alphaToken = "AlphaOnlyToken"
	betaToken  = "BetaOnlyToken"
)

func indexedRepository(t *testing.T) (data string, alpha, beta store.Context) {
	t.Helper()
	requireIndexer(t)

	source := sourceRepo(t)
	commit(t, source, "alpha.go", "package demo\n\nfunc "+alphaToken+"() {}\n", "alpha")
	mustGit(t, "-C", source, "checkout", "-q", "-b", "release/v2", "HEAD~1")
	commit(t, source, "beta.go", "package demo\n\nfunc "+betaToken+"() {}\n", "beta")
	mustGit(t, "-C", source, "checkout", "-q", "main")

	data = t.TempDir()
	if _, err := repoRun(t, data, "add", "demo", "--url", source); err != nil {
		t.Fatalf("repo add: %v", err)
	}
	if _, err := contextRun(t, data, "create", "ctx-alpha", "--repo", "demo", "--ref", "main"); err != nil {
		t.Fatalf("context create ctx-alpha: %v", err)
	}
	if _, err := contextRun(t, data, "create", "ctx-beta", "--repo", "demo", "--ref", "release/v2"); err != nil {
		t.Fatalf("context create ctx-beta: %v", err)
	}
	return data, contextRecord(t, data, "ctx-alpha"), contextRecord(t, data, "ctx-beta")
}

// shards returns the search index files of the named repository, and every
// other file in the index directory, so a test can assert both that the
// repository has exactly one shard and that no second index was built beside
// it.
func shards(t *testing.T, data, repository string) (own, other []string) {
	t.Helper()
	dir := filepath.Join(data, "zoekt")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	shard := regexp.MustCompile(`^` + regexp.QuoteMeta(repository) + `_v[0-9]+\.[0-9]+\.zoekt$`)
	for _, entry := range entries {
		if shard.MatchString(entry.Name()) {
			own = append(own, entry.Name())
			continue
		}
		other = append(other, entry.Name())
	}
	return own, other
}

// searchable reports whether the token is findable in c through the v0.1.0
// search adapter, which is the path a search_code call takes: the same repo:
// and branch: filters, against a real server over the index vacmcp built.
func searchable(t *testing.T, url string, c store.Context, token string) bool {
	t.Helper()
	cfg := &config.Config{Providers: config.Providers{Zoekt: config.Zoekt{URL: url}}}
	results, err := zoekt.New(cfg).Search(t.Context(), vacctx.CodeContext{
		ID:         c.ID,
		Repository: c.Repository,
		Branch:     c.Branch,
		Revision:   c.Revision,
		GraphRef:   c.GraphRef,
	}, provider.SearchQuery{Query: token})
	if err != nil {
		t.Fatalf("Search(%s, %s): %v", c.ID, token, err)
	}
	return len(results) > 0
}

// TestContextCreateIndexesEverySearchRefIntoOneShard is AC #1 and AC #2: both
// contexts' search refs exist in the repository's Zoekt index, and the two of
// them share the one shard rather than each building an index of their own.
func TestContextCreateIndexesEverySearchRefIntoOneShard(t *testing.T) {
	// The shared installation: its main defines alphaToken and its release/v2
	// defines betaToken, which is what tells an index holding both refs from one
	// answering out of the wrong version.
	data := preparedManaged(t).data
	alpha, beta := contextRecord(t, data, preparedLegacy), contextRecord(t, data, preparedModern)
	clone := filepath.Join(data, "repos", "demo")

	// AC #1, in git: the ref the record names is in the clone and points at the
	// commit the context pinned, which is what the index was built from.
	for _, c := range []store.Context{alpha, beta} {
		if got := gitOut(t, "-C", clone, "rev-parse", "--verify", "refs/heads/"+c.Branch); got != c.Revision {
			t.Errorf("%s: refs/heads/%s = %s, want the pinned revision %s", c.ID, c.Branch, got, c.Revision)
		}
	}
	if alpha.Revision == beta.Revision {
		t.Fatal("the two contexts pinned the same commit, so nothing below tells their versions apart")
	}

	// AC #2: one shard for the repository, and nothing else in the index
	// directory — no per-context index beside it.
	own, other := shards(t, data, "demo")
	if len(own) != 1 {
		t.Errorf("shards of demo = %v, want exactly one shared index", own)
	}
	if len(other) != 0 {
		t.Errorf("the index directory also holds %v, want only the repository's own shard", other)
	}

	url := demorepo.StartZoekt(t, filepath.Join(data, "zoekt"))

	// AC #1, in the index: Zoekt itself reports both refs, in that one shard.
	for _, c := range []store.Context{alpha, beta} {
		indexed, err := searchRefIndexed(t.Context(), url, c)
		if err != nil {
			t.Fatalf("searchRefIndexed(%s): %v", c.ID, err)
		}
		if !indexed {
			t.Errorf("%s: search ref %q is not in the index", c.ID, c.Branch)
		}
	}

	// And the shared index is still version isolated: each context finds its
	// own branch's token and not the other's.
	if !searchable(t, url, alpha, alphaToken) {
		t.Errorf("%s cannot find %s, which its own revision defines", alpha.ID, alphaToken)
	}
	if !searchable(t, url, beta, betaToken) {
		t.Errorf("%s cannot find %s, which its own revision defines", beta.ID, betaToken)
	}
	if searchable(t, url, beta, alphaToken) {
		t.Errorf("%s finds %s, which only exists on the other context's revision", beta.ID, alphaToken)
	}
}

// TestContextRemoveTakesOnlyItsOwnRefOutOfTheIndex is AC #3: the removed
// context's ref is gone from the clone and from the index, its source is no
// longer searchable, and the context that shared the index is untouched.
func TestContextRemoveTakesOnlyItsOwnRefOutOfTheIndex(t *testing.T) {
	data, alpha, beta := indexedRepository(t)
	clone := filepath.Join(data, "repos", "demo")

	if _, err := contextRun(t, data, "remove", alpha.ID); err != nil {
		t.Fatalf("context remove %s: %v", alpha.ID, err)
	}

	if out, err := exec.Command("git", "-C", clone, "rev-parse", "--verify", "refs/heads/"+alpha.Branch).CombinedOutput(); err == nil {
		t.Errorf("refs/heads/%s survived the removal as %s", alpha.Branch, strings.TrimSpace(string(out)))
	}
	if got := gitOut(t, "-C", clone, "rev-parse", "--verify", "refs/heads/"+beta.Branch); got != beta.Revision {
		t.Errorf("%s: refs/heads/%s = %s, want the removal to have left it at %s", beta.ID, beta.Branch, got, beta.Revision)
	}
	if own, _ := shards(t, data, "demo"); len(own) != 1 {
		t.Errorf("shards of demo = %v, want the surviving context's index to still be there", own)
	}

	url := demorepo.StartZoekt(t, filepath.Join(data, "zoekt"))

	indexed, err := searchRefIndexed(t.Context(), url, alpha)
	if err != nil {
		t.Fatalf("searchRefIndexed(%s): %v", alpha.ID, err)
	}
	if indexed {
		t.Errorf("%s: search ref %q is still in the index after the context was removed", alpha.ID, alpha.Branch)
	}
	if searchable(t, url, alpha, alphaToken) {
		t.Errorf("%s: %s is still searchable after the context was removed", alpha.ID, alphaToken)
	}

	// The other context is unaffected: still indexed, still finds its own
	// token.
	indexed, err = searchRefIndexed(t.Context(), url, beta)
	if err != nil {
		t.Fatalf("searchRefIndexed(%s): %v", beta.ID, err)
	}
	if !indexed {
		t.Errorf("%s: removing the other context took this one's search ref %q out of the index", beta.ID, beta.Branch)
	}
	if !searchable(t, url, beta, betaToken) {
		t.Errorf("%s can no longer find %s after the other context was removed", beta.ID, betaToken)
	}
}

// TestIndexingRestoresAContextRecordWhoseRefIsMissing covers the state every
// context created before this existed is in — a record naming a search ref that
// was computed but never created — and the same state a create or a remove
// interrupted halfway leaves behind.
//
// Indexing names every context's ref, and a ref the clone does not have is a
// hard failure of the whole run, so trusting the records without them would
// wedge every later create and remove of that repository. On the remove path
// that failure lands after the record is gone, which is the one outcome AC #3
// forbids: a removed context whose source is still in the served shard.
func TestIndexingRestoresAContextRecordWhoseRefIsMissing(t *testing.T) {
	data, alpha, beta := indexedRepository(t)
	clone := filepath.Join(data, "repos", "demo")

	mustGit(t, "-C", clone, "update-ref", "-d", "refs/heads/"+alpha.Branch)

	// The remove path first, because it is the one whose failure would leave
	// beta's source in the index with no record of it.
	if _, err := contextRun(t, data, "remove", beta.ID); err != nil {
		t.Fatalf("context remove %s with a dangling record present: %v", beta.ID, err)
	}
	if got := gitOut(t, "-C", clone, "rev-parse", "--verify", "refs/heads/"+alpha.Branch); got != alpha.Revision {
		t.Errorf("%s: refs/heads/%s = %s, want the indexing to have restored %s", alpha.ID, alpha.Branch, got, alpha.Revision)
	}

	// And the create path, on the same repository.
	if _, err := contextRun(t, data, "create", "ctx-gamma", "--repo", "demo", "--ref", "release/v2"); err != nil {
		t.Fatalf("context create after a dangling record: %v", err)
	}

	url := demorepo.StartZoekt(t, filepath.Join(data, "zoekt"))
	for _, c := range []store.Context{alpha, contextRecord(t, data, "ctx-gamma")} {
		indexed, err := searchRefIndexed(t.Context(), url, c)
		if err != nil {
			t.Fatalf("searchRefIndexed(%s): %v", c.ID, err)
		}
		if !indexed {
			t.Errorf("%s: search ref %q is not in the index", c.ID, c.Branch)
		}
	}
	if !searchable(t, url, alpha, alphaToken) {
		t.Errorf("%s cannot find %s after its ref was restored", alpha.ID, alphaToken)
	}
}

// TestContextRemoveSurvivesAMissingClone keeps a context removable when the
// clone it was indexed from has been deleted underneath it. Without the source
// there is nothing to rebuild an index from and nothing that may stay
// searchable, so the shards go and the records can be removed — the alternative
// is a repository whose contexts cannot be removed and which therefore cannot
// be removed either.
func TestContextRemoveSurvivesAMissingClone(t *testing.T) {
	data, alpha, beta := indexedRepository(t)

	if err := os.RemoveAll(filepath.Join(data, "repos", "demo")); err != nil {
		t.Fatalf("deleting the clone: %v", err)
	}
	for _, c := range []store.Context{alpha, beta} {
		if _, err := contextRun(t, data, "remove", c.ID); err != nil {
			t.Fatalf("context remove %s without a clone: %v", c.ID, err)
		}
	}

	if own, _ := shards(t, data, "demo"); len(own) != 0 {
		t.Errorf("shards of demo = %v, want none once there is no source behind them", own)
	}
	if _, err := repoRun(t, data, "remove", "demo"); err != nil {
		t.Errorf("repo remove after its contexts were removed: %v", err)
	}
}

// TestContextRemoveDeletesTheIndexWithTheLastContext covers what a rebuild
// cannot: with no context left there is no set of branches to index, so the
// repository's shard has to go, or the last removed context's source would stay
// searchable in it.
func TestContextRemoveDeletesTheIndexWithTheLastContext(t *testing.T) {
	data, alpha, beta := indexedRepository(t)

	for _, c := range []store.Context{alpha, beta} {
		if _, err := contextRun(t, data, "remove", c.ID); err != nil {
			t.Fatalf("context remove %s: %v", c.ID, err)
		}
	}

	own, other := shards(t, data, "demo")
	if len(own) != 0 {
		t.Errorf("shards of demo = %v, want none once the last context is gone", own)
	}
	if len(other) != 0 {
		t.Errorf("the index directory still holds %v", other)
	}
}
