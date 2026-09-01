//go:build integration

package main

// A context over several repositories, driven through the CLI against real git,
// a real Zoekt index and a real graph engine. What is being checked is what only
// the real engines can answer: that two members are really checked out, really
// indexed into their own repositories' shards and really given graphs of their
// own, and that a removal takes all of it apart. The rules those stages follow —
// which member a stage reaches, in which order, and what the record says
// afterwards — are the managed package's own and are checked there.

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/internal/demorepo"
	"github.com/tc3oliver/version-aware-code-mcp/managed"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// The two repositories a workspace is made of, and a token only one of them
// defines: a member answering out of the other repository's source would be
// visible as the wrong token being findable.
const (
	apiToken = "ApiOnlySymbol"
	webToken = "WebOnlySymbol"
)

// twoManagedRepositories returns a data directory with "api" and "web" cloned
// into it, and the commit each of their mains points at. The two commits are
// different, so a member pinned to the wrong one is visible in the record.
func twoManagedRepositories(t *testing.T) (data, apiSHA, webSHA string) {
	t.Helper()
	requireIndexer(t)
	cbmOrSkip(t)

	data = t.TempDir()
	t.Cleanup(func() { discardGraphs(t, data) })

	shas := map[string]string{}
	for name, token := range map[string]string{"api": apiToken, "web": webToken} {
		source := filepath.Join(t.TempDir(), name)
		mustGit(t, "init", "-q", "-b", "main", source)
		commit(t, source, "go.mod", "module "+name+"\n\ngo 1.26\n", "module")
		shas[name] = commit(t, source, name+".go", "package "+name+"\n\nfunc "+token+"() {}\n", name)
		if _, err := repoRun(t, data, "add", name, "--url", source); err != nil {
			t.Fatalf("repo add %s: %v", name, err)
		}
	}
	if shas["api"] == shas["web"] {
		t.Fatal("the two repositories are at the same commit, so nothing below tells the members apart")
	}
	return data, shas["api"], shas["web"]
}

// createStack runs `context create` with one --repo/--ref pair per repository,
// which is the command this whole task is about.
func createStack(t *testing.T, data, id string) (string, error) {
	t.Helper()
	return contextRun(t, data, "create", id, "--repo", "api", "--ref", "main", "--repo", "web", "--ref", "main")
}

