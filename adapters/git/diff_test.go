package git_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	gitadapter "github.com/tc3oliver/version-aware-code-mcp/adapters/git"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// This file is the evidence for the diff capability: that a comparison is
// between the two revisions the contexts pin and nothing else, and that the
// hunks it reports are the lines git reported rather than an approximation.
//
// Diffing is a capability of its own, so the adapter satisfies it in addition to
// the source contract asserted at the top of git_test.go, not instead of it.
var _ provider.SourceDiffer = (*gitadapter.Provider)(nil)

// The two versions of processor.go the change-classification tests compare, and
// the other files around them: one only in the from revision, one only in the
// to revision, one byte-identical in both.
const (
	processorFrom = "package demo\n\nfunc Process() {\n\tLegacyHandler()\n}\n\nfunc Helper() {}\n"
	processorTo   = "package demo\n\nfunc Process() {\n\tNewHandler()\n\tLog()\n}\n\nfunc Helper() {}\n"
)

// changedRepo is the fixture every classification test compares, built once per
// test so nothing it does can leak into another.
func changedRepo(t *testing.T) (repoPath, fromRevision, toRevision string) {
	t.Helper()
	return diffRepo(t,
		map[string]string{
			"processor.go": processorFrom,
			"removed.go":   "package gone\n",
			"same.go":      "package demo\n\nfunc Same() {}\n",
		},
		map[string]string{
			"processor.go": processorTo,
			"added.go":     "package arrived\n",
			"same.go":      "package demo\n\nfunc Same() {}\n",
		},
	)
}

// diffFor runs the comparison the tests are about: one path, between two
// contexts pinned to the fixture's two commits.
func diffFor(t *testing.T, repoPath, fromRevision, toRevision, filePath string) (*provider.SourceDiff, error) {
	t.Helper()
	return providerFor(repoPath).Diff(
		t.Context(),
		contextAt("app-v1", "release/1.x", fromRevision),
		contextAt("app-v2", "release/2.x", toRevision),
		provider.SourceDiffRequest{Path: filePath},
	)
}

// TestDiffClassifiesTheChange is AC #4: the four things that can have happened
// to a path between two revisions, each read off a real repository.
func TestDiffClassifiesTheChange(t *testing.T) {
	repoPath, fromRevision, toRevision := changedRepo(t)

	tests := map[string]struct {
		path string
		want provider.DiffChange
	}{
		"only in the to revision":   {"added.go", provider.ChangeAdded},
		"only in the from revision": {"removed.go", provider.ChangeRemoved},
		"different in both":         {"processor.go", provider.ChangeModified},
		"identical in both":         {"same.go", provider.ChangeUnchanged},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := diffFor(t, repoPath, fromRevision, toRevision, tc.path)
			if err != nil {
				t.Fatalf("Diff(%s) error = %v", tc.path, err)
			}
			if got.Change != tc.want {
				t.Errorf("Change = %q, want %q", got.Change, tc.want)
			}
			if got.Path != tc.path {
				t.Errorf("Path = %q, want %q", got.Path, tc.path)
			}
			if got.Binary {
				t.Errorf("Binary = true for the source file %s", tc.path)
			}
			if tc.want == provider.ChangeUnchanged && len(got.Hunks) != 0 {
				t.Errorf("Hunks = %+v, want none for an unchanged file", got.Hunks)
			}
			if tc.want != provider.ChangeUnchanged && len(got.Hunks) == 0 {
				t.Errorf("Hunks is empty, want the lines that changed")
			}
		})
	}
}

