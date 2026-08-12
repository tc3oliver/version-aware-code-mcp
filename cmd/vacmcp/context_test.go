package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/internal/demorepo"
	"github.com/tc3oliver/version-aware-code-mcp/store"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// The context commands are tested against real git repositories, cloned by the
// real `repo add`, never against a fake git. What is being checked is that a
// branch, a tag and an abbreviation all name the same commit and that the
// commit written down is the one git resolved — which only a real object
// database can answer. The helpers these tests share with the repo commands
// (sourceRepo, commit, mustGit, gitOut, repoRun, openStore, codeFor) are in
// repo_test.go.

// contextRun runs one `vacmcp context` command against dataDir through the
// top-level dispatch, so the CLI wiring is exercised too, and returns what it
// printed.
func contextRun(t *testing.T, dataDir string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := run(append(append([]string{"context"}, args...), "--data-dir", dataDir), &out)
	return out.String(), err
}

// managed returns a data directory with the repository "demo" added, cloned
// from a source that has a tag and a second branch, along with the commit that
// main and that second branch point at. The two commits are different, so a
// test can tell which ref a context actually resolved.
func managed(t *testing.T) (data, mainSHA, branchSHA string) {
	t.Helper()
	// Creating a context indexes it into Zoekt and builds its CBM graph, so
	// these tests need both real engines — and, for CBM, must not leave the
	// graphs they build in its store, which outlives the temporary data
	// directory below.
	requireIndexer(t)
	cbmOrSkip(t)

	source := sourceRepo(t)
	mainSHA = gitOut(t, "-C", source, "rev-parse", "HEAD")
	mustGit(t, "-C", source, "tag", "v1")

	// A branch other than the one HEAD points at, and with a slash in its name:
	// in a --no-checkout clone it exists only as a remote-tracking ref, which is
	// the case the literal ref cannot resolve on its own.
	mustGit(t, "-C", source, "checkout", "-q", "-b", "release/2.x")
	branchSHA = commit(t, source, "two.txt", "two\n", "two")
	mustGit(t, "-C", source, "checkout", "-q", "main")
	if mainSHA == branchSHA {
		t.Fatal("the branch commit did not move off main")
	}

	data = t.TempDir()
	t.Cleanup(func() { discardGraphs(t, data) })
	if _, err := repoRun(t, data, "add", "demo", "--url", source); err != nil {
		t.Fatalf("repo add: %v", err)
	}
	return data, mainSHA, branchSHA
}

// contextRecord returns the stored record of id.
func contextRecord(t *testing.T, dataDir, id string) store.Context {
	t.Helper()
	c, err := openStore(t, dataDir).Context(id)
	if err != nil {
		t.Fatalf("Context(%s): %v", id, err)
	}
	return c
}

// TestContextCreatePinsEveryKindOfRefToTheSameFullSHA is AC #1: a branch, a tag
// and an abbreviated SHA naming one commit all pin that commit's full SHA, and
// a branch that is only a remote-tracking ref in the clone resolves too.
func TestContextCreatePinsEveryKindOfRefToTheSameFullSHA(t *testing.T) {
	data, mainSHA, branchSHA := managed(t)

	for _, tc := range []struct{ id, ref, want string }{
		{"by-branch", "main", mainSHA},
		{"by-tag", "v1", mainSHA},
		{"by-short-sha", mainSHA[:8], mainSHA},
		{"by-full-sha", mainSHA, mainSHA},
		{"by-remote-branch", "release/2.x", branchSHA},
	} {
		out, err := contextRun(t, data, "create", tc.id, "--repo", "demo", "--ref", tc.ref)
		if err != nil {
			t.Fatalf("context create %s --ref %s: %v", tc.id, tc.ref, err)
		}
		if want := tc.id + "\t" + contextReady + "\t" + tc.want + "\n"; out != want {
			t.Errorf("context create printed %q, want %q", out, want)
		}

		c := contextRecord(t, data, tc.id)
		if c.Revision != tc.want {
			t.Errorf("--ref %s pinned %q, want %q", tc.ref, c.Revision, tc.want)
		}
		// The stored revision is a full commit SHA and nothing that could move:
		// the ref that was typed must not survive into the record.
		if len(c.Revision) != fullSHA || strings.TrimLeft(c.Revision, "0123456789abcdef") != "" {
			t.Errorf("--ref %s pinned %q, want %d hexadecimal digits", tc.ref, c.Revision, fullSHA)
		}
		if c.Repository != "demo" || c.State != contextReady {
			t.Errorf("record = %+v, want repository demo in state %s", c, contextReady)
		}
	}
}

// TestContextCreateRefusesWhatItCannotResolve keeps a context from being
// recorded at all unless its revision is a commit that is really there. A ref
// beginning with a dash is in the table because it must reach git as a revision
// git cannot find, never as an option git obeys.
func TestContextCreateRefusesWhatItCannotResolve(t *testing.T) {
	data, mainSHA, _ := managed(t)

	for _, tc := range []struct {
		name string
		args []string
		want vacerr.Code
	}{
		{"unknown ref", []string{"create", "ctx", "--repo", "demo", "--ref", "no-such-branch"}, vacerr.RevisionNotFound},
		{"a ref that is a range", []string{"create", "ctx", "--repo", "demo", "--ref", "main..release/2.x"}, vacerr.RevisionNotFound},
		{"a ref that is not a commit", []string{"create", "ctx", "--repo", "demo", "--ref", "main^{tree}"}, vacerr.RevisionNotFound},
		{"a ref that looks like an option", []string{"create", "ctx", "--repo", "demo", "--ref", "-version"}, vacerr.RevisionNotFound},
		{"unmanaged repository", []string{"create", "ctx", "--repo", "absent", "--ref", mainSHA}, vacerr.RepositoryNotFound},
	} {
		_, err := contextRun(t, data, tc.args...)
		if got := codeFor(t, err); got != tc.want {
			t.Errorf("%s: code = %q, want %q", tc.name, got, tc.want)
		}
	}

	if contexts, err := openStore(t, data).Contexts(); err != nil || len(contexts) != 0 {
		t.Errorf("contexts = %+v (err %v), want a refused create to record nothing", contexts, err)
	}
}

