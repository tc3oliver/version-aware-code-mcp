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
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
// ponytail: no cross-process lock. Two packages generating the fixture at the
// same time on a cold checkout would race; add a lock file if that ever bites.
func Generate(t testing.TB) string {
	t.Helper()
	root := moduleRoot(t)
	repo := filepath.Join(root, "testdata", "versioned-demo-repo")
	if !complete(repo) {
		script := filepath.Join(root, "testdata", "gen-versioned-demo-repo.sh")
		if out, err := exec.Command(script).CombinedOutput(); err != nil {
			t.Fatalf("demorepo: %s: %v\n%s", script, err, out)
		}
	}
	return repo
}

// Revision returns the commit hash branch points at in the repository at repo,
// resolved at call time. Regenerating the fixture may move it, so a test must
// take the revision from here instead of naming a hash.
func Revision(t testing.TB, repo, branch string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "rev-parse", "refs/heads/"+branch).Output()
	if err != nil {
		t.Fatalf("demorepo: git rev-parse %s: %v", branch, err)
	}
	return strings.TrimSpace(string(out))
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
// only state the generator leaves behind on success.
func complete(repo string) bool {
	for _, branch := range []string{Main, V1, V2} {
		if exec.Command("git", "-C", repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch).Run() != nil {
			return false
		}
	}
	return true
}
