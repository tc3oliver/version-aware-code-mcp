package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tc3oliver/version-aware-code-mcp/evidence"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/resolver"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// The levels trace_calls will walk. doc-1 fixes the range; a request outside it
// is refused rather than clamped, because a clamped walk answers a question
// nobody asked and looks exactly like the answer to the one they did.
const (
	minDepth = 1
	maxDepth = 5
)

// traceCallsInput is what a client sends.
//
// Every field is optional to the schema and none of them are constrained there
// — no enum on direction, no bounds on depth. That is deliberate: a value the
// schema rejects comes back as the SDK's own validation failure, while a value
// the tool rejects comes back as this project's error model. Keeping every
// value check on one side of that line means one wrong argument has one shape,
// whichever field it was.
type traceCallsInput struct {
	Context   string `json:"context,omitempty" jsonschema:"the context id from list_contexts; the trace runs in that version's graph and no other"`
	Symbol    string `json:"symbol,omitempty" jsonschema:"the function to trace, named as it is written in the source"`
	Direction string `json:"direction,omitempty" jsonschema:"callers to walk towards the functions that call the symbol, callees to walk towards the ones it calls"`
	Depth     int    `json:"depth,omitempty" jsonschema:"how many levels to walk, from 1 to 5"`
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

// AddTraceCalls registers the trace_calls tool on srv, resolving contexts with
// contexts and querying graph.
//
// The tool walks the call graph around one symbol inside one version context.
// The context is resolved first and the resolved [vacctx.CodeContext] — graph
// reference included — is what the provider is handed, so every query lands in
// the graph that context names and no query can reach another version's.
//
// It validates depth here rather than passing it on. Depth crosses a trust
// boundary, and doc-1 bounds it at 1 to 5; quietly clamping an out-of-range
// depth would answer a shallower question than the caller asked without saying
// so. Everything else about the request — the symbol, the direction, whether
// the symbol names exactly one function — is checked downstream, in the same
// error model, and is not second-guessed here.
func AddTraceCalls(srv *mcp.Server, contexts *resolver.Resolver, graph provider.GraphProvider) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:  "trace_calls",
		Title: "Trace calls in a version's graph",
		Description: "Trace the call graph around a symbol inside one version context. " +
			"Pass a context id from list_contexts: the trace runs in that version's graph, never another's. " +
			"direction is \"callers\" (the functions that call the symbol) or \"callees\" (the ones it calls); " +
			"depth is how many levels to walk, from 1 to 5. " +
			"Returns the calls, the context they were traced in and the source locations backing them. " +
			"A symbol that matches several functions is reported as SYMBOL_AMBIGUOUS with the candidates, rather than one of them being picked.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in traceCallsInput) (*mcp.CallToolResult, any, error) {
		if in.Depth < minDepth || in.Depth > maxDepth {
			return traceCallsFailed(vacerr.New(
				vacerr.InvalidArgument,
				fmt.Sprintf("trace_calls: depth %d is outside the supported range %d-%d", in.Depth, minDepth, maxDepth),
				map[string]any{"context": in.Context, "symbol": in.Symbol, "depth": in.Depth},
			))
		}

		codeCtx, err := contexts.Resolve(ctx, in.Context)
		if err != nil {
			return traceCallsFailed(err)
		}

		callGraph, err := graph.TraceCalls(ctx, codeCtx, provider.TraceRequest{
			Symbol:    in.Symbol,
			Direction: provider.Direction(in.Direction),
			Depth:     in.Depth,
		})
		if err != nil {
			return traceCallsFailed(err)
		}

		out, err := traceCallsOutput(codeCtx, callGraph, in)
		if err != nil {
			return nil, nil, err
		}
		return nil, out, nil
	})
}

// traceCallsOutput builds the successful result: the call graph, the context it
// was traced in and one evidence location per call site.
func traceCallsOutput(codeCtx vacctx.CodeContext, graph *provider.CallGraph, in traceCallsInput) (evidence.Output, error) {
	// Both are built empty rather than nil: a client reading [] learns there are
	// no calls, where null leaves it guessing whether any were looked for.
	calls := make([]call, 0, len(graph.Edges))
	items := make([]evidence.Evidence, 0, len(graph.Edges))
	seen := map[evidence.Evidence]bool{}

	for _, edge := range graph.Edges {
		calls = append(calls, call{Caller: edge.Caller, Callee: edge.Callee, Path: edge.Path, Line: edge.Line})
		// Several calls written in one function cite one location, so the same
		// citation is listed once.
		item := evidence.At(edge.Path, edge.Line, edge.Line, "")
		if !seen[item] {
			seen[item] = true
			items = append(items, item)
		}
	}

	out, err := evidence.New(codeCtx, items...)
	if err != nil {
		return evidence.Output{}, err
	}
	return out.WithResult(traceCallsResult{
		Symbol:    graph.Symbol,
		Direction: in.Direction,
		Depth:     in.Depth,
		Calls:     calls,
	}), nil
}

// traceCallsFailed turns a failure into a tool error result carrying doc-1's
// error shape, {"error": {"code": ..., "message": ..., "details": {}}}.
//
// The SDK's own handling would reduce the error to its Error() string, which
// drops the code a client is supposed to branch on and the details a
// SYMBOL_AMBIGUOUS answer is made of, so the payload is written here instead.
// Anything that is not a *[vacerr.Error] has no code to report and is handed
// back to the SDK unchanged.
func traceCallsFailed(err error) (*mcp.CallToolResult, any, error) {
	var vErr *vacerr.Error
	if !errors.As(err, &vErr) {
		return nil, nil, err
	}
	body, marshalErr := json.Marshal(vErr)
	if marshalErr != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: string(body)}},
	}, nil, nil
}
