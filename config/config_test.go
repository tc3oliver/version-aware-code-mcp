package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// write puts body in a temporary file and returns its path.
func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// loadErrCode loads body and returns the vacerr code it failed with.
func loadErrCode(t *testing.T, body string) vacerr.Code {
	t.Helper()
	cfg, err := config.Load(write(t, body))
	if err == nil {
		t.Fatalf("Load() = %+v, want error", cfg)
	}
	var vErr *vacerr.Error
	if !errors.As(err, &vErr) {
		t.Fatalf("Load() error = %v (%T), want *vacerr.Error", err, err)
	}
	return vErr.Code
}

const valid = `
server:
  address: 127.0.0.1:8080
providers:
  zoekt:
    url: http://127.0.0.1:6070
  cbm:
    command: codebase-memory-mcp
repositories:
  example/backend:
    path: /srv/repos/backend
contexts:
  app-v1:
    repository: example/backend
    branch: release/1.x
    revision: 8af31e2
    graph_ref: backend-v1
  app-v2:
    repository: example/backend
    branch: release/2.x
    revision: 94cb821
    graph_ref: backend-v2
`

func TestLoadValidConfig(t *testing.T) {
	cfg, err := config.Load(write(t, valid))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Server.Address != "127.0.0.1:8080" {
		t.Errorf("Server.Address = %q", cfg.Server.Address)
	}
	if cfg.Providers.Zoekt.URL != "http://127.0.0.1:6070" {
		t.Errorf("Providers.Zoekt.URL = %q", cfg.Providers.Zoekt.URL)
	}
	if cfg.Providers.CBM.Command != "codebase-memory-mcp" {
		t.Errorf("Providers.CBM.Command = %q", cfg.Providers.CBM.Command)
	}
	if got := cfg.Repositories["example/backend"].Path; got != "/srv/repos/backend" {
		t.Errorf("Repositories[example/backend].Path = %q", got)
	}

	if len(cfg.Contexts) != 2 {
		t.Fatalf("len(Contexts) = %d, want 2", len(cfg.Contexts))
	}
	// A context naming one repository is a workspace of one member, and the ID
	// comes from the key on both. The whole point of the type is that the two
	// versions of one repository stay separate all the way down to graph_ref.
	want := vacctx.Workspace{
		ID: "app-v2",
		Members: []vacctx.CodeContext{{
			ID:         "app-v2",
			Repository: "example/backend",
			Branch:     "release/2.x",
			Revision:   "94cb821",
			GraphRef:   "backend-v2",
		}},
	}
	if got := cfg.Contexts["app-v2"]; !reflect.DeepEqual(got, want) {
		t.Errorf("Contexts[app-v2] = %+v, want %+v", got, want)
	}
	if only(t, cfg, "app-v1").GraphRef == only(t, cfg, "app-v2").GraphRef {
		t.Error("the two contexts share a graph_ref")
	}
}

// only returns the single member of a context, and fails the test if it does not
// have exactly one. Everything a v0.4.0 configuration could write is one member,
// so this is what "unchanged" means for those tests.
func only(t *testing.T, cfg *config.Config, id string) vacctx.CodeContext {
	t.Helper()
	workspace, ok := cfg.Contexts[id]
	if !ok {
		t.Fatalf("no context %q was loaded", id)
	}
	if len(workspace.Members) != 1 {
		t.Fatalf("context %q has %d members, want the one it declared", id, len(workspace.Members))
	}
	return workspace.Members[0]
}

// A context may name several repositories, each pinned to its own revision, and
// each keeps its own everything: sharing a workspace does not merge a branch, a
// revision or a graph reference into the one next to it.
func TestLoadMultiRepositoryContext(t *testing.T) {
	body := `
repositories:
  example/backend: {path: /srv/repos/backend}
  example/frontend: {path: /srv/repos/frontend}
contexts:
  release-5:
    members:
      - {repository: example/backend, branch: release/5.x, revision: 8af31e2, graph_ref: backend-v5}
      - {repository: example/frontend, branch: main, revision: 94cb821, graph_ref: frontend-v5}
`
	cfg, err := config.Load(write(t, body))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	want := vacctx.Workspace{
		ID: "release-5",
		Members: []vacctx.CodeContext{
			{ID: "release-5", Repository: "example/backend", Branch: "release/5.x", Revision: "8af31e2", GraphRef: "backend-v5"},
			{ID: "release-5", Repository: "example/frontend", Branch: "main", Revision: "94cb821", GraphRef: "frontend-v5"},
		},
	}
	if got := cfg.Contexts["release-5"]; !reflect.DeepEqual(got, want) {
		t.Errorf("Contexts[release-5] = %+v, want %+v", got, want)
	}
}

