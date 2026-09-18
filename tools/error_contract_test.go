package tools

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/internal/deadline"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/server"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// The error codes a client branches on, checked at the tier that answers a pull
// request rather than only at the one that needs a real Zoekt and a real CBM.
//
// A code that is exported and switchable-on but whose wire behaviour nothing
// asserts is a contract nobody is holding: the envelope could stop carrying it,
// or carry a different one, and every tag-free test would still pass.

// TestSearchCodeErrorMapping covers the two codes search_code can answer with.
// tools/search_code_test.go had no error case at all, so both came only from
// search_code_integration_test.go, behind the build tag ci-fast.yml does not
// build.
func TestSearchCodeErrorMapping(t *testing.T) {
	t.Run("context_not_found", func(t *testing.T) {
		srv := server.New(testVersion)
		AddSearchCode(srv, engine.New(tracedContexts{}, wiredSearch{}, nil, nil))

		failure, raw := failedCall(t, connectTo(t, srv), "search_code",
			map[string]any{"context": "no-such-context", "query": "Process"})
		if failure.Code != vacerr.ContextNotFound {
			t.Errorf("search_code on an unconfigured context failed with %s, want %s: %s",
				failure.Code, vacerr.ContextNotFound, raw)
		}
	})

	// An engine built without a search provider. The code says which capability
	// is missing rather than reporting an empty result, which would read as
	// "this version has no such code".
	t.Run("search_provider_unavailable", func(t *testing.T) {
		srv := server.New(testVersion)
		AddSearchCode(srv, engine.New(tracedContexts{}, nil, nil, nil))

		failure, raw := failedCall(t, connectTo(t, srv), "search_code",
			map[string]any{"context": "traced", "query": "Process"})
		if failure.Code != vacerr.SearchProviderUnavailable {
			t.Errorf("search_code with no search provider failed with %s, want %s: %s",
				failure.Code, vacerr.SearchProviderUnavailable, raw)
		}
	})
}

// TestRepositoryNotFoundReachesTheWire is the code with the most producers in the
// module — twenty-six of them — and it appeared in no test under tools/ at all,
// tag-free or tagged. It is what a query that needs source answers with when the
// server was built without a source provider, for the reason engine.go gives:
// the code set has no source equivalent, and adding one would change the public
// tool API.
//
// All three tools that read source are checked, because each one produces it
// from its own call site.
func TestRepositoryNotFoundReachesTheWire(t *testing.T) {
	// No source provider anywhere: the graph is present so trace_calls is
	// unaffected, which is what makes this about source rather than about an
	// empty engine.
	eng := engine.New(tracedContexts{}, wiredSearch{}, stubGraph{}, nil)

	srv := server.New(testVersion)
	AddGetCode(srv, eng)
	AddCompareCode(srv, eng)
	AddSearchHistory(srv, eng)
	session := connectTo(t, srv)

	cases := map[string]map[string]any{
		"get_code":       {"context": "traced", "path": "alpha.go", "start_line": 1, "end_line": 2},
		"compare_code":   {"from_context": "traced", "to_context": "traced", "path": "alpha.go"},
		"search_history": {"context": "traced"},
	}
	for tool, args := range cases {
		t.Run(tool, func(t *testing.T) {
			failure, raw := failedCall(t, session, tool, args)
			if failure.Code != vacerr.RepositoryNotFound {
				t.Errorf("%s with no source provider failed with %s, want %s: %s",
					tool, failure.Code, vacerr.RepositoryNotFound, raw)
			}
		})
	}
}

// TestSourceMismatchReachesTheWire is about the details rather than the code.
// get_code_test.go already reaches SOURCE_MISMATCH through a real repository;
// what nothing asserted is that the two revisions survive the envelope, and they
// are the answer — a caller compares them to see which side is stale, so an
// envelope that carried the code without them would leave the client unable to
// act on the failure.
func TestSourceMismatchReachesTheWire(t *testing.T) {
	srv := server.New(testVersion)
	AddGetCode(srv, engine.New(tracedContexts{}, nil, nil, mismatchingSource{}))

	failure, raw := failedCall(t, connectTo(t, srv), "get_code",
		map[string]any{"context": "traced", "path": "alpha.go", "start_line": 1, "end_line": 2})
	if failure.Code != vacerr.SourceMismatch {
		t.Fatalf("get_code failed with %s, want %s: %s", failure.Code, vacerr.SourceMismatch, raw)
	}
	// The two revisions the constructor puts there, which are what a caller
	// compares to see which side is stale.
	for _, want := range []string{"declared_revision", "actual_revision"} {
		if _, ok := failure.Details[want]; !ok {
			t.Errorf("the SOURCE_MISMATCH envelope carries %v, want it to name %s: %s",
				failure.Details, want, raw)
		}
	}
}

// mismatchingSource is a source backend that read the wrong revision, which is
// the one failure vacerr has a constructor of its own for.
type mismatchingSource struct{}

func (mismatchingSource) Read(_ context.Context, codeCtx vacctx.CodeContext, path string, _, _ int) (*provider.SourceContent, error) {
	return nil, vacerr.NewSourceMismatch(
		codeCtx.Revision,
		"0000000000000000000000000000000000000000",
		map[string]any{"context": codeCtx.ID, "path": path},
	)
}

