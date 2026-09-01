package integration

// v0.2.0's release gate: doc-1 §15's four version-correctness checks asked of
// contexts the management plane built, and the lifecycle gate decision-4 adds
// on top of them.
//
// It is the same binary, the same STDIO transport and the same four tools as
// release_gate_test.go, and deliberately the same four checks — what differs is
// where the configuration came from. In v0.1.0 a person wrote the contexts by
// hand; here `vacmcp repo add` and `vacmcp context create` resolve the
// revisions, check the source out, build the index and the graph and generate
// the configuration, and any of those steps could pin, index or trace the wrong
// version without a hand-written file ever being wrong. So the isolation is
// asked again of what the management plane produced.
//
// Nothing is simulated. The remote is a real bare repository, the clone, fetch
// and worktree are real git, the search is a real zoekt-webserver over the
// index `context create` built, and the graph is real codebase-memory-mcp.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/internal/demorepo"
)

// The managed repository, and the graph engine the generated configuration
// names. The context IDs are release_gate_test.go's v1 and v2: the same names
// answering the same four questions is the whole point of running the gate
// twice.
const (
	managedRepository = "demo"
	cbmCommand        = "codebase-memory-mcp"
)

// TestManagedLifecycleReleaseGate is doc-1 §15's gate, run through the
// management plane.
//
// One `repo add` and two `context create`s are the whole of the setup — no
// zoekt-git-index, no worktree, no index_repository, no configuration file —
// and the four checks that follow are the ones v0.1.0 ships red-blocks on.
func TestManagedLifecycleReleaseGate(t *testing.T) {
	binary, data := installation(t)
	remote := remoteRepository(t)

	vacmcpRun(t, binary, data, "repo", "add", managedRepository, "--url", remote)
	createContext(t, binary, data, v1, managedPin{managedRepository, demorepo.V1})
	createContext(t, binary, data, v2, managedPin{managedRepository, demorepo.V2})

	assertVersionIsolation(t, serveManaged(t, binary, data), v1, v2)
}

// TestManagedContextSurvivesTheRemoteMoving is decision-4's lifecycle gate: a
// context pins a revision, and a remote that moves afterwards does not move it.
//
// This is the guarantee the whole of v0.2.0 rests on. `repo add` and `context
// create` are the point where an automated onboarding could quietly start
// tracking a branch instead of pinning a commit, and a branch that has moved is
// exactly when that difference stops being invisible: the sync brings the new
// commit into the clone, and the already-created context has to keep answering
// out of the old one.
//
// The moved branch is made to carry the other release's source, so "still the
// old revision" is not only a SHA in a record — the served answers change
// symbol if the pin slipped, and the four checks below catch it.
func TestManagedContextSurvivesTheRemoteMoving(t *testing.T) {
	binary, data := installation(t)
	remote := remoteRepository(t)

	vacmcpRun(t, binary, data, "repo", "add", managedRepository, "--url", remote)

	const before, after = "v1-before", "v1-after"
	pinned := createContext(t, binary, data, before, managedPin{managedRepository, demorepo.V1})
	// demorepo.Revision addresses the fixture's own layout, and the remote here
	// is a bare repository, so the branch is read straight out of it.
	if want := gitOut(t, "-C", remote, "rev-parse", "refs/heads/"+demorepo.V1); pinned != want {
		t.Fatalf("context %s pinned %s, want %s, the commit %s is on", before, pinned, want, demorepo.V1)
	}

	// The remote moves: release/v1 gains a commit carrying release/v2's
	// handler, so the branch a context was pinned on now holds the other
	// version's source.
	moved := advanceRemote(t, remote, demorepo.V1)
	if moved == pinned {
		t.Fatal("the remote's release/v1 did not move, so nothing below would be under test")
	}
	vacmcpRun(t, binary, data, "repo", "sync", managedRepository)

	// The fetch really happened: the clone in the data directory has the new
	// commit. Without this the checks below would pass for a sync that did
	// nothing at all.
	clone := filepath.Join(data, "repos", managedRepository)
	if got := gitOut(t, "-C", clone, "rev-parse", "--verify", moved+"^{commit}"); got != moved {
		t.Fatalf("the clone resolves %s to %q after `repo sync`, want the moved commit fetched", moved, got)
	}

	// And the context did not move with it. The record is the only place a
	// pinned revision is written down, and `context status` is what an operator
	// reads it back through.
	if got := contextRevision(t, binary, data, before); got != pinned {
		t.Errorf(`LIFECYCLE GATE FAIL — decision-4: `+
			"\n  context %s was created pinned to %s"+
			"\n  the remote's %s then moved to %s and `repo sync` fetched it"+
			"\n  the context now reports %s"+
			"\ndecision-4: `repo sync` only fetches; it must not change any existing context's pinned revision.",
			before, pinned, demorepo.V1, moved, got)
	}

	// The new revision is reachable the only way decision-4 allows: as another
	// context. Both are then READY over one repository and one branch,
	// separated by nothing but the commit each pinned.
	if got := createContext(t, binary, data, after, managedPin{managedRepository, demorepo.V1}); got != moved {
		t.Fatalf("context %s pinned %s, want the moved commit %s", after, got, moved)
	}

	// The same four checks as the gate above, over two commits of one branch
	// rather than two branches: the older context must still answer with the
	// handler its commit calls, and must not have picked up the newer one's.
	assertVersionIsolation(t, serveManaged(t, binary, data), before, after)
}

