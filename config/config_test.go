package config_test

import (
	"errors"
	"os"
	"path/filepath"
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
	// The ID comes from the key, and the whole point of the type is that the two
	// versions of one repository stay separate all the way down to graph_ref.
	want := vacctx.CodeContext{
		ID:         "app-v2",
		Repository: "example/backend",
		Branch:     "release/2.x",
		Revision:   "94cb821",
		GraphRef:   "backend-v2",
	}
	if got := cfg.Contexts["app-v2"]; got != want {
		t.Errorf("Contexts[app-v2] = %+v, want %+v", got, want)
	}
	if cfg.Contexts["app-v1"].GraphRef == cfg.Contexts["app-v2"].GraphRef {
		t.Error("the two contexts share a graph_ref")
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