// TestContextCreateOverTwoRepositoriesReachesReadyWithBothMembersBuilt is the
// whole of a multi-repository create: one context, two members, each pinned to
// its own repository's commit, each with a checkout, a search ref in its own
// clone and a graph of its own — and READY only because all of that is there and
// was verified.
func TestContextCreateOverTwoRepositoriesReachesReadyWithBothMembersBuilt(t *testing.T) {
	data, apiSHA, webSHA := twoManagedRepositories(t)

	out, err := createStack(t, data, "stack")
	if err != nil {
		t.Fatalf("context create over two repositories: %v", err)
	}
	want := "stack\t" + managed.ContextReady + "\tapi\t" + apiSHA + "\n" +
		"stack\t" + managed.ContextReady + "\tweb\t" + webSHA + "\n"
	if out != want {
		t.Errorf("context create printed %q, want a row per repository: %q", out, want)
	}

	c := contextRecord(t, data, "stack")
	if c.State != managed.ContextReady {
		t.Fatalf("state = %q, want %s", c.State, managed.ContextReady)
	}
	if len(c.Members) != 2 {
		t.Fatalf("the record carries %d members, want the two repositories: %+v", len(c.Members), c)
	}
	// Each member pinned its own repository's full SHA, into the one record.
	for i, member := range []struct{ repository, revision string }{{"api", apiSHA}, {"web", webSHA}} {
		if got := c.Members[i]; got.Repository != member.repository || got.Revision != member.revision {
			t.Errorf("member %d = %+v, want %s at %s", i, got, member.repository, member.revision)
		}
		if len(c.Members[i].Revision) != fullSHA {
			t.Errorf("member %s pins %q, want a full commit SHA", c.Members[i].Repository, c.Members[i].Revision)
		}
	}

	// The two members' artifacts are named apart and really are two: the
	// checkouts are in their own repositories' directories, the search refs are
	// in their own clones on their own commits, and the graphs are two projects.
	names := map[string]string{}
	for _, member := range c.Members {
		worktree := filepath.Join(data, "worktrees", member.Repository, "stack")
		if head := gitOut(t, "-C", worktree, "rev-parse", "HEAD"); head != member.Revision {
			t.Errorf("the worktree of %s is at %q, want the pinned %q", member.Repository, head, member.Revision)
		}
		clone := filepath.Join(data, "repos", member.Repository)
		if ref := gitOut(t, "-C", clone, "rev-parse", "--verify", "refs/heads/"+member.Branch); ref != member.Revision {
			t.Errorf("the search ref of %s is at %q, want the pinned %q", member.Repository, ref, member.Revision)
		}
		mustGit(t, "check-ref-format", "refs/heads/"+member.Branch)
		if err := graphExists(t.Context(), c.ID, member); err != nil {
			t.Errorf("the graph of member %s: %v", member.Repository, err)
		}
		for kind, name := range map[string]string{"search ref": member.Branch, "graph": member.GraphRef, "worktree": worktree} {
			if first, taken := names[name]; taken {
				t.Errorf("the %s of %s is also the %s of %s: %q", kind, member.Repository, first, name, name)
			}
			names[name] = kind + " of " + member.Repository
		}
	}

	// Each repository has its own shard, and each member's ref is in the index
	// of its own repository: one repository is one shard however many contexts
	// or members reach it.
	url := demorepo.StartZoekt(t, filepath.Join(data, "zoekt"))
	for _, member := range c.Members {
		if own, _ := shards(t, data, member.Repository); len(own) != 1 {
			t.Errorf("shards of %s = %v, want exactly one", member.Repository, own)
		}
		indexed, err := searchRefIndexed(t.Context(), url, member)
		if err != nil {
			t.Fatalf("searchRefIndexed(%s): %v", member.Repository, err)
		}
		if !indexed {
			t.Errorf("the search ref %q of member %s is not in the index", member.Branch, member.Repository)
		}
	}
	// And each member's source is the source of its own repository, which is
	// what a member indexed from the wrong checkout would fail.
	for _, member := range c.Members {
		token := map[string]string{"api": apiToken, "web": webToken}[member.Repository]
		other := map[string]string{"api": webToken, "web": apiToken}[member.Repository]
		if !searchable(t, url, c.ID, member, token) {
			t.Errorf("member %s cannot find %s, which its own source defines", member.Repository, token)
		}
		if searchable(t, url, c.ID, member, other) {
			t.Errorf("member %s finds %s, which only the other repository defines", member.Repository, other)
		}
	}

	// READY means the artifacts, asked again: verify re-runs the same checks per
	// member and reports a row for each.
	verified, err := contextRun(t, data, "verify", "stack")
	if err != nil {
		t.Fatalf("context verify: %v", err)
	}
	if verified != "stack\tOK\tapi\t"+apiSHA+"\nstack\tOK\tweb\t"+webSHA+"\n" {
		t.Errorf("context verify printed %q, want a row per repository", verified)
	}
	if !served(t, data, "stack") {
		t.Error("a READY two-repository context is not in the registry the query plane reads")
	}

	// list and status report every member.
	listed, err := contextRun(t, data, "list")
	if err != nil {
		t.Fatalf("context list: %v", err)
	}
	if listed != "stack\tapi\t"+apiSHA+"\t"+managed.ContextReady+"\nstack\tweb\t"+webSHA+"\t"+managed.ContextReady+"\n" {
		t.Errorf("context list printed %q, want a row per repository", listed)
	}
	status, err := contextRun(t, data, "status", "stack")
	if err != nil {
		t.Fatalf("context status: %v", err)
	}
	for _, member := range c.Members {
		for _, want := range []string{member.Repository, member.Revision, member.Branch, member.GraphRef,
			filepath.Join(data, "worktrees", member.Repository, "stack")} {
			if !strings.Contains(status, want) {
				t.Errorf("context status printed\n%s\nwant it to report %q", status, want)
			}
		}
	}
}

