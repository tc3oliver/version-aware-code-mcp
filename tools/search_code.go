package tools

import (
	"context"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/evidence"
)

// searchCodeInput is what the caller asks for. There is no repository or branch
// field: the context decides where the search runs, so a query cannot widen its
// own scope.
type searchCodeInput struct {
	Context string `json:"context" jsonschema:"the id of the version context to search in, as listed by list_contexts"`
	Query   string `json:"query" jsonschema:"what to search for; supports the sym:, file: and lang: filters"`
}

// searchMatch is one match, as doc-1 §5 names it: where it is and the line
// itself.
type searchMatch struct {
	Path    string `json:"path" jsonschema:"the file the match is in, relative to the repository root"`
	Line    int    `json:"line" jsonschema:"the 1-based line number of the match"`
	Snippet string `json:"snippet" jsonschema:"the matched line"`
}

// searchCodeOutput is the whole successful result.
//
// The value on the wire is built by the evidence package rather than from this
// type: [evidence.Output] merges its context and evidence over the payload it
// is given, which is why the zero Context and Evidence handed to WithResult
// below never reach a client. This type exists so the schema an agent reads
// before calling describes the same three keys, and because the SDK validates
// every result against that schema, the two cannot drift apart unnoticed.
//
// matches and evidence carry the same facts on purpose: for a search the match
// is its own citation, and doc-1 asks for both — §5 for the matches, §6 for the
// evidence that makes the answer checkable.
type searchCodeOutput struct {
	Context  listedContext       `json:"context" jsonschema:"the version context every match is confined to"`
	Evidence []evidence.Evidence `json:"evidence" jsonschema:"one citation per match, so every result can be checked at its source"`
	Matches  []searchMatch       `json:"matches" jsonschema:"the matches in the engine's ranked order, empty when the query matches nothing in this context"`
}

// AddSearchCode registers the search_code tool on srv, answered by eng.
//
// The tool decodes the call, hands it to [engine.Engine.SearchCode] and encodes
// what comes back: resolving the context, searching it and citing the matches
// are the engine's, so this holds no version logic of its own to keep correct.
// doc-1 §23's eighth success criterion — search answers whether or not CBM is
// present — is [engine.Engine.SearchCode]'s, which reaches no graph provider.
//
// Every failure is reported as the error model's wire shape,
// {"error": {"code": ..., "message": ..., "details": {}}}, on a result marked
// as an error. An unconfigured context is CONTEXT_NOT_FOUND and nothing
// else: the tool does not fall back to another context and does not answer an
// unanswerable question with an empty result, which would read as "this version
// has no such code".
func AddSearchCode(srv *mcp.Server, eng *engine.Engine) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:  "search_code",
		Title: "Search code in a version context",
		Description: "Search the code of one version context. The context decides the repository and the branch searched, " +
			"so every match belongs to that version and no other; call list_contexts first to see which ids exist. " +
			"Supports Zoekt's sym:, file: and lang: filters. Returns the matches with the context and the evidence backing them.",
		Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
		OutputSchema: searchCodeSchema(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in searchCodeInput) (*mcp.CallToolResult, any, error) {
		result, err := eng.SearchCode(ctx, engine.SearchCodeRequest{Context: in.Context, Query: in.Query})
		if err != nil {
			return failed(err)
		}

		// Not a nil slice: null and [] are different answers to an agent, and a
		// query that matches nothing on this branch is an answer.
		matches := make([]searchMatch, 0, len(result.Matches()))
		for _, match := range result.Matches() {
			matches = append(matches, searchMatch{Path: match.Path, Line: match.Line, Snippet: match.Snippet})
		}

		out, err := evidence.New(result.Context(), result.Evidence()...)
		if err != nil {
			// The engine only returns a result it could scope, so this is a bug
			// rather than a caller's mistake. It is still reported instead of
			// ignored: an output that cannot be scoped must not be sent.
			return failed(err)
		}
		return nil, out.WithResult(searchCodeOutput{Matches: matches}), nil
	})
}

// searchCodeSchema is the schema inferred from [searchCodeOutput], with the two
// lists made non-nullable for the same reason list_contexts does it: inference
// maps every Go slice to ["null", "array"] because a nil slice marshals to
// null, and neither of these is ever null. "No matches on this branch" is [],
// and an agent reading the schema should learn that.
func searchCodeSchema() *jsonschema.Schema {
	schema, err := jsonschema.For[searchCodeOutput](nil)
	if err != nil {
		// Inference fails only on a type this package declares, which makes it
		// a programming error rather than a runtime one. mcp.AddTool panics on
		// a bad tool for the same reason.
		panic("tools: cannot infer the search_code output schema: " + err.Error())
	}
	schema.Properties["evidence"].Types = []string{"array"}
	schema.Properties["matches"].Types = []string{"array"}
	return schema
}
