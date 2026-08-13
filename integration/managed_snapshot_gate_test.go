package integration

// decision-6's guarantee, asked of a real managed server: what a `vacmcp serve
// --managed` reads when it starts is what it serves until it stops, and nothing
// takes those contexts apart underneath it.
//
// The two tests below are the two halves of that. One holds the refusal — the
// commands that would change a served context fail closed while the server
// runs, the server keeps answering throughout, and the change goes through once
// it has stopped. The other holds the exception: `repo sync` runs beside a
// server and is not refused, because a fetch moves no pinned revision, and that
// is asserted against a remote that really moved rather than assumed.
//
// Nothing is simulated here either. The server is the built binary over STDIO,
// the search is a real zoekt-webserver, the graph is real codebase-memory-mcp,
// and the management commands are separate processes of the same binary, which
// is the only arrangement in which the lock they take against each other means
// anything.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tc3oliver/version-aware-code-mcp/internal/demorepo"
)

// TestManagedSnapshotIsFixedForTheLifeOfTheServer is decision-6 end to end, and
// the scenario TASK-40 asks for: a running server, the four commands that would
// change what it serves refused, everything it was serving still answering,
// then the server stopped, the same command succeeding, and a new server that
// reflects the change.
func TestManagedSnapshotIsFixedForTheLifeOfTheServer(t *testing.T) {
	binary, data := installation(t)
	remote := remoteRepository(t)

	vacmcpRun(t, binary, data, "repo", "add", managedRepository, "--url", remote)
	pinned := createContext(t, binary, data, v1, demorepo.V1)

	served, stop := startManaged(t, binary, data)

	// Every command that changes an artifact or a record is refused, and told
	// so in terms of the server rather than in terms of a lock.
	for _, command := range [][]string{
		{"context", "remove", v1},
		{"context", "create", v2, "--repo", managedRepository, "--ref", demorepo.V2},
		{"context", "retry", v1},
		{"repo", "remove", managedRepository},
	} {
		name := strings.Join(command[:2], " ")
		out := vacmcpRefused(t, binary, data, command...)
		for _, want := range []string{"INVALID_ARGUMENT", name, "managed server is running", "serve --managed", "start the server again"} {
			if !strings.Contains(out, want) {
				t.Errorf("`vacmcp %s` while a managed server runs printed:\n%s\nwant it to mention %q", strings.Join(command, " "), out, want)
			}
		}
	}

	// Refused means nothing happened, not that it happened and was reported:
	// the record is still there, still pinned where it was, and still READY.
	if got := contextRevision(t, binary, data, v1); got != pinned {
		t.Errorf("context %s pins %s after the refused commands, want %s unchanged", v1, got, pinned)
	}
	if listed := vacmcpRun(t, binary, data, "context", "list"); !strings.Contains(listed, v1) || !strings.Contains(listed, "READY") {
		t.Errorf("context list after the refused commands:\n%s\nwant %s still READY", listed, v1)
	}

	// And the server that refused them is still serving all of it, which is
	// the property the refusal exists to keep: search, graph and source, each
	// stamped with the revision this context pins.
	assertStillServing(t, served, v1, pinned)

	// Stopped, the same commands go through.
	stop()
	vacmcpRun(t, binary, data, "context", "remove", v1)
	moved := createContext(t, binary, data, v2, demorepo.V2)
	if moved == pinned {
		t.Fatalf("context %s pinned %s, the same commit as %s: the two contexts below would not differ", v2, moved, v1)
	}

	listed := vacmcpRun(t, binary, data, "context", "list")
	if strings.Contains(listed, v1) || !strings.Contains(listed, v2) {
		t.Fatalf("context list after the server stopped:\n%s\nwant %s gone and %s there", listed, v1, v2)
	}

	// A new server, and with it a new snapshot: the registry it serves is the
	// records as they are now, not as they were when the first one started.
	restarted, _ := startManaged(t, binary, data)

	var registry struct {
		Contexts []contextStamp `json:"contexts"`
	}
	answers(t, restarted, "list_contexts", map[string]any{}, &registry)
	if len(registry.Contexts) != 1 || registry.Contexts[0].ID != v2 || registry.Contexts[0].Revision != moved {
		t.Fatalf("the restarted server lists %+v, want only %s at %s", registry.Contexts, v2, moved)
	}

	// The removed context is gone from it, not merely unlisted.
	if _, isError := restarted.call(t, "search_code", map[string]any{"context": v1, "query": legacyHandler}); !isError {
		t.Errorf("search_code(context=%s) on the restarted server answered, want the removed context refused", v1)
	}
	// And the created one answers out of its own revision.
	var found searchResult
	answers(t, restarted, "search_code", map[string]any{"context": v2, "query": newHandler}, &found)
	if len(found.Matches) == 0 {
		t.Errorf("search_code(context=%s, query=%s) on the restarted server returned no match, want the handler the revision it pins contains", v2, newHandler)
	}
	if found.Context.Revision != moved {
		t.Errorf("search_code(context=%s) answered stamped %s, want %s", v2, found.Context.Revision, moved)
	}
}