// TestServeManagedRefusesAContextItCannotAnswerIn is what a READY
// multi-repository context is to the query plane today: a context the server
// knows about and will not answer a question in.
//
// Expanding a query over several members is not implemented, and answering in
// the first member would be the one thing worse than refusing — a whole
// repository's worth of code silently outside the scope of an answer that names
// the context the caller asked for. So this holds the refusal, and it is the
// test that has to change when the query plane learns to fan out.
func TestServeManagedRefusesAContextItCannotAnswerIn(t *testing.T) {
	data, _, _ := twoManagedRepositories(t)
	if _, err := createStack(t, data, "stack"); err != nil {
		t.Fatalf("context create: %v", err)
	}
	if _, err := contextRun(t, data, "create", "api-only", "--repo", "api", "--ref", "main"); err != nil {
		t.Fatalf("context create api-only: %v", err)
	}

	session, _ := serveManaged(t, data, demorepo.StartZoekt(t, filepath.Join(data, "zoekt")))

	// It is served, in the sense that the server knows it: a READY context is in
	// the registry whatever it names.
	if got, want := listedContexts(t, session), []string{"api-only", "stack"}; !slices.Equal(got, want) {
		t.Errorf("list_contexts returned %v, want %v", got, want)
	}

	res := callTool(t, session, "search_code", map[string]any{"context": "stack", "query": apiToken})
	if !res.IsError {
		t.Fatalf("search_code in a two-repository context succeeded, want it refused: %s", resultText(t, res))
	}
	if got := errorCode(t, res); got != vacerr.InvalidArgument {
		t.Errorf("search_code in a two-repository context = %q, want %q", got, vacerr.InvalidArgument)
	}
	if text := resultText(t, res); !strings.Contains(text, "api") || !strings.Contains(text, "web") {
		t.Errorf("the refusal reads %q, want it to name both repositories the context is over", text)
	}

	// The server answers otherwise, so the refusal above is about this context
	// and not about a server that could not serve anything.
	if !searchFinds(t, session, "api-only", apiToken) {
		t.Error("search_code(api-only) found nothing, so the refusal above would pass for the wrong reason")
	}
}

// TestContextRetryRebuildsEveryMemberWithoutRepinningAny is AC-level immutability
// across a retry of a context over two repositories: the artifacts are thrown
// away and made again, and every member comes back pinned to exactly the
// revision it was created with, under exactly the names it was created with.
func TestContextRetryRebuildsEveryMemberWithoutRepinningAny(t *testing.T) {
	data, apiSHA, webSHA := twoManagedRepositories(t)

	// A create that gets as far as the graph and stops there, so both members
	// have real partial artifacts and no graph.
	working := os.Getenv("PATH")
	t.Setenv("PATH", brokenCBM(t)+string(os.PathListSeparator)+working)
	if _, err := createStack(t, data, "stack"); err == nil {
		t.Fatal("context create with a failing graph engine returned nil, want an error")
	}
	t.Setenv("PATH", working)

	failed := contextRecord(t, data, "stack")
	if failed.State != managed.ContextFailed {
		t.Fatalf("state = %q, want %s", failed.State, managed.ContextFailed)
	}
	if served(t, data, "stack") {
		t.Error("a FAILED two-repository context is in the registry the query plane reads")
	}

	out, err := contextRun(t, data, "retry", "stack")
	if err != nil {
		t.Fatalf("context retry: %v", err)
	}
	if want := "stack\t" + managed.ContextReady + "\tapi\t" + apiSHA + "\nstack\t" + managed.ContextReady + "\tweb\t" + webSHA + "\n"; out != want {
		t.Errorf("context retry printed %q, want %q", out, want)
	}

	rebuilt := contextRecord(t, data, "stack")
	if !slices.Equal(rebuilt.Members, failed.Members) {
		t.Errorf("the retry changed what the context pins:\n before %+v\n  after %+v", failed.Members, rebuilt.Members)
	}
	if rebuilt.State != managed.ContextReady {
		t.Errorf("state after a retry = %q, want %s", rebuilt.State, managed.ContextReady)
	}
	if _, err := contextRun(t, data, "verify", "stack"); err != nil {
		t.Errorf("context verify after a retry: %v", err)
	}
	for _, member := range rebuilt.Members {
		if err := graphExists(t.Context(), rebuilt.ID, member); err != nil {
			t.Errorf("the graph a retry built for member %s: %v", member.Repository, err)
		}
		worktree := filepath.Join(data, "worktrees", member.Repository, "stack")
		if head := gitOut(t, "-C", worktree, "rev-parse", "HEAD"); head != member.Revision {
			t.Errorf("the worktree of %s after a retry is at %q, want the pinned %q", member.Repository, head, member.Revision)
		}
	}
}

