// Package git_test is the evidence for the git source adapter: that it reads
// the revision a context declares rather than whatever the working tree happens
// to be on, and that it fails closed when it cannot.
package git_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gitadapter "github.com/tc3oliver/version-aware-code-mcp/adapters/git"
	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/internal/demorepo"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// The adapter satisfies the contract it is written against. A compile-time
// assertion is the whole check: the interface has one method and no optional
// parts.
var _ provider.SourceProvider = (*gitadapter.Provider)(nil)

const repoName = "example/demo"

// providerFor wires repoPath up under repoName, which is how every context in
// these tests names its repository.
func providerFor(repoPath string) *gitadapter.Provider {
	return gitadapter.New(&config.Config{
		Repositories: map[string]config.Repository{repoName: {Path: repoPath}},
	})
}

// contextAt is a context declaring one branch and revision of that repository.
func contextAt(id, branch, revision string) vacctx.CodeContext {
	return vacctx.CodeContext{
		ID:         id,
		Repository: repoName,
		Branch:     branch,
		Revision:   revision,
		GraphRef:   "demo-" + id,
	}
}

// errorOf fails the test unless err is a *vacerr.Error, and returns it.
func errorOf(t *testing.T, err error) *vacerr.Error {
	t.Helper()
	var vErr *vacerr.Error
	if !errors.As(err, &vErr) {
		t.Fatalf("error = %v (%T), want *vacerr.Error", err, err)
	}
	return vErr
}

// requireGit skips when there is no git to run. Everything here reads a real
// repository; there is nothing left to assert without it.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
}

// demo returns the generated versioned demo repository. Its HEAD is on main
// while the contexts under test declare release/v1 and release/v2, which is the
// arrangement the adapter has to serve.
func demo(t *testing.T) string {
	t.Helper()
	requireGit(t)
	return demorepo.Generate(t)
}

// processor.go is the file that differs across the release branches, and lines
// 4 to 6 are the body of Process() on all of them.
const (
	processorPath        = "processor.go"
	processorFuncStart   = 4
	processorFuncEnd     = 6
	processorTotalLines  = 6
	processorHeaderLines = 3
)

// TestReadSamePathAcrossVersions is AC #1 and doc-1's first v0.1.0 success
// criterion at the same time: one repository, one path, two contexts, and the
// content each one gets is its own version's. The working tree is on main
// throughout, so neither answer can be coming from the checkout.
func TestReadSamePathAcrossVersions(t *testing.T) {
	repo := demo(t)
	p := providerFor(repo)

	v1 := contextAt("demo-v1", demorepo.V1, demorepo.Revision(t, repo, demorepo.V1))
	v2 := contextAt("demo-v2", demorepo.V2, demorepo.Revision(t, repo, demorepo.V2))

	first, err := p.Read(t.Context(), v1, processorPath, processorFuncStart, processorFuncEnd)
	if err != nil {
		t.Fatalf("Read(%s) error = %v", demorepo.V1, err)
	}
	second, err := p.Read(t.Context(), v2, processorPath, processorFuncStart, processorFuncEnd)
	if err != nil {
		t.Fatalf("Read(%s) error = %v", demorepo.V2, err)
	}

	if first.Content == second.Content {
		t.Fatalf("both versions returned the same content, version isolation is broken:\n%s", first.Content)
	}
	if !strings.Contains(first.Content, "LegacyHandler") || strings.Contains(first.Content, "NewHandler") {
		t.Errorf("%s content = %q, want the LegacyHandler call", demorepo.V1, first.Content)
	}
	if !strings.Contains(second.Content, "NewHandler") || strings.Contains(second.Content, "LegacyHandler") {
		t.Errorf("%s content = %q, want the NewHandler call", demorepo.V2, second.Content)
	}
	t.Logf("%s %s:%d-%d = %q", demorepo.V1, processorPath, first.StartLine, first.EndLine, first.Content)
	t.Logf("%s %s:%d-%d = %q", demorepo.V2, processorPath, second.StartLine, second.EndLine, second.Content)
}