// TestDiffHunkLines is AC #6 and the whole point of a structured diff: a known
// change, so the hunk headers and every line's side can be pinned exactly. The
// classification above only says something happened; this says what.
func TestDiffHunkLines(t *testing.T) {
	repoPath, fromRevision, toRevision := changedRepo(t)

	got, err := diffFor(t, repoPath, fromRevision, toRevision, "processor.go")
	if err != nil {
		t.Fatalf("Diff() error = %v", err)
	}
	if len(got.Hunks) != 1 {
		t.Fatalf("Hunks = %+v, want exactly one", got.Hunks)
	}

	// processor.go is seven lines from and eight lines to, and the one changed
	// line is inside three lines of context of all of them, so git reports the
	// whole file as a single hunk starting at line 1 of each side.
	hunk := got.Hunks[0]
	if hunk.OldStart != 1 || hunk.OldLines != 7 || hunk.NewStart != 1 || hunk.NewLines != 8 {
		t.Errorf("hunk header = @@ -%d,%d +%d,%d @@, want @@ -1,7 +1,8 @@",
			hunk.OldStart, hunk.OldLines, hunk.NewStart, hunk.NewLines)
	}

	want := []provider.DiffLine{
		{Kind: provider.LineContext, Content: "package demo"},
		{Kind: provider.LineContext, Content: ""},
		{Kind: provider.LineContext, Content: "func Process() {"},
		{Kind: provider.LineRemoved, Content: "\tLegacyHandler()"},
		{Kind: provider.LineAdded, Content: "\tNewHandler()"},
		{Kind: provider.LineAdded, Content: "\tLog()"},
		{Kind: provider.LineContext, Content: "}"},
		{Kind: provider.LineContext, Content: ""},
		{Kind: provider.LineContext, Content: "func Helper() {}"},
	}
	if !slices.Equal(hunk.Lines, want) {
		t.Errorf("lines = %+v,\nwant %+v", hunk.Lines, want)
	}
}

// TestDiffSeparateHunks is AC #6 for a file whose changes are far enough apart
// to be reported separately: two hunks, each with its own start on each side, so
// the line numbers cannot be coming from a parser that assumes the file begins
// at the change.
func TestDiffSeparateHunks(t *testing.T) {
	var from, to strings.Builder
	for i := 1; i <= 20; i++ {
		fmt.Fprintf(&from, "line %d\n", i)
		if i == 2 || i == 18 {
			fmt.Fprintf(&to, "line %d changed\n", i)
			continue
		}
		fmt.Fprintf(&to, "line %d\n", i)
	}
	repoPath, fromRevision, toRevision := diffRepo(t,
		map[string]string{"list.go": from.String()},
		map[string]string{"list.go": to.String()},
	)

	got, err := diffFor(t, repoPath, fromRevision, toRevision, "list.go")
	if err != nil {
		t.Fatalf("Diff() error = %v", err)
	}
	if got.Change != provider.ChangeModified {
		t.Fatalf("Change = %q, want MODIFIED", got.Change)
	}
	if len(got.Hunks) != 2 {
		t.Fatalf("Hunks = %+v, want two", got.Hunks)
	}

	// Both changes are one line replaced by one line, so each side of each hunk
	// spans the changed line plus its three lines of context — and the second
	// hunk starts at line 15 of both sides, which is the number a caller cites.
	for i, want := range []provider.DiffHunk{
		{OldStart: 1, OldLines: 5, NewStart: 1, NewLines: 5},
		{OldStart: 15, OldLines: 6, NewStart: 15, NewLines: 6},
	} {
		hunk := got.Hunks[i]
		if hunk.OldStart != want.OldStart || hunk.OldLines != want.OldLines || hunk.NewStart != want.NewStart || hunk.NewLines != want.NewLines {
			t.Errorf("hunk %d = @@ -%d,%d +%d,%d @@, want @@ -%d,%d +%d,%d @@", i,
				hunk.OldStart, hunk.OldLines, hunk.NewStart, hunk.NewLines,
				want.OldStart, want.OldLines, want.NewStart, want.NewLines)
		}
	}

	// The second hunk's header carries a trailing heading ("@@ -15,6 +15,6 @@
	// line 14"), which is git's guess at the enclosing scope and not a line of
	// either revision. It must not turn up among the lines.
	want := []provider.DiffLine{
		{Kind: provider.LineContext, Content: "line 15"},
		{Kind: provider.LineContext, Content: "line 16"},
		{Kind: provider.LineContext, Content: "line 17"},
		{Kind: provider.LineRemoved, Content: "line 18"},
		{Kind: provider.LineAdded, Content: "line 18 changed"},
		{Kind: provider.LineContext, Content: "line 19"},
		{Kind: provider.LineContext, Content: "line 20"},
	}
	if !slices.Equal(got.Hunks[1].Lines, want) {
		t.Errorf("second hunk lines = %+v,\nwant %+v", got.Hunks[1].Lines, want)
	}
}