// The two spellings are two ways of writing a context and not two halves of
// one. A file that uses both under one ID has said two things, and neither of
// them is safe to assume was the one meant.
func TestLoadRejectsAContextDeclaredBothWays(t *testing.T) {
	body := `
repositories:
  example/backend: {path: /srv/repos/backend}
contexts:
  app-v1:
    repository: example/backend
    branch: release/1.x
    revision: 8af31e2
    graph_ref: backend-v1
    members:
      - {repository: example/backend, branch: release/2.x, revision: 94cb821, graph_ref: backend-v2}
`
	if got := loadErrCode(t, body); got != vacerr.InvalidArgument {
		t.Errorf("code = %q, want INVALID_ARGUMENT", got)
	}
}

// A context with no repository in it is no scope at all: every query naming it
// would have nowhere to run. Both spellings of "none" are refused, because an
// empty list and a missing one are the same absence.
func TestLoadRejectsAContextWithNoMember(t *testing.T) {
	for name, members := range map[string]string{"an empty list": "[]", "nothing at all": ""} {
		t.Run(name, func(t *testing.T) {
			body := "repositories:\n  example/backend: {path: /srv/repos/backend}\ncontexts:\n  app-v1: {members: " + members + "}\n"
			if got := loadErrCode(t, body); got != vacerr.InvalidArgument {
				t.Errorf("code = %q, want INVALID_ARGUMENT", got)
			}
		})
	}
}

// One repository twice in one context pins two revisions of it under one name,
// and a workspace that answered in both would report one repository's code twice
// with no way to say which version each half came from. The error names the
// repository, because that is what the operator has to go and look at.
func TestLoadRejectsARepositoryDeclaredTwiceInOneContext(t *testing.T) {
	body := `
repositories:
  example/backend: {path: /srv/repos/backend}
contexts:
  release-5:
    members:
      - {repository: example/backend, branch: release/1.x, revision: 8af31e2, graph_ref: backend-v1}
      - {repository: example/backend, branch: release/2.x, revision: 94cb821, graph_ref: backend-v2}
`
	_, err := config.Load(write(t, body))
	if err == nil {
		t.Fatal("Load() = nil error, want the repeated repository refused")
	}
	var vErr *vacerr.Error
	if !errors.As(err, &vErr) {
		t.Fatalf("Load() error = %v (%T), want *vacerr.Error", err, err)
	}
	if vErr.Code != vacerr.InvalidArgument {
		t.Errorf("code = %q, want INVALID_ARGUMENT", vErr.Code)
	}
	if vErr.Details["repository"] != "example/backend" {
		t.Errorf("details say repository %v, want the one declared twice", vErr.Details["repository"])
	}
}

// The graph_ref rule is about graphs, not about contexts: one CBM project holds
// one version's graph, so two members pinning different revisions may not share
// one wherever they are written. Inside one context is the case a context-by-
// context check would miss.
func TestLoadRejectsSharedGraphRefBetweenMembers(t *testing.T) {
	for name, body := range map[string]string{
		"in one context": `
repositories:
  example/backend: {path: /srv/repos/backend}
  example/frontend: {path: /srv/repos/frontend}
contexts:
  release-5:
    members:
      - {repository: example/backend, branch: release/5.x, revision: 8af31e2, graph_ref: shared}
      - {repository: example/frontend, branch: main, revision: 94cb821, graph_ref: shared}
`,
		"across two contexts": `
repositories:
  example/backend: {path: /srv/repos/backend}
  example/frontend: {path: /srv/repos/frontend}
contexts:
  release-5:
    members:
      - {repository: example/backend, branch: release/5.x, revision: 8af31e2, graph_ref: shared}
  release-6:
    members:
      - {repository: example/backend, branch: release/6.x, revision: 94cb821, graph_ref: shared}
`,
	} {
		t.Run(name, func(t *testing.T) {
			if got := loadErrCode(t, body); got != vacerr.InvalidArgument {
				t.Errorf("code = %q, want INVALID_ARGUMENT", got)
			}
		})
	}
}