// TestReadCarriesTheVersionContext is AC #2: a result locates itself. The
// adapter supplies path, line range and the revision it actually read at; the
// repository and branch travel on the CodeContext the caller resolved, and the
// whole five-part record has to be complete.
func TestReadCarriesTheVersionContext(t *testing.T) {
	repo := demo(t)
	revision := demorepo.Revision(t, repo, demorepo.V2)
	codeCtx := contextAt("demo-v2", demorepo.V2, revision)

	got, err := providerFor(repo).Read(t.Context(), codeCtx, processorPath, processorFuncStart, processorFuncEnd)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	want := provider.SourceContent{
		Path:      processorPath,
		StartLine: processorFuncStart,
		EndLine:   processorFuncEnd,
		Content:   got.Content,
		Revision:  revision,
	}
	if *got != want {
		t.Errorf("Read() = %+v, want %+v", *got, want)
	}
	for name, value := range map[string]string{
		"repository": codeCtx.Repository,
		"branch":     codeCtx.Branch,
		"revision":   got.Revision,
		"path":       got.Path,
		"content":    got.Content,
	} {
		if strings.TrimSpace(value) == "" {
			t.Errorf("%s is empty; a result must carry the whole version context", name)
		}
	}
	t.Logf("repository=%s branch=%s revision=%s path=%s lines=%d-%d content=%q",
		codeCtx.Repository, codeCtx.Branch, got.Revision, got.Path, got.StartLine, got.EndLine, got.Content)
}

// TestReadResolvesTheDeclaredRevisionForm covers the revision spellings a
// configuration may use. Whatever the context declares, the result reports the
// full SHA it was read at, so the claim can be checked rather than trusted.
func TestReadResolvesTheDeclaredRevisionForm(t *testing.T) {
	repo := demo(t)
	revision := demorepo.Revision(t, repo, demorepo.V1)
	p := providerFor(repo)

	for _, declared := range []string{revision, revision[:7], "refs/heads/" + demorepo.V1, demorepo.V1} {
		t.Run(declared, func(t *testing.T) {
			got, err := p.Read(t.Context(), contextAt("demo-v1", demorepo.V1, declared), processorPath, processorFuncStart, processorFuncEnd)
			if err != nil {
				t.Fatalf("Read() error = %v", err)
			}
			if got.Revision != revision {
				t.Errorf("Revision = %q, want the full SHA %q", got.Revision, revision)
			}
			if !strings.Contains(got.Content, "LegacyHandler") {
				t.Errorf("content = %q, want the v1 body", got.Content)
			}
		})
	}
}

// TestReadReturnsExactLines pins the line arithmetic: 1-based, inclusive at both
// ends, and the bytes come back exactly as they are in the file rather than
// reassembled approximately.
func TestReadReturnsExactLines(t *testing.T) {
	repo := demo(t)
	codeCtx := contextAt("demo-v1", demorepo.V1, demorepo.Revision(t, repo, demorepo.V1))
	p := providerFor(repo)

	whole, err := p.Read(t.Context(), codeCtx, processorPath, 1, processorTotalLines)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	head, err := p.Read(t.Context(), codeCtx, processorPath, 1, processorHeaderLines)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	body, err := p.Read(t.Context(), codeCtx, processorPath, processorFuncStart, processorTotalLines)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	if got := head.Content + body.Content; got != whole.Content {
		t.Errorf("the two halves are %q, want the whole file %q", got, whole.Content)
	}
	if first, err := p.Read(t.Context(), codeCtx, processorPath, 1, 1); err != nil {
		t.Fatalf("Read() error = %v", err)
	} else if first.Content != "package demo\n" {
		t.Errorf("line 1 = %q, want %q", first.Content, "package demo\n")
	}
}

// TestReadRejectsIllegalLineRanges is the trust boundary. A line range arrives
// from a tool caller, so a nonsensical one is answered with INVALID_ARGUMENT —
// never a panic, and never quietly clamped to something the caller did not ask
// for and would go on to cite as evidence.
func TestReadRejectsIllegalLineRanges(t *testing.T) {
	repo := demo(t)
	codeCtx := contextAt("demo-v1", demorepo.V1, demorepo.Revision(t, repo, demorepo.V1))
	p := providerFor(repo)

	tests := map[string]struct{ start, end int }{
		"zero start":       {0, 3},
		"negative start":   {-5, 3},
		"negative end":     {1, -1},
		"inverted":         {5, 2},
		"end beyond EOF":   {1, processorTotalLines + 1},
		"start beyond EOF": {processorTotalLines + 1, processorTotalLines + 2},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := p.Read(t.Context(), codeCtx, processorPath, tc.start, tc.end)
			if err == nil {
				t.Fatalf("Read(%d, %d) = %+v, want INVALID_ARGUMENT", tc.start, tc.end, got)
			}
			if code := errorOf(t, err).Code; code != vacerr.InvalidArgument {
				t.Errorf("code = %q, want INVALID_ARGUMENT", code)
			}
			if got != nil {
				t.Errorf("Read() = %+v, want no content alongside the error", got)
			}
		})
	}
}