func TestContextCreateRejectsAnUnusableName(t *testing.T) {
	data, _, _ := managed(t)

	// "a..b" is in the list because the store's allowlist admits it while a git
	// ref may not contain it, so it is the one name the generated search ref
	// would be broken by.
	for _, id := range []string{"../escape", "a/b", "..", "a..b", ".hidden", ""} {
		if _, err := contextRun(t, data, "create", id, "--repo", "demo", "--ref", "main"); err == nil {
			t.Errorf("context create %q returned nil, want an error", id)
		}
	}
	if contexts, err := openStore(t, data).Contexts(); err != nil || len(contexts) != 0 {
		t.Errorf("contexts = %+v (err %v), want an unusable name to record nothing", contexts, err)
	}
}

func TestContextCreateRequiresARepositoryAndARef(t *testing.T) {
	data, _, _ := managed(t)

	if _, err := contextRun(t, data, "create", "ctx", "--ref", "main"); err == nil {
		t.Error("context create without --repo returned nil, want an error")
	}
	if _, err := contextRun(t, data, "create", "ctx", "--repo", "demo"); err == nil {
		t.Error("context create without --ref returned nil, want an error")
	}
	if _, err := contextRun(t, data, "create"); err == nil {
		t.Error("context create without NAME returned nil, want an error")
	}
}

// TestContextCreateRefusesToReplaceAManagedContext is half of AC #2: creating
// over a managed context is refused rather than repinning it, which is what
// makes "another revision is another context" true of the code and not just of
// the documentation.
func TestContextCreateRefusesToReplaceAManagedContext(t *testing.T) {
	data, mainSHA, branchSHA := managed(t)

	if _, err := contextRun(t, data, "create", "app", "--repo", "demo", "--ref", "main"); err != nil {
		t.Fatalf("context create: %v", err)
	}
	before := contextRecord(t, data, "app")

	_, err := contextRun(t, data, "create", "app", "--repo", "demo", "--ref", "release/2.x")
	if got := codeFor(t, err); got != vacerr.InvalidArgument {
		t.Errorf("second context create code = %q, want %q", got, vacerr.InvalidArgument)
	}

	after := contextRecord(t, data, "app")
	if after != before {
		t.Errorf("the record changed across a refused create:\n before %+v\n  after %+v", before, after)
	}
	if after.Revision != mainSHA {
		t.Errorf("revision = %q, want the original %q", after.Revision, mainSHA)
	}
	if after.Revision == branchSHA {
		t.Error("the refused create repinned the context to the other revision")
	}
}

// TestContextRecordIsNeverRewritten is the other half of AC #2: every command
// that is not create or remove leaves the record exactly as it was, down to the
// timestamp any rewrite would have stamped. There is no subcommand that edits
// one, so this is the whole surface a pinned revision could move through.
func TestContextRecordIsNeverRewritten(t *testing.T) {
	data, mainSHA, _ := managed(t)
	if _, err := contextRun(t, data, "create", "app", "--repo", "demo", "--ref", "main"); err != nil {
		t.Fatalf("context create: %v", err)
	}
	before := contextRecord(t, data, "app")

	for _, args := range [][]string{{"list"}, {"status", "app"}, {"verify", "app"}} {
		if _, err := contextRun(t, data, args...); err != nil {
			t.Fatalf("context %s: %v", strings.Join(args, " "), err)
		}
		if after := contextRecord(t, data, "app"); after != before {
			t.Errorf("context %s rewrote the record:\n before %+v\n  after %+v", strings.Join(args, " "), before, after)
		}
	}

	// The source moving on is exactly what a context is pinned against: main
	// now points somewhere else and the context does not.
	source := gitOut(t, "-C", filepath.Join(data, "repos", "demo"), "remote", "get-url", "origin")
	moved := commit(t, source, "three.txt", "three\n", "three")
	if _, err := repoRun(t, data, "sync", "demo"); err != nil {
		t.Fatalf("repo sync: %v", err)
	}
	if _, err := contextRun(t, data, "verify", "app"); err != nil {
		t.Fatalf("context verify after sync: %v", err)
	}
	after := contextRecord(t, data, "app")
	if after != before {
		t.Errorf("the record changed across a sync and a verify:\n before %+v\n  after %+v", before, after)
	}
	if after.Revision != mainSHA || after.Revision == moved {
		t.Errorf("revision = %q, want it still pinned to %q and not to %q", after.Revision, mainSHA, moved)
	}
}

