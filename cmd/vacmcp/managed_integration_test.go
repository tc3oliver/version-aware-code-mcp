//go:build integration

package main

// Managed serve is tested through the CLI a release ships and against the real
// engines: a data directory built by the real `repo add` and `context create`,
// a real Zoekt web server over the index those built, and the server started as
// `vacmcp serve --managed` over STDIO. What is being checked — that the query
// plane serves the READY contexts and nothing else, and that serving reaches no
// network — is a property of the whole path, and a stub anywhere on it would be
// the part under test answering for itself.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tc3oliver/version-aware-code-mcp/internal/demorepo"
	"github.com/tc3oliver/version-aware-code-mcp/managed"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// TestServeManagedServesExactlyTheReadyContexts is AC #1 and AC #2, end to end
// against real git, a real Zoekt index and a real graph engine.
//
// The data directory holds two READY contexts over one repository, each with a
// token the other version does not have, and one context whose graph could not
// be built. `serve --managed` then has to serve the first two and answer for
// the third exactly as it answers for a context that was never managed at all.
//
// AC #2 is checked by what the server ran rather than by reading the code: git
// is a recording shim first on the server's PATH, so every git the serving
// process invoked is in a log, and none of them may be a clone or a fetch.
func TestServeManagedServesExactlyTheReadyContexts(t *testing.T) {
	cbmOrSkip(t)
	data, alpha, beta := indexedRepository(t)
	t.Cleanup(func() { discardGraphs(t, data) })

	// A third context that gets as far as the graph and stops there, so the
	// data directory holds a context the query plane must not serve.
	t.Run("a context whose graph cannot be built", func(t *testing.T) {
		t.Setenv("PATH", brokenCBM(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
		if _, err := contextRun(t, data, "create", "ctx-broken", "--repo", "demo", "--ref", "main"); err == nil {
			t.Fatal("context create with a failing graph engine returned nil, want an error")
		}
	})
	if got := contextRecord(t, data, "ctx-broken").State; got != managed.ContextFailed {
		t.Fatalf("ctx-broken is in state %q, want %s", got, managed.ContextFailed)
	}

	session, gitLog := serveManaged(t, data, demorepo.StartZoekt(t, filepath.Join(data, "zoekt")))

	// AC #1, through list_contexts: the served registry is the READY records
	// and nothing else.
	if got, want := listedContexts(t, session), []string{alpha.ID, beta.ID}; !slices.Equal(got, want) {
		t.Errorf("list_contexts returned %v, want exactly the READY contexts %v", got, want)
	}

	// AC #1, through the search plane: each context finds its own version's
	// token and not the other's, which is the isolation the generated
	// configuration has to carry through.
	for _, c := range []struct {
		context string
		token   string
		want    bool
	}{
		{alpha.ID, alphaToken, true},
		{alpha.ID, betaToken, false},
		{beta.ID, betaToken, true},
		{beta.ID, alphaToken, false},
	} {
		if got := searchFinds(t, session, c.context, c.token); got != c.want {
			t.Errorf("search_code(%s, %s) found something = %v, want %v", c.context, c.token, got, c.want)
		}
	}

	// The other two tools answer in managed mode as well, which is what says the
	// generated configuration wired up the clone and the graph engine and not
	// only the search endpoint.
	read := callTool(t, session, "get_code", map[string]any{"context": alpha.ID, "path": "alpha.go", "start_line": 1, "end_line": 3})
	if read.IsError {
		t.Errorf("get_code in %s: %s", alpha.ID, resultText(t, read))
	} else if got := resultText(t, read); !strings.Contains(got, alphaToken) {
		t.Errorf("get_code in %s returned %s, want the source of that version", alpha.ID, got)
	}
	// A trace goes through codebase-memory-mcp, so what is asked of it here is
	// that it reached this context's own graph. Whether that graph holds the
	// symbol is CBM's answer to give; an unreachable engine or a context the
	// server does not know are not.
	traced := callTool(t, session, "trace_calls", map[string]any{"context": alpha.ID, "symbol": alphaToken, "direction": "callees", "depth": 1})
	if traced.IsError {
		if code := errorCode(t, traced); code == vacerr.GraphProviderUnavailable || code == vacerr.ContextNotFound {
			t.Errorf("trace_calls in %s = %q, want it to reach that context's graph: %s", alpha.ID, code, resultText(t, traced))
		}
	}

	// AC #1's other half: a FAILED context is not a context that fails
	// differently, it is a context that is not there. The comparison is against
	// an id that really was never managed.
	for _, id := range []string{"ctx-broken", "never-managed"} {
		res := callTool(t, session, "search_code", map[string]any{"context": id, "query": alphaToken})
		if !res.IsError {
			t.Fatalf("search_code in %s succeeded, want an error result", id)
		}
		if got := errorCode(t, res); got != vacerr.ContextNotFound {
			t.Errorf("search_code in %s = %q, want %q", id, got, vacerr.ContextNotFound)
		}
	}

	// AC #2: the serving process ran git — the resolver checks every revision
	// against the clone — and none of it reached a remote.
	ran := gitRuns(t, gitLog)
	if len(ran) == 0 {
		t.Fatal("the serving process ran no git at all, so the check below would pass for the wrong reason")
	}
	for _, invocation := range ran {
		for _, forbidden := range []string{"clone", "fetch", "remote", "ls-remote", "push", "pull"} {
			if slices.Contains(strings.Fields(invocation), forbidden) {
				t.Errorf("serve --managed ran `git %s`, want the query plane to reach no remote", invocation)
			}
		}
	}
	t.Logf("git run by serve --managed:\n%s", strings.Join(ran, "\n"))
}

// serveManaged runs the CLI as `vacmcp serve --managed` over STDIO and returns
// a client session on it, along with the path of the log every git the server
// runs is recorded in.
func serveManaged(t *testing.T, dataDir, zoektURL string) (*mcp.ClientSession, string) {
	t.Helper()

	gitLog := filepath.Join(t.TempDir(), "git.log")
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		serveManagedEnv+"="+dataDir,
		serveManagedZoektEnv+"="+zoektURL,
		"PATH="+recordingGit(t, gitLog)+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	cmd.Stderr = os.Stderr

	client := mcp.NewClient(&mcp.Implementation{Name: "vacmcp-test", Version: version}, nil)
	session, err := client.Connect(t.Context(), &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, gitLog
}

// recordingGit returns a directory holding a git that appends its arguments to
// logPath and then runs the real one.
//
// It changes nothing about what git does, which is what makes the log evidence
// rather than a different experiment: the server under test still resolves
// revisions against the clone exactly as it would in production, and every
// invocation it made is readable afterwards.
func recordingGit(t *testing.T, logPath string) string {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("git is not on PATH: %v", err)
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + logPath + "\"\nexec \"" + real + "\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return dir
}

// gitRuns returns one line per git the recorded process ran. A log that is not
// there is no runs, which is a legitimate outcome the caller decides about.
func gitRuns(t *testing.T, logPath string) []string {
	t.Helper()
	body, err := os.ReadFile(logPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading %s: %v", logPath, err)
	}
	var runs []string
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			runs = append(runs, line)
		}
	}
	return runs
}

// listedContexts returns the ids list_contexts reports, in the order it reports
// them.
func listedContexts(t *testing.T, session *mcp.ClientSession) []string {
	t.Helper()
	res := callTool(t, session, "list_contexts", nil)
	if res.IsError {
		t.Fatalf("list_contexts reported an error result: %v", res.Content)
	}

	var payload struct {
		Contexts []struct {
			ID string `json:"id"`
		} `json:"contexts"`
	}
	if err := json.Unmarshal([]byte(resultText(t, res)), &payload); err != nil {
		t.Fatalf("decoding list_contexts: %v", err)
	}
	ids := make([]string, 0, len(payload.Contexts))
	for _, c := range payload.Contexts {
		ids = append(ids, c.ID)
	}
	return ids
}

// searchFinds reports whether search_code finds token in the named context.
func searchFinds(t *testing.T, session *mcp.ClientSession, contextID, token string) bool {
	t.Helper()
	res := callTool(t, session, "search_code", map[string]any{"context": contextID, "query": token})
	if res.IsError {
		t.Fatalf("search_code(%s, %s) reported an error result: %s", contextID, token, resultText(t, res))
	}

	var payload struct {
		Matches []struct {
			Path string `json:"path"`
		} `json:"matches"`
	}
	if err := json.Unmarshal([]byte(resultText(t, res)), &payload); err != nil {
		t.Fatalf("decoding search_code: %v", err)
	}
	return len(payload.Matches) > 0
}

func callTool(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("tools/call %s: %v", name, err)
	}
	return res
}

// errorCode returns the vacerr code an error result carries.
func errorCode(t *testing.T, res *mcp.CallToolResult) vacerr.Code {
	t.Helper()
	var payload struct {
		Error struct {
			Code vacerr.Code `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(resultText(t, res)), &payload); err != nil {
		t.Fatalf("decoding an error result: %v", err)
	}
	return payload.Error.Code
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatal("the result carries no content")
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("result content = %#v, want text", res.Content[0])
	}
	return text.Text
}