// TestManagedMultiMemberContextSurvivesEachMembersRemoteMoving is TASK-86
// AC #11: decision-11 §3's pin-per-member guarantee, over two repositories
// instead of one, and checked the way an agent would notice a failure of it —
// through what the query tools actually serve, not only the record
// cmd/vacmcp's own TestRepoSyncMovesNoMemberOfAMultiRepositoryContext reads
// back. Both remotes move after the context is created, `repo sync --all`
// fetches both, and each member still has to answer with the commit it was
// pinned to and find none of what only the moved commit carries.
func TestManagedMultiMemberContextSurvivesEachMembersRemoteMoving(t *testing.T) {
	binary, data := installation(t)

	const r1, r2 = "r1", "r2"
	const r1Token, r2Token = "R1OnlySymbol", "R2OnlySymbol"
	remotes := map[string]string{
		r1: soloRemote(t, r1, r1Token),
		r2: soloRemote(t, r2, r2Token),
	}
	for _, name := range []string{r1, r2} {
		vacmcpRun(t, binary, data, "repo", "add", name, "--url", remotes[name])
	}

	pinned := createMultiContext(t, binary, data, "multi", managedPin{r1, "main"}, managedPin{r2, "main"})

	moved := map[string]string{}
	movedToken := map[string]string{r1: "R1LaterSymbol", r2: "R2LaterSymbol"}
	for _, name := range []string{r1, r2} {
		moved[name] = advanceGenericRemote(t, remotes[name], "main", name, movedToken[name])
		if moved[name] == pinned[name] {
			t.Fatalf("the remote's %s main did not move, so nothing below would be under test", name)
		}
	}
	vacmcpRun(t, binary, data, "repo", "sync", "--all")

	for _, name := range []string{r1, r2} {
		clone := filepath.Join(data, "repos", name)
		if got := gitOut(t, "-C", clone, "rev-parse", "--verify", moved[name]+"^{commit}"); got != moved[name] {
			t.Fatalf("the clone of %s resolves %s to %q after `repo sync --all`, want the moved commit fetched", name, moved[name], got)
		}
	}

	ownToken := map[string]string{r1: r1Token, r2: r2Token}
	v := serveManaged(t, binary, data)
	for _, name := range []string{r1, r2} {
		found := searchIn(t, v, 11, "multi", name, ownToken[name])
		if found.Context.Repository != name || found.Context.Revision != pinned[name] {
			t.Errorf(`LIFECYCLE GATE FAIL — decision-11 §3, member %s: `+
				"\n  member %s was pinned to %s at context creation"+
				"\n  its remote's main then moved to %s and `repo sync --all` fetched it"+
				"\n  search_code(context=multi, repository=%s) now answers with context %+v"+
				"\ndecision-11 §3: a remote moving after a member is pinned must not move what that member answers.",
				name, name, pinned[name], moved[name], name, found.Context)
		}
		if len(found.Matches) == 0 {
			t.Errorf("search_code(context=multi, repository=%s, %s) = no matches, want %s's own symbol", name, ownToken[name], name)
		}

		// The commit `repo sync` fetched carries a token this member's own
		// pinned revision never had: it must stay unreachable through this
		// context, the same isolation check 1 above makes for a single
		// repository's branches.
		leaked := searchIn(t, v, 11, "multi", name, movedToken[name])
		if len(leaked.Matches) != 0 {
			t.Errorf("search_code(context=multi, repository=%s, %s) = %+v, want 0: %s is only on the commit `repo sync` fetched, which this member must not have moved onto", name, movedToken[name], leaked.Matches, movedToken[name])
		}
	}
}

