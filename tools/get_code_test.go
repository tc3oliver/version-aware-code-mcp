package tools

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	gitadapter "github.com/tc3oliver/version-aware-code-mcp/adapters/git"
	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/evidence"
	"github.com/tc3oliver/version-aware-code-mcp/internal/demorepo"
	"github.com/tc3oliver/version-aware-code-mcp/resolver"
	"github.com/tc3oliver/version-aware-code-mcp/server"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// getCodeWire is a successful result as a client receives it: doc-1's context
// block and evidence array, plus this tool's payload. Decoding into it is what
// pins the field names — a wrong json tag leaves its field empty and the
// assertion on it fails.
type getCodeWire struct {
	Context struct {
		ID         string `json:"id"`
		Repository string `json:"repository"`
		Branch     string `json:"branch"`
		Revision   string `json:"revision"`
	} `json:"context"`
	Evidence  []evidence.Evidence `json:"evidence"`
	Path      string              `json:"path"`
	StartLine int                 `json:"start_line"`
	EndLine   int                 `json:"end_line"`
	Content   string              `json:"content"`
}

// AC #1: the same path, read in two contexts over one repository, comes back as
// each context's own version — and every result says which version that was.
//
// The demo repository's working tree is on main throughout, so neither answer
// can be coming from the checkout.
func TestGetCodeReturnsTheContextsOwnVersion(t *testing.T) {
	cfg := getCodeConfig(t)
	session := getCodeSession(t, cfg)

	// processor.go lines 4 to 6 are the body of Process() on every branch:
	// LegacyHandler on release/v1, NewHandler on release/v2.
	v1, rawV1 := getCodeCall(t, session, "demo-v1", "processor.go", 4, 6)
	v2, rawV2 := getCodeCall(t, session, "demo-v2", "processor.go", 4, 6)
	t.Logf("demo-v1 -> %s", rawV1)
	t.Logf("demo-v2 -> %s", rawV2)

	if v1.Content == v2.Content {
		t.Fatalf("both contexts returned the same content, version isolation is broken:\n%s", v1.Content)
	}
	if !strings.Contains(v1.Content, "LegacyHandler") || strings.Contains(v1.Content, "NewHandler") {
		t.Errorf("demo-v1 content = %q, want the LegacyHandler call", v1.Content)
	}
	if !strings.Contains(v2.Content, "NewHandler") || strings.Contains(v2.Content, "LegacyHandler") {
		t.Errorf("demo-v2 content = %q, want the NewHandler call", v2.Content)
	}

	// The whole record doc-1 requires: repository, branch, revision, path and
	// line range travelling with the content.
	for id, got := range map[string]getCodeWire{"demo-v1": v1, "demo-v2": v2} {
		codeCtx := cfg.Contexts[id]
		if got.Context.ID != id {
			t.Errorf("%s: context.id = %q, want %q", id, got.Context.ID, id)
		}
		if got.Context.Repository != codeCtx.Repository {
			t.Errorf("%s: context.repository = %q, want %q", id, got.Context.Repository, codeCtx.Repository)
		}
		if got.Context.Branch != codeCtx.Branch {
			t.Errorf("%s: context.branch = %q, want %q", id, got.Context.Branch, codeCtx.Branch)
		}
		if got.Context.Revision != codeCtx.Revision {
			t.Errorf("%s: context.revision = %q, want the revision it was read at, %q", id, got.Context.Revision, codeCtx.Revision)
		}
		if got.Path != "processor.go" || got.StartLine != 4 || got.EndLine != 6 {
			t.Errorf("%s: located at %s:%d-%d, want processor.go:4-6", id, got.Path, got.StartLine, got.EndLine)
		}
		// The Tool Contract's evidence array, citing where the content is.
		want := evidence.At("processor.go", 4, 6, "")
		if len(got.Evidence) != 1 || got.Evidence[0] != want {
			t.Errorf("%s: evidence = %+v, want exactly %+v", id, got.Evidence, want)
		}
	}

	// GraphRef is the CBM project backing a context: internal, and a tool's
	// output is the only place it could leak from.
	for _, raw := range []string{rawV1, rawV2} {
		if strings.Contains(raw, "graph") {
			t.Errorf("get_code leaked the graph reference: %s", raw)
		}
	}
}

