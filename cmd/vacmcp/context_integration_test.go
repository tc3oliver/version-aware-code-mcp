//go:build integration

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/internal/demorepo"
	"github.com/tc3oliver/version-aware-code-mcp/managed"
	"github.com/tc3oliver/version-aware-code-mcp/store"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// The context commands are tested against real git repositories, cloned by the
// real `repo add`, never against a fake git. What is being checked is that a
// branch, a tag and an abbreviation all name the same commit and that the
// commit written down is the one git resolved — which only a real object
// database can answer. The helpers these tests share with the repo commands
// (sourceRepo, commit, mustGit, gitOut, repoRun, openStore, codeFor) are in
// repo_test.go, and contextRecord, which the lifecycle's own rules read too,
// is in context_test.go.

// The length of the full commit SHA a context pins, and of the prefix of it the
// generated names carry. They are asserted from the outside here: a record that
// stopped carrying a whole SHA, or a name that stopped carrying enough of one to
// tell two contexts apart, is a regression these tests have to see.
const (
	fullSHA  = 40
	shortSHA = 12
)

// contextRecord returns the stored record of id, for the assertions that read a
// record rather than what a command printed.
func contextRecord(t *testing.T, dataDir, id string) store.Context {
	t.Helper()
	c, err := openStore(t, dataDir).Context(id)
	if err != nil {
		t.Fatalf("Context(%s): %v", id, err)
	}
	return c
}

// only is the member of a record that names one repository, which is what the
// contexts these tests create are. A record with several is a mistake in the
// test rather than something to pick a member out of, so it stops the test.
func only(t *testing.T, c store.Context) store.ContextMember {
	t.Helper()
	if len(c.Members) != 1 {
		t.Fatalf("context %s names %d repositories, want the one this test created", c.ID, len(c.Members))
	}
	return c.Members[0]
}

// scope is the version scope the query plane resolves one member into, which is
// what the v0.1.0 adapters take.
func scope(id string, m store.ContextMember) vacctx.CodeContext {
	return vacctx.CodeContext{ID: id, Repository: m.Repository, Branch: m.Branch, Revision: m.Revision, GraphRef: m.GraphRef}
}

// served reports whether the query plane's registry has id: the READY records
// `serve --managed` builds its configuration out of, asked of the data
// directory as it is now. That every other state is not merely absent from it
// but indistinguishable from a context that was never managed is the managed
// package's own rule, and is checked there.
func served(t *testing.T, dataDir, id string) bool {
	t.Helper()
	ready, err := readyContexts(openStore(t, dataDir))
	if err != nil {
		t.Fatalf("readyContexts: %v", err)
	}
	return slices.ContainsFunc(ready, func(c store.Context) bool { return c.ID == id })
}

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
func managedDemo(t *testing.T) (data, mainSHA, branchSHA string) {
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

// TestContextCreatePinsEveryKindOfRefToTheSameFullSHA is AC #1: a branch, a tag
// and an abbreviated SHA naming one commit all pin that commit's full SHA, and
// a branch that is only a remote-tracking ref in the clone resolves too.
func TestContextCreatePinsEveryKindOfRefToTheSameFullSHA(t *testing.T) {
	data, mainSHA, branchSHA := managedDemo(t)

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
		// One row per repository the context names, which for a context over
		// one is the row this command has always printed.
		if want := tc.id + "\t" + managed.ContextReady + "\tdemo\t" + tc.want + "\n"; out != want {
			t.Errorf("context create printed %q, want %q", out, want)
		}

		c := contextRecord(t, data, tc.id)
		member := only(t, c)
		if member.Revision != tc.want {
			t.Errorf("--ref %s pinned %q, want %q", tc.ref, member.Revision, tc.want)
		}
		// The stored revision is a full commit SHA and nothing that could move:
		// the ref that was typed must not survive into the record.
		if len(member.Revision) != fullSHA || strings.TrimLeft(member.Revision, "0123456789abcdef") != "" {
			t.Errorf("--ref %s pinned %q, want %d hexadecimal digits", tc.ref, member.Revision, fullSHA)
		}
		if member.Repository != "demo" || c.State != managed.ContextReady {
			t.Errorf("record = %+v, want repository demo in state %s", c, managed.ContextReady)
		}
	}
}

