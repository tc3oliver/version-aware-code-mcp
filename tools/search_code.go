package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/evidence"
)

// searchCodeInput is what the caller asks for. There is still no branch or
// revision field: the context decides which version the search runs in, so a
// query cannot widen its own scope.
//
// Repository is not a way out of that either. It selects one of the repositories
// the context already names — the ones list_contexts reports as its members —
// and a repository the context does not name is refused rather than searched, so
// the answer is bounded by the configuration whether it is given or not. It is
// optional because a search is answered in every repository of the context by
// default, which for a context naming one is the search this tool has always
// run.
type searchCodeInput struct {
	Context    string `json:"context" jsonschema:"the id of the version context to search in, as listed by list_contexts"`
	Repository string `json:"repository,omitempty" jsonschema:"one of the repositories the context names, as listed by list_contexts, to search only that one; leave it out to search every repository the context names"`
	Query      string `json:"query" jsonschema:"what to search for; supports the sym:, file: and lang: filters"`
}

// searchMatch is one match, as doc-1 §5 names it: where it is and the line
// itself, plus which version it is in when the answer covers more than one.
//
// Repository and Revision are omitted in a context naming one repository, and
// are the two fields the evidence package omits from a citation there, for its
// reason: the answer has exactly one repository and one revision it can be
// about, both already in the context block, so repeating them per match would be
// the same fact in two places — and they are also fields a v0.4.0 client would
// not expect. When the search covered several repositories they are the only
// thing that says which version a line is from, and they are the searched
// member's own, copied off [engine.Match] rather than worked out here.
type searchMatch struct {
	Path       string `json:"path" jsonschema:"the file the match is in, relative to the repository root"`
	Line       int    `json:"line" jsonschema:"the 1-based line number of the match"`
	Snippet    string `json:"snippet" jsonschema:"the matched line"`
	Repository string `json:"repository,omitempty" jsonschema:"the repository the match is in, present when the context names several"`
	Revision   string `json:"revision,omitempty" jsonschema:"the revision the match was found at, present when the context names several"`
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
		Description: "Search the code of one version context. The context decides the branch searched, " +
			"so every match belongs to that version and no other; call list_contexts first to see which ids exist. " +
			"A context naming several repositories is searched in all of them, and each match then says which repository and revision it is from; " +
			"pass repository — one of the members list_contexts reports for that context — to search only one of them. " +
			"Supports Zoekt's sym:, file: and lang: filters. Returns the matches with the context and the evidence backing them.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in searchCodeInput) (*mcp.CallToolResult, any, error) {
		result, err := eng.SearchCode(ctx, engine.SearchCodeRequest{
			Context:    in.Context,
			Repository: in.Repository,
			Query:      in.Query,
		})
		if err != nil {
			return failed(err)
		}

		// The members that were searched, which is the context narrowed by
		// in.Repository when it was given: what the answer covers is the engine's
		// to report, and it is read here only to decide which of the two match
		// shapes says it — the same count the evidence package's context block
		// follows, so the two halves of one document cannot disagree about how
		// many versions it is about.
		attributed := len(result.Context().Members) > 1

		// Not a nil slice: null and [] are different answers to an agent, and a
		// query that matches nothing on this branch is an answer.
		matches := make([]searchMatch, 0, len(result.Matches()))
		for _, match := range result.Matches() {
			found := searchMatch{Path: match.Path, Line: match.Line, Snippet: match.Snippet}
			if attributed {
				found.Repository, found.Revision = match.Repository, match.Revision
			}
			matches = append(matches, found)
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
