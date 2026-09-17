package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/evidence"
)

// searchHistoryInput is what the caller asks for.
//
// There is no branch, revision or "since" field, for the reason searchCodeInput
// has no branch: the context decides where the walk starts — the commit it pins
// — so a query cannot widen its own scope. A context pinned to an older commit
// does not see the commits made after it, and nothing here can ask it to.
//
// Repository selects one of the repositories the context already names, exactly
// as it does for search_code, and for the same reason it is optional there:
// "what changed in this version" is a question about the whole workspace, so
// history spans every member unless the caller narrows it. A repository the
// context does not name is refused rather than searched.
//
// Query, Symbol and Path combine with AND, and a filter that matches nothing
// yields an empty result rather than being dropped — widening a search nobody
// asked to widen answers a different question. What each one means is
// [provider.HistoryQuery]'s to define and is not re-stated differently here.
//
// Limit is that contract too, unchanged: it caps entries — commit-path
// occurrences — not commits, so a multi-file commit spends one of the budget per
// path. Nothing here re-counts it, because a wire-level cap on commits would be
// a second, disagreeing meaning for the same field.
type searchHistoryInput struct {
	Context    string `json:"context" jsonschema:"the id of the version context to search the history of, as listed by list_contexts"`
	Repository string `json:"repository,omitempty" jsonschema:"one of the repositories the context names, as listed by list_contexts, to search only that one's history; leave it out to search every repository the context names"`
	Query      string `json:"query,omitempty" jsonschema:"match commits whose message contains this text, case-insensitively; a literal substring test, not a pattern and not a ranking"`
	Symbol     string `json:"symbol,omitempty" jsonschema:"match commits that changed the number of occurrences of this exact string (git's pickaxe); it is not resolved semantically and does not follow a rename"`
	Path       string `json:"path,omitempty" jsonschema:"match only commits touching this path, relative to the repository root"`
	Limit      int    `json:"limit,omitempty" jsonschema:"the most entries to return per repository, counted in commit-path occurrences rather than commits, so a multi-file commit spends one per path; leave it out for the provider's default bound, and a negative value is an error rather than 'unbounded'"`
}

// historyCommit is one commit-path occurrence: a commit that touched several
// paths produces one entry per path, so a multi-file commit keeps every file's
// provenance instead of being attributed to whichever path git printed first.
//
// Repository follows the rule searchMatch's does, and for its reason: it is
// omitted when the answer covers one repository, because the context block
// already says which one and repeating it per commit would put the same fact in
// two places where they can disagree. When the walk covered several it is the
// only thing saying which version a commit is from.
//
// Commit is always the full 40-character id — never a branch, a tag, HEAD or an
// abbreviation — and Timestamp is RFC3339 in UTC, so the same commit reports the
// same bytes whatever the reader's timezone is. Both are the provider's
// guarantees, passed through rather than re-formatted here.
type historyCommit struct {
	Commit     string `json:"commit" jsonschema:"the full 40-character commit id"`
	Path       string `json:"path" jsonschema:"the file this entry is about, relative to the repository root; a commit touching several paths has one entry per path"`
	Author     string `json:"author" jsonschema:"the commit author"`
	Timestamp  string `json:"timestamp" jsonschema:"when the commit was made, RFC3339 in UTC"`
	Message    string `json:"message" jsonschema:"the commit message"`
	Repository string `json:"repository,omitempty" jsonschema:"the repository the commit is in, present when the answer covers several"`
}

// searchHistoryResult is the tool-specific half of the payload. The other half —
// the context the walk was confined to and the citation backing each commit — is
// what [evidence.Output] puts next to it, which is why this struct does not
// repeat them.
type searchHistoryResult struct {
	Commits []historyCommit `json:"commits"`
}