// TestDiffOneSidedHunks pins the hunk arithmetic for a file that exists on one
// side only, which is where a side contributes no lines at all. git writes those
// ranges in its short form — "@@ -1 +0,0 @@" — where the side that is there
// spans a single line and the side that is not starts at the line it would have
// followed. Reading that as a one-line range rather than as line zero is the
// difference between citing the right line and citing nothing.
func TestDiffOneSidedHunks(t *testing.T) {
	repoPath, fromRevision, toRevision := changedRepo(t)

	tests := map[string]struct {
		path string
		want provider.DiffHunk
	}{
		"added.go": {"added.go", provider.DiffHunk{
			OldStart: 0, OldLines: 0, NewStart: 1, NewLines: 1,
			Lines: []provider.DiffLine{{Kind: provider.LineAdded, Content: "package arrived"}},
		}},
		"removed.go": {"removed.go", provider.DiffHunk{
			OldStart: 1, OldLines: 1, NewStart: 0, NewLines: 0,
			Lines: []provider.DiffLine{{Kind: provider.LineRemoved, Content: "package gone"}},
		}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := diffFor(t, repoPath, fromRevision, toRevision, tc.path)
			if err != nil {
				t.Fatalf("Diff(%s) error = %v", tc.path, err)
			}
			if len(got.Hunks) != 1 {
				t.Fatalf("Hunks = %+v, want exactly one", got.Hunks)
			}
			hunk := got.Hunks[0]
			if hunk.OldStart != tc.want.OldStart || hunk.OldLines != tc.want.OldLines || hunk.NewStart != tc.want.NewStart || hunk.NewLines != tc.want.NewLines {
				t.Errorf("hunk header = @@ -%d,%d +%d,%d @@, want @@ -%d,%d +%d,%d @@",
					hunk.OldStart, hunk.OldLines, hunk.NewStart, hunk.NewLines,
					tc.want.OldStart, tc.want.OldLines, tc.want.NewStart, tc.want.NewLines)
			}
			if !slices.Equal(hunk.Lines, tc.want.Lines) {
				t.Errorf("lines = %+v, want %+v", hunk.Lines, tc.want.Lines)
			}
		})
	}
}

// TestDiffBinaryFile is AC #5: git will not show binary content as lines and
// neither will this. The result says the file changed and that the change is not
// text, which is an answer; inventing lines for it would not be.
func TestDiffBinaryFile(t *testing.T) {
	repoPath, fromRevision, toRevision := diffRepo(t,
		map[string]string{"blob.bin": "\x00\x01vacmcp\x00binary one\x00"},
		map[string]string{"blob.bin": "\x00\x01vacmcp\x00binary two, longer\x00\x02"},
	)

	got, err := diffFor(t, repoPath, fromRevision, toRevision, "blob.bin")
	if err != nil {
		t.Fatalf("Diff() error = %v", err)
	}
	if !got.Binary {
		t.Errorf("Binary = false, want true for a file git reports as binary")
	}
	if got.Change != provider.ChangeModified {
		t.Errorf("Change = %q, want MODIFIED", got.Change)
	}
	if len(got.Hunks) != 0 {
		t.Errorf("Hunks = %+v, want none: binary content has no lines to report", got.Hunks)
	}
}

