package main

// What the generated configuration is made of, and which modes may be combined,
// are decided by the records and the flags alone, so they are checked here
// without an engine. Managed serve itself — the server started as
// `vacmcp serve --managed` over a real Zoekt index and real graphs — is in
// managed_integration_test.go: that property is one of the whole path, and a
// stub anywhere on it would be the part under test answering for itself.

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/managed"
	"github.com/tc3oliver/version-aware-code-mcp/store"
)

// TestManagedConfigServesTheReadyRecordsAndNoRemoteURL is what the generated
// configuration is made of, asked of the records alone: the READY contexts, the
// local clone of each repository they name, and no trace of the remote the
// clone came from.
func TestManagedConfigServesTheReadyRecordsAndNoRemoteURL(t *testing.T) {
	const remote = "https://git.example.invalid/private/backend.git"
	const revision = "0123456789abcdef0123456789abcdef01234567"

	s := openStore(t, t.TempDir())
	if err := s.PutRepository(store.Repository{Name: "demo", URL: remote, State: managed.RepositoryReady}); err != nil {
		t.Fatalf("PutRepository: %v", err)
	}
	for id, state := range map[string]string{"app-ready": managed.ContextReady, "app-failed": managed.ContextFailed, "app-building": managed.ContextIndexingGraph} {
		if err := s.PutContext(store.Context{
			ID:         id,
			Repository: "demo",
			Branch:     "vacmcp/" + id + "-" + revision[:12],
			Revision:   revision,
			GraphRef:   "vacmcp-demo-" + id + "-" + revision[:12],
			State:      state,
		}); err != nil {
			t.Fatalf("PutContext(%s): %v", id, err)
		}
	}

	cfg, err := managedConfig(s, "http://127.0.0.1:6070", managed.CBMCommand)
	if err != nil {
		t.Fatalf("managedConfig: %v", err)
	}

	if got := slices.Sorted(maps.Keys(cfg.Contexts)); !slices.Equal(got, []string{"app-ready"}) {
		t.Errorf("the generated configuration carries %v, want only the READY context", got)
	}
	clone, err := s.RepositoryDir("demo")
	if err != nil {
		t.Fatalf("RepositoryDir: %v", err)
	}
	if got := cfg.Repositories["demo"].Path; got != clone {
		t.Errorf("repository path = %q, want the local clone %q", got, clone)
	}

	// The file is what an operator reads to see which contexts a server is
	// serving, and it must be readable without handing them a remote URL: the
	// query plane never reaches a remote, so the record's URL has no business
	// being written into a runtime configuration.
	body, err := os.ReadFile(filepath.Join(s.RuntimeDir(), generatedConfig))
	if err != nil {
		t.Fatalf("reading the generated configuration: %v", err)
	}
	generated := string(body)
	if strings.Contains(generated, remote) {
		t.Errorf("the generated configuration carries the remote URL:\n%s", generated)
	}
	for _, want := range []string{clone, "app-ready", "http://127.0.0.1:6070", managed.CBMCommand} {
		if !strings.Contains(generated, want) {
			t.Errorf("the generated configuration does not name %q:\n%s", want, generated)
		}
	}
	for _, unwanted := range []string{"app-failed", "app-building"} {
		if strings.Contains(generated, unwanted) {
			t.Errorf("the generated configuration names the non-READY context %s:\n%s", unwanted, generated)
		}
	}
}

