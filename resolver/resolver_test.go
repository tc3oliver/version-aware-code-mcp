package resolver_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/resolver"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// repo is a throwaway git repository with two commits, so a test can point a
// context at a revision the checkout is not on.
type repo struct {
	path string
	// first and head are the full SHAs of the two commits; head is what the
	// repository is actually checked out at.
	first, head string
}

// newRepo builds a real repository in a temporary directory. The git config
// files are pinned away so the machine's own settings — signing, hooks, a
// default branch name — cannot change what the test observes.
func newRepo(t *testing.T) repo {
	t.Helper()
	r := repo{path: t.TempDir()}

	git := func(args ...string) string {
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
		git("add", ".")
		git("-c", "user.name=vacmcp", "-c", "user.email=vacmcp@example.invalid", "commit", "-m", message)
		return git("rev-parse", "HEAD")
	}

	git("init")
	r.first = commit("package demo\n\nfunc Process() { LegacyHandler() }\n", "v1")
	r.head = commit("package demo\n\nfunc Process() { NewHandler() }\n", "v2")
	if r.first == r.head {
		t.Fatalf("both commits are %s, the repository never moved", r.head)
	}
	return r
}

// contextIn is one context declaring revision.
func contextIn(revision string) vacctx.CodeContext {
	return vacctx.CodeContext{
		ID:         "app-v2",
		Repository: "example/backend",
		Branch:     "release/2.x",
		Revision:   revision,
		GraphRef:   "backend-v2",
	}
}

// configFor wires r up as the repository behind a single context declaring
// revision.
func configFor(r repo, revision string) *config.Config {
	return &config.Config{
		Repositories: map[string]config.Repository{"example/backend": {Path: r.path}},
		Contexts:     map[string]vacctx.CodeContext{"app-v2": contextIn(revision)},
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

// TestResolveConfiguredContext is AC #1: a configured ID resolves to its
// CodeContext, graph_ref included.
func TestResolveConfiguredContext(t *testing.T) {
	r := newRepo(t)

	got, err := resolver.New(configFor(r, r.head)).Resolve(t.Context(), "app-v2")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if want := contextIn(r.head); got != want {
		t.Errorf("Resolve() = %+v, want %+v", got, want)
	}
}

// TestResolveTwoContextsOverOneRepository is doc-1's first v0.1.0 success
// criterion — two versions of one repository existing at the same time — and it
// is the regression this test pins.
//
// config/example.yaml gives both of its contexts the same repository, so both
// resolve against one working directory, and a working directory has one HEAD.
// A resolver that required HEAD to equal the declared revision could therefore
// serve only one of the two; the other would fail with SOURCE_MISMATCH forever.
// Resolution verifies the revision is available, not that the checkout is
// sitting on it.
func TestResolveTwoContextsOverOneRepository(t *testing.T) {
	r := newRepo(t)
	// app-v1 declares the older commit, app-v2 the one the tree is on. Neither
	// is privileged.
	cfg := &config.Config{
		Repositories: map[string]config.Repository{"example/backend": {Path: r.path}},
		Contexts: map[string]vacctx.CodeContext{
			"app-v1": {Repository: "example/backend", Branch: "release/1.x", Revision: r.first, GraphRef: "backend-v1"},
			"app-v2": {Repository: "example/backend", Branch: "release/2.x", Revision: r.head, GraphRef: "backend-v2"},
		},
	}
	res := resolver.New(cfg)

	for id, wantRevision := range map[string]string{"app-v1": r.first, "app-v2": r.head} {
		got, err := res.Resolve(t.Context(), id)
		if err != nil {
			t.Fatalf("Resolve(%q) error = %v; both versions of one repository must resolve", id, err)
		}
		if got.ID != id || got.Revision != wantRevision {
			t.Errorf("Resolve(%q) = %+v, want revision %s", id, got, wantRevision)
		}
	}
}

// TestResolveAcceptsAbbreviatedRevision covers the revision form doc-1's own
// config example uses: a short SHA has to resolve like the commit it names.
func TestResolveAcceptsAbbreviatedRevision(t *testing.T) {
	r := newRepo(t)

	got, err := resolver.New(configFor(r, r.head[:7])).Resolve(t.Context(), "app-v2")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.Revision != r.head[:7] {
		t.Errorf("Revision = %q, want the declared %q", got.Revision, r.head[:7])
	}
}

// TestResolveUnknownContextID is AC #2: an unconfigured ID is an error, never a
// guess. The configuration holds exactly one context, so returning it for any
// other ID — or for the empty ID — would be the "helpful" behaviour this server
// must not have.
func TestResolveUnknownContextID(t *testing.T) {
	r := newRepo(t)
	res := resolver.New(configFor(r, r.head))

	for _, id := range []string{"app-v1", "app-v", "APP-V2", "app-v2 ", ""} {
		t.Run("id="+id, func(t *testing.T) {
			got, err := res.Resolve(t.Context(), id)
			if err == nil {
				t.Fatalf("Resolve(%q) = %+v, want error", id, got)
			}
			if code := errorOf(t, err).Code; code != vacerr.ContextNotFound {
				t.Errorf("code = %q, want CONTEXT_NOT_FOUND", code)
			}
			if got != (vacctx.CodeContext{}) {
				t.Errorf("Resolve(%q) = %+v, want the zero context", id, got)
			}
		})
	}
}

// TestVerifyWorktreeSourceMismatch is AC #3: the context declares the first
// commit while the working tree is checked out at the second, so a provider
// that can only serve that tree is stopped instead of handing back the wrong
// version's bytes.
func TestVerifyWorktreeSourceMismatch(t *testing.T) {
	r := newRepo(t)

	err := resolver.VerifyWorktree(t.Context(), r.path, contextIn(r.first))
	if err == nil {
		t.Fatal("VerifyWorktree() = nil, want SOURCE_MISMATCH")
	}

	vErr := errorOf(t, err)
	if vErr.Code != vacerr.SourceMismatch {
		t.Fatalf("code = %q, want SOURCE_MISMATCH", vErr.Code)
	}
	// The mismatch has to travel with the two revisions it is about, otherwise
	// the operator cannot tell which side is stale.
	if vErr.Details["declared_revision"] != r.first {
		t.Errorf("details[declared_revision] = %v, want %s", vErr.Details["declared_revision"], r.first)
	}
	if vErr.Details["actual_revision"] != r.head {
		t.Errorf("details[actual_revision] = %v, want %s", vErr.Details["actual_revision"], r.head)
	}
}

// TestVerifyWorktreeMismatchIsNotDowngradable pins the shape of the signal: a
// mismatch arrives only as a non-nil Go error that marshals to the error
// envelope. There is no severity field to demote it with and no success-shaped
// output a caller could serve instead, so continuing past it takes ignoring an
// error outright.
func TestVerifyWorktreeMismatchIsNotDowngradable(t *testing.T) {
	r := newRepo(t)

	err := resolver.VerifyWorktree(t.Context(), r.path, contextIn(r.first))
	wire, marshalErr := errorOf(t, err).MarshalJSON()
	if marshalErr != nil {
		t.Fatalf("MarshalJSON() error = %v", marshalErr)
	}
	got := string(wire)
	if !strings.HasPrefix(got, `{"error":{"code":"SOURCE_MISMATCH"`) {
		t.Errorf("wire = %s, want a SOURCE_MISMATCH error envelope", got)
	}
	for _, forbidden := range []string{`"warning"`, `"severity"`, `"content"`} {
		if strings.Contains(got, forbidden) {
			t.Errorf("wire contains %s: %s", forbidden, got)
		}
	}
}

// TestVerifyWorktreeOnDeclaredRevision is the passing side: the tree is on the
// declared commit, however that commit is spelled, so a provider reading it is
// cleared to serve.
func TestVerifyWorktreeOnDeclaredRevision(t *testing.T) {
	r := newRepo(t)

	for _, revision := range []string{r.head, r.head[:7], "HEAD"} {
		if err := resolver.VerifyWorktree(t.Context(), r.path, contextIn(revision)); err != nil {
			t.Errorf("VerifyWorktree(%q) error = %v", revision, err)
		}
	}
}

// TestVerifyWorktreeCannotCheck covers the two ways the comparison cannot be
// made at all. Neither may pass: a check that could not be carried out is not a
// check that passed.
func TestVerifyWorktreeCannotCheck(t *testing.T) {
	r := newRepo(t)

	tests := map[string]struct {
		path, revision string
		want           vacerr.Code
	}{
		"not a repository": {t.TempDir(), r.head, vacerr.RepositoryNotFound},
		"unknown revision": {r.path, "0123456789abcdef0123456789abcdef01234567", vacerr.RevisionNotFound},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			err := resolver.VerifyWorktree(t.Context(), tc.path, contextIn(tc.revision))
			if err == nil {
				t.Fatal("VerifyWorktree() = nil, want error")
			}
			if code := errorOf(t, err).Code; code != tc.want {
				t.Errorf("code = %q, want %q", code, tc.want)
			}
		})
	}
}