// TestGetCodeReportsTheRevisionItRead covers a context declaring its revision as
// a branch name rather than a SHA. The result reports the commit the bytes came
// from, so the caller can check the claim instead of trusting the spelling in
// the configuration.
func TestGetCodeReportsTheRevisionItRead(t *testing.T) {
	cfg := getCodeConfig(t)
	full := cfg.Contexts["demo-v2"].Revision
	cfg.Contexts["demo-v2"] = vacctx.CodeContext{
		Repository: "example/demo",
		Branch:     demorepo.V2,
		Revision:   demorepo.V2, // a branch name, which is a legal revision to declare
		GraphRef:   "demo-v2-graph",
	}

	got, raw := getCodeCall(t, getCodeSession(t, cfg), "demo-v2", "processor.go", 4, 6)
	if got.Context.Revision != full {
		t.Errorf("context.revision = %q, want the full SHA %q it was read at", got.Context.Revision, full)
	}
	if !strings.Contains(got.Content, "NewHandler") {
		t.Errorf("content = %q, want the v2 body", got.Content)
	}
	t.Logf("declared revision %q -> %s", demorepo.V2, raw)
}

// AC #2: a path the revision does not have is an explicit error. Empty content
// would be indistinguishable from an empty file, and the caller could not tell
// it had asked for something that is not there.
func TestGetCodeMissingPathIsAnError(t *testing.T) {
	session := getCodeSession(t, getCodeConfig(t))

	// version.go is on the release branches but not on main, so the same path is
	// readable in one context and absent in another.
	if _, raw := getCodeCall(t, session, "demo-v1", "version.go", 1, 1); !strings.Contains(raw, "package demo") {
		t.Fatalf("version.go should be readable at %s, got %s", demorepo.V1, raw)
	}

	for name, args := range map[string]struct {
		codeCtx, path string
		start, end    int
	}{
		"path on another branch": {"demo-main", "version.go", 1, 1},
		"path in no revision":    {"demo-v1", "does/not/exist.go", 1, 1},
		"line range past EOF":    {"demo-v1", "processor.go", 1, 999},
		"inverted line range":    {"demo-v1", "processor.go", 6, 2},
		"path escaping the repo": {"demo-v1", "../../etc/passwd", 1, 1},
	} {
		t.Run(name, func(t *testing.T) {
			vErr, raw := getCodeError(t, session, args.codeCtx, args.path, args.start, args.end)
			if vErr.Code != vacerr.InvalidArgument {
				t.Errorf("code = %q, want INVALID_ARGUMENT", vErr.Code)
			}
			if strings.Contains(raw, `"content"`) {
				t.Errorf("error result carries content: %s", raw)
			}
			t.Logf("%s -> %s", name, raw)
		})
	}
}

// AC #3: source that is not the declared revision fails the request. This is the
// invariant the server exists for, so the check is made on what reaches the
// client: an error envelope, no content to fall back on, and no way to read it
// as a warning.
//
// Reading the object database makes the content correct by construction, so the
// mismatch has to be provoked where that guarantee can fail: a repository that
// records the file at the declared revision but can no longer produce its bytes.
// The only content left for that path on the machine is the working tree's,
// which is on a different commit.
func TestGetCodeSourceMismatchFailsTheRequest(t *testing.T) {
	repo := getCodeDivergedRepo(t)
	cfg := &config.Config{
		Repositories: map[string]config.Repository{"example/demo": {Path: repo.path}},
		Contexts: map[string]vacctx.CodeContext{
			"app-v1": {Repository: "example/demo", Branch: "release/1.x", Revision: repo.first, GraphRef: "app-v1-graph"},
		},
	}

	vErr, raw := getCodeError(t, getCodeSession(t, cfg), "app-v1", "process.go", 1, 3)
	t.Logf("SOURCE_MISMATCH wire = %s", raw)

	if vErr.Code != vacerr.SourceMismatch {
		t.Fatalf("code = %q, want SOURCE_MISMATCH", vErr.Code)
	}
	if vErr.Details["declared_revision"] != repo.first {
		t.Errorf("details[declared_revision] = %v, want %s", vErr.Details["declared_revision"], repo.first)
	}
	if vErr.Details["actual_revision"] != repo.head {
		t.Errorf("details[actual_revision] = %v, want %s", vErr.Details["actual_revision"], repo.head)
	}
	// Fail closed all the way to the wire: no source content of either version,
	// and nothing the client could mistake for an answer.
	for _, leak := range []string{"LegacyHandler", "NewHandler", `"content"`, `"evidence"`} {
		if strings.Contains(raw, leak) {
			t.Errorf("mismatch result contains %s: %s", leak, raw)
		}
	}
}