// TestContextCreateRefusesWhatItCannotResolve keeps a context from being
// recorded at all unless its revision is a commit that is really there. A ref
// beginning with a dash is in the table because it must reach git as a revision
// git cannot find, never as an option git obeys.
func TestContextCreateRefusesWhatItCannotResolve(t *testing.T) {
	data, mainSHA, _ := managedDemo(t)

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
	data, _, _ := managedDemo(t)

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
	data, _, _ := managedDemo(t)

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
// It builds its own installation, as every test of a command that writes does:
// what it holds is that a refused create writes nothing, and the shared
// installation is only for tests that ask nothing of it but answers.
func TestContextCreateRefusesToReplaceAManagedContext(t *testing.T) {
	data, mainSHA, branchSHA := managedDemo(t)

	if _, err := contextRun(t, data, "create", "app", "--repo", "demo", "--ref", "main"); err != nil {
		t.Fatalf("context create: %v", err)
	}
	before := contextRecord(t, data, "app")

	_, err := contextRun(t, data, "create", "app", "--repo", "demo", "--ref", "release/2.x")
	if got := codeFor(t, err); got != vacerr.InvalidArgument {
		t.Errorf("second context create code = %q, want %q", got, vacerr.InvalidArgument)
	}

	after := contextRecord(t, data, "app")
	if !sameContext(after, before) {
		t.Errorf("the record changed across a refused create:\n before %+v\n  after %+v", before, after)
	}
	if got := only(t, after).Revision; got != mainSHA {
		t.Errorf("revision = %q, want the original %q", got, mainSHA)
	}
	if only(t, after).Revision == branchSHA {
		t.Error("the refused create repinned the context to the other revision")
	}
}

// TestContextRecordIsNeverRewritten is the other half of AC #2: every command
// that is not create or remove leaves the record exactly as it was, down to the
// timestamp any rewrite would have stamped. There is no subcommand that edits
// one, so this is the whole surface a pinned revision could move through.
func TestContextRecordIsNeverRewritten(t *testing.T) {
	data, mainSHA, _ := managedDemo(t)
	if _, err := contextRun(t, data, "create", "app", "--repo", "demo", "--ref", "main"); err != nil {
		t.Fatalf("context create: %v", err)
	}
	before := contextRecord(t, data, "app")

	for _, args := range [][]string{{"list"}, {"status", "app"}, {"verify", "app"}} {
		if _, err := contextRun(t, data, args...); err != nil {
			t.Fatalf("context %s: %v", strings.Join(args, " "), err)
		}
		if after := contextRecord(t, data, "app"); !sameContext(after, before) {
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
	if !sameContext(after, before) {
		t.Errorf("the record changed across a sync and a verify:\n before %+v\n  after %+v", before, after)
	}
	if got := only(t, after).Revision; got != mainSHA || got == moved {
		t.Errorf("revision = %q, want it still pinned to %q and not to %q", got, mainSHA, moved)
	}
}

// TestContextCreateGeneratesTheInternalNames is AC #4: the user gives a name, a
// repository and a ref, and the search ref, the graph project and the worktree
// path all come out of that.
func TestContextCreateGeneratesTheInternalNames(t *testing.T) {
	prepared := preparedManaged(t)
	data, revision := prepared.data, prepared.modern

	// The two contexts of the shared installation that pin one revision, so
	// nothing but the name they were given differs between the records below.
	first := only(t, contextRecord(t, data, preparedModern))
	second := only(t, contextRecord(t, data, preparedTwin))
	// A context over one repository keeps generating exactly the names v0.4.0
	// generated, which is what lets a data directory it built be verified here.
	if first.Branch != "vacmcp/"+preparedModern+"-"+revision[:shortSHA] {
		t.Errorf("search ref = %q, want it derived from the context name and the short SHA", first.Branch)
	}
	if first.GraphRef != "vacmcp-demo-"+preparedModern+"-"+revision[:shortSHA] {
		t.Errorf("graph ref = %q, want it derived from the repository, the context name and the short SHA", first.GraphRef)
	}
	// Two contexts of one repository at one revision still get names of their
	// own, or removing either would take the other's artifacts with it.
	if first.Branch == second.Branch || first.GraphRef == second.GraphRef {
		t.Errorf("%s %+v and %s %+v share a generated name", preparedModern, first, preparedTwin, second)
	}
	// The generated search ref is one git will accept, since the search index
	// lifecycle has to be able to create it.
	mustGit(t, "check-ref-format", "refs/heads/"+first.Branch)

	out, err := contextRun(t, data, "status", preparedModern)
	if err != nil {
		t.Fatalf("context status: %v", err)
	}
	worktree := filepath.Join(data, "worktrees", "demo", preparedModern)
	for _, want := range []string{preparedModern, "demo", revision, managed.ContextReady, first.Branch, first.GraphRef, worktree} {
		if !strings.Contains(out, want) {
			t.Errorf("context status printed\n%s\nwant it to report %q", out, want)
		}
	}
}

// TestContextVerifyReportsWhetherTheRevisionIsStillThere covers the first of
// the verification's checks: the pinned commit is still in the repository's
// object database. It writes nothing either way.
func TestContextVerifyReportsWhetherTheRevisionIsStillThere(t *testing.T) {
	data, mainSHA, _ := managedDemo(t)
	if _, err := contextRun(t, data, "create", "app", "--repo", "demo", "--ref", "main"); err != nil {
		t.Fatalf("context create: %v", err)
	}

	out, err := contextRun(t, data, "verify", "app")
	if err != nil {
		t.Fatalf("context verify: %v", err)
	}
	if want := "app\tOK\tdemo\t" + mainSHA + "\n"; out != want {
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
	if after := contextRecord(t, data, "app"); !sameContext(after, before) {
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
	data, mainSHA, _ := managedDemo(t)
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

	if after := contextRecord(t, data, "keep"); !sameContext(after, before) {
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
	data, mainSHA, branchSHA := managedDemo(t)

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
	want := fmt.Sprintf("alpha\tdemo\t%s\t%s\nbeta\tdemo\t%s\t%s\n", mainSHA, managed.ContextReady, branchSHA, managed.ContextReady)
	if out != want {
		t.Errorf("context list printed\n%q\nwant\n%q", out, want)
	}
}

func TestContextStatusOfAnUnmanagedContext(t *testing.T) {
	data, _, _ := managedDemo(t)
	if _, err := contextRun(t, data, "status", "absent"); codeFor(t, err) != vacerr.ContextNotFound {
		t.Errorf("context status of an unmanaged context, want %q", vacerr.ContextNotFound)
	}
	if _, err := contextRun(t, data, "status"); err == nil {
		t.Error("context status without NAME returned nil, want an error")
	}
}

// TestContextCreateReachesReadyOnlyWithEveryArtifactVerified is AC #1 and AC #2
// end to end, against real git, Zoekt and CBM: a create that gets through every
// stage ends READY and passes the verification again afterwards, and each of
// the artifacts the verification checks fails it when it is broken underneath.
func TestContextCreateReachesReadyOnlyWithEveryArtifactVerified(t *testing.T) {
	data, mainSHA, branchSHA := managedDemo(t)

	out, err := contextRun(t, data, "create", "app", "--repo", "demo", "--ref", "main")
	if err != nil {
		t.Fatalf("context create: %v", err)
	}
	if want := "app\t" + managed.ContextReady + "\tdemo\t" + mainSHA + "\n"; out != want {
		t.Errorf("context create printed %q, want %q", out, want)
	}

	c := contextRecord(t, data, "app")
	member := only(t, c)
	if c.State != managed.ContextReady {
		t.Fatalf("state = %q, want %s", c.State, managed.ContextReady)
	}
	// AC #3 and AC #4 from the other side: a READY context is the one thing the
	// query plane can read.
	if !served(t, data, "app") {
		t.Error("a READY context is not in the registry the query plane reads")
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
	if ref := gitOut(t, "-C", clone, "rev-parse", "--verify", "refs/heads/"+member.Branch); ref != mainSHA {
		t.Errorf("search ref = %q, want the pinned %q", ref, mainSHA)
	}
	if err := graphExists(t.Context(), c.ID, member); err != nil {
		t.Errorf("the graph of a READY context: %v", err)
	}

	// A search ref moved onto another commit is the wrong-version answer this
	// server refuses to give, whatever the record still says.
	mustGit(t, "-C", clone, "update-ref", "refs/heads/"+member.Branch, branchSHA)
	_, err = contextRun(t, data, "verify", "app")
	if got := codeFor(t, err); got != vacerr.SourceMismatch {
		t.Errorf("verify with the search ref on another commit: code = %q, want %q", got, vacerr.SourceMismatch)
	}

	// A search ref that is gone is a context that cannot be searched.
	mustGit(t, "-C", clone, "update-ref", "-d", "refs/heads/"+member.Branch)
	_, err = contextRun(t, data, "verify", "app")
	if got := codeFor(t, err); got != vacerr.SearchProviderUnavailable {
		t.Errorf("verify without the search ref: code = %q, want %q", got, vacerr.SearchProviderUnavailable)
	}

	// And so is an index that was deleted after it was built.
	mustGit(t, "-C", clone, "update-ref", "refs/heads/"+member.Branch, mainSHA)
	built, _ := shards(t, data, "demo")
	if len(built) == 0 {
		t.Fatal("demo has no shard, want the one the create built")
	}
	for _, name := range built {
		if err := os.Remove(filepath.Join(data, "zoekt", name)); err != nil {
			t.Fatalf("Remove(%s): %v", name, err)
		}
	}
	_, err = contextRun(t, data, "verify", "app")
	if got := codeFor(t, err); got != vacerr.SearchProviderUnavailable {
		t.Errorf("verify without the shard: code = %q, want %q", got, vacerr.SearchProviderUnavailable)
	}
	if !strings.Contains(err.Error(), "no search index") {
		t.Errorf("verify without the shard reported %v, want it to name the missing index", err)
	}

	// None of that verification wrote anything: READY was granted by the
	// lifecycle and is not something verify grants or withdraws.
	if after := contextRecord(t, data, "app"); !sameContext(after, c) {
		t.Errorf("verification rewrote the record:\n before %+v\n  after %+v", c, after)
	}
}

// TestContextCreateLandsInFailedWhenTheGraphCannotBeBuilt is AC #1's failure
// edge and AC #3, against the real engines: the graph engine fails, so the
// context is FAILED rather than a context with a worktree and a search index
// that the query plane would serve anyway.
func TestContextCreateLandsInFailedWhenTheGraphCannotBeBuilt(t *testing.T) {
	data, mainSHA, _ := managedDemo(t)
	// A codebase-memory-mcp that fails, in front of the real one on PATH. The
	// stages before it are the real ones, so what this shows is a create that
	// got as far as the graph and stopped there.
	t.Setenv("PATH", brokenCBM(t)+string(os.PathListSeparator)+os.Getenv("PATH"))

	if _, err := contextRun(t, data, "create", "app", "--repo", "demo", "--ref", "main"); err == nil {
		t.Fatal("context create with a failing graph engine returned nil, want an error")
	}

	c := contextRecord(t, data, "app")
	member := only(t, c)
	if c.State != managed.ContextFailed {
		t.Errorf("state = %q, want %s", c.State, managed.ContextFailed)
	}
	if member.Revision != mainSHA {
		t.Errorf("revision = %q, want the pinned %q", member.Revision, mainSHA)
	}

	// The stages before the graph ran, in order: the checkout and the search
	// ref are there, which is what makes the state the record carries the stage
	// it stopped in rather than the stage it never left.
	if head := gitOut(t, "-C", filepath.Join(data, "worktrees", "demo", "app"), "rev-parse", "HEAD"); head != mainSHA {
		t.Errorf("worktree HEAD = %q, want the pinned %q from PREPARING_SOURCE", head, mainSHA)
	}
	clone := filepath.Join(data, "repos", "demo")
	if ref := gitOut(t, "-C", clone, "rev-parse", "--verify", "refs/heads/"+member.Branch); ref != mainSHA {
		t.Errorf("search ref = %q, want the pinned %q from INDEXING_SEARCH", ref, mainSHA)
	}

	// AC #3: none of that makes it readable. A FAILED context is not there as
	// far as the query plane is concerned.
	if served(t, data, "app") {
		t.Error("a FAILED context is in the registry the query plane reads")
	}
	// The management plane still sees it, or a failure would be a context
	// nobody could inspect, remove or retry.
	if out, err := contextRun(t, data, "list"); err != nil || !strings.Contains(out, managed.ContextFailed) {
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
	if err := os.WriteFile(filepath.Join(dir, managed.CBMCommand), []byte(script), 0o700); err != nil {
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
	data, mainSHA, _ = managedDemo(t)

	working := os.Getenv("PATH")
	t.Setenv("PATH", brokenCBM(t)+string(os.PathListSeparator)+working)
	if _, err := contextRun(t, data, "create", id, "--repo", "demo", "--ref", "main"); err == nil {
		t.Fatal("context create with a failing graph engine returned nil, want an error")
	}
	t.Setenv("PATH", working)

	failed = contextRecord(t, data, id)
	if failed.State != managed.ContextFailed {
		t.Fatalf("state = %q, want %s", failed.State, managed.ContextFailed)
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
	if want := "app\t" + managed.ContextReady + "\tdemo\t" + mainSHA + "\n"; out != want {
		t.Errorf("context retry printed %q, want %q", out, want)
	}

	// The retry rebuilt this revision's artifacts. It did not repin the
	// context: the revision and both generated names are the ones the failed
	// create wrote.
	c := contextRecord(t, data, "app")
	member := only(t, c)
	if member != only(t, failed) {
		t.Errorf("the retry changed what the context pins:\n before %+v\n  after %+v", only(t, failed), member)
	}
	if c.State != managed.ContextReady {
		t.Errorf("state after a retry = %q, want %s", c.State, managed.ContextReady)
	}

	// READY here means the same thing it means after a create: every artifact
	// asked of itself, not a state that was written down.
	if _, err := contextRun(t, data, "verify", "app"); err != nil {
		t.Errorf("context verify after a retry: %v", err)
	}
	if !served(t, data, "app") {
		t.Error("a context a retry drove to READY is not in the registry the query plane reads")
	}
	if head := gitOut(t, "-C", filepath.Join(data, "worktrees", "demo", "app"), "rev-parse", "HEAD"); head != mainSHA {
		t.Errorf("worktree HEAD after a retry = %q, want the pinned %q", head, mainSHA)
	}
	if err := graphExists(t.Context(), c.ID, member); err != nil {
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
	killed.State = managed.ContextIndexingGraph
	if err := s.PutContext(killed); err != nil {
		t.Fatalf("PutContext: %v", err)
	}

	// Not READY, and not readable as one. A context caught mid-lifecycle is
	// indistinguishable from one that was never managed, exactly as a FAILED one
	// is.
	if served(t, data, "app") {
		t.Error("a context killed mid-lifecycle is in the registry the query plane reads")
	}
	if out, err := contextRun(t, data, "list"); err != nil || !strings.Contains(out, managed.ContextIndexingGraph) {
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
	if after := contextRecord(t, data, "app"); !sameContext(after, before) {
		t.Errorf("a refused create changed the stuck record:\n before %+v\n  after %+v", before, after)
	}

	if _, err := contextRun(t, data, "retry", "app"); err != nil {
		t.Fatalf("context retry of a context killed mid-lifecycle: %v", err)
	}
	c := contextRecord(t, data, "app")
	if c.State != managed.ContextReady || only(t, c).Revision != mainSHA {
		t.Errorf("record after a retry = %+v, want %s pinned to %s", c, managed.ContextReady, mainSHA)
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
	data, _, _ := managedDemo(t)
	if _, err := contextRun(t, data, "create", "app", "--repo", "demo", "--ref", "main"); err != nil {
		t.Fatalf("context create: %v", err)
	}
	before := contextRecord(t, data, "app")

	if _, err := contextRun(t, data, "retry", "app"); codeFor(t, err) != vacerr.InvalidArgument {
		t.Errorf("context retry of a READY context = %v, want %q", err, vacerr.InvalidArgument)
	}
	if after := contextRecord(t, data, "app"); !sameContext(after, before) {
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
	data, mainSHA, branchSHA := managedDemo(t)

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
	if alpha.State != managed.ContextReady || beta.State != managed.ContextReady {
		t.Fatalf("states after concurrent creates = %q and %q, want both %s", alpha.State, beta.State, managed.ContextReady)
	}
	// The sync ran beside them and moved neither: each context pins the
	// revision of the ref it asked for.
	if only(t, alpha).Revision != mainSHA || only(t, beta).Revision != branchSHA {
		t.Errorf("revisions = %q and %q, want %q and %q", only(t, alpha).Revision, only(t, beta).Revision, mainSHA, branchSHA)
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
		member := only(t, c)
		indexed, err := searchRefIndexed(t.Context(), url, member)
		if err != nil {
			t.Fatalf("searchRefIndexed(%s): %v", c.ID, err)
		}
		if !indexed {
			t.Errorf("%s is READY but its search ref %q is not in the index a concurrent create rebuilt", c.ID, member.Branch)
		}
	}
}

// TestContextRemovePersistsRemovingBeforeItTouchesAnArtifact is AC #1: the
// state is written down before any artifact is taken apart, so a removal that
// dies on its very first step leaves a record saying what is happening to it
// rather than one still claiming READY over a graph that is already gone.
//
// The graph engine is broken for the length of the run, which fails the first
// artifact step there is. Everything the context owns is therefore still there
// afterwards, and the record already says REMOVING: only a write that happens
// before the artifacts are touched produces that pair.
func TestContextRemovePersistsRemovingBeforeItTouchesAnArtifact(t *testing.T) {
	data, _, _ := managedDemo(t)
	if _, err := contextRun(t, data, "create", "app", "--repo", "demo", "--ref", "main"); err != nil {
		t.Fatalf("context create: %v", err)
	}
	ready := only(t, contextRecord(t, data, "app"))

	working := os.Getenv("PATH")
	t.Setenv("PATH", brokenCBM(t)+string(os.PathListSeparator)+working)
	if _, err := contextRun(t, data, "remove", "app"); err == nil {
		t.Fatal("context remove with a failing graph engine returned nil, want an error")
	}
	t.Setenv("PATH", working)

	if got := contextRecord(t, data, "app").State; got != managed.ContextRemoving {
		t.Errorf("state after a removal that failed on its first step = %q, want %s", got, managed.ContextRemoving)
	}
	if head := gitOut(t, "-C", filepath.Join(data, "worktrees", "demo", "app"), "rev-parse", "HEAD"); head != ready.Revision {
		t.Errorf("worktree HEAD = %q, want a removal that got no further than the state to have left it at %q", head, ready.Revision)
	}
	if got := gitOut(t, "-C", filepath.Join(data, "repos", "demo"), "rev-parse", "--verify", "refs/heads/"+ready.Branch); got != ready.Revision {
		t.Errorf("search ref = %q, want a removal that got no further than the state to have left it at %q", got, ready.Revision)
	}

	// AC #2 at its first instant: from the line the state was written, the
	// context is out of the registry the query plane reads.
	if served(t, data, "app") {
		t.Error("a REMOVING context is in the registry the query plane reads")
	}
	// And a retry is not the way out of REMOVING, which is what keeps it
	// one-way: the only thing that ends it is the removal itself.
	if _, err := contextRun(t, data, "retry", "app"); codeFor(t, err) != vacerr.InvalidArgument {
		t.Errorf("context retry of a REMOVING context, want %q", vacerr.InvalidArgument)
	}
	if out, err := contextRun(t, data, "status", "app"); err != nil || !strings.Contains(out, "vacmcp context remove app") {
		t.Errorf("context status printed\n%s(err %v)\nwant it to point at the removal", out, err)
	}

	// The same command again, with the engine back, finishes what it started.
	if out, err := contextRun(t, data, "remove", "app"); err != nil || out != "app\tREMOVED\n" {
		t.Errorf("re-running context remove = %q (err %v), want it to finish the removal", out, err)
	}
	if _, err := openStore(t, data).Context("app"); codeFor(t, err) != vacerr.ContextNotFound {
		t.Errorf("the record after the removal finished = %v, want %q", err, vacerr.ContextNotFound)
	}
}

// TestContextRemoveResumesAfterACrashAtAnyStep is AC #2 and AC #3: a removal
// killed at any one of its steps leaves a context the query plane does not
// serve, and running the very same command again finishes it and leaves
// nothing behind.
//
// The crash is produced by taking the steps `context remove` takes, in its
// order, and stopping — against the same real git, Zoekt and CBM the command
// drives. That is exactly what a killed process leaves, and unlike killing one
// it stops in a defined place: the state is persisted before any artifact is
// touched, so every prefix of those steps is a state a record can really be
// found in afterwards.
func TestContextRemoveResumesAfterACrashAtAnyStep(t *testing.T) {
	cbmOrSkip(t)
	for _, crash := range []struct {
		what  string
		after int
	}{
		{"REMOVING is persisted and nothing is touched yet", 0},
		{"the worktree and the graph are gone", 1},
		{"the search ref is dropped", 2},
		{"the index is rebuilt", 3},
	} {
		t.Run(crash.what, func(t *testing.T) {
			data, removing, kept := indexedRepository(t)
			t.Cleanup(func() { discardGraphs(t, data) })
			crashRemoval(t, data, removing, kept, crash.after)

			if got := contextRecord(t, data, removing.ID).State; got != managed.ContextRemoving {
				t.Fatalf("state after the crash = %q, want %s", got, managed.ContextRemoving)
			}
			// The rebuild that ran with the record still in front of it left the
			// ref the record names out, rather than re-creating the source the
			// step before it had just dropped.
			if crash.after == 3 {
				assertNoSearchRef(t, data, removing)
			}

			// AC #2, through a server that comes up on the data directory as it
			// is after the crash: the context is not in the registry it serves,
			// and every query tool answers for it exactly as it does for an id
			// that was never managed at all.
			session, _ := serveManaged(t, data, demorepo.StartZoekt(t, filepath.Join(data, "zoekt")))
			if got, want := listedContexts(t, session), []string{kept.ID}; !slices.Equal(got, want) {
				t.Errorf("list_contexts = %v, want only the context that is not being removed, %v", got, want)
			}
			for _, call := range []struct {
				tool string
				args map[string]any
			}{
				{"search_code", map[string]any{"context": removing.ID, "query": alphaToken}},
				{"trace_calls", map[string]any{"context": removing.ID, "symbol": alphaToken, "direction": "callees", "depth": 1}},
				{"get_code", map[string]any{"context": removing.ID, "path": "alpha.go", "start_line": 1, "end_line": 3}},
			} {
				res := callTool(t, session, call.tool, call.args)
				if !res.IsError {
					t.Fatalf("%s in a context being removed succeeded, want an error result", call.tool)
				}
				if got := errorCode(t, res); got != vacerr.ContextNotFound {
					t.Errorf("%s in a context being removed = %q, want %q: %s", call.tool, got, vacerr.ContextNotFound, resultText(t, res))
				}
			}
			// The server is answering rather than failing everything: the other
			// context of the same repository is served out of the same index, so
			// the answers above are about this context and not about a server
			// that could not serve anything.
			if !searchFinds(t, session, kept.ID, betaToken) {
				t.Errorf("search_code(%s, %s) found nothing, so the answers above would pass for the wrong reason", kept.ID, betaToken)
			}
			_ = session.Close()

			// AC #3: the same command again, and it finishes.
			if out, err := contextRun(t, data, "remove", removing.ID); err != nil || out != removing.ID+"\tREMOVED\n" {
				t.Fatalf("context remove after a crash = %q (err %v), want it to finish the removal", out, err)
			}
			assertRemoved(t, data, removing, kept)
		})
	}
}

// crashRemoval takes the steps `context remove` takes against c, in its order —
// the state, then the graph and the checkout, then the search ref, then the
// shard rebuilt beside the record left behind — and stops after the given
// number of them, leaving what a process killed there would leave. kept is the
// other context of the same repository, whose search ref is the one that
// survives the rebuild.
//
// Those steps are re-implemented here rather than called: the removal's stages
// live in managed/ and are unexported. What is faithful to the command is the
// order and the engines — the same git, codebase-memory-mcp and zoekt-git-index
// it drives — and not the code path. Three things this does not reproduce: it
// takes neither the server lock nor the repository lock, and writes REMOVING
// straight to the store rather than through the transition check; it builds the
// clone and worktree paths itself instead of asking the store for them; and its
// rebuild names kept.Branch outright, where the command indexes every
// non-REMOVING record of the repository and drops the shards when none is left.
// For this fixture's two contexts, one removing and one kept, those two rebuilds
// produce the same shard; a third context, or none surviving, would not be
// covered by this and would need the real rebuild.
//
// Every step here must also succeed, where the command's tolerate a graph, a
// clone or a ref that is already gone so that an interrupted removal stays
// resumable. Here they are all present by construction.
func crashRemoval(t *testing.T, data string, c, kept store.Context, steps int) {
	t.Helper()
	member, keptMember := only(t, c), only(t, kept)
	clone := filepath.Join(data, "repos", member.Repository)
	worktree := filepath.Join(data, "worktrees", member.Repository, c.ID)

	// The state first, as the command writes it first. A crash before this is
	// not a state of the removal at all: nothing had happened yet.
	c.State = managed.ContextRemoving
	if err := openStore(t, data).PutContext(c); err != nil {
		t.Fatalf("PutContext: %v", err)
	}

	for i, step := range []func(){
		func() {
			mustRun(t, managed.CBMCommand, "cli", "delete_project", "--project", member.GraphRef)
			if err := os.RemoveAll(worktree); err != nil {
				t.Fatalf("RemoveAll(%s): %v", worktree, err)
			}
			mustGit(t, "-C", clone, "worktree", "prune")
		},
		func() { mustGit(t, "-C", clone, "update-ref", "-d", "refs/heads/"+member.Branch) },
		func() {
			mustRun(t, indexer, "-index", filepath.Join(data, "zoekt"), "-branches", keptMember.Branch, clone)
		},
	} {
		if i >= steps {
			return
		}
		step()
	}
}

// mustRun runs a command and fails the test with its output if it does not
// succeed.
func mustRun(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

// assertNoSearchRef fails when the clone still has the context's search ref.
func assertNoSearchRef(t *testing.T, data string, c store.Context) {
	t.Helper()
	member := only(t, c)
	clone := filepath.Join(data, "repos", member.Repository)
	out, err := exec.Command("git", "-C", clone, "rev-parse", "--verify", "refs/heads/"+member.Branch).CombinedOutput()
	if err == nil {
		t.Errorf("refs/heads/%s is in the clone as %s, want the removal to have taken it out", member.Branch, strings.TrimSpace(string(out)))
	}
}

// assertRemoved is what a finished removal leaves: no record, no worktree, no
// graph, no search ref and nothing of it in the index — with the other context
// of the same repository untouched, so none of it passes because everything
// was destroyed.
func assertRemoved(t *testing.T, data string, removed, kept store.Context) {
	t.Helper()
	s := openStore(t, data)
	member, keptMember := only(t, removed), only(t, kept)
	if _, err := s.Context(removed.ID); codeFor(t, err) != vacerr.ContextNotFound {
		t.Errorf("the record of %s after the removal = %v, want %q", removed.ID, err, vacerr.ContextNotFound)
	}
	worktree, err := s.WorktreeDir(member.Repository, removed.ID)
	if err != nil {
		t.Fatalf("WorktreeDir: %v", err)
	}
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Errorf("the worktree of %s is still at %s (err = %v)", removed.ID, worktree, err)
	}
	assertNoSearchRef(t, data, removed)
	if err := graphExists(t.Context(), removed.ID, member); err == nil {
		t.Errorf("codebase-memory-mcp still holds the graph %q of the removed context", member.GraphRef)
	}

	url := demorepo.StartZoekt(t, filepath.Join(data, "zoekt"))
	indexed, err := searchRefIndexed(t.Context(), url, member)
	if err != nil {
		t.Fatalf("searchRefIndexed(%s): %v", removed.ID, err)
	}
	if indexed {
		t.Errorf("%s: search ref %q is still in the index after the removal finished", removed.ID, member.Branch)
	}
	if searchable(t, url, removed.ID, member, alphaToken) {
		t.Errorf("%s: %s is still searchable after the removal finished", removed.ID, alphaToken)
	}

	if got := contextRecord(t, data, kept.ID); !sameContext(got, kept) {
		t.Errorf("the removal changed the other context:\n before %+v\n  after %+v", kept, got)
	}
	if !searchable(t, url, kept.ID, keptMember, betaToken) {
		t.Errorf("%s can no longer find %s after the other context was removed", kept.ID, betaToken)
	}
}
