// Package integration runs vacmcp the way an agent runs it — the binary a
// release ships, started on a configuration file, talking to a real Zoekt, a
// real codebase-memory-mcp and real git checkouts.
//
// It holds doc-1 §15's version-correctness release gate. Version isolation is
// why this project exists, so these four checks are not ordinary tests: any
// single cross-version contamination is a Release FAIL.
//
// The gate is asked through the built binary rather than through tools wired up
// in-process, because the binary is what ships. Loading the configuration,
// registering the four tools in cmd/vacmcp, the STDIO transport and the choice
// of adapter are all on the path an agent hits, and a mistake there — the wrong
// adapter, a tool never registered, a configuration silently ignored — would
// leak versions to every real client while an in-process gate stayed green. The
// price is one `go build` per run.
package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/internal/demorepo"
)

// The contexts testdata/prepare-fixture.sh configures, one per release branch of
// the versioned demo repository, and the symbols that tell the two versions
// apart: Process calls LegacyHandler on release/v1 and NewHandler on release/v2.
const (
	v1 = "demo-v1"
	v2 = "demo-v2"

	process       = "Process"
	legacyHandler = "LegacyHandler"
	newHandler    = "NewHandler"
)

// TestVersionCorrectnessReleaseGate is doc-1 §15's release gate.
//
// One repository, one query, one traced symbol; the only thing that differs
// between the halves is which context asked, and that alone decides the answer.
// Each check is its own subtest, so a red build names the isolation that broke
// and a single check can be re-run on its own:
//
//	go test -run 'TestVersionCorrectnessReleaseGate/check_3' ./integration/...
func TestVersionCorrectnessReleaseGate(t *testing.T) {
	vacmcp := connect(t)

	// Check 1. The contamination check proper: a symbol that exists only on the
	// other release must not be visible from this context.
	t.Run("check 1: demo-v1 does not see NewHandler", func(t *testing.T) {
		found := vacmcp.searchCode(t, 1, v1, newHandler)
		if len(found.Matches) != 0 {
			gateFail(t, 1, found.Context, newHandler, fmt.Sprintf(
				"search_code returned %d matches, want 0. %s is written only on %s, so each line below reached %s from a version it never asked for:\n%s",
				len(found.Matches), newHandler, demorepo.V2, v1, matchLines(found.Matches)))
		}
	})

	// Check 2. The other half of the same fact, and what keeps check 1 honest: a
	// context that answers nothing can never be caught leaking.
	t.Run("check 2: demo-v2 does see NewHandler", func(t *testing.T) {
		found := vacmcp.searchCode(t, 2, v2, newHandler)
		if len(found.Matches) == 0 {
			gateFail(t, 2, found.Context, newHandler, fmt.Sprintf(
				"search_code returned 0 matches, want at least one: %s does contain %s. This context is searching another version's branch, or an index that does not carry this one — and it empties check 1 of meaning, because a context that answers nothing passes it for free.",
				demorepo.V2, newHandler))
		}
	})

	// Check 3. The same isolation in the graph: the trace has to come from the
	// CBM project this context names, and only that one.
	t.Run("check 3: demo-v1 traces Process to LegacyHandler", func(t *testing.T) {
		traced := vacmcp.traceCalls(t, 3, v1, process)
		assertTracedHandler(t, 3, traced, legacyHandler, newHandler, demorepo.V1, demorepo.V2)
	})

	// Check 4. The mirror image, so the graph isolation cannot be one context
	// simply answering the same thing for everything.
	t.Run("check 4: demo-v2 traces Process to NewHandler", func(t *testing.T) {
		traced := vacmcp.traceCalls(t, 4, v2, process)
		assertTracedHandler(t, 4, traced, newHandler, legacyHandler, demorepo.V2, demorepo.V1)
	})
}

// assertTracedHandler holds one traced call chain to its own version: it has to
// reach the handler this release calls, and it must not reach the other
// release's, which is what a trace answered from the wrong graph looks like.
func assertTracedHandler(t *testing.T, check int, traced traceResult, want, absent, branch, otherBranch string) {
	t.Helper()

	if _, ok := reaches(traced.Calls, want); !ok {
		gateFail(t, check, traced.Context, process, fmt.Sprintf(
			"the traced call chain never reaches %s, which is the handler %s calls on %s. The chain that came back:\n%s",
			want, process, branch, callLines(traced.Calls)))
	}
	if leaked, ok := reaches(traced.Calls, absent); ok {
		gateFail(t, check, traced.Context, process, fmt.Sprintf(
			"the traced call chain reaches %s (%s -> %s at %s:%d), which is %s's handler and not this context's: the trace was answered from another version's graph. The chain that came back:\n%s",
			absent, leaked.Caller, leaked.Callee, leaked.Path, leaked.Line, otherBranch, callLines(traced.Calls)))
	}
}

