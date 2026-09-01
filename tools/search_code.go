package tools

import (
	"context"

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

// searchCodeResult is the tool-specific half of the payload. The other half —
// the context every match is confined to and the citation backing each one — is
// what [evidence.Output] puts next to it, which is why this struct does not
// repeat them.
//
// matches and evidence carry the same facts on purpose: for a search the match
// is its own citation, and doc-1 asks for both — §5 for the matches, §6 for the
// evidence that makes the answer checkable.
type searchCodeResult struct {
	Matches []searchMatch `json:"matches"`
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
	// Out is any and no output schema is declared, as for get_code and the two
	// comparison tools: the shape on the wire is [evidence.Output]'s to define,
	// and a mirror of it here would not only drift, it would be enforced — the
	// SDK validates output against a declared schema, so a stale copy starts
	// rejecting valid results.
	//
	// This tool did declare one, inferred from a struct whose context block was
	// the flat four-field one. That is exactly the copy the warning is about, and
	// a workspace of several repositories is what went stale under it: its
	// context block carries members instead, and the schema refused the result on
	// the way out — a protocol fault in place of an answer, carrying none of this
	// server's error model. The evidence package decides what a context block
	// looks like, in one place, and this is the tool that used to disagree.
	mcp.AddTool(srv, &mcp.Tool{
		Name:  "search_code",
		Title: "Search code in a version context",
		Description: "Search the code of one version context. The context decides the repository and the branch searched, " +
			"so every match belongs to that version and no other; call list_contexts first to see which ids exist. " +
			"Supports Zoekt's sym:, file: and lang: filters. Returns the matches with the context and the evidence backing them.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
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

		// NewWorkspace rather than New, because a search is answered in the whole
		// workspace it ran in: the citations are passed on grouped exactly as the
		// engine grouped them, so each one goes out attributed to the member it
		// was found in and no attribution is decided here. A workspace of one
		// member — every context this tool could answer before — marshals to the
		// same bytes New produced for it.
		out, err := evidence.NewWorkspace(result.Context(), result.Evidence()...)
		if err != nil {
			// The engine only returns a result it could scope, so this is a bug
			// rather than a caller's mistake. It is still reported instead of
			// ignored: an output that cannot be scoped must not be sent.
			return failed(err)
		}
		return nil, out.WithResult(searchCodeResult{Matches: matches}), nil
	})
}
