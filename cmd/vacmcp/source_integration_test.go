//go:build integration

package main

// The worktree and the graph a context owns, tested against real git
// repositories and a real codebase-memory-mcp. There is no fake CBM here:
// decision-3 rules a mock out as an integration result, and what is being
// checked — that two versions of one repository end up as two graphs that
// answer differently — is exactly what a stub could not tell the truth about.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	cbmadapter "github.com/tc3oliver/version-aware-code-mcp/adapters/cbm"
	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/managed"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/store"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// The two versions of the demo source. Process calls LegacyHandler on the first
// commit and NewHandler on the second, so which handler a graph reports is
// which revision it was built from.
const (
	legacyHandler = "LegacyHandler"
	newHandler    = "NewHandler"
)

// cbmOrSkip skips a test when the graph engine is not on PATH. `context create`
// runs it, so without it there is nothing to test rather than something to fail.
func cbmOrSkip(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath(managed.CBMCommand); err != nil {
		t.Skipf("%s is not on PATH: %v", managed.CBMCommand, err)
	}
}

// graphExists reports, as an error when it does not, whether
// codebase-memory-mcp holds the graph the context names.
//
// It asks CBM what it has rather than the lifecycle's own verification, so what
// a create built and what a removal took away are observed from outside the code
// that did either.
func graphExists(ctx context.Context, id string, m store.ContextMember) error {
	out, err := exec.CommandContext(ctx, managed.CBMCommand, "cli", "list_projects").Output()
	if err != nil {
		return fmt.Errorf("%s cli list_projects: %w", managed.CBMCommand, err)
	}
	var body struct {
		Projects []struct {
			Name string `json:"name"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(out, &body); err != nil {
		return fmt.Errorf("list_projects did not answer with the JSON it promises: %w", err)
	}
	for _, project := range body.Projects {
		if project.Name == m.GraphRef {
			return nil
		}
	}
	return fmt.Errorf("codebase-memory-mcp holds no graph %q for context %q", m.GraphRef, id)
}

// discardGraphs deletes the graph of every context still recorded in dataDir.
//
// A data directory is a temporary directory the test framework removes, but a
// graph is not in it: it lives in CBM's own store, keyed by a name carrying the
// commit SHA of a repository these tests create afresh every run. Left behind,
// each run would add graphs to a developer's CBM that nothing will ever name
// again.
func discardGraphs(t *testing.T, dataDir string) {
	t.Helper()
	discardGraphsIn(dataDir, t.Logf)
}

// discardGraphsIn is discardGraphs for a caller that has no *testing.T to
// report to, which the shared installation's teardown does not: it runs from
// TestMain, after the last test.
func discardGraphsIn(dataDir string, logf func(string, ...any)) {
	s, err := store.Open(dataDir)
	if err != nil {
		logf("cleanup: store.Open(%s): %v", dataDir, err)
		return
	}
	contexts, err := s.Contexts()
	if err != nil {
		logf("cleanup: Contexts(): %v", err)
		return
	}

	// Concurrently, because CBM is fine with that and a test that created five
	// contexts would otherwise spend five subprocess startups on tidying up.
	var wg sync.WaitGroup
	for _, c := range contexts {
		// Every member of every context: a graph is one repository at one
		// revision, so a context over two owns two of them.
		for _, m := range c.Members {
			if m.GraphRef == "" {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				if out, err := exec.Command(managed.CBMCommand, "cli", "delete_project", "--project", m.GraphRef).CombinedOutput(); err != nil {
					logf("cleanup: delete_project %s: %v\n%s", m.GraphRef, err, out)
				}
			}()
		}
	}
	wg.Wait()
}

// versionedSource creates a repository whose one function calls a different
// helper on each of two commits, and returns the path and the two revisions.
func versionedSource(t *testing.T, name string) (path, first, second string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), name)
	mustGit(t, "init", "-q", "-b", "main", path)
	commit(t, path, "go.mod", "module "+name+"\n\ngo 1.26\n", "module")
	first = commit(t, path, "processor.go", processorSource(legacyHandler), "the legacy handler")
	second = commit(t, path, "processor.go", processorSource(newHandler), "the new handler")
	if first == second {
		t.Fatal("the second commit did not move off the first")
	}
	return path, first, second
}

func processorSource(handler string) string {
	return "package demo\n\nfunc " + handler + "() {}\n\nfunc Process() {\n\t" + handler + "()\n}\n"
}

// managedVersions returns a data directory with one repository added and one
// context per version of it, along with the two revisions.
func managedVersions(t *testing.T, v1, v2 string) (data, first, second string) {
	t.Helper()
	cbmOrSkip(t)
	source, first, second := versionedSource(t, "demo")

	data = t.TempDir()
	t.Cleanup(func() { discardGraphs(t, data) })
	if _, err := repoRun(t, data, "add", "demo", "--url", source); err != nil {
		t.Fatalf("repo add: %v", err)
	}
	for id, revision := range map[string]string{v1: first, v2: second} {
		if _, err := contextRun(t, data, "create", id, "--repo", "demo", "--ref", revision); err != nil {
			t.Fatalf("context create %s: %v", id, err)
		}
	}
	return data, first, second
}

// graphProvider returns the v0.1.0 CBM adapter, pointed at the same binary the
// context commands index with. Asking through it is what makes this a test of
// the graphs a context now owns rather than of CBM's command line.
func graphProvider(t *testing.T) *cbmadapter.Provider {
	t.Helper()
	p := cbmadapter.New(&config.Config{Providers: config.Providers{CBM: config.CBM{Command: managed.CBMCommand}}})
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// tracedCallees returns what the context's graph says Process calls.
func tracedCallees(t *testing.T, p *cbmadapter.Provider, id string, m store.ContextMember) []string {
	t.Helper()
	graph, err := p.TraceCalls(t.Context(), scope(id, m), provider.TraceRequest{Symbol: "Process", Direction: provider.Callees, Depth: 2})
	if err != nil {
		t.Fatalf("TraceCalls(%s, Process): %v", id, err)
	}

	var names []string
	for _, edge := range graph.Edges {
		if edge.Caller == "Process" {
			names = append(names, edge.Callee)
		}
	}
	return names
}

// TestContextCreateBuildsAWorktreeAndAGraphPerContext is AC #1, AC #2 and AC #3
// at once: two contexts of one repository, and each ends up with a checkout of
// its own revision, a graph that answers about that revision, and no clone of
// its own.
func TestContextCreateBuildsAWorktreeAndAGraphPerContext(t *testing.T) {
	prepared := preparedManaged(t)
	data, first, second := prepared.data, prepared.legacy, prepared.modern
	clone := filepath.Join(data, "repos", "demo")

	// AC #1. The checkout is the pinned commit, and it really is that version's
	// source: a worktree on the right commit with the wrong content would index
	// into a graph nothing else could catch.
	for _, tc := range []struct{ id, revision, handler string }{
		{"app-v1", first, legacyHandler},
		{"app-v2", second, newHandler},
	} {
		worktree := filepath.Join(data, "worktrees", "demo", tc.id)
		if head := gitOut(t, "-C", worktree, "rev-parse", "HEAD"); head != tc.revision {
			t.Errorf("%s worktree HEAD = %q, want the pinned %q", tc.id, head, tc.revision)
		}
		body, err := os.ReadFile(filepath.Join(worktree, "processor.go"))
		if err != nil {
			t.Fatalf("%s: no checked-out source: %v", tc.id, err)
		}
		if !strings.Contains(string(body), tc.handler) {
			t.Errorf("%s worktree holds\n%s\nwant the version that calls %s", tc.id, body, tc.handler)
		}

		// AC #3. A worktree's .git is a file pointing at administrative files
		// inside the clone, so the two contexts read one object database. A
		// second clone would be a directory here, with objects of its own.
		info, err := os.Stat(filepath.Join(worktree, ".git"))
		if err != nil {
			t.Fatalf("%s: no .git in the worktree: %v", tc.id, err)
		}
		if info.IsDir() {
			t.Errorf("%s has a .git directory, so it is a clone of its own rather than a worktree", tc.id)
		}
		pointer, err := os.ReadFile(filepath.Join(worktree, ".git"))
		if err != nil {
			t.Fatalf("%s: cannot read .git: %v", tc.id, err)
		}
		if !strings.Contains(string(pointer), clone) {
			t.Errorf("%s .git = %q, want it to point into the clone at %s", tc.id, pointer, clone)
		}
		if _, err := os.Stat(filepath.Join(worktree, ".git", "objects")); err == nil {
			t.Errorf("%s has an object database of its own", tc.id)
		}
	}

	// The clone administers both, which is the same statement from git's side.
	listed := gitOut(t, "-C", clone, "worktree", "list")
	for _, id := range []string{"app-v1", "app-v2"} {
		if !strings.Contains(listed, filepath.Join(data, "worktrees", "demo", id)) {
			t.Errorf("git worktree list =\n%s\nwant it to administer %s", listed, id)
		}
	}

	// AC #2. The graphs exist and each one answers about its own version. This
	// is the whole point of the two of them: one symbol, two contexts, two
	// answers.
	p := graphProvider(t)
	for _, tc := range []struct{ id, want, absent string }{
		{"app-v1", legacyHandler, newHandler},
		{"app-v2", newHandler, legacyHandler},
	} {
		member := only(t, contextRecord(t, data, tc.id))
		if err := graphExists(t.Context(), tc.id, member); err != nil {
			t.Fatalf("%s: the graph was not built: %v", tc.id, err)
		}

		called := tracedCallees(t, p, tc.id, member)
		if !slices.Contains(called, tc.want) {
			t.Errorf("%s: Process calls %v, want it to include %s", tc.id, called, tc.want)
		}
		if slices.Contains(called, tc.absent) {
			t.Errorf("%s: Process calls %v, which is the other version's handler: the graphs are contaminated", tc.id, called)
		}
		t.Logf("%s (graph_ref %s) Process -> %v", tc.id, member.GraphRef, called)
	}

	// AC #2, the other half: the generated name is internal. The MCP tools are
	// checked against it in tools/, and on the management plane only `context
	// status` prints it.
	out, err := contextRun(t, data, "list")
	if err != nil {
		t.Fatalf("context list: %v", err)
	}
	for _, id := range []string{"app-v1", "app-v2"} {
		if ref := only(t, contextRecord(t, data, id)).GraphRef; strings.Contains(out, ref) {
			t.Errorf("context list printed the graph reference %q:\n%s", ref, out)
		}
	}
}

// TestContextRemoveTakesOnlyItsOwnWorktreeAndGraph is AC #4: one context's
// artifacts go and the other's stay, in a repository both are pinned in.
func TestContextRemoveTakesOnlyItsOwnWorktreeAndGraph(t *testing.T) {
	data, _, second := managedVersions(t, "drop", "keep")
	clone := filepath.Join(data, "repos", "demo")
	dropped := only(t, contextRecord(t, data, "drop"))
	kept := only(t, contextRecord(t, data, "keep"))

	if _, err := contextRun(t, data, "remove", "drop"); err != nil {
		t.Fatalf("context remove: %v", err)
	}

	// The removed context's worktree is gone, and so is git's record of it:
	// without the prune the clone would still administer a missing worktree and
	// refuse to create another one at that path.
	worktree := filepath.Join(data, "worktrees", "demo", "drop")
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Errorf("the worktree of drop is still there (err = %v)", err)
	}
	if listed := gitOut(t, "-C", clone, "worktree", "list"); strings.Contains(listed, worktree) {
		t.Errorf("git worktree list =\n%s\nwant the removed worktree pruned", listed)
	}
	if err := graphExists(t.Context(), "drop", dropped); err == nil {
		t.Errorf("CBM still holds the graph %q of the removed context", dropped.GraphRef)
	}

	// The sibling is untouched: its checkout is still on its own revision and
	// its graph still answers.
	if head := gitOut(t, "-C", filepath.Join(data, "worktrees", "demo", "keep"), "rev-parse", "HEAD"); head != second {
		t.Errorf("keep worktree HEAD = %q after removing drop, want %q", head, second)
	}
	if err := graphExists(t.Context(), "keep", kept); err != nil {
		t.Fatalf("removing drop took the graph of keep: %v", err)
	}
	if called := tracedCallees(t, graphProvider(t), "keep", kept); !slices.Contains(called, newHandler) {
		t.Errorf("keep: Process calls %v after removing drop, want it to include %s", called, newHandler)
	}

	// And the same context name can be created again, which only holds if the
	// worktree really was pruned rather than just deleted.
	if _, err := contextRun(t, data, "create", "drop", "--repo", "demo", "--ref", second); err != nil {
		t.Errorf("re-creating a removed context: %v", err)
	}

	// A clone deleted by hand leaves nothing to prune, and must not be what
	// makes a context impossible to remove.
	if err := os.RemoveAll(clone); err != nil {
		t.Fatalf("RemoveAll(%s): %v", clone, err)
	}
	if _, err := contextRun(t, data, "remove", "keep"); err != nil {
		t.Errorf("removing a context whose clone is gone: %v", err)
	}
}

// TestContextVerifyChecksTheArtifacts covers the internal verification the
// readiness lifecycle calls: the checkout is still the pinned revision, and the
// graph is still there. Both are asked of the artifact itself, so both fail
// when it is tampered with underneath.
func TestContextVerifyChecksTheArtifacts(t *testing.T) {
	data, first, second := managedVersions(t, "app-v1", "app-v2")

	if _, err := contextRun(t, data, "verify", "app-v1"); err != nil {
		t.Fatalf("context verify of a freshly created context: %v", err)
	}

	// A worktree checked out somewhere else by hand is the wrong-version answer
	// this server refuses to give, so it is SOURCE_MISMATCH and not a record
	// this command quietly updates.
	mustGit(t, "-C", filepath.Join(data, "worktrees", "demo", "app-v1"), "checkout", "-q", "--detach", second)
	_, err := contextRun(t, data, "verify", "app-v1")
	if got := codeFor(t, err); got != vacerr.SourceMismatch {
		t.Errorf("verify of a worktree on another commit: code = %q, want %q", got, vacerr.SourceMismatch)
	}
	if got := only(t, contextRecord(t, data, "app-v1")).Revision; got != first {
		t.Errorf("a failed verify repinned the context to %q, want %q", got, first)
	}

	// A graph deleted out from under a context is GRAPH_PROVIDER_UNAVAILABLE
	// rather than a context that reports itself as fine.
	survivor := only(t, contextRecord(t, data, "app-v2"))
	if out, err := exec.Command(managed.CBMCommand, "cli", "delete_project", "--project", survivor.GraphRef).CombinedOutput(); err != nil {
		t.Fatalf("delete_project %s: %v\n%s", survivor.GraphRef, err, out)
	}
	_, err = contextRun(t, data, "verify", "app-v2")
	if got := codeFor(t, err); got != vacerr.GraphProviderUnavailable {
		t.Errorf("verify of a context whose graph is gone: code = %q, want %q", got, vacerr.GraphProviderUnavailable)
	}
}

// TestConcurrentContextCreatesIndexIndependentGraphs is the concurrency
// verification decision-4 asks for, run through the command that does the
// indexing.
//
// Two independent repositories, two contexts, both created at the same moment:
// codebase-memory-mcp is indexing two different projects concurrently, which is
// the capability that decides whether the call in source.go needs serialising.
// Measured directly against the binary as well — two, four and six concurrent
// `cli index_repository` runs, with and without a warm daemon — every one of
// them exited 0 and left a graph that queries independently, so there is no
// semaphore around it. This test is what would go red if that stopped being
// true.
func TestConcurrentContextCreatesIndexIndependentGraphs(t *testing.T) {
	cbmOrSkip(t)
	data := t.TempDir()
	t.Cleanup(func() { discardGraphs(t, data) })

	repositories := []string{"alpha", "beta"}
	revisions := map[string]string{}
	for _, name := range repositories {
		source, _, second := versionedSource(t, name)
		if _, err := repoRun(t, data, "add", name, "--url", source); err != nil {
			t.Fatalf("repo add %s: %v", name, err)
		}
		revisions[name] = second
	}

	var wg sync.WaitGroup
	for _, name := range repositories {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := contextRun(t, data, "create", name+"-ctx", "--repo", name, "--ref", revisions[name]); err != nil {
				t.Errorf("concurrent context create %s: %v", name, err)
			}
		}()
	}
	wg.Wait()

	// Both graphs are there and neither is the other's: a concurrent index that
	// had interfered would show up as a missing graph or as one project holding
	// the other repository's symbols.
	p := graphProvider(t)
	for _, name := range repositories {
		member := only(t, contextRecord(t, data, name+"-ctx"))
		if err := graphExists(t.Context(), name+"-ctx", member); err != nil {
			t.Fatalf("%s: concurrent indexing left no graph: %v", name, err)
		}
		called := tracedCallees(t, p, name+"-ctx", member)
		if !slices.Contains(called, newHandler) {
			t.Errorf("%s: Process calls %v, want it to include %s", name, called, newHandler)
		}
		t.Logf("%s (graph_ref %s) Process -> %v", name, member.GraphRef, called)
	}
}