// TestRepoSyncRunsBesideARunningManagedServer is TASK-40's AC #3, measured
// rather than reasoned.
//
// `repo sync` is deliberately not one of the commands refused above: it fetches
// remote refs into the clone and writes its own repository record, and touches
// no context record, no worktree, no search ref and no graph — so a running
// server's snapshot has nothing in it for a fetch to invalidate. That is a
// claim about what the command does not do, and the only way to hold it is to
// run it against a server that is up and ask the server afterwards.
//
// The remote is moved first, so the fetch really brings a new commit in, and
// the branch it moves is the one the running context was pinned on, carrying
// the other release's source. If a sync could reach a served context at all,
// this is the arrangement in which it would show.
func TestRepoSyncRunsBesideARunningManagedServer(t *testing.T) {
	binary, data := installation(t)
	remote := remoteRepository(t)

	vacmcpRun(t, binary, data, "repo", "add", managedRepository, "--url", remote)
	pinned := createContext(t, binary, data, v1, demorepo.V1)

	moved := advanceRemote(t, remote, demorepo.V1)
	if moved == pinned {
		t.Fatal("the remote's release/v1 did not move, so the sync below would fetch nothing")
	}

	served, _ := startManaged(t, binary, data)

	// Not refused: the command succeeds while the server is up. vacmcpRun
	// fails the test if it does not.
	vacmcpRun(t, binary, data, "repo", "sync", managedRepository)

	// And it really fetched, so what follows is asserted after a sync that did
	// something rather than after one that quietly did nothing.
	clone := filepath.Join(data, "repos", managedRepository)
	if got := gitOut(t, "-C", clone, "rev-parse", "--verify", moved+"^{commit}"); got != moved {
		t.Fatalf("the clone resolves %s to %q after `repo sync`, want the moved commit fetched", moved, got)
	}

	// The server that was running through all of it is untouched: the record it
	// serves still pins the old commit, and every tool still answers out of it.
	if got := contextRevision(t, binary, data, v1); got != pinned {
		t.Errorf("context %s pins %s after a `repo sync` beside a running server, want %s", v1, got, pinned)
	}
	assertStillServing(t, served, v1, pinned)
}