// TestContextVerifyFailsOnAnyMembersArtifact is verification per member: a
// search ref moved onto another commit, or a graph deleted, fails the whole
// context whichever member it belonged to.
//
// The second member is the one broken in each case, because a check that only
// ever looked at the first would pass every one of them.
func TestContextVerifyFailsOnAnyMembersArtifact(t *testing.T) {
	data, _, webSHA := twoManagedRepositories(t)
	if _, err := createStack(t, data, "stack"); err != nil {
		t.Fatalf("context create: %v", err)
	}
	c := contextRecord(t, data, "stack")
	second := c.Members[len(c.Members)-1]
	if second.Repository != "web" {
		t.Fatalf("the second member is %s, want web", second.Repository)
	}
	clone := filepath.Join(data, "repos", "web")

	// A search ref on another commit is SOURCE_MISMATCH: the wrong-version
	// answer this server exists to prevent, whatever the record still says.
	other := commit(t, gitOut(t, "-C", clone, "remote", "get-url", "origin"), "two.go", "package web\n\nfunc Two() {}\n", "two")
	mustGit(t, "-C", clone, "fetch", "--quiet", "origin")
	mustGit(t, "-C", clone, "update-ref", "refs/heads/"+second.Branch, other)
	_, err := contextRun(t, data, "verify", "stack")
	if got := codeFor(t, err); got != vacerr.SourceMismatch {
		t.Errorf("verify with the second member's search ref on another commit = %q, want %q", got, vacerr.SourceMismatch)
	}
	mustGit(t, "-C", clone, "update-ref", "refs/heads/"+second.Branch, webSHA)

	// A worktree checked out somewhere else is the same failure from the source
	// side.
	worktree := filepath.Join(data, "worktrees", "web", "stack")
	mustGit(t, "-C", worktree, "checkout", "-q", "--detach", other)
	if _, err := contextRun(t, data, "verify", "stack"); codeFor(t, err) != vacerr.SourceMismatch {
		t.Errorf("verify with the second member's worktree on another commit = %v, want %q", err, vacerr.SourceMismatch)
	}
	mustGit(t, "-C", worktree, "checkout", "-q", "--detach", webSHA)

	// And a graph deleted out from under the second member is
	// GRAPH_PROVIDER_UNAVAILABLE rather than a context that reports itself fine.
	mustRun(t, managed.CBMCommand, "cli", "delete_project", "--project", second.GraphRef)
	if _, err := contextRun(t, data, "verify", "stack"); codeFor(t, err) != vacerr.GraphProviderUnavailable {
		t.Errorf("verify with the second member's graph gone = %v, want %q", err, vacerr.GraphProviderUnavailable)
	}

	// None of it rewrote the record: verification reports what it found.
	if after := contextRecord(t, data, "stack"); !sameContext(after, c) {
		t.Errorf("verification rewrote the record:\n before %+v\n  after %+v", c, after)
	}
}

