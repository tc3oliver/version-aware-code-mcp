package engine

import (
	"context"

	"github.com/tc3oliver/version-aware-code-mcp/evidence"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// SearchHistoryRequest is a search of one version's commit history.
//
// Repository narrows the search to the one repository of the context that names
// it, exactly as [SearchCodeRequest.Repository] does. History is a search-like
// capability rather than a narrowing one: left blank it covers every repository
// the context names, because "what changed in this version" is a question about
// the whole workspace, and answering it out of one member would leave the rest
// silently outside the answer. It selects, it does not scope — a repository the
// context does not name is refused rather than searched.
//
// Query, Symbol and Path are combined with AND. See [provider.HistoryQuery] for
// what each one means; the semantics are the provider's and are not re-stated
// differently here.
type SearchHistoryRequest struct {
	Context    string
	Repository string
	Query      string
	Symbol     string
	Path       string
	Limit      int
}

// Commit is one commit-path occurrence, with the repository it belongs to.
//
// Its fields are exported for the reason [Match]'s are: it is a leaf of a
// result, and the version claim around it lives on the [SearchHistoryResult]
// that no caller outside this package can forge.
//
// Repository is the searched member's own, copied off the context that was
// handed to the provider rather than read back out of what the provider
// returned — the same rule [Match] follows, for the same reason.
type Commit struct {
	Repository string
	Commit     string
	Path       string
	Author     string
	Timestamp  string
	Message    string
}

// SearchHistoryResult is the commits with the version they were found in and one
// citation per commit-path occurrence.
type SearchHistoryResult struct {
	answer
	commits []Commit
}

// Commits reports the matches grouped by the member they were found in, in the
// order [answer.Context] lists those members, and inside a group in git's own
// newest-first order.
//
// There is deliberately no ordering across the groups: two repositories have no
// shared history, so interleaving their commits by date would invent a sequence
// that never happened. It is empty, not an error, when nothing in any of those
// versions matched.
func (r SearchHistoryResult) Commits() []Commit { return r.commits }

// SearchHistory walks the commit history of a version context.
//
// The walk is scoped to the commit each member pins, never to the checkout or
// the default branch: a context pinned to an older commit does not see the
// commits made after it. That is what makes the answer version-aware rather than
// merely repository-aware.
//
// Every selected member must answer. A member whose history cannot be read
// fails the whole request rather than being skipped, because a history that
// quietly omitted one repository would look like a complete answer for the
// context while part of it was never looked at — the same rule
// [Engine.SearchCode] follows.
func (e *Engine) SearchHistory(ctx context.Context, req SearchHistoryRequest) (SearchHistoryResult, error) {
	workspace, err := e.resolve(ctx, req.Context)
	if err != nil {
		return SearchHistoryResult{}, err
	}
	members, err := selectMembers(req.Context, req.Repository, workspace)
	if err != nil {
		return SearchHistoryResult{}, err
	}

	// After the resolve, not before it: whether a version exists is the
	// configuration's answer and must not depend on which providers this server
	// was built with, exactly as in SearchCode / TraceCalls / GetCode.
	//
	// History is discovered by type assertion on the source provider rather than
	// taken as a fifth constructor argument, which is what [provider.SourceDiffer]
	// already does: a backend that reads a revision's bytes cannot necessarily
	// walk its history, and adding a parameter to [New] would break every existing
	// caller for a capability most of them do not have.
	if e.source == nil {
		return SearchHistoryResult{}, vacerr.New(
			vacerr.RepositoryNotFound,
			"search_history: this server was built with no source provider, so no repository can be read",
			map[string]any{"context": req.Context},
		)
	}
	history, ok := e.source.(provider.HistoryProvider)
	if !ok {
		return SearchHistoryResult{}, vacerr.New(
			vacerr.SourceHistoryUnavailable,
			"search_history: this server's source provider reads revisions but cannot walk their history",
			map[string]any{"context": req.Context},
		)
	}

	query := provider.HistoryQuery{
		Query:  req.Query,
		Symbol: req.Symbol,
		Path:   req.Path,
		Limit:  req.Limit,
	}

	// In the members' own order, one after another: separate members are separate
	// queries, so the order they are asked in is the only order the results have.
	var commits []Commit
	cited := make([][]evidence.Evidence, 0, len(members))
	for _, member := range members {
		found, herr := history.SearchHistory(ctx, member, query)
		if herr != nil {
			return SearchHistoryResult{}, herr
		}

		// Built empty rather than nil so a member that matched nothing still
		// carries evidence: "this version has no such commit" is an answer with an
		// empty citation list, not an answer with no citation list.
		citations := make([]evidence.Evidence, 0, len(found))
		for _, entry := range found {
			commits = append(commits, Commit{
				Repository: member.Repository,
				Commit:     entry.Commit,
				Path:       entry.Path,
				Author:     entry.Author,
				Timestamp:  entry.Timestamp,
				Message:    entry.Message,
			})
			// A commit's citation is the path it changed. There is no line range to
			// cite — the change is the whole file's difference at that commit — so
			// the range is the file itself rather than an invented span.
			citations = append(citations, evidence.At(entry.Path, 0, 0, ""))
		}
		cited = append(cited, citations)
	}

	// The members that were searched, not the whole workspace: a search narrowed
	// to one repository is answered in that one, and reporting the context's other
	// members beside it would claim they were looked in.
	searched := vacctx.Workspace{ID: workspace.ID, Members: members}
	return SearchHistoryResult{answer{searched, cited}, commits}, nil
}