// TestContextCreateGeneratesTheInternalNames is AC #4: the user gives a name, a
// repository and a ref, and the search ref, the graph project and the worktree
// path all come out of that.
func TestContextCreateGeneratesTheInternalNames(t *testing.T) {
	data, mainSHA, _ := managed(t)
	for _, id := range []string{"app-v1", "app-v2"} {
		if _, err := contextRun(t, data, "create", id, "--repo", "demo", "--ref", "main"); err != nil {
			t.Fatalf("context create %s: %v", id, err)
		}
	}

	first, second := contextRecord(t, data, "app-v1"), contextRecord(t, data, "app-v2")
	if first.Branch != "vacmcp/app-v1-"+mainSHA[:shortSHA] {
		t.Errorf("search ref = %q, want it derived from the context name and the short SHA", first.Branch)
	}
	if first.GraphRef != "vacmcp-demo-app-v1-"+mainSHA[:shortSHA] {
		t.Errorf("graph ref = %q, want it derived from the repository, the context name and the short SHA", first.GraphRef)
	}
	// Two contexts of one repository at one revision still get names of their
	// own, or removing either would take the other's artifacts with it.
	if first.Branch == second.Branch || first.GraphRef == second.GraphRef {
		t.Errorf("app-v1 %+v and app-v2 %+v share a generated name", first, second)
	}
	// The generated search ref is one git will accept, since the search index
	// lifecycle has to be able to create it.
	mustGit(t, "check-ref-format", "refs/heads/"+first.Branch)

	out, err := contextRun(t, data, "status", "app-v1")
	if err != nil {
		t.Fatalf("context status: %v", err)
	}
	worktree := filepath.Join(data, "worktrees", "demo", "app-v1")
	for _, want := range []string{"app-v1", "demo", mainSHA, contextReady, first.Branch, first.GraphRef, worktree} {
		if !strings.Contains(out, want) {
			t.Errorf("context status printed\n%s\nwant it to report %q", out, want)
		}
	}
}

// TestContextVerifyReportsWhetherTheRevisionIsStillThere covers the first of
// the verification's checks: the pinned commit is still in the repository's
// object database. It writes nothing either way.
func TestContextVerifyReportsWhetherTheRevisionIsStillThere(t *testing.T) {
	data, mainSHA, _ := managed(t)
	if _, err := contextRun(t, data, "create", "app", "--repo", "demo", "--ref", "main"); err != nil {
		t.Fatalf("context create: %v", err)
	}

	out, err := contextRun(t, data, "verify", "app")
	if err != nil {
		t.Fatalf("context verify: %v", err)
	}
	if want := "app\tOK\t" + mainSHA + "\n"; out != want {
		t.Errorf("context verify printed %q, want %q", out, want)
	}

	// A clone that is gone is a context that can no longer be served, and
	// verify is what says so instead of a tool call finding out later.
	before := contextRecord(t, data, "app")
	if err := os.RemoveAll(filepath.Join(data, "repos", "demo")); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	_, err = contextRun(t, data, "verify", "app")
	if got := codeFor(t, err); got != vacerr.RevisionNotFound {
		t.Errorf("context verify without the clone code = %q, want %q", got, vacerr.RevisionNotFound)
	}
	if after := contextRecord(t, data, "app"); after != before {
		t.Errorf("a failed verify rewrote the record:\n before %+v\n  after %+v", before, after)
	}

	if _, err := contextRun(t, data, "verify", "absent"); codeFor(t, err) != vacerr.ContextNotFound {
		t.Errorf("context verify of an unmanaged context = %v, want %q", err, vacerr.ContextNotFound)
	}
}