// What each of doc-1 §15's four checks holds, and the command that answers the
// first question a red gate raises: is the fixture wrong, or is vacmcp wrong?
// Both are CONTRIBUTING.md's, run from the module root against the same index
// and the same graph the server just used.
var gateChecks = map[int]struct{ isolation, reproduce string }{
	1: {
		"search_code stays inside the branch its context names",
		"zoekt -index_dir testdata/fixture/zoekt-index 'NewHandler branch:release/v1'    # expect no matches",
	},
	2: {
		"search_code finds what the branch its context names does contain",
		"zoekt -index_dir testdata/fixture/zoekt-index 'NewHandler branch:release/v2'    # expect matches",
	},
	3: {
		"trace_calls answers from the graph its context names",
		"codebase-memory-mcp cli trace_path --project vacmcp-demo-v1 --function-name Process --direction outbound --depth 3    # expect LegacyHandler",
	},
	4: {
		"trace_calls answers from the graph its context names",
		"codebase-memory-mcp cli trace_path --project vacmcp-demo-v2 --function-name Process --direction outbound --depth 3    # expect NewHandler",
	},
}

// gateFail reports one check of the release gate as failed.
//
// It is written for the moment it fires: a red release build and nobody holding
// any context. "want 0, got 3" would not say which isolation broke, so the
// message names the check, the version context the answer came back stamped
// with, the symbol at issue, what the leak looked like line by line, and how to
// ask the engine the same question directly.
func gateFail(t *testing.T, check int, stamp contextStamp, symbol, detail string) {
	t.Helper()
	about := gateChecks[check]
	t.Errorf(`
RELEASE GATE FAIL — doc-1 §15, check %d of 4: %s
  context:   %s -> repository %s, branch %s, revision %s
  symbol:    %s
  happened:  %s
  reproduce: %s
doc-1 §15: any single cross-version contamination is a Release FAIL. v0.1.0 does not ship with this check red.`,
		check, about.isolation,
		stamp.ID, stamp.Repository, stamp.Branch, stamp.Revision,
		symbol, detail, about.reproduce)
}

// contextStamp is the version context every tool result carries, as a client
// reads it. A failure quotes the answer's own stamp rather than the
// configuration, because which version the server thought it was answering for
// is half of what a leak is.
type contextStamp struct {
	ID         string `json:"id"`
	Repository string `json:"repository"`
	Branch     string `json:"branch"`
	Revision   string `json:"revision"`
}

// searchResult and traceResult are the parts of the two tools' output the gate
// reads. The wire is the contract, so these are decoded from the JSON the
// client received rather than shared with the server's own types.
type (
	searchResult struct {
		Context contextStamp `json:"context"`
		Matches []match      `json:"matches"`
	}
	match struct {
		Path    string `json:"path"`
		Line    int    `json:"line"`
		Snippet string `json:"snippet"`
	}

	traceResult struct {
		Context contextStamp `json:"context"`
		Symbol  string       `json:"symbol"`
		Calls   []edge       `json:"calls"`
	}
	edge struct {
		Caller string `json:"caller"`
		Callee string `json:"callee"`
		Path   string `json:"path"`
		Line   int    `json:"line"`
	}
)

// vacmcp is a client session on a server the built binary is running, together
// with the configuration it was started on.
type vacmcp struct {
	session *mcp.ClientSession
	cfg     *config.Config
}

// connect builds the binary, starts it as an MCP server over STDIO against the
// prepared fixture and connects a client to it.
func connect(t *testing.T) vacmcp {
	t.Helper()
	fixture := demorepo.Prepared(t)

	cfg, err := config.Load(fixture.Config)
	if err != nil {
		t.Fatalf("loading the prepared configuration %s: %v", fixture.Config, err)
	}
	if _, err := exec.LookPath(cfg.Providers.CBM.Command); err != nil {
		t.Skipf("codebase-memory-mcp is not runnable at %q, see CONTRIBUTING.md: %v", cfg.Providers.CBM.Command, err)
	}

	binary := build(t)
	configPath := configPointedAt(t, fixture.Config, cfg.Providers.Zoekt.URL, demorepo.StartZoekt(t, fixture.ZoektIndex))

	cmd := exec.Command(binary, "serve", "--stdio", "--config", configPath)
	// The protocol stream owns stdout, so anything the server has to say about
	// itself arrives on stderr and belongs in this test's output.
	cmd.Stderr = os.Stderr

	client := mcp.NewClient(&mcp.Implementation{Name: "vacmcp-release-gate", Version: "0.0.0-test"}, nil)
	session, err := client.Connect(t.Context(), &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connecting to `vacmcp serve --stdio --config %s`: %v", configPath, err)
	}
	t.Cleanup(func() { _ = session.Close() })

	return vacmcp{session: session, cfg: cfg}
}

