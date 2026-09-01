// Package demorepo gives tests the versioned demo repository the
// version-correctness release gate runs against.
//
// The fixture is generated, never committed: a nested .git directory cannot be
// tracked by this repository, so testdata/versioned-demo-repo is in .gitignore
// and testdata/gen-versioned-demo-repo.sh produces it on demand.
//
// Every revision is resolved by running git against the generated repository,
// so no test ever has to name a commit hash.
package demorepo

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The branches the generator creates. Process() calls LegacyHandler on V1 and
// NewHandler on V2.
const (
	Main = "main"
	V1   = "release/v1"
	V2   = "release/v2"
)

// Generate returns the absolute path of the demo repository, running the
// generator script unless the fixture is already there with all its branches.
// The script pins the commit identity and dates, so an existing fixture and a
// freshly generated one have the same revisions.
//
// Generation is serialised across processes. `go test ./...` runs each
// package's binary in parallel and more than one of them lands here on a cold
// checkout; two generators running at once would each be swapping the fixture
// path out from under the other. mkdir is atomic on every platform, so exactly
// one process generates and the others wait.
//
// The lock serialises generators, and only generators: a caller that finds the
// fixture already complete returns it without taking the lock, and the tests it
// hands the path to then read that fixture for as long as they run. So what
// keeps a reader off a fixture being rebuilt is not this lock but the
// generator, which builds in a staging directory and publishes by rename —
// leaving no moment where the fixture path holds anything other than a finished
// repository. complete() therefore answers about a settled fixture or not at
// all, which is what makes both of the unlocked checks below safe.
//
// It was not always so. The generator used to build in place, and its three
// branches all exist from `checkout -b release/v2` onward — two commits before
// release/v2 holds anything of its own. A caller taking the early return in
// that window got a fixture whose release/v1 and release/v2 held the same
// processor.go, and tools/get_code reported that as broken version isolation,
// which is the last thing this project's tests should be able to say falsely.
// internal/demorepo's publish_test.go holds the generator to the invariant.
func Generate(t testing.TB) string {
	t.Helper()
	root := moduleRoot(t)
	repo := filepath.Join(root, "testdata", "versioned-demo-repo")
	if complete(repo) {
		return repo
	}

	lock := filepath.Join(root, "testdata", ".demorepo-lock")
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if err := os.Mkdir(lock, 0o755); err == nil {
			defer func() { _ = os.Remove(lock) }()
			// Re-check: the holder we queued behind may have just published it.
			if !complete(repo) {
				script := filepath.Join(root, "testdata", "gen-versioned-demo-repo.sh")
				if out, err := exec.Command(script).CombinedOutput(); err != nil {
					t.Fatalf("demorepo: %s: %v\n%s", script, err, out)
				}
			}
			return repo
		} else if !os.IsExist(err) {
			t.Fatalf("demorepo: lock %s: %v", lock, err)
		}
		if complete(repo) {
			return repo
		}
		if time.Now().After(deadline) {
			t.Fatalf("demorepo: timed out waiting for another process to generate the fixture; remove %s if it is stale", lock)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Revision returns the commit hash branch points at in the repository at repo,
// resolved at call time. Regenerating the fixture may move it, so a test must
// take the revision from here instead of naming a hash.
func Revision(t testing.TB, repo, branch string) string {
	t.Helper()
	out, err := gitIn(repo, "rev-parse", "refs/heads/"+branch).Output()
	if err != nil {
		t.Fatalf("demorepo: git rev-parse %s: %v", branch, err)
	}
	return strings.TrimSpace(string(out))
}

// gitIn runs git against the fixture and only the fixture. --git-dir is given
// explicitly because `git -C repo` does not fail when repo has no .git: it
// searches upward and finds the enclosing vacmcp repository instead. That makes
// the fixture's own branch names resolve against the wrong repository, and a
// half-generated fixture would silently be reported as complete.
func gitIn(repo string, args ...string) *exec.Cmd {
	return exec.Command("git", append([]string{"--git-dir", filepath.Join(repo, ".git"), "--work-tree", repo}, args...)...)
}

// moduleRoot returns the module directory, found from this file's own path so a
// test can be run from any working directory.
func moduleRoot(t testing.TB) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("demorepo: cannot locate the package source")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(self)))
}

// complete reports whether repo already holds all three branches, which is the
// only state the generator leaves behind on success. Because the generator
// publishes the finished fixture by renaming it into place, this is a question
// about a fixture that is not being written to: the three branches are never
// visible here mid-build.
func complete(repo string) bool {
	for _, branch := range []string{Main, V1, V2} {
		if gitIn(repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch).Run() != nil {
			return false
		}
	}
	return true
}
