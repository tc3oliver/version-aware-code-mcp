// Command embed is a whole vacmcp query plane embedded in another program: the
// interfaces an embedder implements, the query, and the shutdown.
//
// The context source and the search provider below hold canned data, so this
// runs with no Zoekt, no codebase-memory-mcp and no checkout — what it
// demonstrates is the embedding contract, not the backends. A real embedding
// loads a config.Config, hands engine.New a resolver.New(cfg) as its context
// source and the adapters in adapters/zoekt, adapters/cbm and adapters/git as
// its providers; nothing else in this file changes.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// The two versions this program answers in. A real one reads them from a
// configuration file; the engine only ever sees them through the interface
// below.
var versions = demoContexts{
	{ID: "demo-v1", Repository: "demo", Branch: "release/v1", Revision: "1111111111111111111111111111111111111111", GraphRef: "demo-v1"},
	{ID: "demo-v2", Repository: "demo", Branch: "release/v2", Revision: "2222222222222222222222222222222222222222", GraphRef: "demo-v2"},
}

// demoContexts stands in for *resolver.Resolver. An engine.ContextSource is the
// two methods below and nothing more, so a program keeping its version scopes
// in a database, a service or — here — a slice implements it without touching
// this project's configuration format.
type demoContexts []vacctx.CodeContext

func (d demoContexts) Contexts() []vacctx.CodeContext { return d }

// An id that names no version is refused rather than guessed at: answering out
// of some other version is exactly the failure vacmcp exists to prevent.
func (d demoContexts) Resolve(_ context.Context, id string) (vacctx.CodeContext, error) {
	for _, codeCtx := range d {
		if codeCtx.ID == id {
			return codeCtx, nil
		}
	}
	return vacctx.CodeContext{}, vacerr.New(
		vacerr.ContextNotFound,
		"no context named "+id,
		map[string]any{"context": id},
	)
}

// What the stand-in index holds, per branch: `Process()` calls a different
// handler in each version, which is the difference a version-aware search has
// to report.
var index = map[string][]provider.SearchResult{
	"release/v1": {{Path: "processor.go", Line: 5, Snippet: "return LegacyHandler(req)"}},
	"release/v2": {{Path: "processor.go", Line: 5, Snippet: "return NewHandler(req)"}},
}

// demoSearch stands in for adapters/zoekt. Like every provider it is handed the
// version scope alongside the query and must confine its matches to it.
type demoSearch struct{}

func (demoSearch) Search(_ context.Context, codeCtx vacctx.CodeContext, query provider.SearchQuery) ([]provider.SearchResult, error) {
	var matches []provider.SearchResult
	for _, candidate := range index[codeCtx.Branch] {
		if strings.Contains(candidate.Snippet, query.Query) {
			matches = append(matches, candidate)
		}
	}
	return matches, nil
}

// Having a Close is what hands ownership to the Engine: engine.Engine.Close
// closes the dependencies that implement io.Closer and leaves the others — such
// as demoContexts above — untouched. A provider this program wanted to keep
// using afterwards would not have this method.
func (demoSearch) Close() error {
	_, err := fmt.Println("demoSearch.Close: closed by engine.Close")
	return err
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	// Only a search provider: with no graph and no source provider, TraceCalls
	// and GetCode fail with a provider-unavailable error and search is
	// unaffected.
	eng := engine.New(versions, demoSearch{}, nil, nil)

	// Both errors, so a failed query still closes the engine and a failed close
	// is still reported.
	return errors.Join(searchEveryVersion(eng, "Handler"), eng.Close())
}

func searchEveryVersion(eng *engine.Engine, query string) error {
	ctx := context.Background()

	var lines []string
	for _, codeCtx := range eng.ListContexts(ctx) {
		result, err := eng.SearchCode(ctx, engine.SearchCodeRequest{Context: codeCtx.ID, Query: query})
		if err != nil {
			return err
		}

		// Every result names the version it was answered in and cites where it
		// can be checked; neither is optional, and neither had to be assembled
		// here.
		answered := result.Context()
		lines = append(lines, fmt.Sprintf("%s: %s %s @ %s", answered.ID, answered.Repository, answered.Branch, answered.Revision))
		for _, match := range result.Matches() {
			lines = append(lines, fmt.Sprintf("  match    %s:%d  %s", match.Path, match.Line, match.Snippet))
		}
		for _, citation := range result.Evidence() {
			at := citation.Location
			lines = append(lines, fmt.Sprintf("  evidence %s:%d-%d", at.Path, at.StartLine, at.EndLine))
		}
	}

	_, err := fmt.Println(strings.Join(lines, "\n"))
	return err
}
