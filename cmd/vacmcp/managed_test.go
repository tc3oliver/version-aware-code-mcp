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
			ID: id,
			Members: []store.ContextMember{{
				Repository: "demo",
				Branch:     "vacmcp/" + id + "-" + revision[:12],
				Revision:   revision,
				GraphRef:   "vacmcp-demo-" + id + "-" + revision[:12],
			}},
			State: state,
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

// TestManagedConfigWritesTheSpellingTheMemberCountDecides is the generated
// file's shape, which is the record's shape all over again: a context over one
// repository is written inline, exactly as every configuration before contexts
// could name several, and only a context over two becomes a members list.
//
// That the file is loadable is not a separate check bolted on: managedConfig
// returns what config.Load makes of the file it wrote, so a spelling Load
// refused would fail this test by never returning a configuration at all.
func TestManagedConfigWritesTheSpellingTheMemberCountDecides(t *testing.T) {
	const apiSHA = "0123456789abcdef0123456789abcdef01234567"
	const webSHA = "89abcdef0123456789abcdef0123456789abcdef"

	s := openStore(t, t.TempDir())
	for _, name := range []string{"api", "web"} {
		if err := s.PutRepository(store.Repository{Name: name, State: managed.RepositoryReady}); err != nil {
			t.Fatalf("PutRepository(%s): %v", name, err)
		}
	}
	single := store.Context{
		ID: "api-v1",
		Members: []store.ContextMember{
			{Repository: "api", Branch: "vacmcp/api-v1-" + apiSHA[:12], Revision: apiSHA, GraphRef: "vacmcp-api-api-v1-" + apiSHA[:12]},
		},
		State: managed.ContextReady,
	}
	stack := store.Context{
		ID: "stack",
		Members: []store.ContextMember{
			{Repository: "api", Branch: "vacmcp/api-stack-" + apiSHA[:12], Revision: apiSHA, GraphRef: "vacmcp-api-stack-" + apiSHA[:12]},
			{Repository: "web", Branch: "vacmcp/web-stack-" + webSHA[:12], Revision: webSHA, GraphRef: "vacmcp-web-stack-" + webSHA[:12]},
		},
		State: managed.ContextReady,
	}
	for _, c := range []store.Context{single, stack} {
		if err := s.PutContext(c); err != nil {
			t.Fatalf("PutContext(%s): %v", c.ID, err)
		}
	}

	cfg, err := managedConfig(s, defaultZoektURL, managed.CBMCommand)
	if err != nil {
		t.Fatalf("managedConfig: %v", err)
	}

	// Both contexts came back through config.Load, with every member's own
	// repository, branch, revision and graph.
	for _, want := range []store.Context{single, stack} {
		workspace, served := cfg.Contexts[want.ID]
		if !served {
			t.Errorf("the generated configuration does not carry %s", want.ID)
			continue
		}
		if len(workspace.Members) != len(want.Members) {
			t.Errorf("%s has %d members, want %d: %+v", want.ID, len(workspace.Members), len(want.Members), workspace)
			continue
		}
		for i, member := range workspace.Members {
			record := want.Members[i]
			if member.ID != want.ID || member.Repository != record.Repository || member.Branch != record.Branch ||
				member.Revision != record.Revision || member.GraphRef != record.GraphRef {
				t.Errorf("%s member %d = %+v, want the record's %+v", want.ID, i, member, record)
			}
		}
	}
	// Every member's repository is declared, or config.Load would have refused
	// the file rather than serving a context pointing at nothing.
	for _, name := range []string{"api", "web"} {
		clone, err := s.RepositoryDir(name)
		if err != nil {
			t.Fatalf("RepositoryDir(%s): %v", name, err)
		}
		if got := cfg.Repositories[name].Path; got != clone {
			t.Errorf("repository %s path = %q, want the local clone %q", name, got, clone)
		}
	}

	// And the file itself is written the two ways, which is what keeps a
	// managed installation of single-repository contexts producing the
	// configuration it always did.
	body, err := os.ReadFile(filepath.Join(s.RuntimeDir(), generatedConfig))
	if err != nil {
		t.Fatalf("reading the generated configuration: %v", err)
	}
	generated := string(body)
	if !strings.Contains(generated, "api-v1:\n        repository: api") {
		t.Errorf("the one-repository context is not written inline:\n%s", generated)
	}
	if !strings.Contains(generated, "stack:\n        members:") {
		t.Errorf("the two-repository context is not written as a members list:\n%s", generated)
	}
	if strings.Count(generated, "members:") != 1 {
		t.Errorf("the generated configuration writes %d members lists, want the one the two-repository context needs:\n%s", strings.Count(generated, "members:"), generated)
	}
	t.Logf("generated configuration:\n%s", generated)
}

// TestReadyContextsServesWholeContextsOnly is what the query plane reads,
// asked of a data directory whose contexts name several repositories: READY is
// the whole of it, and a context that failed is out of it entirely rather than
// in it with the members that worked.
func TestReadyContextsServesWholeContextsOnly(t *testing.T) {
	const revision = "0123456789abcdef0123456789abcdef01234567"

	s := openStore(t, t.TempDir())
	for id, state := range map[string]string{"stack-ready": managed.ContextReady, "stack-failed": managed.ContextFailed} {
		if err := s.PutContext(store.Context{
			ID: id,
			Members: []store.ContextMember{
				{Repository: "api", Branch: "vacmcp/api-" + id + "-" + revision[:12], Revision: revision, GraphRef: "vacmcp-api-" + id + "-" + revision[:12]},
				{Repository: "web", Branch: "vacmcp/web-" + id + "-" + revision[:12], Revision: revision, GraphRef: "vacmcp-web-" + id + "-" + revision[:12]},
			},
			State: state,
		}); err != nil {
			t.Fatalf("PutContext(%s): %v", id, err)
		}
	}

	ready, err := readyContexts(s)
	if err != nil {
		t.Fatalf("readyContexts: %v", err)
	}
	if len(ready) != 1 || ready[0].ID != "stack-ready" {
		t.Fatalf("the query plane reads %+v, want only the READY context", ready)
	}
	if len(ready[0].Members) != 2 {
		t.Errorf("the READY context is served with %d members, want both", len(ready[0].Members))
	}
}

// TestManagedConfigOfADataDirectoryWithNothingReady is the empty case: a data
// directory whose contexts are all still being built, or all failed, is the
// server `serve` without a --config gives — no contexts, and no error either.
func TestManagedConfigOfADataDirectoryWithNothingReady(t *testing.T) {
	s := openStore(t, t.TempDir())
	if err := s.PutContext(store.Context{ID: "app", Members: []store.ContextMember{{Repository: "demo"}}, State: managed.ContextFailed}); err != nil {
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
			ID: id,
			Members: []store.ContextMember{{
				Repository: "demo",
				Branch:     "vacmcp/" + id + "-" + revision[:12],
				Revision:   revision,
				GraphRef:   "vacmcp-demo-" + id + "-" + revision[:12],
			}},
			State: state,
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