// TestContextRemoveTakesEveryMembersArtifactsApart is the removal of a context
// over two repositories: every worktree, every graph and every search ref goes,
// each affected repository's index is rebuilt, and the record is deleted last.
//
// A context of one of those repositories is kept throughout, so nothing here
// passes because everything was destroyed.
func TestContextRemoveTakesEveryMembersArtifactsApart(t *testing.T) {
	data, _, _ := twoManagedRepositories(t)
	if _, err := createStack(t, data, "stack"); err != nil {
		t.Fatalf("context create: %v", err)
	}
	if _, err := contextRun(t, data, "create", "api-only", "--repo", "api", "--ref", "main"); err != nil {
		t.Fatalf("context create api-only: %v", err)
	}
	removed := contextRecord(t, data, "stack")
	kept := contextRecord(t, data, "api-only")

	if out, err := contextRun(t, data, "remove", "stack"); err != nil || out != "stack\tREMOVED\n" {
		t.Fatalf("context remove = %q (err %v)", out, err)
	}

	if _, err := openStore(t, data).Context("stack"); codeFor(t, err) != vacerr.ContextNotFound {
		t.Errorf("the record after the removal = %v, want %q", err, vacerr.ContextNotFound)
	}
	url := demorepo.StartZoekt(t, filepath.Join(data, "zoekt"))
	for _, member := range removed.Members {
		worktree := filepath.Join(data, "worktrees", member.Repository, "stack")
		if _, err := os.Stat(worktree); !os.IsNotExist(err) {
			t.Errorf("the worktree of member %s is still there (err = %v)", member.Repository, err)
		}
		clone := filepath.Join(data, "repos", member.Repository)
		if listed := gitOut(t, "-C", clone, "worktree", "list"); strings.Contains(listed, worktree) {
			t.Errorf("git worktree list in %s =\n%s\nwant the removed worktree pruned", member.Repository, listed)
		}
		if out, err := exec.Command("git", "-C", clone, "rev-parse", "--verify", "refs/heads/"+member.Branch).CombinedOutput(); err == nil {
			t.Errorf("the search ref of member %s survived the removal as %s", member.Repository, strings.TrimSpace(string(out)))
		}
		if err := graphExists(t.Context(), "stack", member); err == nil {
			t.Errorf("codebase-memory-mcp still holds the graph %q of member %s", member.GraphRef, member.Repository)
		}
		indexed, err := searchRefIndexed(t.Context(), url, member)
		if err != nil {
			t.Fatalf("searchRefIndexed(%s): %v", member.Repository, err)
		}
		if indexed {
			t.Errorf("the search ref of member %s is still in the index of %s", member.Repository, member.Repository)
		}
	}
	// web had no other context, so its index went with the last ref in it; api
	// still has one, and it still answers.
	if own, _ := shards(t, data, "web"); len(own) != 0 {
		t.Errorf("shards of web = %v, want none once the only context in it is gone", own)
	}
	if got := contextRecord(t, data, "api-only"); !sameContext(got, kept) {
		t.Errorf("the removal changed the other context:\n before %+v\n  after %+v", kept, got)
	}
	if !searchable(t, url, kept.ID, only(t, kept), apiToken) {
		t.Errorf("%s can no longer find %s after the other context was removed", kept.ID, apiToken)
	}
}