// AC #4: an unknown context is CONTEXT_NOT_FOUND. No fuzzy match, no case
// folding, no "the only other context" — the requested path exists in the
// configured contexts, so an implementation that guessed would return content.
func TestGetCodeUnknownContextIsContextNotFound(t *testing.T) {
	session := getCodeSession(t, getCodeConfig(t))

	for _, id := range []string{"", "   ", "demo-v3", "DEMO-V1", " demo-v1", "demo", "demo-v1 "} {
		t.Run("context="+id, func(t *testing.T) {
			vErr, raw := getCodeError(t, session, id, "processor.go", 4, 6)
			if vErr.Code != vacerr.ContextNotFound {
				t.Errorf("code = %q, want CONTEXT_NOT_FOUND", vErr.Code)
			}
			if vErr.Details["context"] != id {
				t.Errorf("details[context] = %v, want the id that was asked for, %q", vErr.Details["context"], id)
			}
			for _, leak := range []string{"LegacyHandler", "NewHandler", `"content"`} {
				if strings.Contains(raw, leak) {
					t.Errorf("unknown context answered with %s: %s", leak, raw)
				}
			}
			t.Logf("context=%q -> %s", id, raw)
		})
	}
}

// TestGetCodeIsDiscoverable checks what an agent sees before it ever calls the
// tool: that it is registered, and that all four arguments are required — a call
// missing one is rejected by the SDK rather than reaching the handler with a
// zero value it would have to guess about.
func TestGetCodeIsDiscoverable(t *testing.T) {
	session := getCodeSession(t, getCodeConfig(t))

	tools, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	var tool *mcp.Tool
	for _, candidate := range tools.Tools {
		if candidate.Name == "get_code" {
			tool = candidate
		}
	}
	if tool == nil {
		t.Fatalf("tools/list returned %d tools, none named get_code", len(tools.Tools))
	}

	raw, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatalf("marshal input schema: %v", err)
	}
	var schema struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("decode input schema %s: %v", raw, err)
	}
	for _, field := range []string{"context", "path", "start_line", "end_line"} {
		if _, ok := schema.Properties[field]; !ok {
			t.Errorf("input schema has no %s property: %s", field, raw)
		}
		if !slices.Contains(schema.Required, field) {
			t.Errorf("input schema does not require %s: %s", field, raw)
		}
	}
	t.Logf("get_code input schema = %s", raw)

	// A call missing an argument never reaches the provider.
	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "get_code",
		Arguments: map[string]any{"context": "demo-v1", "path": "processor.go"},
	})
	if err != nil {
		t.Fatalf("tools/call get_code: %v", err)
	}
	if !res.IsError {
		t.Errorf("a call without a line range succeeded: %v", res.Content)
	}
}

// getCodeConfig is the demo repository wired up as three contexts over one
// clone: doc-1's first success criterion, two versions of a repository
// coexisting, plus main so a path present in only some revisions can be asked
// for in one that does not have it.
//
// Every revision is resolved from the generated fixture, never written down.
func getCodeConfig(t *testing.T) *config.Config {
	t.Helper()
	repo := demorepo.Generate(t)
	cfg := &config.Config{
		Repositories: map[string]config.Repository{"example/demo": {Path: repo}},
		Contexts:     map[string]vacctx.CodeContext{},
	}
	for id, branch := range map[string]string{"demo-main": demorepo.Main, "demo-v1": demorepo.V1, "demo-v2": demorepo.V2} {
		cfg.Contexts[id] = vacctx.CodeContext{
			Repository: "example/demo",
			Branch:     branch,
			Revision:   demorepo.Revision(t, repo, branch),
			GraphRef:   id + "-graph",
		}
	}
	return cfg
}

// getCodeSession serves get_code over stateless Streamable HTTP and connects a
// client to it, so every assertion is made on what came back over a real wire
// rather than on a Go value. The engine behind the tool is wired to the real
// resolver and the real git source adapter: nothing between the client and the
// repository is a stub. get_code reaches neither of the other two providers, so
// the engine is built without them and one that tried would fail the test.
func getCodeSession(t *testing.T, cfg *config.Config) *mcp.ClientSession {
	t.Helper()

	srv := server.New(testVersion)
	AddGetCode(srv, engine.New(resolver.New(cfg), nil, nil, gitadapter.New(cfg)))

	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true},
	))
	t.Cleanup(httpServer.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "vacmcp-test", Version: testVersion}, nil)
	clientSession, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: httpServer.URL}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	return clientSession
}

