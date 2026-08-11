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

// configFor wires r up as the repository behind a single context declaring
// revision.
func configFor(r repo, revision string) *config.Config {
	return &config.Config{
		Repositories: map[string]config.Repository{"example/backend": {Path: r.path}},
		Contexts: map[string]vacctx.CodeContext{
			"app-v2": {
				ID:         "app-v2",
				Repository: "example/backend",
				Branch:     "release/2.x",
				Revision:   revision,
				GraphRef:   "backend-v2",
			},
		},
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
// CodeContext, graph_ref included, when the checkout is on the declared
// revision.
func TestResolveConfiguredContext(t *testing.T) {
	r := newRepo(t)

	got, err := resolver.New(configFor(r, r.head)).Resolve(t.Context(), "app-v2")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	want := vacctx.CodeContext{
		ID:         "app-v2",
		Repository: "example/backend",
		Branch:     "release/2.x",
		Revision:   r.head,
		GraphRef:   "backend-v2",
	}
	if got != want {
		t.Errorf("Resolve() = %+v, want %+v", got, want)
	}
}

// TestResolveAcceptsAbbreviatedRevision keeps the mismatch check honest in the
// other direction: doc-1's own config example declares a short SHA, and a short
// SHA of the commit the repository is on is not a mismatch.
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

// TestResolveSourceMismatch is AC #3 and the reason this package exists: the
// context declares the first commit while the repository is checked out at the
// second, so resolution fails closed instead of letting a tool answer from the
// wrong version.
func TestResolveSourceMismatch(t *testing.T) {
	r := newRepo(t)

	got, err := resolver.New(configFor(r, r.first)).Resolve(t.Context(), "app-v2")
	if err == nil {
		t.Fatalf("Resolve() = %+v, want SOURCE_MISMATCH", got)
	}
	if got != (vacctx.CodeContext{}) {
		t.Errorf("Resolve() = %+v, want the zero context: a mismatched context must not be usable", got)
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

// TestResolveUnreadableRepository and TestResolveUnknownRevision cover the two
// ways verification cannot be carried out. Neither may pass: a context whose
// revision could not be checked is not a verified context.
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
