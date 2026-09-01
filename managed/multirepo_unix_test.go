//go:build unix

package managed

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// The lifecycle of a context over several repositories, run against real git
// and stubbed engines.
//
// The engines are stubs and not the real Zoekt and CBM, which is the opposite
// of what cmd/vacmcp's integration tests do and is deliberate: what is checked
// here is which members a stage reaches, in which order, and what the record
// says afterwards — questions about this package's own control flow, whose
// answers a real indexer would only make slower and machine-dependent. Whether
// the artifacts those stages build are usable is the integration tests'
// question, and no stub is allowed near it.
//
// Unix only, because the stubs are shell scripts. The rules they exercise hold
// everywhere; a Windows run gets them from the integration tier.

// failingIndexer makes the stub indexer fail while it is set, which is how a
// create is stopped in the same place on every machine.
const failingIndexer = "VACMCP_TEST_INDEXER_FAILS"

// stubEngines puts a graph engine that succeeds and an indexer that fails in
// front of PATH.
//
// A create therefore always stops at INDEXING_SEARCH, with the stages before it
// really run — the record written, the revisions pinned, every member checked
// out. That is a context with real partial artifacts, which is what a retry and
// a removal have to cope with. Clearing failingIndexer lets a later rebuild get
// through, for the tests that need one.
func stubEngines(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	for name, script := range map[string]string{
		indexer: "#!/bin/sh\nif [ -n \"$" + failingIndexer + "\" ]; then echo 'zoekt-git-index: no' >&2; exit 1; fi\nexit 0\n",
		CBMCommand: "#!/bin/sh\ncase \"$2\" in\n" +
			"  index_repository) echo '{\"status\":\"indexed\"}' ;;\n" +
			"  list_projects) echo '{\"projects\":[]}' ;;\n" +
			"esac\nexit 0\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil {
			t.Fatalf("WriteFile(%s): %v", name, err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(failingIndexer, "1")
}

// failedStack creates a two-repository context that stops at INDEXING_SEARCH,
// and returns the data directory and the commits its members pinned.
func failedStack(t *testing.T, id string) (data, apiSHA, webSHA string) {
	t.Helper()
	data, apiSHA, webSHA = twoRepositories(t)
	stubEngines(t)

	m, err := NewContextManager(data)
	if err != nil {
		t.Fatalf("NewContextManager: %v", err)
	}
	if _, err := m.Create(context.Background(), id, []Pin{
		{Repository: "api", Ref: "main"},
		{Repository: "web", Ref: "main"},
	}); err == nil {
		t.Fatal("Create with a failing indexer returned nil, want an error")
	}
	return data, apiSHA, webSHA
}

// TestCreatePinsEveryMemberBeforeAnyArtifactIsBuilt is the record a
// two-repository create writes: one context, two members, each resolved once
// and pinned to its own full commit SHA, with names of its own.
//
// It also covers what a member failing a stage does to the context: the graph
// stage was never reached, so the context is FAILED whole rather than a context
// with the members that worked. The query plane cannot see it at all, and the
// management plane can see all of it.
func TestCreatePinsEveryMemberBeforeAnyArtifactIsBuilt(t *testing.T) {
	data, apiSHA, webSHA := failedStack(t, "stack")
	s := openStore(t, data)

	c, err := s.Context("stack")
	if err != nil {
		t.Fatalf("Context(stack): %v", err)
	}
	if c.State != ContextFailed {
		t.Errorf("state = %q, want %s: a member that failed a stage fails the context", c.State, ContextFailed)
	}
	if len(c.Members) != 2 {
		t.Fatalf("record carries %d members, want the two repositories it was created over: %+v", len(c.Members), c)
	}
	for i, want := range []struct{ repository, revision string }{{"api", apiSHA}, {"web", webSHA}} {
		if got := c.Members[i]; got.Repository != want.repository || got.Revision != want.revision {
			t.Errorf("member %d = %+v, want %s pinned to %s", i, got, want.repository, want.revision)
		}
		if len(c.Members[i].Revision) != fullSHA {
			t.Errorf("member %s pins %q, want a full commit SHA", c.Members[i].Repository, c.Members[i].Revision)
		}
	}
	if err := verifyIdentity(c); err != nil {
		t.Errorf("the record a create wrote does not verify its own names: %v", err)
	}

	// Every member really got through the stage before the one that failed:
	// PREPARING_SOURCE is over the whole member list, so both checkouts are
	// there and both are on their own pinned commit. That is what a stage
	// running over every member looks like from outside, as against one running
	// over the first.
	for _, m := range c.Members {
		_, worktree, err := memberPaths(s, c.ID, m)
		if err != nil {
			t.Fatalf("memberPaths(%s): %v", m.Repository, err)
		}
		if head := gitOut(t, "-C", worktree, "rev-parse", "HEAD"); head != m.Revision {
			t.Errorf("the worktree of %s is at %q, want the pinned %q", m.Repository, head, m.Revision)
		}
	}
	// And the stage that failed got as far as asserting the first repository's
	// search ref before the indexer refused it, which is where the whole
	// context stopped: the second repository was never reached, because a
	// member failing a stage ends the stage rather than being skipped over.
	first := c.Members[0]
	firstClone, _, err := memberPaths(s, c.ID, first)
	if err != nil {
		t.Fatalf("memberPaths: %v", err)
	}
	if ref := gitOut(t, "-C", firstClone, "rev-parse", "--verify", "refs/heads/"+first.Branch); ref != first.Revision {
		t.Errorf("the search ref of %s is at %q, want the pinned %q", first.Repository, ref, first.Revision)
	}

	// Not readable, and indistinguishable from a context that was never
	// managed: a context is READY or it is not there.
	if _, err := readyContext(s, "stack"); codeFor(t, err) != vacerr.ContextNotFound {
		t.Errorf("the query plane reads a FAILED context (err = %v), want %q", err, vacerr.ContextNotFound)
	}

	// The management plane still has all of it, or a failure would be a context
	// nobody could inspect, remove or retry.
	m, err := NewContextManager(data)
	if err != nil {
		t.Fatalf("NewContextManager: %v", err)
	}
	list, err := m.List()
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	if len(list) != 1 || len(list[0].Members) != 2 || list[0].State != ContextFailed {
		t.Errorf("List() = %+v, want the FAILED context with both its members", list)
	}
	status, err := m.Status("stack")
	if err != nil {
		t.Fatalf("Status(stack): %v", err)
	}
	if len(status.Artifacts) != 2 {
		t.Fatalf("Status reports %d members, want 2: %+v", len(status.Artifacts), status)
	}
	for i, member := range c.Members {
		got := status.Artifacts[i]
		if got.Repository != member.Repository || got.Revision != member.Revision ||
			got.SearchRef != member.Branch || got.GraphRef != member.GraphRef || got.Worktree == "" {
			t.Errorf("Status member %d = %+v, want the artifacts of %+v", i, got, member)
		}
	}

	// And the create is runnable again under another name, which is what a
	// refused one leaves behind and a failed one does not: this id is taken for
	// good, whatever state it stopped in.
	if _, err := m.Create(context.Background(), "stack", []Pin{{Repository: "api", Ref: "main"}}); codeFor(t, err) != vacerr.InvalidArgument {
		t.Errorf("Create over a FAILED context = %v, want %q", err, vacerr.InvalidArgument)
	}
}

// TestRetryDiscardsEveryMemberBeforeItRebuildsAnything is the order a retry
// runs in, and the reason it has that order: what an interrupted run left is
// thrown away for every member first, so a retry that itself fails part way
// through has not rebuilt one member on top of another's leftovers.
//
// The rebuild is made to fail on the first member, by taking its clone away
// after the create. If the retry discarded and rebuilt member by member, the
// second member's checkout would never have been touched. It is gone, so the
// discarding ran over all of them before the first rebuild was attempted.
//
// Nothing re-resolves a revision, which is checked the only way it can be: the
// record afterwards pins exactly what it pinned before, names and all.
func TestRetryDiscardsEveryMemberBeforeItRebuildsAnything(t *testing.T) {
	data, _, _ := failedStack(t, "stack")
	s := openStore(t, data)
	before, err := s.Context("stack")
	if err != nil {
		t.Fatalf("Context(stack): %v", err)
	}

	// Something of each member's, to see whether the retry threw it away.
	markers := map[string]string{}
	for _, m := range before.Members {
		_, worktree, err := memberPaths(s, before.ID, m)
		if err != nil {
			t.Fatalf("memberPaths: %v", err)
		}
		marker := filepath.Join(worktree, "left-behind")
		if err := os.WriteFile(marker, []byte("from the failed create"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		markers[m.Repository] = marker
	}

	// The first member's clone goes, so its rebuild is the one that fails and
	// the second member is never rebuilt.
	if err := os.RemoveAll(filepath.Join(data, "repos", "api")); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	m, err := NewContextManager(data)
	if err != nil {
		t.Fatalf("NewContextManager: %v", err)
	}
	if _, err := m.Retry(context.Background(), "stack"); err == nil {
		t.Fatal("Retry with the first member's clone gone returned nil, want an error")
	}

	for repository, marker := range markers {
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Errorf("the worktree of %s still holds what the failed create left (err = %v), want every member discarded before anything was rebuilt", repository, err)
		}
	}

	after, err := s.Context("stack")
	if err != nil {
		t.Fatalf("Context(stack) after the retry: %v", err)
	}
	if !slices.Equal(after.Members, before.Members) {
		t.Errorf("the retry changed what the context pins:\n before %+v\n  after %+v", before.Members, after.Members)
	}
	if after.State != ContextFailed {
		t.Errorf("state after a failed retry = %q, want %s", after.State, ContextFailed)
	}
}

// TestRemoveTakesEveryMemberApart is what a removal leaves: no record, and no
// checkout and no search ref of any member, in any of the repositories the
// context reached. Another context of the same repositories keeps both.
//
// Whether the shards a rebuild leaves behind are right is Zoekt's answer to
// give, and cmd/vacmcp's integration tests ask it. What is here is that every
// member is reached, which is this package's own.
func TestRemoveTakesEveryMemberApart(t *testing.T) {
	data, apiSHA, webSHA := failedStack(t, "stack")
	s := openStore(t, data)
	removed, err := s.Context("stack")
	if err != nil {
		t.Fatalf("Context(stack): %v", err)
	}

	// A context of the same two repositories that is not being removed, whose
	// artifacts the removal must leave alone.
	kept := stack(t, "kept", apiSHA, webSHA, ContextReady)
	if err := s.PutContext(kept); err != nil {
		t.Fatalf("PutContext(kept): %v", err)
	}
	for _, member := range kept.Members {
		repoDir, worktree, err := memberPaths(s, kept.ID, member)
		if err != nil {
			t.Fatalf("memberPaths: %v", err)
		}
		if err := os.MkdirAll(worktree, 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		mustGit(t, "-C", repoDir, "update-ref", "refs/heads/"+member.Branch, member.Revision)
	}

	// The removal rebuilds the index of every repository it touched, so the
	// indexer has to be one that works from here on.
	t.Setenv(failingIndexer, "")

	m, err := NewContextManager(data)
	if err != nil {
		t.Fatalf("NewContextManager: %v", err)
	}
	if err := m.Remove(context.Background(), "stack"); err != nil {
		t.Fatalf("Remove(stack): %v", err)
	}

	if _, err := s.Context("stack"); codeFor(t, err) != vacerr.ContextNotFound {
		t.Errorf("the record after the removal = %v, want %q", err, vacerr.ContextNotFound)
	}
	for _, member := range removed.Members {
		repoDir, worktree, err := memberPaths(s, removed.ID, member)
		if err != nil {
			t.Fatalf("memberPaths: %v", err)
		}
		if _, err := os.Stat(worktree); !os.IsNotExist(err) {
			t.Errorf("the worktree of %s is still at %s (err = %v)", member.Repository, worktree, err)
		}
		if out, err := gitOutput(t.Context(), "-C", repoDir, "rev-parse", "--verify", "refs/heads/"+member.Branch); err == nil {
			t.Errorf("the search ref of %s is still in the clone as %s", member.Repository, out)
		}
	}

	// And the other context of the same repositories is untouched: none of the
	// above passes because everything was destroyed.
	survivor, err := s.Context("kept")
	if err != nil {
		t.Fatalf("Context(kept) after the other removal: %v", err)
	}
	if !slices.Equal(survivor.Members, kept.Members) {
		t.Errorf("the removal changed the other context:\n before %+v\n  after %+v", kept.Members, survivor.Members)
	}
	for _, member := range kept.Members {
		repoDir, worktree, err := memberPaths(s, kept.ID, member)
		if err != nil {
			t.Fatalf("memberPaths: %v", err)
		}
		if _, err := os.Stat(worktree); err != nil {
			t.Errorf("the removal took the worktree of %s from the context that was kept: %v", member.Repository, err)
		}
		if got := gitOut(t, "-C", repoDir, "rev-parse", "--verify", "refs/heads/"+member.Branch); got != member.Revision {
			t.Errorf("the removal moved the search ref of the kept context in %s to %q", member.Repository, got)
		}
	}
}

// TestRemoveFinishesAfterItWasInterrupted is the crash recovery a record cannot
// express: REMOVING says the artifacts are being taken apart and never how far
// that got, so running the same command again has to re-attempt every member
// and tolerate the ones already gone.
//
// The interruption is reproduced by leaving the record in REMOVING with one
// member's checkout already deleted, which is exactly what a process killed
// between the two members leaves.
func TestRemoveFinishesAfterItWasInterrupted(t *testing.T) {
	data, _, _ := failedStack(t, "stack")
	s := openStore(t, data)
	c, err := s.Context("stack")
	if err != nil {
		t.Fatalf("Context(stack): %v", err)
	}

	_, first, err := memberPaths(s, c.ID, c.Members[0])
	if err != nil {
		t.Fatalf("memberPaths: %v", err)
	}
	if err := os.RemoveAll(first); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	c.State = ContextRemoving
	if err := s.PutContext(c); err != nil {
		t.Fatalf("PutContext: %v", err)
	}

	// From the state alone, the query plane has already stopped serving it.
	if _, err := readyContext(s, "stack"); codeFor(t, err) != vacerr.ContextNotFound {
		t.Errorf("the query plane reads a REMOVING context (err = %v), want %q", err, vacerr.ContextNotFound)
	}
	// And a retry is not the way out: what ends REMOVING is the removal.
	m, err := NewContextManager(data)
	if err != nil {
		t.Fatalf("NewContextManager: %v", err)
	}
	if _, err := m.Retry(context.Background(), "stack"); codeFor(t, err) != vacerr.InvalidArgument {
		t.Errorf("Retry of a REMOVING context = %v, want %q", err, vacerr.InvalidArgument)
	}

	if err := m.Remove(context.Background(), "stack"); err != nil {
		t.Fatalf("Remove after an interrupted removal: %v", err)
	}
	if _, err := s.Context("stack"); codeFor(t, err) != vacerr.ContextNotFound {
		t.Errorf("the record after the removal finished = %v, want %q", err, vacerr.ContextNotFound)
	}
	for _, member := range c.Members {
		_, worktree, err := memberPaths(s, c.ID, member)
		if err != nil {
			t.Fatalf("memberPaths: %v", err)
		}
		if _, err := os.Stat(worktree); !os.IsNotExist(err) {
			t.Errorf("the worktree of %s survived the second removal (err = %v)", member.Repository, err)
		}
	}
}

// TestManagedServerRefusesEveryLifecycleCommand is decision-6 for a context
// over several repositories: while a server serves the data directory, nothing
// that would take a context apart underneath it runs, and the refusal is asked
// before any repository's own lock so it is one answer for the whole command
// rather than one per member.
func TestManagedServerRefusesEveryLifecycleCommand(t *testing.T) {
	data, apiSHA, webSHA := twoRepositories(t)
	if err := openStore(t, data).PutContext(stack(t, "stack", apiSHA, webSHA, ContextFailed)); err != nil {
		t.Fatalf("PutContext: %v", err)
	}

	release, err := HoldServerLock(data)
	if err != nil {
		t.Fatalf("HoldServerLock: %v", err)
	}
	defer release()

	contexts, err := NewContextManager(data)
	if err != nil {
		t.Fatalf("NewContextManager: %v", err)
	}
	repositories, err := NewRepositoryManager(data)
	if err != nil {
		t.Fatalf("NewRepositoryManager: %v", err)
	}
	for name, refused := range map[string]func() error{
		"context create": func() error {
			_, err := contexts.Create(context.Background(), "another", []Pin{
				{Repository: "api", Ref: "main"},
				{Repository: "web", Ref: "main"},
			})
			return err
		},
		"context retry":  func() error { _, err := contexts.Retry(context.Background(), "stack"); return err },
		"context remove": func() error { return contexts.Remove(context.Background(), "stack") },
		"repo remove":    func() error { return repositories.Remove("api") },
	} {
		if got := codeFor(t, refused()); got != vacerr.InvalidArgument {
			t.Errorf("%s while a managed server runs = %q, want %q", name, got, vacerr.InvalidArgument)
		}
	}

	// Nothing was half done on the way to being refused: the record is as it
	// was and no context was created.
	list, err := contexts.List()
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	if len(list) != 1 || list[0].ID != "stack" || list[0].State != ContextFailed {
		t.Errorf("List() = %+v, want the one record, untouched", list)
	}
}