// A member that names an undeclared repository is the same mistake a
// single-repository context makes, and gets the same code, so a typo inside a
// members list is as legible as one outside it.
func TestLoadRejectsUnknownRepositoryInAMember(t *testing.T) {
	body := `
repositories:
  example/backend: {path: /srv/repos/backend}
contexts:
  release-5:
    members:
      - {repository: example/backend, branch: release/5.x, revision: 8af31e2, graph_ref: backend-v5}
      - {repository: example/frontend, branch: main, revision: 94cb821, graph_ref: frontend-v5}
`
	if got := loadErrCode(t, body); got != vacerr.RepositoryNotFound {
		t.Errorf("code = %q, want REPOSITORY_NOT_FOUND", got)
	}
}

// Strict parsing reaches inside a members list: a field this version does not
// know is refused there exactly as it is at the top of a context, so a
// misspelling cannot silently leave a member unscoped.
func TestLoadRejectsAnUnknownFieldInAMember(t *testing.T) {
	body := `
repositories:
  example/backend: {path: /srv/repos/backend}
contexts:
  release-5:
    members:
      - {repository: example/backend, branch: release/5.x, revision: 8af31e2, graph_ref: backend-v5, llm_model: gpt}
`
	if got := loadErrCode(t, body); got != vacerr.InvalidArgument {
		t.Errorf("code = %q, want INVALID_ARGUMENT", got)
	}
}

// TestLoadExampleFile keeps the shipped example honest: it is loaded by the same
// parser as a user's file, so it cannot drift away from the schema.
func TestLoadExampleFile(t *testing.T) {
	cfg, err := config.Load("example.yaml")
	if err != nil {
		t.Fatalf("Load(example.yaml) error = %v", err)
	}
	if len(cfg.Contexts) == 0 {
		t.Error("example.yaml declares no context")
	}
}

func TestLoadRejectsMissingRequiredFields(t *testing.T) {
	tests := map[string]string{
		"missing repository": `
repositories:
  example/backend: {path: /srv/repos/backend}
contexts:
  app-v1: {branch: release/1.x, revision: 8af31e2, graph_ref: backend-v1}
`,
		"missing branch": `
repositories:
  example/backend: {path: /srv/repos/backend}
contexts:
  app-v1: {repository: example/backend, revision: 8af31e2, graph_ref: backend-v1}
`,
		"missing revision": `
repositories:
  example/backend: {path: /srv/repos/backend}
contexts:
  app-v1: {repository: example/backend, branch: release/1.x, graph_ref: backend-v1}
`,
		"missing graph_ref": `
repositories:
  example/backend: {path: /srv/repos/backend}
contexts:
  app-v1: {repository: example/backend, branch: release/1.x, revision: 8af31e2}
`,
		"blank revision": `
repositories:
  example/backend: {path: /srv/repos/backend}
contexts:
  app-v1: {repository: example/backend, branch: release/1.x, revision: "  ", graph_ref: backend-v1}
`,
		"missing repository path": `
repositories:
  example/backend: {}
contexts:
  app-v1: {repository: example/backend, branch: release/1.x, revision: 8af31e2, graph_ref: backend-v1}
`,
		"no contexts": `
repositories:
  example/backend: {path: /srv/repos/backend}
`,
		"empty context ID": `
repositories:
  example/backend: {path: /srv/repos/backend}
contexts:
  "": {repository: example/backend, branch: release/1.x, revision: 8af31e2, graph_ref: backend-v1}
`,
	}

	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if got := loadErrCode(t, body); got != vacerr.InvalidArgument {
				t.Errorf("code = %q, want INVALID_ARGUMENT", got)
			}
		})
	}
}