// TestContextRemoveOnlyAffectsItsOwnContext is AC #3: the removed context's
// record and worktree go, and another context of the same repository keeps
// both.
func TestContextRemoveOnlyAffectsItsOwnContext(t *testing.T) {
	data, mainSHA, _ := managed(t)
	for _, id := range []string{"drop", "keep"} {
		if _, err := contextRun(t, data, "create", id, "--repo", "demo", "--ref", "main"); err != nil {
			t.Fatalf("context create %s: %v", id, err)
		}
	}

	// Stand-ins for the checkouts the source-preparation step puts here, so the
	// removal has something of each context's to take or to leave.
	worktrees := map[string]string{}
	for _, id := range []string{"drop", "keep"} {
		dir := filepath.Join(data, "worktrees", "demo", id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte(id), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		worktrees[id] = dir
	}
	before := contextRecord(t, data, "keep")

	out, err := contextRun(t, data, "remove", "drop")
	if err != nil {
		t.Fatalf("context remove: %v", err)
	}
	if out != "drop\tREMOVED\n" {
		t.Errorf("context remove printed %q, want it to report drop as removed", out)
	}

	if _, err := openStore(t, data).Context("drop"); codeFor(t, err) != vacerr.ContextNotFound {
		t.Errorf("Context(drop) after remove = %v, want %q", err, vacerr.ContextNotFound)
	}
	if _, err := os.Stat(worktrees["drop"]); !os.IsNotExist(err) {
		t.Errorf("the worktree of drop is still there (err = %v)", err)
	}

	if after := contextRecord(t, data, "keep"); after != before {
		t.Errorf("removing drop changed keep:\n before %+v\n  after %+v", before, after)
	}
	if body, err := os.ReadFile(filepath.Join(worktrees["keep"], "file.txt")); err != nil || string(body) != "keep" {
		t.Errorf("the worktree of keep = %q (err %v), want it untouched", body, err)
	}
	// The clone the removed context pinned a revision in is not the removed
	// context's to delete: another context is still pinned in it.
	if got := gitOut(t, "-C", filepath.Join(data, "repos", "demo"), "rev-parse", "--verify", mainSHA+"^{commit}"); got != mainSHA {
		t.Errorf("the clone resolves %s to %q after a context removal, want it untouched", mainSHA, got)
	}

	// Removing it again is the not-found error, and a context whose worktree
	// was never created is removable all the same.
	if _, err := contextRun(t, data, "remove", "drop"); codeFor(t, err) != vacerr.ContextNotFound {
		t.Errorf("second context remove = %v, want %q", err, vacerr.ContextNotFound)
	}
	if err := os.RemoveAll(worktrees["keep"]); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if _, err := contextRun(t, data, "remove", "keep"); err != nil {
		t.Errorf("removing a context with no worktree: %v", err)
	}
}

func TestContextListReportsEveryContext(t *testing.T) {
	data, mainSHA, branchSHA := managed(t)

	out, err := contextRun(t, data, "list")
	if err != nil {
		t.Fatalf("context list on a data directory with no contexts: %v", err)
	}
	if out != "" {
		t.Errorf("context list printed %q, want nothing", out)
	}

	for id, ref := range map[string]string{"beta": "release/2.x", "alpha": "main"} {
		if _, err := contextRun(t, data, "create", id, "--repo", "demo", "--ref", ref); err != nil {
			t.Fatalf("context create %s: %v", id, err)
		}
	}

	out, err = contextRun(t, data, "list")
	if err != nil {
		t.Fatalf("context list: %v", err)
	}
	want := fmt.Sprintf("alpha\tdemo\t%s\t%s\nbeta\tdemo\t%s\t%s\n", mainSHA, contextReady, branchSHA, contextReady)
	if out != want {
		t.Errorf("context list printed\n%q\nwant\n%q", out, want)
	}
}

func TestContextStatusOfAnUnmanagedContext(t *testing.T) {
	data, _, _ := managed(t)
	if _, err := contextRun(t, data, "status", "absent"); codeFor(t, err) != vacerr.ContextNotFound {
		t.Errorf("context status of an unmanaged context, want %q", vacerr.ContextNotFound)
	}
	if _, err := contextRun(t, data, "status"); err == nil {
		t.Error("context status without NAME returned nil, want an error")
	}
}

// TestContextStateMachineOnlyGoesForwards is AC #1's transition rule, one case
// per condition: the successor of every state is allowed, FAILED is allowed out
// of every state that is not terminal, and nothing else is — not a skipped
// stage, not a step back, and nothing at all out of READY or FAILED.
func TestContextStateMachineOnlyGoesForwards(t *testing.T) {
	order := []string{
		contextCreating,
		contextResolving,
		contextPreparingSource,
		contextIndexingSearch,
		contextIndexingGraph,
		contextVerifying,
		contextReady,
	}

	for i, from := range order {
		for j, to := range order {
			want := j == i+1
			if got := advances(from, to); got != want {
				t.Errorf("advances(%s, %s) = %v, want %v", from, to, got, want)
			}
		}
		// Every stage can fail, and READY, the last of them, cannot: a context
		// the query plane is serving must not be walked back into a state where
		// its artifacts are being rebuilt under it.
		if got, want := advances(from, contextFailed), from != contextReady; got != want {
			t.Errorf("advances(%s, %s) = %v, want %v", from, contextFailed, got, want)
		}
		// And nothing leads back out of FAILED: re-running a failed create is
		// creating the lifecycle again, not a record reverting.
		if advances(contextFailed, from) {
			t.Errorf("advances(%s, %s) = true, want false", contextFailed, from)
		}
	}
	if advances(contextFailed, contextFailed) {
		t.Error("advances(FAILED, FAILED) = true, want false")
	}
	if advances("", contextResolving) || advances(contextCreating, "SOMETHING_ELSE") {
		t.Error("advances admitted a state that is not in the machine")
	}
}

// TestAdvanceWritesEveryStateItPassesThrough is the other half of AC #1: the
// record carries the stage the context is in while it is in it, which is what a
// context killed half way through a create leaves behind, and a refused
// transition changes nothing.
func TestAdvanceWritesEveryStateItPassesThrough(t *testing.T) {
	s := openStore(t, t.TempDir())
	record := store.Context{ID: "app", Repository: "demo", State: contextCreating}
	if err := s.PutContext(record); err != nil {
		t.Fatalf("PutContext: %v", err)
	}

	for _, state := range []string{
		contextResolving,
		contextPreparingSource,
		contextIndexingSearch,
		contextIndexingGraph,
		contextVerifying,
		contextReady,
	} {
		if err := advance(s, &record, state); err != nil {
			t.Fatalf("advance to %s: %v", state, err)
		}
		if record.State != state {
			t.Errorf("in-memory state = %q, want %q", record.State, state)
		}
		if stored := contextRecord(t, s.Root(), "app"); stored.State != state {
			t.Errorf("stored state = %q, want %q", stored.State, state)
		}
	}

	// READY is terminal, and a refused transition leaves both the record and
	// the file exactly as they were.
	before := contextRecord(t, s.Root(), "app")
	if err := advance(s, &record, contextVerifying); err == nil {
		t.Error("advance from READY back to VERIFYING returned nil, want an error")
	}
	if record.State != contextReady {
		t.Errorf("in-memory state = %q after a refused transition, want %q", record.State, contextReady)
	}
	if after := contextRecord(t, s.Root(), "app"); after != before {
		t.Errorf("a refused transition rewrote the record:\n before %+v\n  after %+v", before, after)
	}
}

// TestFailRecordsWhereAContextStopped covers the failure edge of every stage:
// the cause is what the command returns, and the record is FAILED so the
// failure can be seen, listed and later retried rather than looking like a
// create that never ran.
func TestFailRecordsWhereAContextStopped(t *testing.T) {
	s := openStore(t, t.TempDir())
	for _, state := range []string{
		contextCreating,
		contextResolving,
		contextPreparingSource,
		contextIndexingSearch,
		contextIndexingGraph,
		contextVerifying,
	} {
		record := store.Context{ID: "app", Repository: "demo", State: state}
		if err := s.PutContext(record); err != nil {
			t.Fatalf("PutContext: %v", err)
		}

		cause := vacerr.New(vacerr.GraphProviderUnavailable, "the graph engine fell over", nil)
		if err := fail(s, &record, cause); !errors.Is(err, cause) {
			t.Errorf("fail in %s returned %v, want the cause", state, err)
		}
		if stored := contextRecord(t, s.Root(), "app"); stored.State != contextFailed {
			t.Errorf("state after a failure in %s = %q, want %s", state, stored.State, contextFailed)
		}
	}
}

// TestReadyContextIsTheOnlyWayIn is AC #3 and AC #4: the contexts the query
// plane can read are exactly the READY ones, and every other id — one that
// failed, one still being built, one that was never managed at all — comes back
// as the same CONTEXT_NOT_FOUND, down to the message and the details.
func TestReadyContextIsTheOnlyWayIn(t *testing.T) {
	s := openStore(t, t.TempDir())
	for _, state := range []string{
		contextCreating,
		contextResolving,
		contextPreparingSource,
		contextIndexingSearch,
		contextIndexingGraph,
		contextVerifying,
		contextReady,
		contextFailed,
	} {
		if err := s.PutContext(store.Context{ID: strings.ToLower(state), Repository: "demo", State: state}); err != nil {
			t.Fatalf("PutContext(%s): %v", state, err)
		}
	}

	// The registry the query plane reads is what this primitive admits, and
	// READY is the whole of it.
	contexts, err := s.Contexts()
	if err != nil {
		t.Fatalf("Contexts(): %v", err)
	}
	var readable []string
	for _, c := range contexts {
		if _, err := readyContext(s, c.ID); err == nil {
			readable = append(readable, c.ID)
		}
	}
	if want := []string{strings.ToLower(contextReady)}; !slices.Equal(readable, want) {
		t.Errorf("the query plane can read %v, want only %v", readable, want)
	}

	// AC #4. A context that is not READY and a context that does not exist are
	// the same answer: not the same code with a different message, the same
	// error. The comparison is the same id twice — once hidden by its state,
	// once really gone — so nothing but the state can account for a difference.
	for _, id := range []string{strings.ToLower(contextFailed), strings.ToLower(contextIndexingGraph)} {
		hidden := errorFor(t, func() error { _, err := readyContext(s, id); return err })
		if hidden.Code != vacerr.ContextNotFound {
			t.Errorf("readyContext(%s) code = %q, want %q", id, hidden.Code, vacerr.ContextNotFound)
		}
		if err := s.DeleteContext(id); err != nil {
			t.Fatalf("DeleteContext(%s): %v", id, err)
		}
		absent := errorFor(t, func() error { _, err := readyContext(s, id); return err })
		if !reflect.DeepEqual(hidden, absent) {
			t.Errorf("readyContext(%s) in %s = %v, want the error an unmanaged context produces, %v", id, strings.ToUpper(id), hidden, absent)
		}
	}
}

// errorFor returns the *vacerr.Error a call failed with.
func errorFor(t *testing.T, call func() error) *vacerr.Error {
	t.Helper()
	err := call()
	if err == nil {
		t.Fatal("got no error, want one")
	}
	var vErr *vacerr.Error
	if !errors.As(err, &vErr) {
		t.Fatalf("error = %v (%T), want *vacerr.Error", err, err)
	}
	return vErr
}

// TestVerifyIdentityRefusesARecordThatNamesAnotherContext is the last of the
// six checks: the four fields the query plane resolves a context into are the
// ones this context's own name and revision generate, so a record cannot point
// at another revision's search ref or graph.
func TestVerifyIdentityRefusesARecordThatNamesAnotherContext(t *testing.T) {
	const revision = "0123456789abcdef0123456789abcdef01234567"
	sound := store.Context{
		ID:         "app",
		Repository: "demo",
		Branch:     searchRef("app", revision),
		Revision:   revision,
		GraphRef:   graphRef("demo", "app", revision),
		State:      contextReady,
	}
	if err := verifyIdentity(sound); err != nil {
		t.Fatalf("verifyIdentity of a record as created: %v", err)
	}

	other := "89abcdef0123456789abcdef0123456789abcdef"
	for name, broken := range map[string]func(c *store.Context){
		"a revision that is not a full SHA": func(c *store.Context) { c.Revision = revision[:shortSHA] },
		"another revision's search ref":     func(c *store.Context) { c.Branch = searchRef("app", other) },
		"another context's search ref":      func(c *store.Context) { c.Branch = searchRef("other", revision) },
		"another revision's graph":          func(c *store.Context) { c.GraphRef = graphRef("demo", "app", other) },
		"another repository's graph":        func(c *store.Context) { c.GraphRef = graphRef("other", "app", revision) },
	} {
		c := sound
		broken(&c)
		err := verifyIdentity(c)
		if got := codeFor(t, err); got != vacerr.SourceMismatch {
			t.Errorf("%s: code = %q, want %q", name, got, vacerr.SourceMismatch)
		}
	}
}

// TestContextCreateReachesReadyOnlyWithEveryArtifactVerified is AC #1 and AC #2
// end to end, against real git, Zoekt and CBM: a create that gets through every
// stage ends READY and passes the verification again afterwards, and each of
// the artifacts the verification checks fails it when it is broken underneath.
func TestContextCreateReachesReadyOnlyWithEveryArtifactVerified(t *testing.T) {
	data, mainSHA, branchSHA := managed(t)

	out, err := contextRun(t, data, "create", "app", "--repo", "demo", "--ref", "main")
	if err != nil {
		t.Fatalf("context create: %v", err)
	}
	if want := "app\t" + contextReady + "\t" + mainSHA + "\n"; out != want {
		t.Errorf("context create printed %q, want %q", out, want)
	}

	s := openStore(t, data)
	c := contextRecord(t, data, "app")
	if c.State != contextReady {
		t.Fatalf("state = %q, want %s", c.State, contextReady)
	}
	// AC #3 and AC #4 from the other side: a READY context is the one thing the
	// query plane can read.
	if _, err := readyContext(s, "app"); err != nil {
		t.Errorf("readyContext of a READY context: %v", err)
	}
	if _, err := contextRun(t, data, "verify", "app"); err != nil {
		t.Errorf("context verify of a READY context: %v", err)
	}

	// Every stage's artifact is really there: the checkout, the search ref on
	// the pinned commit, the shard, the graph.
	clone := filepath.Join(data, "repos", "demo")
	if head := gitOut(t, "-C", filepath.Join(data, "worktrees", "demo", "app"), "rev-parse", "HEAD"); head != mainSHA {
		t.Errorf("worktree HEAD = %q, want the pinned %q", head, mainSHA)
	}
	if ref := gitOut(t, "-C", clone, "rev-parse", "--verify", "refs/heads/"+c.Branch); ref != mainSHA {
		t.Errorf("search ref = %q, want the pinned %q", ref, mainSHA)
	}
	if err := verifyGraph(t.Context(), c); err != nil {
		t.Errorf("the graph of a READY context: %v", err)
	}

	// A search ref moved onto another commit is the wrong-version answer this
	// server refuses to give, whatever the record still says.
	mustGit(t, "-C", clone, "update-ref", "refs/heads/"+c.Branch, branchSHA)
	if got := codeFor(t, verifyContext(t.Context(), s, c)); got != vacerr.SourceMismatch {
		t.Errorf("verify with the search ref on another commit: code = %q, want %q", got, vacerr.SourceMismatch)
	}

	// A search ref that is gone is a context that cannot be searched.
	mustGit(t, "-C", clone, "update-ref", "-d", "refs/heads/"+c.Branch)
	if got := codeFor(t, verifyContext(t.Context(), s, c)); got != vacerr.SearchProviderUnavailable {
		t.Errorf("verify without the search ref: code = %q, want %q", got, vacerr.SearchProviderUnavailable)
	}

	// And so is an index that was deleted after it was built.
	mustGit(t, "-C", clone, "update-ref", "refs/heads/"+c.Branch, mainSHA)
	shards, err := repositoryShards(filepath.Join(data, "zoekt"), "demo")
	if err != nil || len(shards) == 0 {
		t.Fatalf("shards of demo = %v (err %v), want the one the create built", shards, err)
	}
	for _, path := range shards {
		if err := os.Remove(path); err != nil {
			t.Fatalf("Remove(%s): %v", path, err)
		}
	}
	err = verifyContext(t.Context(), s, c)
	if got := codeFor(t, err); got != vacerr.SearchProviderUnavailable {
		t.Errorf("verify without the shard: code = %q, want %q", got, vacerr.SearchProviderUnavailable)
	}
	if !strings.Contains(err.Error(), "no search index") {
		t.Errorf("verify without the shard reported %v, want it to name the missing index", err)
	}

	// None of that verification wrote anything: READY was granted by the
	// lifecycle and is not something verify grants or withdraws.
	if after := contextRecord(t, data, "app"); after != c {
		t.Errorf("verification rewrote the record:\n before %+v\n  after %+v", c, after)
	}
}

// TestContextCreateLandsInFailedWhenTheGraphCannotBeBuilt is AC #1's failure
// edge and AC #3, against the real engines: the graph engine fails, so the
// context is FAILED rather than a context with a worktree and a search index
// that the query plane would serve anyway.
func TestContextCreateLandsInFailedWhenTheGraphCannotBeBuilt(t *testing.T) {
	data, mainSHA, _ := managed(t)
	// A codebase-memory-mcp that fails, in front of the real one on PATH. The
	// stages before it are the real ones, so what this shows is a create that
	// got as far as the graph and stopped there.
	t.Setenv("PATH", brokenCBM(t)+string(os.PathListSeparator)+os.Getenv("PATH"))

	if _, err := contextRun(t, data, "create", "app", "--repo", "demo", "--ref", "main"); err == nil {
		t.Fatal("context create with a failing graph engine returned nil, want an error")
	}

	c := contextRecord(t, data, "app")
	if c.State != contextFailed {
		t.Errorf("state = %q, want %s", c.State, contextFailed)
	}
	if c.Revision != mainSHA {
		t.Errorf("revision = %q, want the pinned %q", c.Revision, mainSHA)
	}

	// The stages before the graph ran, in order: the checkout and the search
	// ref are there, which is what makes the state the record carries the stage
	// it stopped in rather than the stage it never left.
	if head := gitOut(t, "-C", filepath.Join(data, "worktrees", "demo", "app"), "rev-parse", "HEAD"); head != mainSHA {
		t.Errorf("worktree HEAD = %q, want the pinned %q from PREPARING_SOURCE", head, mainSHA)
	}
	clone := filepath.Join(data, "repos", "demo")
	if ref := gitOut(t, "-C", clone, "rev-parse", "--verify", "refs/heads/"+c.Branch); ref != mainSHA {
		t.Errorf("search ref = %q, want the pinned %q from INDEXING_SEARCH", ref, mainSHA)
	}

	// AC #3: none of that makes it readable. A FAILED context is not there as
	// far as the query plane is concerned.
	if _, err := readyContext(openStore(t, data), "app"); codeFor(t, err) != vacerr.ContextNotFound {
		t.Errorf("readyContext of a FAILED context = %v, want %q", err, vacerr.ContextNotFound)
	}
	// The management plane still sees it, or a failure would be a context
	// nobody could inspect, remove or retry.
	if out, err := contextRun(t, data, "list"); err != nil || !strings.Contains(out, contextFailed) {
		t.Errorf("context list = %q (err %v), want it to report the FAILED context", out, err)
	}
}

// brokenCBM returns a directory holding a codebase-memory-mcp that fails the
// way a graph engine that cannot index does: a message on standard error and a
// non-zero exit.
func brokenCBM(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\necho 'index_repository: no' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, cbmCommand), []byte(script), 0o700); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return dir
}

