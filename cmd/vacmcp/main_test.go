package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tc3oliver/version-aware-code-mcp/managed"
	"github.com/tc3oliver/version-aware-code-mcp/store"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// serveConfigEnv makes this test binary run `vacmcp serve --stdio` against the
// configuration file it names, instead of running tests. That way the tools an
// agent can reach are the ones the real CLI registered, over a real pipe.
const serveConfigEnv = "VACMCP_TEST_SERVE_CONFIG"

// The same for `vacmcp serve --managed`: the data directory to serve, the
// Zoekt web server the test brought up for it, and the graph engine to run.
// managed_test.go is where the first two are set.
const (
	serveManagedEnv      = "VACMCP_TEST_SERVE_MANAGED"
	serveManagedZoektEnv = "VACMCP_TEST_SERVE_MANAGED_ZOEKT"
	serveManagedCBMEnv   = "VACMCP_TEST_SERVE_MANAGED_CBM"
)

// cbmStubEnv makes this test binary stand in for codebase-memory-mcp instead of
// running tests, and names the file the stub writes when the session it is
// serving is closed.
//
// It is set on the serve child, which passes it on by being the parent of the
// stub: the graph adapter starts the command it was configured with in this
// environment. Both processes therefore read it, for the two halves of the same
// question — the stub writes the marker, and the serve child checks it is there
// before exiting.
const cbmStubEnv = "VACMCP_TEST_CBM_STUB"

// discardPrepared checks and then takes down the installation the real-engine
// tests share, and is set only in the build those tests are in. It cannot be a
// t.Cleanup: the installation outlives the test that built it, and its graphs
// live in CBM's own store rather than in a temporary directory anything else
// would remove. It reports a run that left the installation changed, which is
// a failure of the run rather than of whichever test read it afterwards.
var discardPrepared func() error

func TestMain(m *testing.M) {
	if path, ok := os.LookupEnv(serveConfigEnv); ok {
		args := []string{"serve", "--stdio"}
		if path != "" {
			args = append(args, "--config", path)
		}
		os.Exit(serveAsChild(args))
	}
	if dataDir, ok := os.LookupEnv(serveManagedEnv); ok {
		args := []string{"serve", "--stdio", "--managed", "--data-dir", dataDir}
		if url := os.Getenv(serveManagedZoektEnv); url != "" {
			args = append(args, "--zoekt-url", url)
		}
		if command := os.Getenv(serveManagedCBMEnv); command != "" {
			args = append(args, "--cbm-command", command)
		}
		os.Exit(serveAsChild(args))
	}
	// After both serve branches, because a serve child has this set too: it is
	// what it passes on to the stub it starts, and it must serve rather than
	// stand in for CBM itself.
	if marker, ok := os.LookupEnv(cbmStubEnv); ok {
		os.Exit(cbmStub(marker))
	}
	code := m.Run()
	if discardPrepared != nil {
		if err := discardPrepared(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			code = 1
		}
	}
	os.Exit(code)
}

// serveAsChild runs one `vacmcp serve` in this process and returns the exit
// code it should leave behind.
//
// The serve variables are unset first because the stub CBM is this same binary:
// the graph adapter starts it with this process's environment, so one left set
// would have the stub serve MCP as vacmcp instead of standing in for CBM.
//
// The marker check afterwards is what makes the shutdown tests below
// discriminate. serve's deferred Engine.Close() closes the CBM session, the
// stub writes the marker as its input closes, and the transport's Close waits
// for the stub to exit — so at the moment serve returns the marker is there if
// and only if that close ran. Without it the stub is still running, because
// this process is still holding its stdin, and the missing marker leaves a
// non-zero exit for the client to read off its own transport's Close.
func serveAsChild(args []string) int {
	for _, name := range []string{serveConfigEnv, serveManagedEnv} {
		_ = os.Unsetenv(name)
	}
	// Nothing may go to stdout but the protocol stream, which is why the
	// output writer here is stderr.
	if err := run(args, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	marker, ok := os.LookupEnv(cbmStubEnv)
	if !ok {
		return 0
	}
	if _, err := os.Stat(marker); err != nil {
		fmt.Fprintf(os.Stderr, "serve returned with the CBM session still open: %v\n", err)
		return 1
	}
	return 0
}

// cbmStub runs this binary as a stand-in for codebase-memory-mcp: the MCP
// server on STDIO the graph adapter keeps a session on, which writes marker
// once that session is closed.
//
// It answers search_graph, the call every trace starts with, with an empty
// graph. That the answer is empty does not matter; that it is an answer does:
// a call failing at the transport would have the adapter retire the session
// and close it there and then, which is the shutdown these tests exist to
// observe happening for the wrong reason. An empty graph comes back as
// SYMBOL_NOT_FOUND instead, which is what the caller asserts it received.
//
// Arguments mean the adapter fell back to `codebase-memory-mcp cli <tool>`,
// which it does only when it has no session. There is nothing to stand in for
// then, and serving MCP down a pipe nobody is speaking it on would hang the
// call, so that is a failed CBM instead.
func cbmStub(marker string) int {
	if len(os.Args) > 1 {
		fmt.Fprintf(os.Stderr, "cbm stub: no cli mode, called with %v\n", os.Args[1:])
		return 1
	}

	srv := mcp.NewServer(&mcp.Implementation{Name: "cbm-stub", Version: "0"}, nil)
	srv.AddTool(
		&mcp.Tool{Name: "search_graph", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `{"cols":[],"groups":[]}`}}}, nil
		},
	)
	if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// TestServeExposesTheConfiguredContexts is the wiring check: `serve --config`