// TestContextRemoveOfATwoRepositoryContextResumes is the crash recovery a
// record cannot express: REMOVING says the artifacts are being taken apart and
// never how far that got, so the same command again re-attempts every member and
// tolerates the ones already gone.
//
// The interruption is produced by taking the first steps the removal takes — the
// state, then one member's graph and checkout — against the same engines the
// command drives, and stopping. That is what a process killed between the two
// members leaves.
func TestContextRemoveOfATwoRepositoryContextResumes(t *testing.T) {
	data, _, _ := twoManagedRepositories(t)
	if _, err := createStack(t, data, "stack"); err != nil {
		t.Fatalf("context create: %v", err)
	}
	c := contextRecord(t, data, "stack")

	c.State = managed.ContextRemoving
	if err := openStore(t, data).PutContext(c); err != nil {
		t.Fatalf("PutContext: %v", err)
	}
	first := c.Members[0]
	mustRun(t, managed.CBMCommand, "cli", "delete_project", "--project", first.GraphRef)
	worktree := filepath.Join(data, "worktrees", first.Repository, "stack")
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatalf("RemoveAll(%s): %v", worktree, err)
	}

	// From the state alone the query plane has stopped serving it, whatever is
	// still on disk.
	if served(t, data, "stack") {
		t.Error("a REMOVING context is in the registry the query plane reads")
	}
	if got := contextRecord(t, data, "stack").State; got != managed.ContextRemoving {
		t.Errorf("state during the interrupted removal = %q, want %s", got, managed.ContextRemoving)
	}

	if out, err := contextRun(t, data, "remove", "stack"); err != nil || out != "stack\tREMOVED\n" {
		t.Fatalf("context remove after an interrupted one = %q (err %v), want it to finish", out, err)
	}
	if _, err := openStore(t, data).Context("stack"); codeFor(t, err) != vacerr.ContextNotFound {
		t.Errorf("the record after the removal finished = %v, want %q", err, vacerr.ContextNotFound)
	}
	for _, member := range c.Members {
		if _, err := os.Stat(filepath.Join(data, "worktrees", member.Repository, "stack")); !os.IsNotExist(err) {
			t.Errorf("the worktree of member %s survived the second removal (err = %v)", member.Repository, err)
		}
		if err := graphExists(t.Context(), "stack", member); err == nil {
			t.Errorf("the graph of member %s survived the second removal", member.Repository)
		}
	}
}

// TestConcurrentLifecycleOnSharedRepositoriesDoesNotDeadlock is the reason the
// repository locks are taken in one order.
//
// Two contexts over the same two repositories, created at the same moment, is
// exactly the interleaving that hangs when each command takes the locks in the
// order it was given them: one holds api and wants web while the other holds web
// and wants api. A deadlock here fails as the package's timeout rather than as
// an assertion, so what the assertions below cover is the other half — that
// serialising them left both contexts, and the shards they share, consistent.
func TestConcurrentLifecycleOnSharedRepositoriesDoesNotDeadlock(t *testing.T) {
	data, apiSHA, webSHA := twoManagedRepositories(t)

	var wg sync.WaitGroup
	for _, id := range []string{"stack-one", "stack-two"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The repositories are named in opposite orders, which is what makes
			// the two commands ask for the same two locks the other way round.
			args := []string{"create", id, "--repo", "api", "--ref", "main", "--repo", "web", "--ref", "main"}
			if id == "stack-two" {
				args = []string{"create", id, "--repo", "web", "--ref", "main", "--repo", "api", "--ref", "main"}
			}
			if _, err := contextRun(t, data, args...); err != nil {
				t.Errorf("concurrent context create %s: %v", id, err)
			}
		}()
	}
	wg.Wait()

	url := demorepo.StartZoekt(t, filepath.Join(data, "zoekt"))
	for _, id := range []string{"stack-one", "stack-two"} {
		c := contextRecord(t, data, id)
		if c.State != managed.ContextReady {
			t.Errorf("%s = %q after concurrent creates, want %s", id, c.State, managed.ContextReady)
		}
		if _, err := contextRun(t, data, "verify", id); err != nil {
			t.Errorf("context verify %s after a concurrent create: %v", id, err)
		}
		// The shards really carry every member of both contexts, asked of Zoekt
		// rather than of the records that claim it: a create that read the older
		// records and finished last would leave a READY context with a member
		// the index has never heard of.
		for _, member := range c.Members {
			indexed, err := searchRefIndexed(t.Context(), url, member)
			if err != nil {
				t.Fatalf("searchRefIndexed(%s, %s): %v", id, member.Repository, err)
			}
			if !indexed {
				t.Errorf("%s is READY but its %s member's search ref %q is not in the index", id, member.Repository, member.Branch)
			}
		}
		// Both contexts pinned the same two commits, whichever order they asked
		// for them in.
		want := map[string]string{"api": apiSHA, "web": webSHA}
		for _, member := range c.Members {
			if member.Revision != want[member.Repository] {
				t.Errorf("%s member %s pins %q, want %q", id, member.Repository, member.Revision, want[member.Repository])
			}
		}
	}
}