// TestOperationTimeoutReachesTheWire is the contract half of the timeout work
// that exists before any adapter sets a budget: whatever produces the code, it
// has to arrive in doc-1's envelope with the three details a caller acts on.
//
// No adapter has a deadline yet, so the code is produced here the way one will
// produce it — through the helper, on a budget that has expired — rather than by
// a literal a real producer would not match.
func TestOperationTimeoutReachesTheWire(t *testing.T) {
	srv := server.New(testVersion)
	AddGetCode(srv, engine.New(tracedContexts{}, nil, nil, timingOutSource{}))

	failure, raw := failedCall(t, connectTo(t, srv), "get_code",
		map[string]any{"context": "traced", "path": "alpha.go", "start_line": 1, "end_line": 2})

	if failure.Code != vacerr.OperationTimeout {
		t.Fatalf("get_code failed with %s, want %s: %s", failure.Code, vacerr.OperationTimeout, raw)
	}
	for key, want := range map[string]any{
		"provider":   deadline.Git,
		"operation":  "read",
		"timeout_ms": float64(1),
	} {
		if failure.Details[key] != want {
			t.Errorf("details[%s] = %v, want %v: %s", key, failure.Details[key], want, raw)
		}
	}
	if len(failure.Details) != 3 {
		t.Errorf("details = %v, want exactly operation, provider and timeout_ms: %s", failure.Details, raw)
	}
}

// TestACallersCancellationIsNotAnOperationTimeout is the boundary, checked where
// it matters: at the tool layer, a caller's own context error must not arrive as
// this server's code.
//
// tools/errors.go hands anything that is not a *vacerr.Error back to the SDK, so
// what a client sees for these is an error result with no code at all — which is
// correct, and is what stops a cancellation from entering the vocabulary clients
// branch on.
func TestACallersCancellationIsNotAnOperationTimeout(t *testing.T) {
	for name, source := range map[string]provider.SourceProvider{
		"caller cancelled":       cancellingSource{err: context.Canceled},
		"caller deadline passed": cancellingSource{err: context.DeadlineExceeded},
	} {
		t.Run(name, func(t *testing.T) {
			srv := server.New(testVersion)
			AddGetCode(srv, engine.New(tracedContexts{}, nil, nil, source))

			res, err := connectTo(t, srv).CallTool(t.Context(), &mcp.CallToolParams{
				Name:      "get_code",
				Arguments: map[string]any{"context": "traced", "path": "alpha.go", "start_line": 1, "end_line": 2},
			})
			if err != nil {
				t.Fatalf("tools/call get_code: %v", err)
			}
			text, ok := res.Content[0].(*mcp.TextContent)
			if !ok {
				t.Fatalf("result content = %#v, want text", res.Content[0])
			}
			if strings.Contains(text.Text, string(vacerr.OperationTimeout)) {
				t.Errorf("a %s arrived as %s: %s", name, vacerr.OperationTimeout, text.Text)
			}
			// Nor as any other code: it is not this server's failure to classify.
			if strings.Contains(text.Text, `"code"`) {
				t.Errorf("a %s arrived carrying an error code: %s", name, text.Text)
			}
		})
	}
}

// timingOutSource is a source backend whose budget has already expired, which is
// what an adapter with a deadline will look like once one is added.
type timingOutSource struct{}

func (timingOutSource) Read(ctx context.Context, _ vacctx.CodeContext, _ string, _, _ int) (*provider.SourceContent, error) {
	budget, cancel := deadline.With(ctx, time.Millisecond, deadline.Git, "read")
	defer cancel()
	<-budget.Done()
	return nil, deadline.Ended(budget)
}

// cancellingSource fails with a caller's own context error, unwrapped, which is
// what an adapter does when the caller is the one that ended the call.
type cancellingSource struct{ err error }

func (s cancellingSource) Read(context.Context, vacctx.CodeContext, string, int, int) (*provider.SourceContent, error) {
	return nil, s.err
}

// TestContextAmbiguousHasNoProducer records the one code that is reserved rather
// than covered.
//
// CONTEXT_AMBIGUOUS is exported and documented, and nothing in the module
// produces it: there is no call site outside vacerr itself and its own test. A
// wire test for it would have to invent the producer it is asserting against,
// which would prove that the fake works.
//
// So this asserts the absence instead. It fails the day someone adds a producer,
// which is the day a wire test becomes possible and required — and it fails just
// as loudly if the code is deleted, which is the other decision worth taking
// deliberately.
func TestContextAmbiguousHasNoProducer(t *testing.T) {
	root := ".."
	var producers []string

	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if name := entry.Name(); name == ".git" || name == "testdata" || name == "backlog" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// vacerr declares it; declaring is not producing.
		if filepath.Base(filepath.Dir(path)) == "vacerr" {
			return nil
		}

		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "ContextAmbiguous" {
				return true
			}
			if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == "vacerr" {
				producers = append(producers, fset.Position(sel.Pos()).String())
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(producers) != 0 {
		t.Errorf("vacerr.ContextAmbiguous is now produced at %v — it is reserved today, so a producer means the wire behaviour needs a test beside the others in this file",
			producers)
	}
}

// failedCall calls a tool and requires the unified error envelope, with nothing
// beside it.
func failedCall(t *testing.T, session *mcp.ClientSession, tool string, args map[string]any) (*vacerr.Error, string) {
	t.Helper()

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("tools/call %s(%v): %v", tool, args, err)
	}
	if len(res.Content) == 0 {
		t.Fatalf("%s(%v) returned no content", tool, args)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("result content = %#v, want text", res.Content[0])
	}
	if !res.IsError {
		t.Fatalf("%s(%v) succeeded, want an error: %s", tool, args, text.Text)
	}
	if res.StructuredContent != nil {
		t.Errorf("%s error result carries structured content: %v", tool, res.StructuredContent)
	}

	var envelope struct {
		Error vacerr.Error `json:"error"`
	}
	if err := json.Unmarshal([]byte(text.Text), &envelope); err != nil {
		t.Fatalf("decode %s: %v", text.Text, err)
	}
	if envelope.Error.Code == "" {
		t.Fatalf("%s failed with no code in the envelope: %s", tool, text.Text)
	}
	return &envelope.Error, text.Text
}