// has to hand the loaded contexts to list_contexts, so an agent connecting to
// the server sees the same versions `vacmcp contexts` prints.
func TestServeExposesTheConfiguredContexts(t *testing.T) {
	got := callListContexts(t, write(t, twoContexts))

	want := []map[string]string{
		{"id": "app-v1", "repository": "example/backend", "branch": "release/1.x", "revision": "8af31e2"},
		{"id": "app-v2", "repository": "example/backend", "branch": "release/2.x", "revision": "94cb821"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("list_contexts returned\n%v\nwant\n%v", got, want)
	}
}

// TestServeWithoutConfigExposesNoContexts pins the other half of --config being
// optional: a server started without one serves an empty list, not an error.
func TestServeWithoutConfigExposesNoContexts(t *testing.T) {
	if got := callListContexts(t, ""); len(got) != 0 {
		t.Errorf("list_contexts returned %v, want no contexts", got)
	}
}

// TestServeStaticStopsCleanlyWhenTheClientDisconnects is TASK-51's shutdown
// path for `serve --config`: closing the transport must make the process exit
// on its own, having closed the engine it built, rather than hanging until the
// SDK's client falls back to SIGTERM.
func TestServeStaticStopsCleanlyWhenTheClientDisconnects(t *testing.T) {
	repo := sourceRepo(t)
	body := fmt.Sprintf(`
providers:
  cbm:
    command: %q
repositories:
  example/backend:
    path: %q
contexts:
  app-v1:
    repository: example/backend
    branch: main
    revision: %q
    graph_ref: backend-v1
`, os.Args[0], repo, gitOut(t, "-C", repo, "rev-parse", "HEAD"))

	disconnectAfterATrace(t, "app-v1", serveConfigEnv+"="+write(t, body))
}

// TestServeManagedStopsCleanlyWhenTheClientDisconnects is the managed-mode
// twin of TestServeStaticStopsCleanlyWhenTheClientDisconnects. It needs no real
// Zoekt and no real CBM: the one context is READY over a repository this test
// cloned itself, and its graph engine is the stub.
func TestServeManagedStopsCleanlyWhenTheClientDisconnects(t *testing.T) {
	data := t.TempDir()
	source := sourceRepo(t)
	if _, err := repoRun(t, data, "add", "demo", "--url", source); err != nil {
		t.Fatalf("repo add: %v", err)
	}
	revision := gitOut(t, "-C", source, "rev-parse", "HEAD")
	if err := openStore(t, data).PutContext(store.Context{
		ID:         "app",
		Repository: "demo",
		Branch:     "vacmcp/app-" + revision[:12],
		Revision:   revision,
		GraphRef:   "vacmcp-demo-app-" + revision[:12],
		State:      managed.ContextReady,
	}); err != nil {
		t.Fatalf("PutContext: %v", err)
	}

	disconnectAfterATrace(t, "app", serveManagedEnv+"="+data, serveManagedCBMEnv+"="+os.Args[0])
}

// disconnectAfterATrace runs the CLI as an MCP server over STDIO in the
// environment env describes, traces once in the context id names so the server
// really opens a CBM session, and then disconnects.
//
// Two things are asserted about the disconnect, and it takes both to say the
// shutdown is clean. That [mcp.CommandTransport]'s Close returns nil is the
// server having exited by itself: Close closes stdin and waits, and reports
// what Wait said only when the process went before it had to be signalled. That
// the exit status was zero is serve having closed the engine on its way out —
// the child checks the stub CBM was really shut down and exits non-zero when it
// was not, so a serve that returned while leaving the session open arrives here
// as a failed Close rather than as a passing test.
func disconnectAfterATrace(t *testing.T, id string, env ...string) {
	t.Helper()

	marker := filepath.Join(t.TempDir(), "cbm-session-closed")
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), append(env, cbmStubEnv+"="+marker)...)
	cmd.Stderr = os.Stderr

	client := mcp.NewClient(&mcp.Implementation{Name: "vacmcp-test", Version: version}, nil)
	session, err := client.Connect(t.Context(), &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	// Nothing is spawned until a query needs it, so this is what starts the CBM
	// session the shutdown then has to close. The stub's empty graph makes the
	// answer SYMBOL_NOT_FOUND, and that answer arriving is how this test knows
	// there is an open session at all: without one there would be nothing to
	// close, and the assertion below would pass for the wrong reason.
	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "trace_calls",
		Arguments: map[string]any{"context": id, "symbol": "nothing", "direction": "callees", "depth": 1},
	})
	if err != nil {
		t.Fatalf("tools/call trace_calls: %v", err)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("result content = %#v, want text", res.Content[0])
	}
	if !res.IsError || !strings.Contains(text.Text, string(vacerr.SymbolNotFound)) {
		t.Fatalf("trace_calls answered %q, want %s from the stub CBM", text.Text, vacerr.SymbolNotFound)
	}

	if err := session.Close(); err != nil {
		t.Errorf("session.Close = %v, want serve to exit on its own once its deferred Engine.Close() ran", err)
	}
}

