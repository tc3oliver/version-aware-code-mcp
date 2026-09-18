// Package zoekt searches code through a Zoekt web server, confined to the
// repository and branch a context names.
//
// Zoekt indexes several branches of a repository into one shard and picks the
// version at query time, so the whole of version isolation here is that every
// query the adapter sends carries a repo: and a branch: filter. There is no
// code path that searches without them.
//
// The caller's query is across a trust boundary and Zoekt's query language has
// grouping and an or operator, so a query that closed the group the adapter
// wraps it in would put its remainder outside those filters and search every
// branch. Two things stop that: the query is refused unless every parenthesis
// in it is closed inside it, and a match is dropped unless the repository and
// branch it reports are the ones the context asked for.
//
// The engine is reached over its JSON API rather than through its Go packages:
// the adapter is a client of a service the deployment already runs, so it costs
// this module no dependency on Zoekt.
package zoekt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/internal/deadline"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// requestBudget bounds a single request to Zoekt. Zoekt stops its own search
// after 20s, so this only has to cover a server that accepts the connection and
// then never answers; a tool call must not hang on it forever.
//
// The duration is unchanged from the http.Client timeout it replaces. What
// changed is who owns it: a client-level timeout produces an error
// indistinguishable from a network failure, so a Zoekt that went quiet was
// reported as a Zoekt that was unavailable. It is now a context budget, which
// [deadline.Ended] can tell apart from the caller's own deadline and from a
// server that really is unreachable.
//
// It is a var rather than a const for the reason cmd/vacmcp's goos is one: a
// test has to be able to stand on the branch it guards without waiting out a
// budget meant for a wedged process. Nothing outside a test ever assigns to it.
var requestBudget = 30 * time.Second

// maxFiles caps how many files one search may return. The query comes from a
// caller who is free to make it match everything, and the whole result is held
// in memory and sent on as evidence.
const maxFiles = 100

// Provider is the Zoekt implementation of [provider.SearchProvider].
type Provider struct {
	url    string
	list   string
	client *http.Client
}

// New returns a Provider talking to the Zoekt web server configured under
// providers.zoekt.url. That server must have its JSON API enabled
// (zoekt-webserver -rpc).
func New(cfg *config.Config) *Provider {
	base := strings.TrimRight(cfg.Providers.Zoekt.URL, "/")
	return &Provider{
		url:  base + "/api/search",
		list: base + "/api/list",
		// No Timeout: the budget is the request context's, set per call in post
		// and IndexedBranches. Two owners of the same deadline would race, and
		// only one of them can say whose deadline it was.
		client: &http.Client{},
	}
}