// soloRemote returns a bare repository a `repo add` can point at: one commit,
// on main, declaring one function named token — this workspace member's own
// version-differentiating symbol, independent of the shared demo repository
// so a two-repository workspace can be built from scratch rather than reusing
// one repository's two branches.
func soloRemote(t *testing.T, name, token string) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), name)
	mustGit(t, "init", "-q", "-b", "main", work)
	if err := os.WriteFile(filepath.Join(work, "go.mod"), []byte("module "+name+"\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatalf("writing %s/go.mod: %v", name, err)
	}
	if err := os.WriteFile(filepath.Join(work, name+".go"), []byte("package "+name+"\n\nfunc "+token+"() {}\n"), 0o600); err != nil {
		t.Fatalf("writing %s/%s.go: %v", name, name, err)
	}
	mustGit(t, "-C", work, "add", "-A")
	mustGit(t, "-C", work,
		"-c", "user.name=vacmcp gate", "-c", "user.email=gate@example.invalid", "-c", "commit.gpgsign=false",
		"commit", "--no-verify", "-q", "-m", "Add "+token)

	remote := filepath.Join(t.TempDir(), name+".git")
	mustGit(t, "clone", "--bare", "--quiet", work, remote)
	return remote
}

// advanceGenericRemote is [advanceRemote] for a repository [soloRemote] built:
// one commit added to branch of remote, declaring a function named token, and
// returns the commit branch now points at.
func advanceGenericRemote(t *testing.T, remote, branch, pkg, token string) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	mustGit(t, "clone", "--quiet", "--branch", branch, remote, work)
	if err := os.WriteFile(filepath.Join(work, "later.go"), []byte("package "+pkg+"\n\nfunc "+token+"() {}\n"), 0o600); err != nil {
		t.Fatalf("writing later.go: %v", err)
	}
	mustGit(t, "-C", work, "add", "-A")
	mustGit(t, "-C", work,
		"-c", "user.name=vacmcp gate", "-c", "user.email=gate@example.invalid", "-c", "commit.gpgsign=false",
		"commit", "--no-verify", "-q", "-m", "Add "+token)
	mustGit(t, "-C", work, "push", "--quiet", "origin", branch)
	return gitOut(t, "-C", work, "rev-parse", "HEAD")
}

// createMultiContext is [createContext] for a context over several
// repositories: every printed row is read, not only the first, so the caller
// gets back each member's own pinned revision rather than only the first
// one's.
func createMultiContext(t *testing.T, binary, data, id string, pins ...managedPin) map[string]string {
	t.Helper()
	args := []string{"context", "create", id}
	for _, p := range pins {
		args = append(args, "--repo", p.repo, "--ref", p.ref)
	}
	out := vacmcpRun(t, binary, data, args...)

	revisions := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 4 || fields[0] != id {
			t.Fatalf("context create %s printed %q, want the id, state, repository and revision on each line", id, out)
		}
		if fields[1] != "READY" {
			t.Fatalf("context create %s left member %s in state %s, want READY: a context the query plane will not serve", id, fields[2], fields[1])
		}
		revisions[fields[2]] = fields[3]
	}
	if len(revisions) != len(pins) {
		t.Fatalf("context create %s printed %d distinct members, want one per repository (%d): %q", id, len(revisions), len(pins), out)
	}
	return revisions
}