// TestManagedConfigOfADataDirectoryWithNothingReady is the empty case: a data
// directory whose contexts are all still being built, or all failed, is the
// server `serve` without a --config gives — no contexts, and no error either.
func TestManagedConfigOfADataDirectoryWithNothingReady(t *testing.T) {
	s := openStore(t, t.TempDir())
	if err := s.PutContext(store.Context{ID: "app", Repository: "demo", State: managed.ContextFailed}); err != nil {
		t.Fatalf("PutContext: %v", err)
	}

	cfg, err := managedConfig(s, defaultZoektURL, managed.CBMCommand)
	if err != nil {
		t.Fatalf("managedConfig of a data directory with nothing READY: %v", err)
	}
	if len(cfg.Contexts) != 0 {
		t.Errorf("the generated configuration carries %v, want no contexts", cfg.Contexts)
	}
	if _, err := os.Stat(filepath.Join(s.RuntimeDir(), generatedConfig)); err != nil {
		t.Errorf("no generated configuration was written: %v", err)
	}
}

// TestDoctorManagedReportsTheRepositoriesAndTheContexts is AC #4: the two
// sections a managed installation has, in the table the rest of doctor prints.
//
// It needs git and nothing else: the repository is really cloned, the contexts
// are records, and the engines are reported on as they are — Zoekt is not
// running here, and that is another row rather than something that stops these
// two sections from being reported.
func TestDoctorManagedReportsTheRepositoriesAndTheContexts(t *testing.T) {
	data := t.TempDir()
	source := sourceRepo(t)
	if _, err := repoRun(t, data, "add", "demo", "--url", source); err != nil {
		t.Fatalf("repo add: %v", err)
	}
	revision := gitOut(t, "-C", source, "rev-parse", "HEAD")

	s := openStore(t, data)
	for id, state := range map[string]string{"app-ready": managed.ContextReady, "app-failed": managed.ContextFailed, "app-building": managed.ContextIndexingGraph} {
		if err := s.PutContext(store.Context{
			ID:         id,
			Repository: "demo",
			Branch:     "vacmcp/" + id + "-" + revision[:12],
			Revision:   revision,
			GraphRef:   "vacmcp-demo-" + id + "-" + revision[:12],
			State:      state,
		}); err != nil {
			t.Fatalf("PutContext(%s): %v", id, err)
		}
	}

	var buf bytes.Buffer
	// The error is Zoekt being unreachable, reported as its own row; the
	// sections below are the point here.
	_ = run([]string{"doctor", "--managed", "--data-dir", data}, &buf)
	out := buf.String()

	for _, section := range []string{"\nRepositories\n", "\nContexts\n"} {
		if !strings.Contains(out, section) {
			t.Errorf("doctor --managed printed no %q section:\n%s", strings.TrimSpace(section), out)
		}
	}

	// The repository row carries the state and when its refs were last fetched.
	if got := status(t, out, "demo"); got != statusOK {
		t.Errorf("demo = %s, want %s", got, statusOK)
	}
	if !strings.Contains(out, "never synced") {
		t.Errorf("the repository row does not say when it was last synced:\n%s", out)
	}

	// A READY context is resolved the way a configured one is; the others are
	// reported with the state keeping them out of the query plane, the failure
	// as a failure and the one still being built as a check that cannot be made
	// yet.
	for row, want := range map[string]string{"app-ready": statusOK, "app-failed": statusFail, "app-building": statusSkip} {
		if got := status(t, out, row); got != want {
			t.Errorf("%s = %s, want %s", row, got, want)
		}
	}
	for _, want := range []string{managed.ContextReady, managed.ContextFailed, managed.ContextIndexingGraph, revision} {
		if !strings.Contains(out, want) {
			t.Errorf("the Contexts section does not report %q:\n%s", want, out)
		}
	}
	t.Logf("doctor --managed:\n%s", out)
}

func TestServeRefusesBothModesAtOnce(t *testing.T) {
	// --stdio so a regression that dropped the refusal fails on a closed input
	// rather than serving forever.
	if err := serve([]string{"--stdio", "--managed", "--config", write(t, twoContexts)}); err == nil {
		t.Error("serve --managed --config returned nil, want an error")
	}
	if err := run([]string{"doctor", "--managed", "--config", write(t, twoContexts)}, &bytes.Buffer{}); err == nil {
		t.Error("doctor --managed --config returned nil, want an error")
	}
}
