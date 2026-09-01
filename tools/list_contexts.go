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

// listedContext is one configured version scope as a client sees it, in one of
// the two shapes its member count decides.
//
// A context naming one repository is the flat repository, branch and revision it
// has always been. A context naming several is its id and a members list
// instead. The shape follows the count rather than members being emitted always,
// for the reason the evidence package's context block does: a members array on
// every context would break every client reading contexts[i].revision, and
// members beside the flat fields would put one fact in two places on the wire,
// where they can disagree.
//
// The fields are the same ones the evidence package puts on the wire, for the
// same reason: GraphRef is the CBM project backing a member, an internal detail
// that never reaches a client.
type listedContext struct {
	ID         string         `json:"id" jsonschema:"the context ID to pass to the other tools"`
	Repository string         `json:"repository,omitempty" jsonschema:"the repository this context is scoped to; present when it names exactly one"`
	Branch     string         `json:"branch,omitempty" jsonschema:"the branch searched in this context; present when it names exactly one repository"`
	Revision   string         `json:"revision,omitempty" jsonschema:"the revision source is read at in this context; present when it names exactly one repository"`
	Members    []listedMember `json:"members,omitempty" jsonschema:"the repositories this context is scoped to, present instead of the three fields above when it names several; a member's repository is what the other tools' repository argument takes"`
}

// listedMember is one repository of a context naming several.
//
// It carries no id of its own, exactly as the evidence package's member does:
// the context's id names the whole set, and a member is not separately
// addressable — it is picked out by its repository, which is what the other
// tools' repository argument takes.
type listedMember struct {
	Repository string `json:"repository" jsonschema:"the repository, and the value to pass as another tool's repository argument"`
	Branch     string `json:"branch" jsonschema:"the branch searched in this repository"`
	Revision   string `json:"revision" jsonschema:"the revision source is read at in this repository"`
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
// eng knows. It takes no arguments and lists every configured context's id and
// the version scope it names, sorted by ID.
//
// It takes no repository argument either, unlike the five tools that answer a
// question inside a context. There is nothing here for one to select: this is
// the call an agent makes to find out which repositories a context has, so
// narrowing it by a repository name would require the answer it is being asked
// for.
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
		Description: "List every configured version context: its id, and the version scope it names. " +
			"Call this first to discover which versions exist, then pass a context id to the other tools. " +
			"A context over one repository carries repository, branch and revision directly; " +
			"a context over several carries a members array instead, one entry per repository with its own branch and revision. " +
			"Those member repositories are the only legal values of the other tools' repository argument, which they require when the context names several. " +
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

// outputSchema is the schema inferred from [listContextsOutput], with its two
// lists made non-nullable.
//
// Schema inference maps every Go slice to ["null", "array"], because a nil slice
// marshals to null. Neither list is ever emitted as one — the context list is
// built empty, and a member list that would be nil is omitted by its json tag
// rather than written as null — and saying so is the whole point of this schema:
// an agent reads it before it calls anything, and it should learn that a list
// here may be empty or absent, never present and null. The schema is still
// derived from the Go type rather than written out by hand, so it cannot drift
// from what the tool returns.
func outputSchema() *jsonschema.Schema {
	schema, err := jsonschema.For[listContextsOutput](nil)
	if err != nil {
		// Inference fails only on a type this package declares, which makes it
		// a programming error rather than a runtime one. mcp.AddTool panics on
		// a bad tool for the same reason.
		panic("tools: cannot infer the list_contexts output schema: " + err.Error())
	}
	contexts := schema.Properties["contexts"]
	contexts.Types = []string{"array"}
	contexts.Items.Properties["members"].Types = []string{"array"}
	return schema
}

// list projects the engine's contexts onto the wire shape, keeping the order it
// answered in, which is sorted by ID so repeated calls answer identically.
//
// One entry per context, in the shape [listedContext] documents: a context is
// one thing an agent can name, so listing a two-repository one as two entries
// sharing an ID — which is what this did while the members shape was being built
// underneath it — reads as two contexts, and an agent that picked one of them
// would be asking in a scope that does not exist.
//
// A workspace with no member at all takes the members branch and is listed with
// no repository of any kind, which is what it is. The configuration refuses one,
// so this is the shape of a context that got here some other way rather than a
// case to design for.
func list(contexts []vacctx.Workspace) []listedContext {
	// Not a nil slice: a nil one marshals to null, and null is not the same
	// answer as "there are none" to the agent reading it.
	listed := make([]listedContext, 0, len(contexts))
	for _, workspace := range contexts {
		if len(workspace.Members) == 1 {
			member := workspace.Members[0]
			listed = append(listed, listedContext{
				ID:         workspace.ID,
				Repository: member.Repository,
				Branch:     member.Branch,
				Revision:   member.Revision,
			})
			continue
		}

		members := make([]listedMember, 0, len(workspace.Members))
		for _, member := range workspace.Members {
			members = append(members, listedMember{
				Repository: member.Repository,
				Branch:     member.Branch,
				Revision:   member.Revision,
			})
		}
		listed = append(listed, listedContext{ID: workspace.ID, Members: members})
	}
	return listed
}