// assertVersionIsolation is doc-1 §15's four checks, asked of any two contexts
// whose source differs by which handler Process calls.
//
// Each is its own subtest, so a red build names the isolation that broke:
//
//	go test -run 'TestManagedLifecycleReleaseGate/check_3' ./integration/...
func assertVersionIsolation(t *testing.T, v vacmcp, legacy, modern string) {
	t.Helper()

	t.Run("check 1: "+legacy+" does not see NewHandler", func(t *testing.T) {
		found := searchIn(t, v, 1, legacy, "", newHandler)
		if len(found.Matches) != 0 {
			managedGateFail(t, 1, legacy, fmt.Sprintf(
				"search_code returned %d matches for %s, want 0. Every line below reached this context from the revision %s pins, which it never asked for:\n%s",
				len(found.Matches), newHandler, modern, matchLines(found.Matches)))
		}
	})

	// What keeps check 1 honest: a context that answers nothing passes it for
	// free.
	t.Run("check 2: "+modern+" does see NewHandler", func(t *testing.T) {
		found := searchIn(t, v, 2, modern, "", newHandler)
		if len(found.Matches) == 0 {
			managedGateFail(t, 2, modern, fmt.Sprintf(
				"search_code returned 0 matches for %s, want at least one: the revision this context pins does contain it. The context is being searched on another revision's ref, or on an index that never got this one — and it empties check 1 of meaning.",
				newHandler))
		}
	})

	t.Run("check 3: "+legacy+" traces Process to LegacyHandler", func(t *testing.T) {
		assertManagedTrace(t, 3, traceIn(t, v, 3, legacy, process), legacyHandler, newHandler, legacy, modern)
	})

	t.Run("check 4: "+modern+" traces Process to NewHandler", func(t *testing.T) {
		assertManagedTrace(t, 4, traceIn(t, v, 4, modern, process), newHandler, legacyHandler, modern, legacy)
	})
}

// assertManagedTrace holds one traced call chain to the revision its context
// pins: it has to reach the handler that revision calls, and it must not reach
// the one the other context's does, which is what a trace answered from the
// wrong graph looks like.
func assertManagedTrace(t *testing.T, check int, traced traceResult, want, absent, own, other string) {
	t.Helper()

	if _, ok := reaches(traced.Calls, want); !ok {
		managedGateFail(t, check, own, fmt.Sprintf(
			"the traced call chain never reaches %s, which is the handler %s calls at the revision this context pins. The chain that came back:\n%s",
			want, process, callLines(traced.Calls)))
	}
	if leaked, ok := reaches(traced.Calls, absent); ok {
		managedGateFail(t, check, own, fmt.Sprintf(
			"the traced call chain reaches %s (%s -> %s at %s:%d), which is %s's handler and not this context's: the trace was answered from another revision's graph. The chain that came back:\n%s",
			absent, leaked.Caller, leaked.Callee, leaked.Path, leaked.Line, other, callLines(traced.Calls)))
	}
}

// searchIn and traceIn ask one managed context one question.
//
// They are the managed gate's own, rather than the vacmcp methods the v0.1.0
// gate uses, because a failure has to be reported against a record: those quote
// the configuration file a managed server does not have, and point at the
// static fixture's index and graph to reproduce with.
// repository narrows the search to one member, as get_code and trace_calls'
// own repository argument does; left blank, the context is searched exactly
// as before repository existed, so every caller of this outside the new
// multi-member gate is unaffected.
func searchIn(t *testing.T, v vacmcp, check int, contextID, repository, query string) searchResult {
	t.Helper()

	args := map[string]any{"context": contextID, "query": query}
	if repository != "" {
		args["repository"] = repository
	}
	raw, isError := v.call(t, "search_code", args)
	if isError {
		managedGateFail(t, check, contextID, fmt.Sprintf(
			"search_code(context=%s, repository=%s, query=%s) refused to answer, so this check never got to run:\n%s", contextID, repository, query, raw))
		t.FailNow()
	}

	var found searchResult
	if err := json.Unmarshal([]byte(raw), &found); err != nil {
		t.Fatalf("decoding the search_code result: %v\n%s", err, raw)
	}
	return found
}

