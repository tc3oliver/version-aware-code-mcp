package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
		if want := tc.id + "\t" + contextCreating + "\t" + tc.want + "\n"; out != want {
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
		if c.Repository != "demo" || c.State != contextCreating {
			t.Errorf("record = %+v, want repository demo in state %s", c, contextCreating)
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
	for _, want := range []string{"app-v1", "demo", mainSHA, contextCreating, first.Branch, first.GraphRef, worktree} {
		if !strings.Contains(out, want) {
			t.Errorf("context status printed\n%s\nwant it to report %q", out, want)
		}
	}
}

// TestContextVerifyReportsWhetherTheRevisionIsStillThere covers what
// verification can cover for a context in CREATING: the pinned commit is still
// in the repository's object database. It writes nothing either way.
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
	want := fmt.Sprintf("alpha\tdemo\t%s\t%s\nbeta\tdemo\t%s\t%s\n", mainSHA, contextCreating, branchSHA, contextCreating)
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

func TestContextRejectsUnknownSubcommands(t *testing.T) {
	for _, args := range [][]string{{"context"}, {"context", "update"}} {
		if err := run(args, &bytes.Buffer{}); err == nil {
			t.Errorf("run(%q) returned nil, want an error", args)
		}
	}
}