// AddSearchHistory registers the search_history tool on srv, answered by eng.
//
// The tool decodes the call, hands it to [engine.Engine.SearchHistory] and
// encodes what comes back. Which commit the walk starts at, which members are
// searched, and the refusal of a repository the context does not name are all
// the engine's, so this holds no version logic of its own to keep correct — the
// same division every other tool here follows.
//
// This is the MCP surface for a capability v0.6.0 shipped with none: the engine,
// the provider interface and the git implementation were all there, and only a
// program embedding the Go package could reach them.
//
// Every failure is reported as the error model's wire shape,
// {"error": {"code": ..., "message": ..., "details": {}}}, on a result marked as
// an error. A source provider that reads revisions but cannot walk their history
// is SOURCE_HISTORY_UNAVAILABLE — a fact about this server's capability rather
// than about the repository — so the tool is still there and still says exactly
// why it cannot answer, instead of an empty list that would read as "this version
// has no such commit".
func AddSearchHistory(srv *mcp.Server, eng *engine.Engine) {
	// No output schema, for the reason search_code declares none: the shape on
	// the wire is [evidence.Output]'s to define, it has two forms, and the SDK
	// enforces a declared schema against every result — so a copy here would not
	// document the shape, it would reject the answers that did not match the copy.
	mcp.AddTool(srv, &mcp.Tool{
		Name:  "search_history",
		Title: "Search commit history in a version context",
		Description: "Search the commit history of one version context. The walk starts at the commit the context pins, " +
			"not at HEAD and not at the default branch, so the same question asked of two versions gives two answers; " +
			"call list_contexts first to see which ids exist. " +
			"A context naming several repositories is searched in all of them, and each commit then says which repository it is from; " +
			"pass repository — one of the members list_contexts reports for that context — to search only one of them. " +
			"query matches the commit message as a literal substring, symbol is git's pickaxe over an exact string, and path restricts the walk to one file; " +
			"they combine with AND. It resolves no symbol semantically, follows no rename and ranks nothing. " +
			"Each entry is one commit-path occurrence, so a commit that touched several paths is returned once per path, repeating its commit id — " +
			"that is the provenance of every file it changed, not a duplicate to collapse, and limit counts those entries rather than commits. " +
			"Returns the entries with the context and the evidence backing them.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in searchHistoryInput) (*mcp.CallToolResult, any, error) {
		result, err := eng.SearchHistory(ctx, engine.SearchHistoryRequest{
			Context:    in.Context,
			Repository: in.Repository,
			Query:      in.Query,
			Symbol:     in.Symbol,
			Path:       in.Path,
			Limit:      in.Limit,
		})
		if err != nil {
			return failed(err)
		}

		// The members that were searched, which is the context narrowed by
		// in.Repository when it was given. Read only to decide which of the two
		// commit shapes says where a commit is from — the same count the evidence
		// package's context block follows, so the two halves of one document
		// cannot disagree about how many versions it is about.
		attributed := len(result.Context().Members) > 1

		// Not a nil slice: null and [] are different answers to an agent, and a
		// version whose history has no such commit is an answer.
		commits := make([]historyCommit, 0, len(result.Commits()))
		for _, commit := range result.Commits() {
			found := historyCommit{
				Commit:    commit.Commit,
				Path:      commit.Path,
				Author:    commit.Author,
				Timestamp: commit.Timestamp,
				Message:   commit.Message,
			}
			if attributed {
				found.Repository = commit.Repository
			}
			commits = append(commits, found)
		}

		// NewWorkspace rather than New, because history is answered in the whole
		// workspace it ran in: the citations are passed on grouped exactly as the
		// engine grouped them, so each goes out attributed to the member it was
		// found in and no attribution is decided here. A workspace of one member
		// marshals to the same bytes New would have produced for it.
		out, err := evidence.NewWorkspace(result.Context(), result.Evidence()...)
		if err != nil {
			// The engine only returns a result it could scope, so this is a bug
			// rather than a caller's mistake. It is still reported instead of
			// ignored: an output that cannot be scoped must not be sent.
			return failed(err)
		}
		return nil, out.WithResult(searchHistoryResult{Commits: commits}), nil
	})
}
