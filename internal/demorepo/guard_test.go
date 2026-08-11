package demorepo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The generator runs inside a checkout of this project, so every git call it
// makes is one directory below a real repository. `git -C repo` does not fail
// when repo has no .git — it searches upward — so a fixture path without .git
// makes the generator commit to the enclosing repository and switch its branch.
// That destroyed an uncommitted working tree once. These tests pin the guard.

// enclosing builds a throwaway git repository holding a copy of the generator,
// with an uncommitted file and a named branch, so a test can assert afterwards
// that none of it moved.
func enclosing(t *testing.T) (root, script string) {
	t.Helper()
	root = t.TempDir()

	src, err := os.ReadFile(filepath.Join(moduleRoot(t), "testdata", "gen-versioned-demo-repo.sh"))
	if err != nil {
		t.Fatalf("read generator: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "testdata"), 0o755); err != nil {
		t.Fatalf("mkdir testdata: %v", err)
	}
	script = filepath.Join(root, "testdata", "gen-versioned-demo-repo.sh")
	if err := os.WriteFile(script, src, 0o755); err != nil {
		t.Fatalf("write generator: %v", err)
	}
	// Mirror the real checkout: the fixture path is ignored, so a successful
	// run leaves the enclosing working tree reported as clean.
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("/testdata/versioned-demo-repo/\n"), 0o644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}

	for _, args := range [][]string{
		{"init", "-q", "-b", "probe", root},
		{"-C", root, "config", "user.name", "probe"},
		{"-C", root, "config", "user.email", "probe@example.invalid"},
		{"-C", root, "add", "-A"},
		{"-C", root, "-c", "commit.gpgsign=false", "commit", "-q", "--no-verify", "-m", "base"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	// Uncommitted work is what the failure destroyed, so leave some.
	if err := os.WriteFile(filepath.Join(root, "precious.txt"), []byte("uncommitted\n"), 0o644); err != nil {
		t.Fatalf("write precious.txt: %v", err)
	}
	return root, script
}

// state captures everything the generator must not move in the enclosing repo.
func state(t *testing.T, root string) (head, branches, status string) {
	t.Helper()
	read := func(args ...string) string {
		out, err := exec.Command("git", append([]string{"-C", root}, args...)...).Output()
		if err != nil {
			t.Fatalf("git %s: %v", strings.Join(args, " "), err)
		}
		return string(out)
	}
	return read("rev-parse", "HEAD"), read("branch", "--list"), read("status", "--porcelain")
}

// TestGeneratorLeavesTheEnclosingRepositoryAlone is the regression test for the
// data loss: a normal, successful run must not touch the repository the fixture
// lives inside.
func TestGeneratorLeavesTheEnclosingRepositoryAlone(t *testing.T) {
	root, script := enclosing(t)
	headBefore, branchesBefore, statusBefore := state(t, root)

	if out, err := exec.Command(script).CombinedOutput(); err != nil {
		t.Fatalf("generator failed: %v\n%s", err, out)
	}

	headAfter, branchesAfter, statusAfter := state(t, root)
	if headAfter != headBefore {
		t.Errorf("enclosing HEAD moved: %q -> %q", headBefore, headAfter)
	}
	if branchesAfter != branchesBefore {
		t.Errorf("enclosing branches changed:\nbefore:\n%s\nafter:\n%s", branchesBefore, branchesAfter)
	}
	if statusAfter != statusBefore {
		t.Errorf("enclosing working tree changed:\nbefore:\n%s\nafter:\n%s", statusBefore, statusAfter)
	}
	if _, err := os.Stat(filepath.Join(root, "precious.txt")); err != nil {
		t.Errorf("uncommitted file did not survive: %v", err)
	}
}

// TestGeneratorRefusesWhenInitLeavesNoGitDir drives the guard directly. A git
// shim reports success for `init` without creating anything, which is the state
// a losing race leaves behind; the generator must abort instead of falling
// through to the enclosing repository.
func TestGeneratorRefusesWhenInitLeavesNoGitDir(t *testing.T) {
	root, script := enclosing(t)
	headBefore, branchesBefore, statusBefore := state(t, root)

	real, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not on PATH: %v", err)
	}
	shimDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatalf("mkdir shim: %v", err)
	}
	shim := "#!/bin/sh\nif [ \"$1\" = init ]; then exit 0; fi\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}

	cmd := exec.Command(script)
	cmd.Env = append(os.Environ(), "PATH="+shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("generator succeeded with no .git; it must refuse\n%s", out)
	}
	if !strings.Contains(string(out), "refusing to run git against the parent repository") {
		t.Errorf("expected the guard's message, got:\n%s", out)
	}

	headAfter, branchesAfter, statusAfter := state(t, root)
	if headAfter != headBefore || branchesAfter != branchesBefore || statusAfter != statusBefore {
		t.Errorf("enclosing repository was modified despite the guard:\nHEAD %q -> %q\nbranches:\n%s\n%s\nstatus:\n%s\n%s",
			headBefore, headAfter, branchesBefore, branchesAfter, statusBefore, statusAfter)
	}
}
