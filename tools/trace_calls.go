package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/evidence"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
)

// traceCallsInput is what a client sends.
//
// Every field is optional to the schema and none of them are constrained there
// — no enum on direction, no bounds on depth. That is deliberate: a value the
// schema rejects comes back as the SDK's own validation failure, while a value
// the tool rejects comes back as this project's error model. Keeping every
// value check on one side of that line means one wrong argument has one shape,
// whichever field it was.
//
// Repository is the same field it is in every other tool: it selects one of the
// repositories the context already names, and one it does not name is refused
// rather than walked. A context naming several has no walk without it, because a
// call graph is one repository's own and there is no union of two to walk; a
// context naming one never needs it.
type traceCallsInput struct {
	Context    string `json:"context,omitempty" jsonschema:"the context id from list_contexts; the trace runs in that version's graph and no other"`
	Repository string `json:"repository,omitempty" jsonschema:"which repository of the context to walk the graph of, as one of the members list_contexts reports for it; required when the context names several, and unnecessary when it names one"`
	Symbol     string `json:"symbol,omitempty" jsonschema:"the function to trace, named as it is written in the source"`
	Direction  string `json:"direction,omitempty" jsonschema:"callers to walk towards the functions that call the symbol, callees to walk towards the ones it calls"`
	Depth      int    `json:"depth,omitempty" jsonschema:"how many levels to walk, from 1 to 5"`
}

// call is one call relation on the wire, located where the call is written.
type call struct {
	Caller string `json:"caller"`
	Callee string `json:"callee"`
	Path   string `json:"path"`
	Line   int    `json:"line"`
}

// traceCallsResult is the tool-specific half of the output; the context and
// evidence half is [evidence.Output]'s.
//
// Symbol is what the graph resolved the request to rather than what was asked
// for, and direction and depth are echoed back, so a client reading the result
// alone can tell which walk produced it.
type traceCallsResult struct {
	Symbol    string `json:"symbol"`
	Direction string `json:"direction"`
	Depth     int    `json:"depth"`
	Calls     []call `json:"calls"`
}

// AddTraceCalls registers the trace_calls tool on srv, answered by eng.
//
// The tool walks the call graph around one symbol inside one version context.
// Resolving that context, bounding the depth doc-1 fixes at 1 to 5 and citing
// the call sites are [engine.Engine.TraceCalls]'s, so a request that reaches a
// graph reached the one its context names and no other. What is left here is
// the wire: decode the call, hand it over, encode what comes back.
func AddTraceCalls(srv *mcp.Server, eng *engine.Engine) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:  "trace_calls",
		Title: "Trace calls in a version's graph",
		Description: "Trace the call graph around a symbol inside one version context. " +
			"Pass a context id from list_contexts: the trace runs in that version's graph, never another's. " +
			"A context naming several repositories requires repository, given as one of the members list_contexts reports for it, because a call graph is one repository's own; leaving it out is INVALID_ARGUMENT. " +
			"direction is \"callers\" (the functions that call the symbol) or \"callees\" (the ones it calls); " +
			"depth is how many levels to walk, from 1 to 5. " +
			"Returns the calls, the context they were traced in and the source locations backing them. " +
			"A symbol that matches several functions is reported as SYMBOL_AMBIGUOUS with the candidates, rather than one of them being picked.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in traceCallsInput) (*mcp.CallToolResult, any, error) {
		result, err := eng.TraceCalls(ctx, engine.TraceCallsRequest{
			Context:    in.Context,
			Repository: in.Repository,
			Symbol:     in.Symbol,
			Direction:  provider.Direction(in.Direction),
			Depth:      in.Depth,
		})
		if err != nil {
			return failed(err)
		}

		graph := result.Graph()
		// Built empty rather than nil: a client reading [] learns there are no
		// calls, where null leaves it guessing whether any were looked for.
		calls := make([]call, 0, len(graph.Edges))
		for _, edge := range graph.Edges {
			calls = append(calls, call{Caller: edge.Caller, Callee: edge.Callee, Path: edge.Path, Line: edge.Line})
		}

		// The workspace a walk is answered in has exactly one member: a walk is one
		// graph, so the engine either had one member or was told which one by
		// in.Repository. So this marshals to the flat context block it always has,
		// whichever of the two it was.
		out, err := evidence.NewWorkspace(result.Context(), result.Evidence()...)
		if err != nil {
			return nil, nil, err
		}
		return nil, out.WithResult(traceCallsResult{
			Symbol:    graph.Symbol,
			Direction: in.Direction,
			Depth:     in.Depth,
			Calls:     calls,
		}), nil
	})
}
