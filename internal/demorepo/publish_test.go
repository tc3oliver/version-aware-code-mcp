package demorepo

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Generate hands the fixture path to its caller the moment complete() says yes,
// without holding the generation lock — before it takes the lock, and again
// while waiting for whoever holds it. Both are safe only if complete() cannot
// say yes about a fixture that is still being built, so that is what is tested
// here: the generator runs, and every time complete() reports the fixture
// usable, the fixture has to actually be a finished one.
//
// The window this reproduces is the one that failed
// tools.TestGetCodeReturnsTheContextsOwnVersion twice on a cold start:
// `checkout -b release/v2 main` creates the third branch two commits before
// release/v2 differs from main, so an in-place build shows three branches whose
// v1 and v2 hold the same processor.go. The two contexts read their two
// distinct revisions and got identical content, and the test said version
// isolation was broken. It was not; the fixture was.

// TestCompleteNeverReportsAFixtureThatIsStillBeingBuilt runs the real generator
// in a temporary root, with the window widened so the reproduction does not
// depend on winning a race, and watches the fixture path throughout.
func TestCompleteNeverReportsAFixtureThatIsStillBeingBuilt(t *testing.T) {
	root := t.TempDir()
	script := generatorWithAWideWindow(t, root)
	repo := filepath.Join(root, "testdata", "versioned-demo-repo")

	var out bytes.Buffer
	cmd := exec.Command(script)
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start generator: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var caught string
	for {
		if caught == "" && complete(repo) {
			caught = unfinished(repo)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("generator failed: %v\n%s", err, out.String())
			}
			if caught != "" {
				t.Fatalf("complete() reported a fixture that was still being built: %s", caught)
			}
			if !complete(repo) {
				t.Fatalf("complete() is false after a successful generation of %s\n%s", repo, out.String())
			}
			if bad := unfinished(repo); bad != "" {
				t.Fatalf("the published fixture is not finished: %s", bad)
			}
			return
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// unfinished returns why repo is not a finished fixture, or "" when it is. It
// looks past the three branches complete() checks, at what a caller of Generate
// would go on to read: whether release/v2 carries its own commit and its own
// processor.go, and whether the generator's closing `checkout main` has run.
func unfinished(repo string) string {
	read := func(args ...string) (string, error) {
		out, err := gitIn(repo, args...).Output()
		return strings.TrimSpace(string(out)), err
	}

	revs := map[string]string{}
	for _, branch := range []string{Main, V1, V2} {
		rev, err := read("rev-parse", "refs/heads/"+branch)
		if err != nil {
			return fmt.Sprintf("%s is unreadable: %v", branch, err)
		}
		revs[branch] = rev
	}
	if revs[V2] == revs[Main] {
		return fmt.Sprintf("%s is still at %s's commit %s, so both releases would answer with the same source", V2, Main, revs[V2])
	}

	src, err := read("show", "refs/heads/"+V2+":processor.go")
	if err != nil {
		return fmt.Sprintf("%s:processor.go is unreadable: %v", V2, err)
	}
	if !strings.Contains(src, "NewHandler") {
		return fmt.Sprintf("%s processor.go does not call NewHandler yet:\n%s", V2, src)
	}

	// Last, because it is the narrowest: the generator's closing checkout runs
	// after the final commit, so the branches can all be right while the working
	// tree is still on release/v2.
	head, err := read("symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return fmt.Sprintf("HEAD is unreadable: %v", err)
	}
	if want := "refs/heads/" + Main; head != want {
		return fmt.Sprintf("HEAD is %s, want %s: the generator has not reached its last step", head, want)
	}
	return ""
}

// generatorWithAWideWindow copies the generator into its own root and holds it
// open at the point the third branch is created, which is where the fixture
// first looks complete to a reader. The pause makes the window large enough to
// observe on purpose rather than by luck; it does not create the window, and a
// generator that publishes only finished work has nothing to observe there.
//
// The copy runs in a temporary root because the real fixture is shared: the
// other packages of `go test ./...` are reading it while this test runs, and
// rebuilding it under them fails them for unrelated reasons. The generator
// builds the same repository wherever it is run.
func generatorWithAWideWindow(t *testing.T, root string) string {
	t.Helper()

	src, err := os.ReadFile(filepath.Join(moduleRoot(t), "testdata", "gen-versioned-demo-repo.sh"))
	if err != nil {
		t.Fatalf("read generator: %v", err)
	}
	// The line that creates the third branch, whichever directory the generator
	// builds in. If it is gone the generator no longer has this shape and this
	// reproduction is testing nothing, so say so instead of passing.
	const marker = "checkout -q -b release/v2 main\n"
	if !strings.Contains(string(src), marker) {
		t.Fatalf("the generator no longer contains %q; this reproduction has to be rewritten around whatever creates the third branch", marker)
	}
	widened := strings.Replace(string(src), marker, marker+"\nsleep 1\n", 1)

	if err := os.Mkdir(filepath.Join(root, "testdata"), 0o755); err != nil {
		t.Fatalf("mkdir testdata: %v", err)
	}
	script := filepath.Join(root, "testdata", "gen-versioned-demo-repo.sh")
	if err := os.WriteFile(script, []byte(widened), 0o700); err != nil {
		t.Fatalf("write generator: %v", err)
	}
	return script
}