// TestDiffRunsGitWithPinnedRevisions is AC #2, read off the command that was
// actually run rather than inferred from the result.
//
// git is shadowed on PATH by a recorder, so the argv the adapter built is
// visible: every argument separate — nothing is assembled into a shell string —
// the two revisions already resolved to full SHAs so neither side can follow a
// ref that moves, and the three flags that stop the machine's git configuration
// from answering instead of git: --no-renames, --no-ext-diff, --no-textconv.
func TestDiffRunsGitWithPinnedRevisions(t *testing.T) {
	repoPath, fromRevision, toRevision := changedRepo(t)

	// The contexts declare a branch name and a short SHA, so a command carrying
	// full SHAs can only have got them from resolving both sides first.
	branch := "diff-from"
	runGit(t, repoPath, "branch", branch, fromRevision)

	log := recordGit(t)
	got, err := providerFor(repoPath).Diff(
		t.Context(),
		contextAt("app-v1", "release/1.x", branch),
		contextAt("app-v2", "release/2.x", toRevision[:8]),
		provider.SourceDiffRequest{Path: "processor.go"},
	)
	if err != nil {
		t.Fatalf("Diff() error = %v", err)
	}
	if got.Change != provider.ChangeModified {
		t.Fatalf("Change = %q, want MODIFIED", got.Change)
	}

	want := []string{
		"-C", repoPath, "diff",
		"--no-color", "--no-ext-diff", "--no-textconv", "--no-renames",
		fromRevision, toRevision, "--", "processor.go",
	}
	for _, argv := range invocations(t, log) {
		if slices.Contains(argv, "diff") {
			if !slices.Equal(argv, want) {
				t.Errorf("git argv = %q,\nwant %q", argv, want)
			}
			return
		}
	}
	t.Fatalf("no git diff was run; recorded invocations: %q", invocations(t, log))
}

// TestDiffRejectsIllegalPaths is AC #3: the same trust boundary a read has, and
// the same INVALID_ARGUMENT, because it is the same validation rather than a
// second copy of it that could drift.
func TestDiffRejectsIllegalPaths(t *testing.T) {
	repoPath, fromRevision, toRevision := changedRepo(t)

	for _, filePath := range []string{"", "   ", "/etc/passwd", "../../etc/passwd", "..", "sub/../../escape.go"} {
		t.Run("path="+filePath, func(t *testing.T) {
			got, err := diffFor(t, repoPath, fromRevision, toRevision, filePath)
			if err == nil {
				t.Fatalf("Diff(%q) = %+v, want INVALID_ARGUMENT", filePath, got)
			}
			if code := errorOf(t, err).Code; code != vacerr.InvalidArgument {
				t.Errorf("code = %q, want INVALID_ARGUMENT", code)
			}
			if got != nil {
				t.Errorf("Diff() = %+v, want no diff alongside the error", got)
			}
		})
	}
}

// TestDiffPathInNeitherRevision covers the case git says nothing about. An empty
// diff for a file that does not exist is not "nothing changed": the comparison
// has no subject, and saying UNCHANGED would be an answer about a file that was
// never there.
func TestDiffPathInNeitherRevision(t *testing.T) {
	repoPath, fromRevision, toRevision := changedRepo(t)

	got, err := diffFor(t, repoPath, fromRevision, toRevision, "does/not/exist.go")
	if err == nil {
		t.Fatalf("Diff() = %+v, want INVALID_ARGUMENT", got)
	}
	if code := errorOf(t, err).Code; code != vacerr.InvalidArgument {
		t.Errorf("code = %q, want INVALID_ARGUMENT", code)
	}
}

// TestDiffUnknownRevisionAndRepository is the resolve the read path uses, on
// both sides: a revision the repository does not have is REVISION_NOT_FOUND
// whichever side declares it, and a repository that is not there is
// REPOSITORY_NOT_FOUND.
func TestDiffUnknownRevisionAndRepository(t *testing.T) {
	repoPath, fromRevision, toRevision := changedRepo(t)
	const absent = "0123456789abcdef0123456789abcdef01234567"

	t.Run("from revision", func(t *testing.T) {
		_, err := diffFor(t, repoPath, absent, toRevision, "processor.go")
		if code := errorOf(t, err).Code; code != vacerr.RevisionNotFound {
			t.Errorf("code = %q, want REVISION_NOT_FOUND", code)
		}
	})
	t.Run("to revision", func(t *testing.T) {
		_, err := diffFor(t, repoPath, fromRevision, absent, "processor.go")
		if code := errorOf(t, err).Code; code != vacerr.RevisionNotFound {
			t.Errorf("code = %q, want REVISION_NOT_FOUND", code)
		}
	})
	t.Run("repository is not a repository", func(t *testing.T) {
		_, err := diffFor(t, t.TempDir(), fromRevision, toRevision, "processor.go")
		if code := errorOf(t, err).Code; code != vacerr.RepositoryNotFound {
			t.Errorf("code = %q, want REPOSITORY_NOT_FOUND", code)
		}
	})
	t.Run("repository is not configured", func(t *testing.T) {
		codeCtx := contextAt("app-v1", "release/1.x", fromRevision)
		codeCtx.Repository = "example/absent"
		_, err := providerFor(repoPath).Diff(t.Context(), codeCtx, codeCtx, provider.SourceDiffRequest{Path: "processor.go"})
		if code := errorOf(t, err).Code; code != vacerr.RepositoryNotFound {
			t.Errorf("code = %q, want REPOSITORY_NOT_FOUND", code)
		}
	})
}