// traceIn walks one context's graph outwards from symbol. Three levels is
// deeper than the demo repository goes, so nothing is missed for being too far.
func traceIn(t *testing.T, v vacmcp, check int, contextID, symbol string) traceResult {
	t.Helper()

	raw, isError := v.call(t, "trace_calls", map[string]any{
		"context": contextID, "symbol": symbol, "direction": "callees", "depth": 3,
	})
	if isError {
		managedGateFail(t, check, contextID, fmt.Sprintf(
			"trace_calls(context=%s, symbol=%s, callees, depth 3) refused to answer, so this check never got to run:\n%s", contextID, symbol, raw))
		t.FailNow()
	}

	var traced traceResult
	if err := json.Unmarshal([]byte(raw), &traced); err != nil {
		t.Fatalf("decoding the trace_calls result: %v\n%s", err, raw)
	}
	return traced
}

// managedGateFail reports one check of the managed gate as failed.
//
// It names the context whose answer was wrong rather than a configuration file,
// because in managed mode there is no file anyone wrote: what to inspect is the
// record, and `context status` is what prints it.
func managedGateFail(t *testing.T, check int, contextID, detail string) {
	t.Helper()
	t.Errorf(`
RELEASE GATE FAIL — doc-1 §15 check %d of 4, through a managed context: %s
  context:   %s
  happened:  %s
  reproduce: vacmcp context status %s --data-dir DIR    # the revision, search ref and graph this context resolves to
doc-1 §15: any single cross-version contamination is a Release FAIL.`,
		check, gateChecks[check].isolation, contextID, detail, contextID)
}

// installation returns the built binary and an empty data directory for it,
// skipping the test where the engines a context is built with are not
// installed.
//
// The graphs go on the way out. A data directory is a temporary directory the
// test framework removes, but a graph is not in it: it lives in CBM's own
// store, and left behind every run would add one nothing will ever name again.
func installation(t *testing.T) (binary, data string) {
	t.Helper()
	for _, engine := range []string{"zoekt-git-index", "zoekt-webserver", cbmCommand} {
		if _, err := exec.LookPath(engine); err != nil {
			t.Skipf("%s is not on PATH, see CONTRIBUTING.md: %v", engine, err)
		}
	}

	binary, data = build(t), t.TempDir()
	t.Cleanup(func() { discardGraphs(t, binary, data) })
	return binary, data
}

// remoteRepository returns a bare clone of the versioned demo repository: the
// remote a user would `repo add`, standing on disk rather than on a network.
//
// It is a clone and not the fixture itself because this repository gets a
// commit pushed into it, and the fixture is shared with every other test in
// this module.
func remoteRepository(t *testing.T) string {
	t.Helper()
	remote := filepath.Join(t.TempDir(), "remote.git")
	mustGit(t, "clone", "--bare", "--quiet", demorepo.Generate(t), remote)
	return remote
}

// advanceRemote moves branch forward in the remote by one commit carrying
// release/v2's source, and returns the commit it now points at.
//
// It goes through a clone, a commit and a push, which is what a remote moving
// actually is — nothing here writes into another repository's object database
// by hand. The new content is taken from the fixture's own release/v2 rather
// than written out again here, so the moved branch really does carry the source
// the gate's symbols come from.
func advanceRemote(t *testing.T, remote, branch string) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	mustGit(t, "clone", "--quiet", "--branch", branch, remote, work)
	mustGit(t, "-C", work, "checkout", "origin/"+demorepo.V2, "--", "handler.go", "processor.go")
	// The caller's global git configuration decides neither the identity nor
	// whether this commit is signed or hooked.
	mustGit(t, "-C", work,
		"-c", "user.name=vacmcp gate", "-c", "user.email=gate@example.invalid", "-c", "commit.gpgsign=false",
		"commit", "--no-verify", "-q", "-m", "Switch "+branch+" to the v2 handler")
	mustGit(t, "-C", work, "push", "--quiet", "origin", branch)
	return gitOut(t, "-C", work, "rev-parse", "HEAD")
}

// managedPin is one repository/ref pair `context create` pins, positional the
// way the CLI's --repo/--ref flags are.
type managedPin struct{ repo, ref string }