// failedContext creates a context whose graph could not be built, and returns
// the data directory, the record it left in FAILED and the revision it pinned.
//
// The graph engine is broken only while the create runs: every stage before it
// is the real one, so what comes back is a context with real partial artifacts —
// a checkout and a search ref — and no graph, which is what a retry has to
// recognise and finish.
func failedContext(t *testing.T, id string) (data string, failed store.Context, mainSHA string) {
	t.Helper()
	data, mainSHA, _ = managed(t)

	working := os.Getenv("PATH")
	t.Setenv("PATH", brokenCBM(t)+string(os.PathListSeparator)+working)
	if _, err := contextRun(t, data, "create", id, "--repo", "demo", "--ref", "main"); err == nil {
		t.Fatal("context create with a failing graph engine returned nil, want an error")
	}
	t.Setenv("PATH", working)

	failed = contextRecord(t, data, id)
	if failed.State != contextFailed {
		t.Fatalf("state = %q, want %s", failed.State, contextFailed)
	}
	return data, failed, mainSHA
}

// TestContextRetryRebuildsAFailedContext is AC #1: a context the graph engine
// left FAILED is driven to READY by `context retry` alone, with nothing deleted
// by hand first, and it comes back pinned to the same revision it always was.
func TestContextRetryRebuildsAFailedContext(t *testing.T) {
	data, failed, mainSHA := failedContext(t, "app")

	out, err := contextRun(t, data, "retry", "app")
	if err != nil {
		t.Fatalf("context retry: %v", err)
	}
	if want := "app\t" + contextReady + "\t" + mainSHA + "\n"; out != want {
		t.Errorf("context retry printed %q, want %q", out, want)
	}

	// The retry rebuilt this revision's artifacts. It did not repin the
	// context: the revision and both generated names are the ones the failed
	// create wrote.
	c := contextRecord(t, data, "app")
	if c.Revision != failed.Revision || c.Branch != failed.Branch || c.GraphRef != failed.GraphRef {
		t.Errorf("the retry changed what the context pins:\n before %+v\n  after %+v", failed, c)
	}
	if c.State != contextReady {
		t.Errorf("state after a retry = %q, want %s", c.State, contextReady)
	}

	// READY here means the same thing it means after a create: every artifact
	// asked of itself, not a state that was written down.
	if _, err := contextRun(t, data, "verify", "app"); err != nil {
		t.Errorf("context verify after a retry: %v", err)
	}
	if _, err := readyContext(openStore(t, data), "app"); err != nil {
		t.Errorf("readyContext after a retry: %v", err)
	}
	if head := gitOut(t, "-C", filepath.Join(data, "worktrees", "demo", "app"), "rev-parse", "HEAD"); head != mainSHA {
		t.Errorf("worktree HEAD after a retry = %q, want the pinned %q", head, mainSHA)
	}
	if err := verifyGraph(t.Context(), c); err != nil {
		t.Errorf("the graph a retry built: %v", err)
	}
}