// callListContexts runs the CLI as an MCP server over STDIO and calls
// list_contexts on it, returning the contexts it listed.
func callListContexts(t *testing.T, configPath string) []map[string]string {
	t.Helper()

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), serveConfigEnv+"="+configPath)
	cmd.Stderr = os.Stderr

	client := mcp.NewClient(&mcp.Implementation{Name: "vacmcp-test", Version: version}, nil)
	session, err := client.Connect(t.Context(), &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_contexts"})
	if err != nil {
		t.Fatalf("tools/call list_contexts: %v", err)
	}
	if res.IsError {
		t.Fatalf("list_contexts reported an error result: %v", res.Content)
	}

	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("result content = %#v, want text", res.Content[0])
	}
	var payload struct {
		Contexts []map[string]string `json:"contexts"`
	}
	if err := json.Unmarshal([]byte(text.Text), &payload); err != nil {
		t.Fatalf("decode %s: %v", text.Text, err)
	}
	return payload.Contexts
}

const twoContexts = `
repositories:
  example/backend:
    path: /srv/repos/backend
contexts:
  app-v2:
    repository: example/backend
    branch: release/2.x
    revision: 94cb821
    graph_ref: backend-v2
  app-v1:
    repository: example/backend
    branch: release/1.x
    revision: 8af31e2
    graph_ref: backend-v1
`

// missingRevision drops a required field, which is the shape of configuration
// error `vacmcp validate` exists to catch.
const missingRevision = `
repositories:
  example/backend:
    path: /srv/repos/backend
contexts:
  app-v1:
    repository: example/backend
    branch: release/1.x
    graph_ref: backend-v1
`

// write puts body in a temporary file and returns its path.
func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestValidateAcceptsTheExampleConfig(t *testing.T) {
	var out bytes.Buffer
	// The shipped example is the configuration users copy, so it is the one
	// worth checking really validates.
	if err := run([]string{"validate", "--config", "../../config/example.yaml"}, &out); err != nil {
		t.Fatalf("validate example.yaml: %v", err)
	}
	if !strings.Contains(out.String(), "ok, 2 contexts") {
		t.Errorf("validate printed %q, want it to report 2 contexts", out.String())
	}
}

func TestValidateRejectsIncompleteConfig(t *testing.T) {
	var out bytes.Buffer
	err := run([]string{"validate", "--config", write(t, missingRevision)}, &out)
	if err == nil {
		t.Fatal("validate returned nil, want an error so the CLI exits non-zero")
	}

	// The error has to reach the user as config produced it: a validate that
	// invented its own error model would hide the vacerr code.
	var vErr *vacerr.Error
	if !errors.As(err, &vErr) {
		t.Fatalf("validate error = %v (%T), want *vacerr.Error", err, err)
	}
	if vErr.Code != vacerr.InvalidArgument {
		t.Errorf("validate error code = %q, want %q", vErr.Code, vacerr.InvalidArgument)
	}
	if !strings.Contains(vErr.Message, "revision") {
		t.Errorf("validate message = %q, want it to name the missing field", vErr.Message)
	}
	if out.Len() != 0 {
		t.Errorf("validate wrote %q to stdout on failure, want nothing", out.String())
	}
}

func TestValidateRequiresConfigPath(t *testing.T) {
	if err := run([]string{"validate"}, &bytes.Buffer{}); err == nil {
		t.Fatal("validate without --config returned nil, want an error")
	}
}

func TestContextsListsTheConfiguredContexts(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"contexts", "--config", write(t, twoContexts)}, &out); err != nil {
		t.Fatalf("contexts: %v", err)
	}

	want := "app-v1\texample/backend\trelease/1.x\t8af31e2\n" +
		"app-v2\texample/backend\trelease/2.x\t94cb821\n"
	if out.String() != want {
		t.Errorf("contexts output =\n%q\nwant\n%q", out.String(), want)
	}
}

func TestVersionPrintsAVersion(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"version"}, &out); err != nil {
		t.Fatalf("version: %v", err)
	}
	if strings.TrimSpace(out.String()) == "" {
		t.Error("version printed nothing, want a version string")
	}
}

func TestUnknownCommandFails(t *testing.T) {
	for _, args := range [][]string{{}, {"diagnose"}} {
		if err := run(args, &bytes.Buffer{}); err == nil {
			t.Errorf("run(%q) returned nil, want an error", args)
		}
	}
}