// createContext runs `vacmcp context create` with one --repo/--ref pair per
// pin and returns the first line's revision, having reached READY.
//
// It takes pins rather than a single repository baked in, because a managed
// context is not always over one: `context create` accepts --repo/--ref
// repeated for a context over several (cmd/vacmcp's multirepo_integration_test.go
// is where that path is driven end to end). Every caller in this package
// still asks for one repository — managedRepository, this file's own — and
// reads back the one line createContext(t, binary, data, id, managedPin{managedRepository, ref})
// prints for it; a caller pinning more than one repository would read the
// rest itself with contextRecord.
func createContext(t *testing.T, binary, data, id string, pins ...managedPin) string {
	t.Helper()
	args := []string{"context", "create", id}
	for _, p := range pins {
		args = append(args, "--repo", p.repo, "--ref", p.ref)
	}
	out := vacmcpRun(t, binary, data, args...)

	// One row per repository the context names (printMembers), so a
	// single-repository create — every caller in this file — is the first and
	// only line: id, state, repository, revision.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	fields := strings.Split(lines[0], "\t")
	if len(fields) != 4 || fields[0] != id {
		t.Fatalf("context create %s printed %q, want the id, state, repository and revision", id, out)
	}
	if fields[1] != "READY" {
		t.Fatalf("context create %s left it in state %s, want READY: a context the query plane will not serve", id, fields[1])
	}
	return fields[3]
}

// contextRevision reads back the revision `vacmcp context status` reports, which
// is the record and not a value this test kept from the create.
func contextRevision(t *testing.T, binary, data, id string) string {
	t.Helper()
	for line := range strings.SplitSeq(vacmcpRun(t, binary, data, "context", "status", id), "\n") {
		if after, found := strings.CutPrefix(line, "revision"); found {
			return strings.TrimSpace(after)
		}
	}
	t.Fatalf("context status %s reported no revision", id)
	return ""
}

// serveManaged starts the binary as `vacmcp serve --managed` over STDIO against
// a real Zoekt web server over the index the contexts built, and connects a
// client to it.
//
// The server runs for the rest of the test: these two gates never stop one,
// where managed_snapshot_gate_test.go does, so the stop [startManaged] returns
// is left to the cleanup it registers.
func serveManaged(t *testing.T, binary, data string) vacmcp {
	t.Helper()
	v, _ := startManaged(t, binary, data)
	return v
}

// discardGraphs removes every context the data directory still has, which is
// what takes their graphs out of CBM's store.
//
// It goes through `context remove` rather than deleting the graphs directly:
// that is the command a user has for this, so a run that leaves graphs behind
// is a real failure of it rather than of a shortcut this test took.
func discardGraphs(t *testing.T, binary, data string) {
	t.Helper()
	// `context list` prints one row per member, not one per context — a
	// multi-member context's id repeats, once per repository it names. Each
	// id is only removed once: a second `context remove` of the same id has
	// nothing left to remove and would report CONTEXT_NOT_FOUND for a
	// context this loop already took care of, not a graph left behind.
	removed := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(runOut(t, binary, data, "context", "list")), "\n") {
		id, _, found := strings.Cut(line, "\t")
		if !found || removed[id] {
			continue
		}
		removed[id] = true
		if out, err := exec.Command(binary, "context", "remove", id, "--data-dir", data).CombinedOutput(); err != nil {
			t.Errorf("cleanup: vacmcp context remove %s left its graph in CBM's store: %v\n%s", id, err, out)
		}
	}
}

// vacmcpRun runs one management-plane command of the built binary against the
// data directory and returns what it printed.
func vacmcpRun(t *testing.T, binary, data string, args ...string) string {
	t.Helper()
	out, err := exec.Command(binary, append(args, "--data-dir", data)...).CombinedOutput()
	if err != nil {
		t.Fatalf("vacmcp %s --data-dir %s: %v\n%s", strings.Join(args, " "), data, err, out)
	}
	return string(out)
}

// runOut is vacmcpRun for a cleanup, where a failure is reported and the rest
// of the tidying still runs.
func runOut(t *testing.T, binary, data string, args ...string) string {
	t.Helper()
	out, err := exec.Command(binary, append(args, "--data-dir", data)...).CombinedOutput()
	if err != nil {
		t.Errorf("cleanup: vacmcp %s: %v\n%s", strings.Join(args, " "), err, out)
		return ""
	}
	return string(out)
}

func mustGit(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func gitOut(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}