// TestContextRetryRecoversAContextKilledMidLifecycle is AC #4: a record left
// behind in an intermediate state by a process that died is not READY to
// anything, cannot be created over, is reported as needing a retry, and is
// recovered by one.
//
// The state is written directly rather than by killing a process, because it is
// the same thing: advance persists each state before the stage it names runs, so
// INDEXING_GRAPH on disk with a worktree and a search ref present and no graph
// is exactly what a create killed during the graph stage leaves.
func TestContextRetryRecoversAContextKilledMidLifecycle(t *testing.T) {
	data, failed, mainSHA := failedContext(t, "app")

	s := openStore(t, data)
	killed := failed
	killed.State = contextIndexingGraph
	if err := s.PutContext(killed); err != nil {
		t.Fatalf("PutContext: %v", err)
	}

	// Not READY, and not readable as one. A context caught mid-lifecycle is
	// indistinguishable from one that was never managed, exactly as a FAILED one
	// is.
	if _, err := readyContext(s, "app"); codeFor(t, err) != vacerr.ContextNotFound {
		t.Errorf("readyContext of a context killed mid-lifecycle = %v, want %q", err, vacerr.ContextNotFound)
	}
	if out, err := contextRun(t, data, "list"); err != nil || !strings.Contains(out, contextIndexingGraph) {
		t.Errorf("context list = %q (err %v), want it to report the stage the context is stuck in", out, err)
	}

	// The management plane says what to do about it rather than leaving an
	// operator to guess whether to wait.
	out, err := contextRun(t, data, "status", "app")
	if err != nil {
		t.Fatalf("context status: %v", err)
	}
	if !strings.Contains(out, "vacmcp context retry app") {
		t.Errorf("context status printed\n%s\nwant it to report that a retry is needed", out)
	}

	// And a second create is not the way back: a context is immutable, so
	// recovering a stuck one is the retry's job specifically.
	before := contextRecord(t, data, "app")
	if _, err := contextRun(t, data, "create", "app", "--repo", "demo", "--ref", "release/2.x"); codeFor(t, err) != vacerr.InvalidArgument {
		t.Errorf("context create over a stuck context = %v, want %q", err, vacerr.InvalidArgument)
	}
	if after := contextRecord(t, data, "app"); after != before {
		t.Errorf("a refused create changed the stuck record:\n before %+v\n  after %+v", before, after)
	}

	if _, err := contextRun(t, data, "retry", "app"); err != nil {
		t.Fatalf("context retry of a context killed mid-lifecycle: %v", err)
	}
	c := contextRecord(t, data, "app")
	if c.State != contextReady || c.Revision != mainSHA {
		t.Errorf("record after a retry = %+v, want %s pinned to %s", c, contextReady, mainSHA)
	}
	if _, err := contextRun(t, data, "verify", "app"); err != nil {
		t.Errorf("context verify after recovering a stuck context: %v", err)
	}
	if out, err := contextRun(t, data, "status", "app"); err != nil || !strings.Contains(out, "not needed") {
		t.Errorf("context status = %q (err %v), want it to report that no retry is needed", out, err)
	}
}