// TestLoadRejectsDuplicateContextID covers a duplicate context ID, which YAML
// sees as a mapping key defined twice: without WithUniqueKeys the second entry
// would silently win and a tool call would resolve to a version nobody wrote.
func TestLoadRejectsDuplicateContextID(t *testing.T) {
	body := `
repositories:
  example/backend: {path: /srv/repos/backend}
contexts:
  app-v1: {repository: example/backend, branch: release/1.x, revision: 8af31e2, graph_ref: backend-v1}
  app-v1: {repository: example/backend, branch: release/2.x, revision: 94cb821, graph_ref: backend-v2}
`
	if got := loadErrCode(t, body); got != vacerr.InvalidArgument {
		t.Errorf("code = %q, want INVALID_ARGUMENT", got)
	}
}

// TestLoadRejectsUnknownRepository is the one validation failure with its own
// code, so a caller can tell a typo in a repository name from any other config
// problem.
func TestLoadRejectsUnknownRepository(t *testing.T) {
	body := `
repositories:
  example/backend: {path: /srv/repos/backend}
contexts:
  app-v1: {repository: example/frontend, branch: release/1.x, revision: 8af31e2, graph_ref: backend-v1}
`
	if got := loadErrCode(t, body); got != vacerr.RepositoryNotFound {
		t.Errorf("code = %q, want REPOSITORY_NOT_FOUND", got)
	}
}

// TestLoadRejectsSharedGraphRefAcrossRevisions guards the invariant the whole
// server rests on: one CBM project holds one version's graph, so two revisions
// may not point at the same one.
func TestLoadRejectsSharedGraphRefAcrossRevisions(t *testing.T) {
	body := `
repositories:
  example/backend: {path: /srv/repos/backend}
contexts:
  app-v1: {repository: example/backend, branch: release/1.x, revision: 8af31e2, graph_ref: backend}
  app-v2: {repository: example/backend, branch: release/2.x, revision: 94cb821, graph_ref: backend}
`
	if got := loadErrCode(t, body); got != vacerr.InvalidArgument {
		t.Errorf("code = %q, want INVALID_ARGUMENT", got)
	}
}

// TestLoadAllowsSharedGraphRefForSameRevision is the flip side: two contexts
// that name the same revision describe the same code, so they may share a graph.
func TestLoadAllowsSharedGraphRefForSameRevision(t *testing.T) {
	body := `
repositories:
  example/backend: {path: /srv/repos/backend}
contexts:
  app-v1: {repository: example/backend, branch: release/1.x, revision: 8af31e2, graph_ref: backend}
  app-v1-alias: {repository: example/backend, branch: release/1.x, revision: 8af31e2, graph_ref: backend}
`
	if _, err := config.Load(write(t, body)); err != nil {
		t.Errorf("Load() error = %v", err)
	}
}

func TestLoadRejectsMalformedInput(t *testing.T) {
	tests := map[string]string{
		"broken syntax":   "contexts:\n\t- tab indent\n",
		"unknown field":   valid + "  app-v3:\n    repository: example/backend\n    branch: b\n    revision: r\n    graph_ref: g\n    llm_model: gpt\n",
		"unknown section": valid + "llm:\n  api_key: sk-test\n",
		"id as a field":   "repositories:\n  r: {path: /p}\ncontexts:\n  app-v1: {id: elsewhere, repository: r, branch: b, revision: rev, graph_ref: g}\n",
		"contexts not a map": `
repositories:
  example/backend: {path: /srv/repos/backend}
contexts: 5
`,
		"empty file":         "",
		"multiple documents": valid + "---\ncontexts:\n  other:\n    repository: example/backend\n",
	}

	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if got := loadErrCode(t, body); got != vacerr.InvalidArgument {
				t.Errorf("code = %q, want INVALID_ARGUMENT", got)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	cfg, err := config.Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err == nil {
		t.Fatalf("Load() = %+v, want error", cfg)
	}
	var vErr *vacerr.Error
	if !errors.As(err, &vErr) || vErr.Code != vacerr.InvalidArgument {
		t.Errorf("Load() error = %v, want INVALID_ARGUMENT", err)
	}
}