// TestRepoRemoveRefusesWhileAMultiRepositoryContextIsAMemberOfIt is the
// dependency check through the command: either clone of a two-repository
// context is one that context still pins a revision in, so removing it is
// refused and the context is named.
func TestRepoRemoveRefusesWhileAMultiRepositoryContextIsAMemberOfIt(t *testing.T) {
	data, _, _ := twoManagedRepositories(t)
	if _, err := createStack(t, data, "stack"); err != nil {
		t.Fatalf("context create: %v", err)
	}

	for _, name := range []string{"api", "web"} {
		_, err := repoRun(t, data, "remove", name)
		if got := codeFor(t, err); got != vacerr.InvalidArgument {
			t.Errorf("repo remove %s = %q, want %q", name, got, vacerr.InvalidArgument)
		}
		if !strings.Contains(err.Error(), "stack") {
			t.Errorf("repo remove %s said %q, want it to name the context that depends on it", name, err)
		}
		if _, err := os.Stat(filepath.Join(data, "repos", name)); err != nil {
			t.Errorf("the clone of %s was deleted by a refused removal: %v", name, err)
		}
	}

	// Removing the context releases both, which is what makes the refusal a
	// step in an order rather than a dead end.
	if _, err := contextRun(t, data, "remove", "stack"); err != nil {
		t.Fatalf("context remove: %v", err)
	}
	for _, name := range []string{"api", "web"} {
		if _, err := repoRun(t, data, "remove", name); err != nil {
			t.Errorf("repo remove %s after the context was gone: %v", name, err)
		}
	}
}

// TestRepoSyncMovesNoMemberOfAMultiRepositoryContext is the invariant this
// project exists for, through the commands: a fetch of both repositories brings
// new commits within reach and leaves every member on exactly the revision it
// was pinned to.
func TestRepoSyncMovesNoMemberOfAMultiRepositoryContext(t *testing.T) {
	data, _, _ := twoManagedRepositories(t)
	if _, err := createStack(t, data, "stack"); err != nil {
		t.Fatalf("context create: %v", err)
	}
	before := contextRecord(t, data, "stack")

	moved := map[string]string{}
	for _, name := range []string{"api", "web"} {
		source := gitOut(t, "-C", filepath.Join(data, "repos", name), "remote", "get-url", "origin")
		moved[name] = commit(t, source, "later.go", "package "+name+"\n\nfunc Later() {}\n", "later")
	}
	if _, err := repoRun(t, data, "sync", "--all"); err != nil {
		t.Fatalf("repo sync --all: %v", err)
	}

	for name, sha := range moved {
		if got := gitOut(t, "-C", filepath.Join(data, "repos", name), "rev-parse", "--verify", sha+"^{commit}"); got != sha {
			t.Errorf("the clone of %s resolves %s to %q after a sync, want the new commit fetched", name, sha, got)
		}
	}
	after := contextRecord(t, data, "stack")
	if !sameContext(after, before) {
		t.Errorf("a sync changed the record:\n before %+v\n  after %+v", before, after)
	}
	for _, member := range after.Members {
		if member.Revision == moved[member.Repository] {
			t.Errorf("member %s moved onto the fetched commit %q", member.Repository, member.Revision)
		}
	}
	if _, err := contextRun(t, data, "verify", "stack"); err != nil {
		t.Errorf("context verify after a sync: %v", err)
	}
}