// TestContextRetryRefusesWhatItMustNotRebuild keeps a retry from being a way to
// take a served context's source away underneath it, and from inventing a
// context that is not managed.
func TestContextRetryRefusesWhatItMustNotRebuild(t *testing.T) {
	data, _, _ := managed(t)
	if _, err := contextRun(t, data, "create", "app", "--repo", "demo", "--ref", "main"); err != nil {
		t.Fatalf("context create: %v", err)
	}
	before := contextRecord(t, data, "app")

	if _, err := contextRun(t, data, "retry", "app"); codeFor(t, err) != vacerr.InvalidArgument {
		t.Errorf("context retry of a READY context = %v, want %q", err, vacerr.InvalidArgument)
	}
	if after := contextRecord(t, data, "app"); after != before {
		t.Errorf("a refused retry changed the record:\n before %+v\n  after %+v", before, after)
	}

	if _, err := contextRun(t, data, "retry", "absent"); codeFor(t, err) != vacerr.ContextNotFound {
		t.Errorf("context retry of an unmanaged context, want %q", vacerr.ContextNotFound)
	}
	if _, err := contextRun(t, data, "retry"); err == nil {
		t.Error("context retry without NAME returned nil, want an error")
	}
}

// TestConcurrentLifecycleOnOneRepositoryStaysConsistent is AC #2 through the
// commands themselves: two creates and a sync of one repository launched
// together leave every one of the four things that have to agree — the clone,
// the records, the search index and the graphs — agreeing.
//
// The index is what this is really about. Every create rebuilds the
// repository's one shared shard out of whatever context records it reads, so
// two of them running at once could have the one that read the older records
// finish last, leaving a shard with no ref for a context the registry calls
// READY. That is a context the query plane would serve out of nothing, and the
// last assertion below is the one that would catch it.
func TestConcurrentLifecycleOnOneRepositoryStaysConsistent(t *testing.T) {
	data, mainSHA, branchSHA := managed(t)

	var wg sync.WaitGroup
	for id, ref := range map[string]string{"alpha": "main", "beta": "release/2.x"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := contextRun(t, data, "create", id, "--repo", "demo", "--ref", ref); err != nil {
				t.Errorf("context create %s: %v", id, err)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := repoRun(t, data, "sync", "demo"); err != nil {
			t.Errorf("repo sync: %v", err)
		}
	}()
	wg.Wait()

	alpha, beta := contextRecord(t, data, "alpha"), contextRecord(t, data, "beta")
	if alpha.State != contextReady || beta.State != contextReady {
		t.Fatalf("states after concurrent creates = %q and %q, want both %s", alpha.State, beta.State, contextReady)
	}
	// The sync ran beside them and moved neither: each context pins the
	// revision of the ref it asked for.
	if alpha.Revision != mainSHA || beta.Revision != branchSHA {
		t.Errorf("revisions = %q and %q, want %q and %q", alpha.Revision, beta.Revision, mainSHA, branchSHA)
	}
	for _, c := range []store.Context{alpha, beta} {
		if _, err := contextRun(t, data, "verify", c.ID); err != nil {
			t.Errorf("context verify %s after a concurrent create: %v", c.ID, err)
		}
	}

	// And the shard really carries both refs, asked of Zoekt rather than of the
	// records that claim it should.
	url := demorepo.StartZoekt(t, filepath.Join(data, "zoekt"))
	for _, c := range []store.Context{alpha, beta} {
		indexed, err := searchRefIndexed(t.Context(), url, c)
		if err != nil {
			t.Fatalf("searchRefIndexed(%s): %v", c.ID, err)
		}
		if !indexed {
			t.Errorf("%s is READY but its search ref %q is not in the index a concurrent create rebuilt", c.ID, c.Branch)
		}
	}
}

func TestContextRejectsUnknownSubcommands(t *testing.T) {
	for _, args := range [][]string{{"context"}, {"context", "update"}} {
		if err := run(args, &bytes.Buffer{}); err == nil {
			t.Errorf("run(%q) returned nil, want an error", args)
		}
	}
}