// assertStillServing holds one context of a running server to the revision it
// pins, across all three of the tools that read source: the search index, the
// graph and the file itself. Each is asked for both handlers, so an answer that
// came from the other version is a failure rather than an absence.
func assertStillServing(t *testing.T, v vacmcp, contextID, revision string) {
	t.Helper()

	var found searchResult
	answers(t, v, "search_code", map[string]any{"context": contextID, "query": legacyHandler}, &found)
	if len(found.Matches) == 0 {
		t.Errorf("search_code(context=%s, query=%s) returned no match, want the handler the revision it pins calls", contextID, legacyHandler)
	}
	if found.Context.Revision != revision {
		t.Errorf("search_code(context=%s) answered stamped %s, want %s", contextID, found.Context.Revision, revision)
	}

	var leaked searchResult
	answers(t, v, "search_code", map[string]any{"context": contextID, "query": newHandler}, &leaked)
	if len(leaked.Matches) != 0 {
		t.Errorf("search_code(context=%s, query=%s) returned %d matches, want 0: that handler is on the other release\n%s",
			contextID, newHandler, len(leaked.Matches), matchLines(leaked.Matches))
	}

	var traced traceResult
	answers(t, v, "trace_calls", map[string]any{
		"context": contextID, "symbol": process, "direction": "callees", "depth": 3,
	}, &traced)
	if _, ok := reaches(traced.Calls, legacyHandler); !ok {
		t.Errorf("the traced chain of %s in %s never reaches %s. The chain that came back:\n%s",
			process, contextID, legacyHandler, callLines(traced.Calls))
	}
	if _, ok := reaches(traced.Calls, newHandler); ok {
		t.Errorf("the traced chain of %s in %s reaches %s, which is the other release's handler. The chain that came back:\n%s",
			process, contextID, newHandler, callLines(traced.Calls))
	}

	// handler.go is where the two releases differ, so reading it is what says
	// which revision the source came out of.
	var read struct {
		Context contextStamp `json:"context"`
		Content string       `json:"content"`
	}
	answers(t, v, "get_code", map[string]any{
		"context": contextID, "path": "handler.go", "start_line": 1, "end_line": 6,
	}, &read)
	if !strings.Contains(read.Content, legacyHandler) || strings.Contains(read.Content, newHandler) {
		t.Errorf("get_code(context=%s, handler.go) returned:\n%s\nwant the source containing %s and not %s", contextID, read.Content, legacyHandler, newHandler)
	}
	if read.Context.Revision != revision {
		t.Errorf("get_code(context=%s) answered stamped %s, want %s", contextID, read.Context.Revision, revision)
	}
}

// startManaged runs `vacmcp serve --managed` over STDIO against a real Zoekt
// over the index the contexts built, connects a client to it, and returns the
// session together with the stop.
//
// stop waits for the process to be gone rather than for the connection to be
// closed, which is the difference this file turns on: the lock that refuses a
// management command is released by the kernel when the server exits, so a test
// that stops a server and then runs one has to mean exited. Closing the client
// session is what the MCP SDK does that for — it closes stdin and waits on the
// command — and it is run once however often stop is called, so a test may stop
// a server explicitly and still leave the cleanup in place.
func startManaged(t *testing.T, binary, data string) (vacmcp, func()) {
	t.Helper()

	cmd := exec.Command(binary, "serve", "--stdio", "--managed",
		"--data-dir", data,
		"--zoekt-url", demorepo.StartZoekt(t, filepath.Join(data, "zoekt")),
		"--cbm-command", cbmCommand)
	// The protocol stream owns stdout, so anything the server has to say about
	// itself arrives on stderr and belongs in this test's output.
	cmd.Stderr = os.Stderr

	client := mcp.NewClient(&mcp.Implementation{Name: "vacmcp-managed-release-gate", Version: "0.0.0-test"}, nil)
	session, err := client.Connect(t.Context(), &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connecting to `vacmcp serve --stdio --managed --data-dir %s`: %v", data, err)
	}

	var once sync.Once
	stop := func() { once.Do(func() { _ = session.Close() }) }
	t.Cleanup(stop)

	// cfg is what a failure with no answer of its own quotes from, and a managed
	// server has no configuration file to quote: every check reads the stamp off
	// the answer it got.
	return vacmcp{session: session}, stop
}

// answers asks one tool of a running server and decodes what came back into
// out. A tool that refused is a failed test here rather than a value to
// inspect: these tests are about a server that is still serving, so an error
// result is the failure itself.
func answers(t *testing.T, v vacmcp, tool string, args map[string]any, out any) {
	t.Helper()

	raw, isError := v.call(t, tool, args)
	if isError {
		t.Fatalf("%s(%v) on a running managed server refused to answer:\n%s", tool, args, raw)
	}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		t.Fatalf("decoding the %s result: %v\n%s", tool, err, raw)
	}
}

// vacmcpRefused runs one management command that is expected to fail and
// returns everything it printed. A command that succeeded is the failure: it
// means the change it was refused went through.
func vacmcpRefused(t *testing.T, binary, data string, args ...string) string {
	t.Helper()
	out, err := exec.Command(binary, append(args, "--data-dir", data)...).CombinedOutput()
	if err == nil {
		t.Fatalf("vacmcp %s --data-dir %s succeeded while a managed server was running, want it refused:\n%s",
			strings.Join(args, " "), data, out)
	}
	return string(out)
}