// IndexedBranches returns the branches Zoekt has in its index for repository,
// which is how a caller finds out whether something is searchable at all rather
// than what is in it.
//
// It is here rather than beside the code that builds the index because this is
// where the client of the engine lives: the same timeout, and a failure that
// comes back as [vacerr.SearchProviderUnavailable] like every other one this
// engine produces. A repository the server does not have is no error — it is an
// empty list, which is the answer.
func (p *Provider) IndexedBranches(ctx context.Context, repository string) ([]string, error) {
	// Anchored and quoted for the same reason the search filter is: repo: is a
	// regexp, and a name is meant to select one repository rather than
	// everything it is a substring of.
	body, err := json.Marshal(struct{ Q string }{"repo:^" + regexp.QuoteMeta(repository) + "$"})
	if err != nil {
		return nil, p.listUnavailable(repository, err)
	}
	ctx, cancel := deadline.With(ctx, requestBudget, deadline.Zoekt, "list")
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.list, bytes.NewReader(body))
	if err != nil {
		return nil, p.listUnavailable(repository, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		if ended := deadline.Ended(ctx); ended != nil {
			return nil, ended
		}
		return nil, p.listUnavailable(repository, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, p.listUnavailable(repository, fmt.Errorf("http %s", resp.Status))
	}

	var reply struct {
		List struct {
			Repos []struct {
				Repository struct {
					Name     string
					Branches []struct{ Name string }
				}
			}
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		return nil, p.listUnavailable(repository, err)
	}

	var branches []string
	for _, repo := range reply.List.Repos {
		// The filter is a regexp and the answer is checked against the name
		// asked about anyway: what a caller wants to know is about this
		// repository, not about one Zoekt matched loosely.
		if repo.Repository.Name != repository {
			continue
		}
		for _, branch := range repo.Repository.Branches {
			branches = append(branches, branch.Name)
		}
	}
	return branches, nil
}

func (p *Provider) listUnavailable(repository string, cause error) *vacerr.Error {
	return vacerr.New(
		vacerr.SearchProviderUnavailable,
		fmt.Sprintf("repository %q: zoekt at %s is not available: %v", repository, p.list, cause),
		map[string]any{"repository": repository, "url": p.list},
	)
}

// Search returns the matches for query inside the repository and branch codeCtx
// names, in Zoekt's ranked order. A query that matches nothing there is no
// error: it is the answer, and on the branch that does not have a symbol it is
// the whole point.
//
// Every failure is a *[vacerr.Error]. A query the caller cannot expect to be
// run — empty, or with parentheses that do not close inside it, or one Zoekt
// cannot parse — is [vacerr.InvalidArgument]. Everything about the engine
// itself, from a refused connection to an unreadable response, is
// [vacerr.SearchProviderUnavailable].
func (p *Provider) Search(ctx context.Context, codeCtx vacctx.CodeContext, query provider.SearchQuery) ([]provider.SearchResult, error) {
	scoped, err := scopedQuery(codeCtx, query.Query)
	if err != nil {
		return nil, err
	}

	files, err := p.post(ctx, codeCtx, scoped)
	if err != nil {
		return nil, err
	}

	var results []provider.SearchResult
	for _, file := range files {
		// The last line of defence for version isolation: a match is only an
		// answer for this context if Zoekt itself reports it in this
		// repository and on this branch. branch: is a substring match, so
		// release/v1 also selects a release/v1-hotfix, and this is where that
		// is settled.
		if file.Repository != codeCtx.Repository || !slices.Contains(file.Branches, codeCtx.Branch) {
			continue
		}
		for _, match := range file.LineMatches {
			// A match on the file's name has no line to cite — Zoekt reports
			// line 0 for it — and a result that cannot be cited is not one this
			// server may hand back.
			if match.FileName {
				continue
			}
			results = append(results, provider.SearchResult{
				Path:    file.FileName,
				Line:    match.LineNumber,
				Snippet: strings.TrimRight(string(match.Line), "\r\n"),
			})
		}
	}
	return results, nil
}

// scopedQuery wraps the caller's query in the context's repository and branch.
// The repository name is matched whole and literally: it is a regexp filter in
// Zoekt, so a name like example.com/backend would otherwise match more
// repositories than the one the context names.
//
// The spaces inside the wrapping parentheses are load bearing. Zoekt reads
// "(sym:Process)" as one regexp atom — a search for the literal text
// "sym:Process" — and only "( sym:Process )" as a group, which is what leaves
// the caller the sym:, file: and lang: filters of doc-1 §12.
func scopedQuery(codeCtx vacctx.CodeContext, query string) (string, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return "", invalid("search_code: query is required", map[string]any{"context": codeCtx.ID})
	}
	if !balanced(query) {
		return "", invalid(
			fmt.Sprintf("search_code: query %q has a parenthesis it does not close", query),
			map[string]any{"context": codeCtx.ID, "query": query},
		)
	}
	return fmt.Sprintf("repo:^%s$ branch:%s ( %s )", regexp.QuoteMeta(codeCtx.Repository), codeCtx.Branch, query), nil
}

// balanced reports whether every parenthesis in query is closed within it.
//
// This is the version isolation check, not a syntax check. The query is sent
// wrapped in parentheses of the adapter's own; a query holding a ")" it never
// opened closes that group early, and everything it writes after it is a
// sibling of the repo: and branch: filters instead of being constrained by
// them — `a) or (b` would search every branch of every repository for b. A
// backslash escapes the next character, which is how Zoekt reads a literal
// parenthesis too.
func balanced(query string) bool {
	depth := 0
	for i := 0; i < len(query); i++ {
		switch query[i] {
		case '\\':
			i++
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

// fileMatch is the part of Zoekt's search response this adapter reads: where a
// match is, and which repository and branches it belongs to. Line is a JSON
// base64 string on the wire, which is what []byte decodes from.
type fileMatch struct {
	FileName    string
	Repository  string
	Branches    []string
	LineMatches []struct {
		Line       []byte
		LineNumber int
		FileName   bool
	}
}

// post runs one search and returns the files it matched.
func (p *Provider) post(ctx context.Context, codeCtx vacctx.CodeContext, query string) ([]fileMatch, error) {
	body, err := json.Marshal(struct {
		Q    string
		Opts struct{ MaxDocDisplayCount int }
	}{Q: query, Opts: struct{ MaxDocDisplayCount int }{maxFiles}})
	if err != nil {
		return nil, p.unavailable(codeCtx, query, err)
	}

	ctx, cancel := deadline.With(ctx, requestBudget, deadline.Zoekt, "search")
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(body))
	if err != nil {
		return nil, p.unavailable(codeCtx, query, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		// The migration: a request that ran out of time used to arrive here as
		// an unreachable engine. A budget that expired is this server's decision
		// and says so; a caller that gave up is returned as itself; only a Zoekt
		// that could not be reached or would not answer is still unavailable.
		if ended := deadline.Ended(ctx); ended != nil {
			return nil, ended
		}
		return nil, p.unavailable(codeCtx, query, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		var failure struct{ Error string }
		_ = json.NewDecoder(resp.Body).Decode(&failure)
		// Zoekt answers 400 when it cannot parse the query, and the query is
		// the caller's. Reporting that as an unavailable engine would send an
		// operator looking at a server that is working perfectly.
		if resp.StatusCode == http.StatusBadRequest {
			return nil, invalid(
				fmt.Sprintf("search_code: zoekt cannot run this query: %s", failure.Error),
				map[string]any{"context": codeCtx.ID, "query": query},
			)
		}
		return nil, p.unavailable(codeCtx, query, fmt.Errorf("http %s: %s", resp.Status, failure.Error))
	}

	var reply struct {
		Result struct{ Files []fileMatch }
	}
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		return nil, p.unavailable(codeCtx, query, err)
	}
	return reply.Result.Files, nil
}

func (p *Provider) unavailable(codeCtx vacctx.CodeContext, query string, cause error) *vacerr.Error {
	return vacerr.New(
		vacerr.SearchProviderUnavailable,
		fmt.Sprintf("context %q: zoekt at %s is not available: %v", codeCtx.ID, p.url, cause),
		map[string]any{"context": codeCtx.ID, "url": p.url, "query": query},
	)
}

func invalid(message string, details map[string]any) *vacerr.Error {
	return vacerr.New(vacerr.InvalidArgument, message, details)
}