// TestReadRejectsIllegalPaths is the same boundary for the path: empty, absolute
// and repository-escaping paths are refused here instead of being handed to git
// to interpret.
func TestReadRejectsIllegalPaths(t *testing.T) {
	repo := demo(t)
	codeCtx := contextAt("demo-v1", demorepo.V1, demorepo.Revision(t, repo, demorepo.V1))
	p := providerFor(repo)

	for _, filePath := range []string{"", "   ", "/etc/passwd", "../../etc/passwd", "..", "sub/../../escape.go"} {
		t.Run("path="+filePath, func(t *testing.T) {
			got, err := p.Read(t.Context(), codeCtx, filePath, 1, 1)
			if err == nil {
				t.Fatalf("Read(%q) = %+v, want INVALID_ARGUMENT", filePath, got)
			}
			if code := errorOf(t, err).Code; code != vacerr.InvalidArgument {
				t.Errorf("code = %q, want INVALID_ARGUMENT", code)
			}
		})
	}
}

// TestReadPathNotInRevision covers a path that is not in the revision at all,
// including one that exists on another branch: a file is only readable in the
// version that has it.
func TestReadPathNotInRevision(t *testing.T) {
	repo := demo(t)
	p := providerFor(repo)

	// version.go exists on release/v1 but not on main, so the same path is
	// legal in one context and absent in the other.
	if _, err := p.Read(t.Context(), contextAt("demo-v1", demorepo.V1, demorepo.Revision(t, repo, demorepo.V1)), "version.go", 1, 1); err != nil {
		t.Fatalf("Read(version.go @ %s) error = %v, want it to be readable there", demorepo.V1, err)
	}

	mainCtx := contextAt("demo-main", demorepo.Main, demorepo.Revision(t, repo, demorepo.Main))
	for _, filePath := range []string{"version.go", "does/not/exist.go"} {
		t.Run(filePath, func(t *testing.T) {
			got, err := p.Read(t.Context(), mainCtx, filePath, 1, 1)
			if err == nil {
				t.Fatalf("Read(%q) = %+v, want INVALID_ARGUMENT", filePath, got)
			}
			if code := errorOf(t, err).Code; code != vacerr.InvalidArgument {
				t.Errorf("code = %q, want INVALID_ARGUMENT", code)
			}
		})
	}
}

// TestReadUnknownRevision is AC #4: a revision this repository does not have is
// REVISION_NOT_FOUND, and nothing is served in its place.
func TestReadUnknownRevision(t *testing.T) {
	repo := demo(t)

	got, err := providerFor(repo).Read(
		t.Context(),
		contextAt("demo-v1", demorepo.V1, "0123456789abcdef0123456789abcdef01234567"),
		processorPath, 1, 1,
	)
	if err == nil {
		t.Fatalf("Read() = %+v, want REVISION_NOT_FOUND", got)
	}
	if code := errorOf(t, err).Code; code != vacerr.RevisionNotFound {
		t.Errorf("code = %q, want REVISION_NOT_FOUND", code)
	}
	if got != nil {
		t.Errorf("Read() = %+v, want no content alongside the error", got)
	}
}

// TestReadUnknownRepository covers the two ways the repository itself is not
// there: a context naming one that is not configured, and a configured path
// that is not a git repository.
func TestReadUnknownRepository(t *testing.T) {
	requireGit(t)

	notConfigured := contextAt("demo-v1", demorepo.V1, "HEAD")
	notConfigured.Repository = "example/absent"

	tests := map[string]vacctx.CodeContext{
		"repository not configured": notConfigured,
		"path is not a repository":  contextAt("demo-v1", demorepo.V1, "HEAD"),
	}
	for name, codeCtx := range tests {
		t.Run(name, func(t *testing.T) {
			// An empty directory is configured under repoName, so the first
			// case fails on the name and the second on the path.
			got, err := providerFor(t.TempDir()).Read(t.Context(), codeCtx, processorPath, 1, 1)
			if err == nil {
				t.Fatalf("Read() = %+v, want REPOSITORY_NOT_FOUND", got)
			}
			if code := errorOf(t, err).Code; code != vacerr.RepositoryNotFound {
				t.Errorf("code = %q, want REPOSITORY_NOT_FOUND", code)
			}
		})
	}
}

