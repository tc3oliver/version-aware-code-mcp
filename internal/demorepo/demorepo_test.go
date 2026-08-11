package demorepo_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/internal/demorepo"
)

// git runs a git command against the fixture and fails the test if it does not
// succeed.
func git(t testing.TB, repo string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func TestGeneratedRepositoryHasTheThreeBranches(t *testing.T) {
	repo := demorepo.Generate(t)

	var got []string
	for _, line := range strings.Split(strings.TrimSpace(git(t, repo, "branch", "--list")), "\n") {
		got = append(got, strings.TrimPrefix(strings.TrimSpace(line), "* "))
	}
	slices.Sort(got)

	want := []string{demorepo.Main, demorepo.V1, demorepo.V2}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("branches = %v, want %v", got, want)
	}
}

func TestProcessCallsTheHandlerOfItsRelease(t *testing.T) {
	repo := demorepo.Generate(t)

	v1 := git(t, repo, "show", demorepo.V1+":processor.go")
	if !strings.Contains(v1, "return LegacyHandler(req)") {
		t.Fatalf("%s Process() does not call LegacyHandler:\n%s", demorepo.V1, v1)
	}

	v2 := git(t, repo, "show", demorepo.V2+":processor.go")
	if !strings.Contains(v2, "return NewHandler(req)") {
		t.Fatalf("%s Process() does not call NewHandler:\n%s", demorepo.V2, v2)
	}
}

// TestNewHandlerExistsOnlyOnV2 guards the fixture side of the release gate:
// searching v1 for NewHandler must find nothing, searching v2 must find it. A
// fixture that leaks the symbol into v1 would make the gate pass by accident.
func TestNewHandlerExistsOnlyOnV2(t *testing.T) {
	repo := demorepo.Generate(t)

	out, err := exec.Command("git", "-C", repo, "grep", "-l", "NewHandler", demorepo.V1).Output()
	var exit *exec.ExitError
	switch {
	case err == nil:
		t.Fatalf("%s mentions NewHandler in:\n%s", demorepo.V1, out)
	case !errors.As(err, &exit) || exit.ExitCode() != 1:
		t.Fatalf("git grep on %s: %v", demorepo.V1, err)
	}

	if files := git(t, repo, "grep", "-l", "NewHandler", demorepo.V2); files == "" {
		t.Fatalf("%s does not mention NewHandler", demorepo.V2)
	}
}

func TestRevisionResolvesADistinctCommitPerBranch(t *testing.T) {
	repo := demorepo.Generate(t)

	seen := map[string]string{}
	for _, branch := range []string{demorepo.Main, demorepo.V1, demorepo.V2} {
		rev := demorepo.Revision(t, repo, branch)
		if kind := strings.TrimSpace(git(t, repo, "cat-file", "-t", rev)); kind != "commit" {
			t.Fatalf("%s revision %q is a %s, not a commit", branch, rev, kind)
		}
		if other, dup := seen[rev]; dup {
			t.Fatalf("%s and %s share revision %s", branch, other, rev)
		}
		seen[rev] = branch
	}
}

// TestRegeneratingKeepsTheRevisions is the idempotency check: the generator
// wipes and rebuilds the fixture, and a rebuild must land on the same commits
// so an already indexed fixture stays valid.
//
// The rebuild runs on a copy of the generator in a temporary root instead of on
// the shared fixture. `go test ./...` runs the packages in parallel, and while
// this one runs most of the others are reading testdata/versioned-demo-repo
// through git, Zoekt and CBM; deleting it under them failed unrelated packages
// with REVISION_NOT_FOUND on a revision that does exist. The script pins the
// commit identity and the dates and rebuilds from scratch wherever it is run,
// so where it runs cannot change what it produces — which is the claim under
// test.
func TestRegeneratingKeepsTheRevisions(t *testing.T) {
	repo := demorepo.Generate(t)
	rebuilt := regenerateElsewhere(t)

	for _, branch := range []string{demorepo.Main, demorepo.V1, demorepo.V2} {
		want := demorepo.Revision(t, repo, branch)
		if got := demorepo.Revision(t, rebuilt, branch); got != want {
			t.Fatalf("%s revision = %s after regeneration, want %s", branch, got, want)
		}
	}
}

// regenerateElsewhere runs a copy of the generator in a temporary root and
// returns the repository it produced there.
func regenerateElsewhere(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	// The generator finds everything from its own path, so a copy in the same
	// place relative to a different root builds that root's fixture.
	src, err := os.ReadFile(filepath.Join("..", "..", "testdata", "gen-versioned-demo-repo.sh"))
	if err != nil {
		t.Fatalf("read generator: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "testdata"), 0o750); err != nil {
		t.Fatalf("mkdir testdata: %v", err)
	}
	script := filepath.Join(root, "testdata", "gen-versioned-demo-repo.sh")
	if err := os.WriteFile(script, src, 0o700); err != nil {
		t.Fatalf("write generator: %v", err)
	}

	if out, err := exec.Command(script).CombinedOutput(); err != nil {
		t.Fatalf("generator failed: %v\n%s", err, out)
	}
	return filepath.Join(root, "testdata", "versioned-demo-repo")
}
