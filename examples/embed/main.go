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
//
// Each is a workspace of one repository, which is what a version context is
// unless it says otherwise: the member carries the scope, and the workspace ID
// is the name a query asks for.
var versions = demoContexts{
	single("demo-v1", vacctx.CodeContext{Repository: "demo", Branch: "release/v1", Revision: "1111111111111111111111111111111111111111", GraphRef: "demo-v1"}),
	single("demo-v2", vacctx.CodeContext{Repository: "demo", Branch: "release/v2", Revision: "2222222222222222222222222222222222222222", GraphRef: "demo-v2"}),
}

// single is one repository as the workspace it is. The member is filed under the
// workspace's ID, because that is the name every result and every citation is
// scoped by.
func single(id string, member vacctx.CodeContext) vacctx.Workspace {
	member.ID = id
	return vacctx.Workspace{ID: id, Members: []vacctx.CodeContext{member}}
}

// demoContexts stands in for *resolver.Resolver. An engine.ContextSource is the
// two methods below and nothing more, so a program keeping its version scopes
// in a database, a service or — here — a slice implements it without touching
// this project's configuration format.
type demoContexts []vacctx.Workspace

// Nothing here can fail, and the error is returned anyway: the interface has one
// so that a source deciding which contexts a caller may see has something to
// report a failure with other than an empty list, which would say there are no
// versions at all.
func (d demoContexts) Contexts(context.Context) ([]vacctx.Workspace, error) { return d, nil }

// An id that names no version is refused rather than guessed at: answering out
// of some other version is exactly the failure vacmcp exists to prevent.
func (d demoContexts) Resolve(_ context.Context, id string) (vacctx.Workspace, error) {
	for _, workspace := range d {
		if workspace.ID == id {
			return workspace, nil
		}
	}
	return vacctx.Workspace{}, vacerr.New(
		vacerr.ContextNotFound,
		"no context named "+id,
		map[string]any{"context": id},
	)
}

// What the stand-in index holds, keyed by the repository and branch it belongs
// to: `Process()` calls a different handler in each version, which is the
// difference a version-aware search has to report.
var index = map[string][]provider.SearchResult{
	"demo/release/v1": {{Path: "processor.go", Line: 5, Snippet: "return LegacyHandler(req)"}},
	"demo/release/v2": {{Path: "processor.go", Line: 5, Snippet: "return NewHandler(req)"}},
}

// demoSearch stands in for adapters/zoekt. Like every provider it is handed the
// version scope alongside the query and must confine its matches to it.
type demoSearch struct{}

func (demoSearch) Search(_ context.Context, codeCtx vacctx.CodeContext, query provider.SearchQuery) ([]provider.SearchResult, error) {
	var matches []provider.SearchResult
	for _, candidate := range index[codeCtx.Repository+"/"+codeCtx.Branch] {
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
	// fails with GRAPH_PROVIDER_UNAVAILABLE and GetCode with
	// REPOSITORY_NOT_FOUND — there is no source-provider-unavailable code, and
	// no repository can be read without one — while search is unaffected.
	eng := engine.New(versions, demoSearch{}, nil, nil)

	// Both errors, so a failed query still closes the engine and a failed close
	// is still reported.
	return errors.Join(searchEveryVersion(eng, "Handler"), eng.Close())
}

func searchEveryVersion(eng *engine.Engine, query string) error {
	ctx := context.Background()

	listed, err := eng.ListContexts(ctx)
	if err != nil {
		return err
	}

	var lines []string
	for _, workspace := range listed {
		result, err := eng.SearchCode(ctx, engine.SearchCodeRequest{Context: workspace.ID, Query: query})
		if err != nil {
			return err
		}

		// Every result names the version it was answered in and cites where it
		// can be checked; neither is optional, and neither had to be assembled
		// here. The version is the workspace the search ran in — one member per
		// repository it covered — and the citations arrive grouped the same way,
		// one list per member, so no citation can be read as another
		// repository's.
		answered := result.Context()
		for _, member := range answered.Members {
			lines = append(lines, fmt.Sprintf("%s: %s %s @ %s", answered.ID, member.Repository, member.Branch, member.Revision))
		}
		for _, match := range result.Matches() {
			lines = append(lines, fmt.Sprintf("  match    %s:%d  %s", match.Path, match.Line, match.Snippet))
		}
		for i, cited := range result.Evidence() {
			for _, citation := range cited {
				at := citation.Location
				lines = append(lines, fmt.Sprintf("  evidence %s:%d-%d in %s",
					at.Path, at.StartLine, at.EndLine, answered.Members[i].Repository))
			}
		}
	}

	_, err = fmt.Println(strings.Join(lines, "\n"))
	return err
}
