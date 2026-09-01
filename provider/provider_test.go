// Package provider_test is the evidence for the two things the interfaces
// promise: each one can be implemented on its own, and each method receives the
// CodeContext that scopes it to a version.
//
// A provider is handed one CodeContext and knows nothing of the workspace it
// came out of, which is why nothing here mentions one: whether the right member
// of the right context arrived is a fact about the engine that chose it, and it
// is checked there, in
// TestTheMemberOfTheNamedWorkspaceIsWhatReachesTheProvider. What this file
// pins is the other half — that whatever the engine chose arrives intact.
package provider_test

import (
	"context"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
)

// The three fakes are separate types on purpose: no shared struct, no shared
// embedding. Implementing one interface must not require the other two.
type searchOnly struct{ got vacctx.CodeContext }

func (s *searchOnly) Search(_ context.Context, codeCtx vacctx.CodeContext, _ provider.SearchQuery) ([]provider.SearchResult, error) {
	s.got = codeCtx
	return nil, nil
}

type graphOnly struct{ got vacctx.CodeContext }

func (g *graphOnly) TraceCalls(_ context.Context, codeCtx vacctx.CodeContext, _ provider.TraceRequest) (*provider.CallGraph, error) {
	g.got = codeCtx
	return &provider.CallGraph{}, nil
}

type sourceOnly struct{ got vacctx.CodeContext }

func (s *sourceOnly) Read(_ context.Context, codeCtx vacctx.CodeContext, _ string, _, _ int) (*provider.SourceContent, error) {
	s.got = codeCtx
	return &provider.SourceContent{}, nil
}

var (
	_ provider.SearchProvider = (*searchOnly)(nil)
	_ provider.GraphProvider  = (*graphOnly)(nil)
	_ provider.SourceProvider = (*sourceOnly)(nil)
)

var codeCtx = vacctx.CodeContext{
	ID:         "app-v2",
	Repository: "example/backend",
	Branch:     "release/2.x",
	Revision:   "94cb821",
	GraphRef:   "backend-v2",
}

func TestEveryMethodReceivesTheCodeContext(t *testing.T) {
	search, graph, source := &searchOnly{}, &graphOnly{}, &sourceOnly{}

	if _, err := search.Search(t.Context(), codeCtx, provider.SearchQuery{Query: "NewHandler"}); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if _, err := graph.TraceCalls(t.Context(), codeCtx, provider.TraceRequest{Symbol: "Process", Direction: provider.Callees, Depth: 2}); err != nil {
		t.Fatalf("TraceCalls() error = %v", err)
	}
	if _, err := source.Read(t.Context(), codeCtx, "internal/process.go", 12, 14); err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	for name, got := range map[string]vacctx.CodeContext{
		"Search":     search.got,
		"TraceCalls": graph.got,
		"Read":       source.got,
	} {
		if got != codeCtx {
			t.Errorf("%s received %+v, want %+v", name, got, codeCtx)
		}
	}
}