// TestManagedServerRefusesEveryLifecycleCommandOverSeveralRepositories is
// decision-6 through the CLI: while a server serves the data directory, nothing
// that would take a context apart underneath it runs, however many repositories
// that context is over.
func TestManagedServerRefusesEveryLifecycleCommandOverSeveralRepositories(t *testing.T) {
	data, _, _ := twoManagedRepositories(t)
	if _, err := createStack(t, data, "stack"); err != nil {
		t.Fatalf("context create: %v", err)
	}
	before := contextRecord(t, data, "stack")

	release, err := managed.HoldServerLock(data)
	if err != nil {
		t.Fatalf("HoldServerLock: %v", err)
	}
	defer release()

	for name, args := range map[string][]string{
		"context create": {"create", "another", "--repo", "api", "--ref", "main", "--repo", "web", "--ref", "main"},
		"context retry":  {"retry", "stack"},
		"context remove": {"remove", "stack"},
	} {
		if _, err := contextRun(t, data, args...); codeFor(t, err) != vacerr.InvalidArgument {
			t.Errorf("%s while a managed server runs = %v, want %q", name, err, vacerr.InvalidArgument)
		}
	}
	if _, err := repoRun(t, data, "remove", "api"); codeFor(t, err) != vacerr.InvalidArgument {
		t.Errorf("repo remove while a managed server runs = %v, want %q", err, vacerr.InvalidArgument)
	}

	// The reading commands are not refused, and nothing was half done on the way
	// to a refusal.
	if _, err := contextRun(t, data, "list"); err != nil {
		t.Errorf("context list while a managed server runs: %v", err)
	}
	if after := contextRecord(t, data, "stack"); !sameContext(after, before) {
		t.Errorf("a refused command changed the record:\n before %+v\n  after %+v", before, after)
	}
	if _, err := openStore(t, data).Context("another"); codeFor(t, err) != vacerr.ContextNotFound {
		t.Errorf("a refused create left a record: %v", err)
	}
	if contexts, err := openStore(t, data).Contexts(); err != nil || len(contexts) != 1 {
		t.Errorf("contexts = %+v (err %v), want only the one that was there", contexts, err)
	}
}

// TestAV040DataDirectoryIsServedWithoutConversion is the upgrade, end to end: a
// data directory whose records are exactly what v0.4.0 wrote is opened, checked
// and served by this version with nothing converted and no record rewritten.
//
// The records are what this version writes for a context over one repository,
// which is the same thing — store's compat_test.go is what holds those two to
// being byte for byte identical, and this is what holds the whole installation
// built on them to still working.
func TestAV040DataDirectoryIsServedWithoutConversion(t *testing.T) {
	data, _, _ := twoManagedRepositories(t)
	if _, err := contextRun(t, data, "create", "api-only", "--repo", "api", "--ref", "main"); err != nil {
		t.Fatalf("context create: %v", err)
	}

	// The record on disk is the v0.4.0 spelling: the repository and its three
	// generated fields inline, and no members list.
	body, err := os.ReadFile(filepath.Join(data, "contexts", "api-only.json"))
	if err != nil {
		t.Fatalf("reading the record: %v", err)
	}
	if strings.Contains(string(body), "members") {
		t.Errorf("a one-repository record carries a members list:\n%s", body)
	}
	before := contextRecord(t, data, "api-only")

	// And it verifies, which is the check that would fail if the names this
	// version generates had moved away from the ones v0.4.0 generated.
	if _, err := contextRun(t, data, "verify", "api-only"); err != nil {
		t.Fatalf("context verify of a record in the v0.4.0 spelling: %v", err)
	}
	session, _ := serveManaged(t, data, demorepo.StartZoekt(t, filepath.Join(data, "zoekt")))
	if got, want := listedContexts(t, session), []string{"api-only"}; !slices.Equal(got, want) {
		t.Errorf("list_contexts returned %v, want %v", got, want)
	}
	if !searchFinds(t, session, "api-only", apiToken) {
		t.Error("a context in the v0.4.0 spelling cannot be searched")
	}
	if after := contextRecord(t, data, "api-only"); !sameContext(after, before) {
		t.Errorf("serving rewrote the record:\n before %+v\n  after %+v", before, after)
	}
}