// TestResolveUnreadableRepository and TestResolveUnknownRevision are those same
// two failures at resolution time: a context whose revision could not be looked
// up at all is not a resolved context.
func TestResolveUnreadableRepository(t *testing.T) {
	cfg := configFor(repo{path: t.TempDir()}, "8af31e2") // a directory, not a git repository

	got, err := resolver.New(cfg).Resolve(t.Context(), "app-v2")
	if err == nil {
		t.Fatalf("Resolve() = %+v, want error", got)
	}
	if code := errorOf(t, err).Code; code != vacerr.RepositoryNotFound {
		t.Errorf("code = %q, want REPOSITORY_NOT_FOUND", code)
	}
}

func TestResolveUnknownRevision(t *testing.T) {
	r := newRepo(t)

	got, err := resolver.New(configFor(r, "0123456789abcdef0123456789abcdef01234567")).Resolve(t.Context(), "app-v2")
	if err == nil {
		t.Fatalf("Resolve() = %+v, want error", got)
	}
	if got != (vacctx.CodeContext{}) {
		t.Errorf("Resolve() = %+v, want the zero context", got)
	}
	if code := errorOf(t, err).Code; code != vacerr.RevisionNotFound {
		t.Errorf("code = %q, want REVISION_NOT_FOUND", code)
	}
}

// TestResolveFromLoadedConfig checks the resolver against the configuration the
// parser actually produces, including the context ID it fills in from the key.
func TestResolveFromLoadedConfig(t *testing.T) {
	r := newRepo(t)
	body := "repositories:\n  example/backend:\n    path: " + r.path +
		"\ncontexts:\n  app-v2:\n    repository: example/backend\n    branch: release/2.x\n    revision: " +
		r.head + "\n    graph_ref: backend-v2\n"
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	got, err := resolver.New(cfg).Resolve(t.Context(), "app-v2")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.ID != "app-v2" || got.Revision != r.head || got.GraphRef != "backend-v2" {
		t.Errorf("Resolve() = %+v", got)
	}
}