// build compiles the binary a release ships. The gate is only worth what it
// runs against, and that is this.
func build(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "vacmcp")
	if out, err := exec.Command("go", "build", "-o", binary, "../cmd/vacmcp").CombinedOutput(); err != nil {
		t.Fatalf("go build ../cmd/vacmcp: %v\n%s", err, out)
	}
	return binary
}

// configPointedAt copies the prepared configuration with its Zoekt URL rewritten
// to the server started for this run. The fixture names the port a long-running
// Zoekt would listen on, which the gate does not depend on being up.
func configPointedAt(t *testing.T, prepared, from, to string) string {
	t.Helper()

	body, err := os.ReadFile(prepared)
	if err != nil {
		t.Fatalf("reading the prepared configuration %s: %v", prepared, err)
	}
	rewritten := strings.Replace(string(body), from, to, 1)
	if rewritten == string(body) {
		t.Fatalf("%s does not contain the Zoekt URL %q it was loaded with, so the gate cannot point the server at its own engine", prepared, from)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(rewritten), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

// searchCode asks one context for one query and returns the answer.
func (v vacmcp) searchCode(t *testing.T, check int, contextID, query string) searchResult {
	t.Helper()

	raw, isError := v.call(t, "search_code", map[string]any{"context": contextID, "query": query})
	if isError {
		gateFail(t, check, v.stamp(contextID), query, fmt.Sprintf(
			"search_code(context=%s, query=%s) refused to answer, so this check never got to run:\n%s", contextID, query, raw))
		t.FailNow()
	}

	var found searchResult
	if err := json.Unmarshal([]byte(raw), &found); err != nil {
		t.Fatalf("decoding the search_code result: %v\n%s", err, raw)
	}
	return found
}

// traceCalls walks one context's graph outwards from symbol. Three levels is
// deeper than the demo repository goes, so nothing is missed for being too far.
func (v vacmcp) traceCalls(t *testing.T, check int, contextID, symbol string) traceResult {
	t.Helper()

	raw, isError := v.call(t, "trace_calls", map[string]any{
		"context": contextID, "symbol": symbol, "direction": "callees", "depth": 3,
	})
	if isError {
		gateFail(t, check, v.stamp(contextID), symbol, fmt.Sprintf(
			"trace_calls(context=%s, symbol=%s, callees, depth 3) refused to answer, so this check never got to run:\n%s", contextID, symbol, raw))
		t.FailNow()
	}

	var traced traceResult
	if err := json.Unmarshal([]byte(raw), &traced); err != nil {
		t.Fatalf("decoding the trace_calls result: %v\n%s", err, raw)
	}
	return traced
}

// call makes one tools/call and returns the text the client received, together
// with whether the server reported it as an error result.
func (v vacmcp) call(t *testing.T, name string, args map[string]any) (string, bool) {
	t.Helper()

	res, err := v.session.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("tools/call %s(%v): %v", name, args, err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("%s returned %d content blocks, want 1", name, len(res.Content))
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("%s returned %#v, want text", name, res.Content[0])
	}
	return text.Text, res.IsError
}

// stamp is the configured context, for a failure that has no answer of its own
// to quote.
func (v vacmcp) stamp(contextID string) contextStamp {
	codeCtx := v.cfg.Contexts[contextID]
	return contextStamp{
		ID:         contextID,
		Repository: codeCtx.Repository,
		Branch:     codeCtx.Branch,
		Revision:   codeCtx.Revision,
	}
}

// reaches reports whether the traced chain names symbol anywhere, and returns
// the call it was found on so a failure can point at the line it is written on.
func reaches(calls []edge, symbol string) (edge, bool) {
	for _, call := range calls {
		if strings.EqualFold(call.Caller, symbol) || strings.EqualFold(call.Callee, symbol) {
			return call, true
		}
	}
	return edge{}, false
}

// matchLines and callLines lay out what came back, one finding per line, so the
// failure shows the leak itself rather than a count of it.
func matchLines(matches []match) string {
	lines := make([]string, 0, len(matches))
	for _, m := range matches {
		lines = append(lines, fmt.Sprintf("             %s:%d  %s", m.Path, m.Line, strings.TrimSpace(m.Snippet)))
	}
	return strings.Join(lines, "\n")
}

func callLines(calls []edge) string {
	if len(calls) == 0 {
		return "             (empty: the trace found no calls at all)"
	}
	lines := make([]string, 0, len(calls))
	for _, call := range calls {
		lines = append(lines, fmt.Sprintf("             %s -> %s  %s:%d", call.Caller, call.Callee, call.Path, call.Line))
	}
	return strings.Join(lines, "\n")
}
