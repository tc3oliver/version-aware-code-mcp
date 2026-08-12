package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/store"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// The repo commands are tested against real git repositories created in
// temporary directories, never against a fake git. What is being checked is that
// a clone really happens, that it has no working tree, and that a fetch really
// brings a new commit within reach — none of which a stub could tell the truth
// about. No network is involved: a local path is a git remote URL like any
// other.

// sourceRepo creates a git repository with one commit and returns its path. It
// stands in for the remote a user would add.
func sourceRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "source")
	mustGit(t, "init", "-q", "-b", "main", dir)
	// Nothing below may run git in a directory that is not a repository: git
	// would search upward and act on whatever repository encloses it.
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Fatalf("git init left no .git in %s: %v", dir, err)
	}
	commit(t, dir, "one.txt", "one\n", "one")
	return dir
}

// commit writes a file in repo and commits it, returning the new commit's SHA.
func commit(t *testing.T, repo, name, body, message string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mustGit(t, "-C", repo, "add", name)
	// The caller's global git configuration decides neither the identity nor
	// whether this commit is signed or hooked.
	mustGit(t, "-C", repo,
		"-c", "user.name=vacmcp test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false",
		"commit", "--no-verify", "-q", "-m", message)
	return gitOut(t, "-C", repo, "rev-parse", "HEAD")
}