// TestReadSourceMismatchWhenObjectStoreCannotServeTheRevision is AC #3.
//
// Reading the object database normally makes the content correct by
// construction, so the mismatch has to be provoked at the one place that
// guarantee can fail: a repository that records the file at the declared
// revision but can no longer produce its bytes — a partial clone that never
// fetched them, a pruned object store. The only content for that path left on
// the machine is the working tree's, which is on a different commit. The
// adapter must not serve it, and the verdict must come from the resolver's
// check rather than a second implementation of it.
func TestReadSourceMismatchWhenObjectStoreCannotServeTheRevision(t *testing.T) {
	requireGit(t)
	r := newRepo(t)
	// The tree is on head; the context declares first, whose blob is gone.
	removeBlob(t, r.path, r.first, "process.go")

	got, err := providerFor(r.path).Read(t.Context(), contextAt("app-v1", "release/1.x", r.first), "process.go", 1, 1)
	if err == nil {
		t.Fatalf("Read() = %+v, want SOURCE_MISMATCH", got)
	}
	if got != nil {
		t.Errorf("Read() = %+v, want no content alongside the error", got)
	}

	vErr := errorOf(t, err)
	if vErr.Code != vacerr.SourceMismatch {
		t.Fatalf("code = %q, want SOURCE_MISMATCH (error: %v)", vErr.Code, err)
	}
	// The mismatch travels with both revisions, so an operator can see which
	// side is stale.
	if vErr.Details["declared_revision"] != r.first {
		t.Errorf("details[declared_revision] = %v, want %s", vErr.Details["declared_revision"], r.first)
	}
	if vErr.Details["actual_revision"] != r.head {
		t.Errorf("details[actual_revision] = %v, want %s", vErr.Details["actual_revision"], r.head)
	}

	// Fail closed all the way to the wire: the caller receives an error
	// envelope with no content in it to fall back on.
	wire, marshalErr := vErr.MarshalJSON()
	if marshalErr != nil {
		t.Fatalf("MarshalJSON() error = %v", marshalErr)
	}
	if !strings.HasPrefix(string(wire), `{"error":{"code":"SOURCE_MISMATCH"`) {
		t.Errorf("wire = %s, want a SOURCE_MISMATCH error envelope", wire)
	}
	if strings.Contains(string(wire), "LegacyHandler") {
		t.Errorf("wire = %s, want no source content in a fail-closed error", wire)
	}
	t.Logf("SOURCE_MISMATCH wire = %s", wire)
}

// repo is a throwaway repository with two commits whose process.go differs, so
// a test can declare a revision the checkout is not on.
type repo struct {
	path        string
	first, head string
}

// newRepo builds that repository. The git configuration files are pinned away
// so the machine's own settings cannot change what the test observes.
func newRepo(t *testing.T) repo {
	t.Helper()
	r := repo{path: t.TempDir()}

	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", r.path}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	commit := func(body, message string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(r.path, "process.go"), []byte(body), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		run("add", ".")
		run("-c", "user.name=vacmcp", "-c", "user.email=vacmcp@example.invalid", "commit", "--no-verify", "-m", message)
		return run("rev-parse", "HEAD")
	}

	run("init")
	r.first = commit("package demo\n\nfunc Process() { LegacyHandler() }\n", "v1")
	r.head = commit("package demo\n\nfunc Process() { NewHandler() }\n", "v2")
	if r.first == r.head {
		t.Fatalf("both commits are %s, the repository never moved", r.head)
	}
	return r
}

// removeBlob deletes the loose object holding filePath's content at revision,
// leaving the commit and its tree intact. That is the state a partial clone is
// in for a file it never fetched.
func removeBlob(t *testing.T, repoPath, revision, filePath string) {
	t.Helper()
	out, err := exec.Command("git", "-C", repoPath, "rev-parse", revision+":"+filePath).Output()
	if err != nil {
		t.Fatalf("git rev-parse %s:%s: %v", revision, filePath, err)
	}
	oid := strings.TrimSpace(string(out))
	object := filepath.Join(repoPath, ".git", "objects", oid[:2], oid[2:])
	if _, err := os.Stat(object); err != nil {
		t.Skipf("blob %s is not a loose object, cannot simulate an unfetched object store: %v", oid, err)
	}
	if err := os.Remove(object); err != nil {
		t.Fatalf("removing %s: %v", object, err)
	}
}