// getCodeRaw calls the tool and returns the result together with the JSON text
// the client received. The text is read rather than the decoded structured
// content because the assertions are about what is and is not on the wire.
func getCodeRaw(t *testing.T, session *mcp.ClientSession, codeCtx, path string, start, end int) (*mcp.CallToolResult, string) {
	t.Helper()

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "get_code",
		Arguments: map[string]any{"context": codeCtx, "path": path, "start_line": start, "end_line": end},
	})
	if err != nil {
		t.Fatalf("tools/call get_code: %v", err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("result carries %d content blocks, want 1", len(res.Content))
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("result content = %#v, want text", res.Content[0])
	}
	return res, text.Text
}

// getCodeCall calls the tool and requires it to have succeeded.
func getCodeCall(t *testing.T, session *mcp.ClientSession, codeCtx, path string, start, end int) (getCodeWire, string) {
	t.Helper()

	res, raw := getCodeRaw(t, session, codeCtx, path, start, end)
	if res.IsError {
		t.Fatalf("get_code(%s, %s) failed: %s", codeCtx, path, raw)
	}
	var got getCodeWire
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return got, raw
}

// getCodeError calls the tool and requires it to have failed with the unified
// error envelope, carrying nothing else.
func getCodeError(t *testing.T, session *mcp.ClientSession, codeCtx, path string, start, end int) (*vacerr.Error, string) {
	t.Helper()

	res, raw := getCodeRaw(t, session, codeCtx, path, start, end)
	if !res.IsError {
		t.Fatalf("get_code(%s, %s) succeeded, want an error: %s", codeCtx, path, raw)
	}
	if res.StructuredContent != nil {
		t.Errorf("error result carries structured content: %v", res.StructuredContent)
	}

	var envelope struct {
		Error struct {
			Code    vacerr.Code    `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("error result is not the unified envelope: %s (%v)", raw, err)
	}
	if envelope.Error.Code == "" || strings.TrimSpace(envelope.Error.Message) == "" {
		t.Fatalf("error result is missing its code or message: %s", raw)
	}
	return vacerr.New(envelope.Error.Code, envelope.Error.Message, envelope.Error.Details), raw
}

// getCodeDiverged is a repository whose working tree is on head while first,
// the revision a context declares, can no longer be served from the object
// database.
type getCodeDiverged struct {
	path        string
	first, head string
}

// getCodeDivergedRepo builds that repository: two commits whose process.go
// differs, with the first commit's blob removed. The commit and its tree stay
// intact, which is the state a partial clone is in for a file it never fetched.
// The git configuration files are pinned away so the machine's own settings
// cannot change what the test observes.
func getCodeDivergedRepo(t *testing.T) getCodeDiverged {
	t.Helper()
	r := getCodeDiverged{path: t.TempDir()}

	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", r.path}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	commit := func(body, message string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(r.path, "process.go"), []byte(body), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		run("add", ".")
		run("-c", "user.name=vacmcp", "-c", "user.email=vacmcp@example.invalid", "commit", "--no-verify", "-m", message)
		return run("rev-parse", "HEAD")
	}

	run("init")
	r.first = commit("package demo\n\nfunc Process() { LegacyHandler() }\n", "v1")
	r.head = commit("package demo\n\nfunc Process() { NewHandler() }\n", "v2")
	if r.first == r.head {
		t.Fatalf("both commits are %s, the repository never moved", r.head)
	}

	oid := run("rev-parse", r.first+":process.go")
	object := filepath.Join(r.path, ".git", "objects", oid[:2], oid[2:])
	// A fresh repository writes loose objects; nothing here packs them. If that
	// ever stops holding, the mismatch is no longer being provoked and the test
	// has to say so rather than quietly pass.
	if _, err := os.Stat(object); err != nil {
		t.Fatalf("blob %s is not a loose object, the mismatch cannot be provoked: %v", oid, err)
	}
	if err := os.Remove(object); err != nil {
		t.Fatalf("removing %s: %v", object, err)
	}
	return r
}