// TestDiffAcrossRepositoriesIsRefused: two repositories share no history, so
// there is no comparison to make between them and none is invented.
func TestDiffAcrossRepositoriesIsRefused(t *testing.T) {
	repoPath, fromRevision, toRevision := changedRepo(t)

	other := contextAt("other-v2", "release/2.x", toRevision)
	other.Repository = "example/other"

	got, err := providerFor(repoPath).Diff(
		t.Context(),
		contextAt("app-v1", "release/1.x", fromRevision),
		other,
		provider.SourceDiffRequest{Path: "processor.go"},
	)
	if err == nil {
		t.Fatalf("Diff() = %+v, want INVALID_ARGUMENT", got)
	}
	if code := errorOf(t, err).Code; code != vacerr.InvalidArgument {
		t.Errorf("code = %q, want INVALID_ARGUMENT", code)
	}
}

// diffRepo builds a throwaway repository with two commits holding the given
// file sets, and returns its path and the two revisions. A file only in before
// is deleted by the second commit and one only in after is added by it, which is
// how a fixture gets an added, a removed, a modified and an untouched path out
// of one repository.
func diffRepo(t *testing.T, before, after map[string]string) (repoPath, fromRevision, toRevision string) {
	t.Helper()
	requireGit(t)
	repoPath = t.TempDir()

	commit := func(files map[string]string, message string) string {
		t.Helper()
		entries, err := os.ReadDir(repoPath)
		if err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
		for _, entry := range entries {
			if entry.Name() == ".git" {
				continue
			}
			if err := os.RemoveAll(filepath.Join(repoPath, entry.Name())); err != nil {
				t.Fatalf("RemoveAll: %v", err)
			}
		}
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(repoPath, name), []byte(body), 0o600); err != nil {
				t.Fatalf("WriteFile(%s): %v", name, err)
			}
		}
		runGit(t, repoPath, "add", "-A")
		runGit(t, repoPath, "-c", "user.name=vacmcp", "-c", "user.email=vacmcp@example.invalid", "commit", "--no-verify", "-m", message)
		return runGit(t, repoPath, "rev-parse", "HEAD")
	}

	runGit(t, repoPath, "init")
	fromRevision = commit(before, "from")
	toRevision = commit(after, "to")
	if fromRevision == toRevision {
		t.Fatalf("both commits are %s, the repository never moved", toRevision)
	}
	return repoPath, fromRevision, toRevision
}

// runGit runs one git command against the fixture. The git configuration files
// are pinned away so the machine's own settings cannot change what the test
// observes.
func runGit(t *testing.T, repoPath string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repoPath}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// recordGit shadows git on PATH with a script that appends each invocation's
// arguments to a log file, one per line, and then runs the real git. Recording
// the argv is the only way to see that the adapter passed separate arguments
// rather than a string it built, which is what the result alone cannot show.
func recordGit(t *testing.T) string {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}

	dir := t.TempDir()
	log := filepath.Join(dir, "argv.log")
	script := fmt.Sprintf("#!/bin/sh\n{ for arg in \"$@\"; do printf '%%s\\n' \"$arg\"; done; printf '%%s\\n' %q; } >> %q\nexec %q \"$@\"\n",
		invocationEnd, log, realGit)
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700); err != nil {
		t.Fatalf("WriteFile(git): %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// invocationEnd separates one recorded invocation from the next. No git argument
// this adapter builds looks like it.
const invocationEnd = "=== end of invocation ==="

// invocations reads the recorder's log back as one argument slice per git run.
func invocations(t *testing.T, log string) [][]string {
	t.Helper()
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", log, err)
	}

	var all [][]string
	var current []string
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		if line == invocationEnd {
			all = append(all, current)
			current = nil
			continue
		}
		current = append(current, line)
	}
	return all
}