func mustGit(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func gitOut(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// repoRun runs one `vacmcp repo` command against dataDir through the top-level
// dispatch, so the CLI wiring is exercised too, and returns what it printed.
func repoRun(t *testing.T, dataDir string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := run(append(append([]string{"repo"}, args...), "--data-dir", dataDir), &out)
	return out.String(), err
}

// openStore returns a store on dataDir, for the assertions that read records
// rather than command output.
func openStore(t *testing.T, dataDir string) *store.Store {
	t.Helper()
	s, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("store.Open(%s): %v", dataDir, err)
	}
	return s
}

// codeFor returns the vacerr code err carries.
func codeFor(t *testing.T, err error) vacerr.Code {
	t.Helper()
	if err == nil {
		t.Fatal("got no error, want one")
	}
	var vErr *vacerr.Error
	if !errors.As(err, &vErr) {
		t.Fatalf("error = %v (%T), want *vacerr.Error", err, err)
	}
	return vErr.Code
}

// TestRepoAddClonesWithoutAWorkingTree is AC #1: a git URL is cloned into the
// data directory and recorded. The clone is --no-checkout, so the file the
// source commits must be in the clone's object database and not in its working
// tree.
func TestRepoAddClonesWithoutAWorkingTree(t *testing.T) {
	source := sourceRepo(t)
	data := t.TempDir()

	out, err := repoRun(t, data, "add", "demo", "--url", source)
	if err != nil {
		t.Fatalf("repo add: %v", err)
	}
	if !strings.Contains(out, "demo\tREADY\t") {
		t.Errorf("repo add printed %q, want it to report demo as READY", out)
	}

	clone := filepath.Join(data, "repos", "demo")
	if _, err := os.Stat(filepath.Join(clone, ".git")); err != nil {
		t.Fatalf("no clone at %s: %v", clone, err)
	}
	if _, err := os.Stat(filepath.Join(clone, "one.txt")); !os.IsNotExist(err) {
		t.Errorf("the clone has a checked-out one.txt (err = %v), want --no-checkout to leave no working tree", err)
	}
	if got := gitOut(t, "-C", clone, "cat-file", "-p", "HEAD:one.txt"); got != "one" {
		t.Errorf("HEAD:one.txt in the clone = %q, want the source's content", got)
	}

	r, err := openStore(t, data).Repository("demo")
	if err != nil {
		t.Fatalf("Repository(demo): %v", err)
	}
	if r.URL != source || r.State != repoReady {
		t.Errorf("record = %+v, want URL %q and state %s", r, source, repoReady)
	}
	if !r.LastSyncAt.IsZero() {
		t.Errorf("record.LastSyncAt = %v, want zero: adding is not syncing", r.LastSyncAt)
	}
}

// TestRepoAddRecordsAFailedClone pins the other half of AC #1's state
// reporting: a clone that fails is a repository the user can see and remove,
// and the command still fails.
func TestRepoAddRecordsAFailedClone(t *testing.T) {
	data := t.TempDir()
	missing := filepath.Join(t.TempDir(), "not-a-repository")

	if _, err := repoRun(t, data, "add", "demo", "--url", missing); err == nil {
		t.Fatal("repo add on an unreachable URL returned nil, want an error")
	}

	r, err := openStore(t, data).Repository("demo")
	if err != nil {
		t.Fatalf("Repository(demo): %v", err)
	}
	if r.State != repoFailed {
		t.Errorf("record.State = %q, want %s", r.State, repoFailed)
	}
}

func TestRepoAddRejectsAnUnusableName(t *testing.T) {
	data := t.TempDir()
	source := sourceRepo(t)

	// A name beginning with a dash is not in this list: the flag package takes
	// it as a flag before any of this is reached, so it never becomes a name.
	for _, name := range []string{"../escape", "a/b", "..", ".", ""} {
		_, err := repoRun(t, data, "add", name, "--url", source)
		if err == nil {
			t.Errorf("repo add %q returned nil, want an error", name)
			continue
		}
		// A name that is not a usable path element is rejected before any git
		// runs, so nothing is left behind for it.
		if entries, _ := os.ReadDir(filepath.Join(data, "repos")); len(entries) != 0 {
			t.Errorf("repo add %q created %v under repos/, want nothing", name, entries)
		}
	}
}

func TestRepoAddRequiresAURL(t *testing.T) {
	if _, err := repoRun(t, t.TempDir(), "add", "demo"); err == nil {
		t.Fatal("repo add without --url returned nil, want an error")
	}
}

// TestRepoAddRefusesToReplaceAManagedRepository keeps a second add from
// silently re-cloning over a repository whose contexts pin revisions in it.
func TestRepoAddRefusesToReplaceAManagedRepository(t *testing.T) {
	data := t.TempDir()
	source := sourceRepo(t)

	if _, err := repoRun(t, data, "add", "demo", "--url", source); err != nil {
		t.Fatalf("repo add: %v", err)
	}
	_, err := repoRun(t, data, "add", "demo", "--url", source)
	if got := codeFor(t, err); got != vacerr.InvalidArgument {
		t.Errorf("second repo add code = %q, want %q", got, vacerr.InvalidArgument)
	}
}

// TestRepoSyncFetchesWithoutMovingAPinnedRevision is AC #2, the invariant this
// project exists for: a sync brings new commits within reach and leaves every
// context on exactly the revision it was pinned to.
func TestRepoSyncFetchesWithoutMovingAPinnedRevision(t *testing.T) {
	source := sourceRepo(t)
	data := t.TempDir()
	if _, err := repoRun(t, data, "add", "demo", "--url", source); err != nil {
		t.Fatalf("repo add: %v", err)
	}

	s := openStore(t, data)
	pinned := gitOut(t, "-C", source, "rev-parse", "HEAD")
	// The context a later version of vacmcp would create. Only its record
	// matters here: it is the only place a pinned revision is written down.
	if err := s.PutContext(store.Context{
		ID: "demo-v1", Repository: "demo", Branch: "main", Revision: pinned, GraphRef: "demo-v1", State: "READY",
	}); err != nil {
		t.Fatalf("PutContext: %v", err)
	}
	before, err := s.Context("demo-v1")
	if err != nil {
		t.Fatalf("Context(demo-v1): %v", err)
	}

	moved := commit(t, source, "two.txt", "two\n", "two")
	if moved == pinned {
		t.Fatal("the second commit did not move the source's HEAD")
	}

	if _, err := repoRun(t, data, "sync", "demo"); err != nil {
		t.Fatalf("repo sync: %v", err)
	}

	// The fetch happened: the new commit is in the clone.
	clone := filepath.Join(data, "repos", "demo")
	if got := gitOut(t, "-C", clone, "rev-parse", "--verify", moved+"^{commit}"); got != moved {
		t.Errorf("the clone resolves %s to %q after sync, want the new commit fetched", moved, got)
	}

	// The pinned revision did not: the context record is untouched, down to the
	// timestamp that any rewrite would have stamped.
	after, err := s.Context("demo-v1")
	if err != nil {
		t.Fatalf("Context(demo-v1) after sync: %v", err)
	}
	if after != before {
		t.Errorf("the context record changed across sync:\n before %+v\n  after %+v", before, after)
	}
	if after.Revision != pinned {
		t.Errorf("pinned revision = %q after sync, want %q", after.Revision, pinned)
	}

	r, err := s.Repository("demo")
	if err != nil {
		t.Fatalf("Repository(demo): %v", err)
	}
	if r.LastSyncAt.IsZero() || r.State != repoReady {
		t.Errorf("record after sync = %+v, want state %s and a last sync time", r, repoReady)
	}
}

// TestRepoSyncAllReportsEveryFailure pins the --all behaviour: every repository
// is fetched whatever the ones before it did, and a repository whose remote has
// gone is recorded as FAILED and named in the error.
func TestRepoSyncAllReportsEveryFailure(t *testing.T) {
	data := t.TempDir()
	good := sourceRepo(t)
	gone := sourceRepo(t)

	for name, url := range map[string]string{"good": good, "gone": gone} {
		if _, err := repoRun(t, data, "add", name, "--url", url); err != nil {
			t.Fatalf("repo add %s: %v", name, err)
		}
	}
	if err := os.RemoveAll(gone); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	out, err := repoRun(t, data, "sync", "--all")
	if err == nil {
		t.Fatal("repo sync --all returned nil, want the unreachable remote reported")
	}
	if !strings.Contains(err.Error(), "gone") {
		t.Errorf("error = %v, want it to name the repository that failed", err)
	}
	// The working repository was still fetched: --all does not stop at the
	// first failure.
	if !strings.Contains(out, "good\tREADY") {
		t.Errorf("repo sync --all printed %q, want the reachable repository synced", out)
	}

	s := openStore(t, data)
	if r, err := s.Repository("gone"); err != nil || r.State != repoFailed {
		t.Errorf("gone = %+v (err %v), want state %s", r, err, repoFailed)
	}
	if r, err := s.Repository("good"); err != nil || r.State != repoReady {
		t.Errorf("good = %+v (err %v), want state %s", r, err, repoReady)
	}
}

func TestRepoSyncArguments(t *testing.T) {
	data := t.TempDir()

	// Neither a name nor --all, and both at once, are refused rather than one
	// of them being guessed at.
	if _, err := repoRun(t, data, "sync"); err == nil {
		t.Error("repo sync without NAME or --all returned nil, want an error")
	}
	if _, err := repoRun(t, data, "sync", "demo", "--all"); err == nil {
		t.Error("repo sync with both NAME and --all returned nil, want an error")
	}

	_, err := repoRun(t, data, "sync", "absent")
	if got := codeFor(t, err); got != vacerr.RepositoryNotFound {
		t.Errorf("repo sync of an unmanaged repository code = %q, want %q", got, vacerr.RepositoryNotFound)
	}
}

// TestRepoRemoveRefusesWhileAContextDependsOnIt is AC #3: the blocking context
// IDs are named, and nothing is deleted.
func TestRepoRemoveRefusesWhileAContextDependsOnIt(t *testing.T) {
	source := sourceRepo(t)
	data := t.TempDir()
	if _, err := repoRun(t, data, "add", "demo", "--url", source); err != nil {
		t.Fatalf("repo add: %v", err)
	}

	s := openStore(t, data)
	for _, id := range []string{"demo-v1", "demo-v2"} {
		if err := s.PutContext(store.Context{ID: id, Repository: "demo", State: "READY"}); err != nil {
			t.Fatalf("PutContext(%s): %v", id, err)
		}
	}
	// A context of another repository must not block this one.
	if err := s.PutContext(store.Context{ID: "other-v1", Repository: "other", State: "READY"}); err != nil {
		t.Fatalf("PutContext(other-v1): %v", err)
	}

	_, err := repoRun(t, data, "remove", "demo")
	if got := codeFor(t, err); got != vacerr.InvalidArgument {
		t.Errorf("repo remove code = %q, want %q", got, vacerr.InvalidArgument)
	}
	for _, id := range []string{"demo-v1", "demo-v2"} {
		if !strings.Contains(err.Error(), id) {
			t.Errorf("error = %v, want it to name the blocking context %s", err, id)
		}
	}
	if strings.Contains(err.Error(), "other-v1") {
		t.Errorf("error = %v, want it not to name a context of another repository", err)
	}

	if _, statErr := os.Stat(filepath.Join(data, "repos", "demo")); statErr != nil {
		t.Errorf("the clone was deleted by a refused removal: %v", statErr)
	}
	if _, err := s.Repository("demo"); err != nil {
		t.Errorf("the record was deleted by a refused removal: %v", err)
	}
}

func TestRepoRemoveDeletesTheCloneAndTheRecord(t *testing.T) {
	source := sourceRepo(t)
	data := t.TempDir()
	if _, err := repoRun(t, data, "add", "demo", "--url", source); err != nil {
		t.Fatalf("repo add: %v", err)
	}

	if _, err := repoRun(t, data, "remove", "demo"); err != nil {
		t.Fatalf("repo remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(data, "repos", "demo")); !os.IsNotExist(err) {
		t.Errorf("the clone is still there after remove (err = %v)", err)
	}
	if _, err := openStore(t, data).Repository("demo"); codeFor(t, err) != vacerr.RepositoryNotFound {
		t.Errorf("Repository(demo) after remove = %v, want %q", err, vacerr.RepositoryNotFound)
	}

	// Removing it again is the not-found error, not a silent success.
	if _, err := repoRun(t, data, "remove", "demo"); codeFor(t, err) != vacerr.RepositoryNotFound {
		t.Errorf("second repo remove = %v, want %q", err, vacerr.RepositoryNotFound)
	}
}

func TestRepoListReportsEveryRepository(t *testing.T) {
	data := t.TempDir()

	out, err := repoRun(t, data, "list")
	if err != nil {
		t.Fatalf("repo list on a fresh data directory: %v", err)
	}
	if out != "" {
		t.Errorf("repo list printed %q on a fresh data directory, want nothing", out)
	}

	first, second := sourceRepo(t), sourceRepo(t)
	for name, url := range map[string]string{"beta": second, "alpha": first} {
		if _, err := repoRun(t, data, "add", name, "--url", url); err != nil {
			t.Fatalf("repo add %s: %v", name, err)
		}
	}

	out, err = repoRun(t, data, "list")
	if err != nil {
		t.Fatalf("repo list: %v", err)
	}
	want := "alpha\tREADY\t" + first + "\tnever synced\n" +
		"beta\tREADY\t" + second + "\tnever synced\n"
	if out != want {
		t.Errorf("repo list printed\n%q\nwant\n%q", out, want)
	}
}

func TestRepoStatusReportsThePathAndTheDependingContexts(t *testing.T) {
	source := sourceRepo(t)
	data := t.TempDir()
	if _, err := repoRun(t, data, "add", "demo", "--url", source); err != nil {
		t.Fatalf("repo add: %v", err)
	}
	if err := openStore(t, data).PutContext(store.Context{ID: "demo-v1", Repository: "demo", State: "READY"}); err != nil {
		t.Fatalf("PutContext: %v", err)
	}

	out, err := repoRun(t, data, "status", "demo")
	if err != nil {
		t.Fatalf("repo status: %v", err)
	}
	for _, want := range []string{"demo", source, repoReady, filepath.Join(data, "repos", "demo"), "never synced", "demo-v1"} {
		if !strings.Contains(out, want) {
			t.Errorf("repo status printed\n%s\nwant it to report %q", out, want)
		}
	}

	if _, err := repoRun(t, data, "status", "absent"); codeFor(t, err) != vacerr.RepositoryNotFound {
		t.Errorf("repo status of an unmanaged repository = %v, want %q", err, vacerr.RepositoryNotFound)
	}
}

func TestRepoRejectsUnknownSubcommands(t *testing.T) {
	for _, args := range [][]string{{"repo"}, {"repo", "clone"}} {
		if err := run(args, &bytes.Buffer{}); err == nil {
			t.Errorf("run(%q) returned nil, want an error", args)
		}
	}
}
