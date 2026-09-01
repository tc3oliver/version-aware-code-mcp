// Package tools holds the MCP tools vacmcp exposes. Each tool attaches itself
// to a [mcp.Server] built by the server package, which is the tool registry.
//
// Every other tool answers a question *inside* one version context and is
// therefore bound by doc-1's Tool Contract: its output carries the context it
// was resolved in plus the evidence backing it, which is what the evidence
// package builds. list_contexts is the exception, and deliberately so — see
// [AddListContexts].
package tools

import (
	"context"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
)

// listedContext is one configured version scope as a client sees it. The four
// fields are the same ones the evidence package puts on the wire, for the same
// reason: GraphRef is the CBM project backing the context, an internal detail
// that never reaches a client.
type listedContext struct {
	ID         string `json:"id" jsonschema:"the context ID to pass to the other tools"`
	Repository string `json:"repository" jsonschema:"the repository this context is scoped to"`
	Branch     string `json:"branch" jsonschema:"the branch searched in this context"`
	Revision   string `json:"revision" jsonschema:"the revision source is read at in this context"`
}

// listContextsOutput is the tool's payload.
//
// It is an object rather than a bare array so a client that predates
// structured content can still read one JSON object, and so later versions can
// add a field without changing the shape.
type listContextsOutput struct {
	Contexts []listedContext `json:"contexts" jsonschema:"the configured version contexts, empty when none are configured"`
}

// AddListContexts registers the list_contexts tool on srv, serving the contexts
// eng knows. It takes no arguments and lists every configured context's id,
// repository, branch and revision, sorted by ID.
//
// This is the tool an agent calls before it knows which versions exist, so an
// empty configuration is an answer and not a failure: it returns an empty list
// rather than an error, and the list is emitted as [] rather than null, which
// to a client is a different answer.
//
// Its output is not wrapped in an evidence.Output, and that is not a hole in
// doc-1's Tool Contract. The contract exists so that no tool answers a question
// about code without saying which version it answered in and citing what backs
// it. list_contexts answers no such question: its payload *is* the set of
// version scopes, each element already carrying the exact four fields the
// contract's context block carries. There is no single context to scope it to
// and no source line to cite, so an envelope here would be an empty ritual
// rather than the guarantee the contract is asking for.
func AddListContexts(srv *mcp.Server, eng *engine.Engine) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:  "list_contexts",
		Title: "List version contexts",
		Description: "List every configured version context: its id, repository, branch and revision. " +
			"Call this first to discover which versions exist, then pass a context id to the other tools. " +
			"Returns an empty list when no context is configured.",
		Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
		OutputSchema: outputSchema(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, listContextsOutput, error) {
		contexts, err := eng.ListContexts(ctx)
		if err != nil {
			// An empty configuration is an answer; a configuration that could
			// not be read is not, and answering [] for it would tell the agent
			// there are no versions to ask about.
			return nil, listContextsOutput{}, err
		}
		return nil, listContextsOutput{Contexts: list(contexts)}, nil
	})
}

// outputSchema is the schema inferred from [listContextsOutput], with the
// context list made non-nullable.
//
// Schema inference maps every Go slice to ["null", "array"], because a nil
// slice marshals to null. This tool never returns one, and saying so is the
// whole point of it: an agent reads this schema before it calls anything, and
// it should learn that the answer is a list that may be empty, not a list that
// may be absent. The schema is still derived from the Go type rather than
// written out by hand, so it cannot drift from it.
func outputSchema() *jsonschema.Schema {
	schema, err := jsonschema.For[listContextsOutput](nil)
	if err != nil {
		// Inference fails only on a type this package declares, which makes it
		// a programming error rather than a runtime one. mcp.AddTool panics on
		// a bad tool for the same reason.
		panic("tools: cannot infer the list_contexts output schema: " + err.Error())
	}
	schema.Properties["contexts"].Types = []string{"array"}
	return schema
}

// list projects the engine's contexts onto the wire shape, keeping the order it
// answered in, which is sorted by ID so repeated calls answer identically.
//
// One entry per repository, which for a context naming one repository — every
// context this server can currently answer a query in — is one entry per
// context, exactly as it has always been. A context naming several is listed as
// several entries sharing its ID rather than as one entry standing for a
// repository picked out of it: an agent reading a single repository, branch and
// revision there would believe the context is scoped to that one alone.
func list(contexts []vacctx.Workspace) []listedContext {
	// Not a nil slice: a nil one marshals to null, and null is not the same
	// answer as "there are none" to the agent reading it.
	listed := make([]listedContext, 0, len(contexts))
	for _, workspace := range contexts {
		for _, member := range workspace.Members {
			listed = append(listed, listedContext{
				ID:         workspace.ID,
				Repository: member.Repository,
				Branch:     member.Branch,
				Revision:   member.Revision,
			})
		}
	}
	return listed
}
