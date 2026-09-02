// History reads a repository's commit history as of the revision a context
// declares, never as of the checkout or the default branch.
//
// The walk is `git log <pinned commit>`, so a context pinned to an older commit
// cannot see the commits made after it. That is the whole point: the same
// question asked of two versions has to give two answers, and a history that
// reported everything reachable from HEAD would answer both of them the same
// way. A repository whose worktree sits on a later commit is the normal case,
// not a special one — nothing here checks the worktree out.
package git

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// defaultHistoryLimit bounds a query that did not ask for a limit.
//
// An unbounded `git log` over a large repository reads its whole history to
// answer a question the caller did not scope, so "no limit given" means this
// default rather than "everything". A caller that wants more says so.
const defaultHistoryLimit = 100

// historyRecordSeparator and historyFieldSeparator delimit `git log --format`
// output. They are ASCII control characters that cannot occur in a commit
// message, an author name or a path, so parsing never has to guess where a
// record ended — a message containing newlines, tabs or the word "commit" is
// still one record.
const (
	historyRecordSeparator = "\x1e" // RS
	historyFieldSeparator  = "\x1f" // US
)

// SearchHistory walks codeCtx's history as of the revision it pins and reports
// the commit-path occurrences that match req.
//
// Filters combine with AND, and a filter that matches nothing yields an empty
// result rather than being dropped: answering a wider question than the one
// asked is the failure mode this avoids.
func (p *Provider) SearchHistory(ctx context.Context, codeCtx vacctx.CodeContext, req provider.HistoryQuery) ([]provider.HistoryEntry, error) {
	limit, err := historyLimit(req.Limit)
	if err != nil {
		return nil, err
	}
	var pathFilter string
	if strings.TrimSpace(req.Path) != "" {
		cleaned, perr := validateHistoryPath(req.Path)
		if perr != nil {
			return nil, perr
		}
		pathFilter = cleaned
	}

	repo, ok := p.repositories[codeCtx.Repository]
	if !ok {
		return nil, vacerr.New(
			vacerr.RepositoryNotFound,
			fmt.Sprintf("context %q references repository %q, which is not configured", codeCtx.ID, codeCtx.Repository),
			map[string]any{"context": codeCtx.ID, "repository": codeCtx.Repository},
		)
	}

	// The pinned commit, resolved once. Everything below walks from THIS commit,
	// which is what keeps the answer inside the version that was asked about.
	revision, err := p.resolve(ctx, codeCtx, repo.Path)
	if err != nil {
		return nil, err
	}

	args := []string{
		"log",
		// Name every path the commit changed, so one entry per commit-path can be
		// built without a second command per commit.
		"--name-only",
		// No merge commits: a merge's file list is a property of how the branches
		// were combined, not of a change someone made, and reporting it would
		// attribute paths to a commit that did not edit them.
		"--no-merges",
		"--format=" + historyRecordSeparator + "%H" + historyFieldSeparator +
			"%an <%ae>" + historyFieldSeparator + "%aI" + historyFieldSeparator + "%B" + historyFieldSeparator,
	}
	if req.Symbol != "" {
		// Pickaxe: commits where the occurrence count of this exact string
		// changed. -S takes its operand attached, so a symbol beginning with a
		// dash cannot be read as an option.
		args = append(args, "-S"+req.Symbol)
	}
	// Read the revision as a value that cannot be taken for an option, then close
	// the option list entirely before any path operand.
	args = append(args, "--end-of-options", revision, "--")
	if pathFilter != "" {
		args = append(args, pathFilter)
	}

	out, err := gitOutput(ctx, repo.Path, args...)
	if err != nil {
		// A cancelled or timed-out walk is the caller's deadline, not a broken
		// repository: report it as itself so it is not classified as a bad query.
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		return nil, vacerr.New(
			vacerr.RepositoryNotFound,
			fmt.Sprintf("context %q: cannot read the history of repository %q at revision %s: %v",
				codeCtx.ID, codeCtx.Repository, revision, err),
			map[string]any{"context": codeCtx.ID, "repository": codeCtx.Repository, "revision": revision},
		)
	}

	return parseHistory(out, req.Query, pathFilter, limit), nil
}

// historyLimit validates the requested cap and applies the default.
func historyLimit(limit int) (int, error) {
	if limit < 0 {
		return 0, invalid(
			fmt.Sprintf("search_history: limit %d is negative; use 0 for the default bound", limit),
			map[string]any{"limit": limit},
		)
	}
	if limit == 0 {
		return defaultHistoryLimit, nil
	}
	return limit, nil
}

// validateHistoryPath is validatePath's history wording. The rules are the same
// — relative, inside the repository — but the message names the caller's tool.
func validateHistoryPath(filePath string) (string, error) {
	cleaned, err := validatePath(filePath)
	if err == nil {
		return cleaned, nil
	}
	var ve *vacerr.Error
	if ok := asVacErr(err, &ve); ok {
		return "", vacerr.New(ve.Code, strings.Replace(ve.Message, "get_code:", "search_history:", 1), ve.Details)
	}
	return "", err
}

// parseHistory turns `git log --name-only` output into commit-path entries.
//
// The message filter is applied HERE rather than as a `git log --grep` argument
// on purpose: --grep takes a regular expression, so a caller's literal text
// containing regex metacharacters would silently mean something else, and a
// pathological pattern would be the caller's to construct. A literal,
// case-insensitive substring test says exactly what it does.
func parseHistory(out, query, pathFilter string, limit int) []provider.HistoryEntry {
	needle := strings.ToLower(strings.TrimSpace(query))
	entries := make([]provider.HistoryEntry, 0, limit)

	for _, record := range strings.Split(out, historyRecordSeparator) {
		if strings.TrimSpace(record) == "" {
			continue
		}
		fields := strings.SplitN(record, historyFieldSeparator, 5)
		if len(fields) < 5 {
			continue // not a record this format produced
		}
		commit := strings.TrimSpace(fields[0])
		author := strings.TrimSpace(fields[1])
		timestamp := normalizeTimestamp(fields[2])
		message := strings.TrimRight(fields[3], "\n")

		if needle != "" && !strings.Contains(strings.ToLower(message), needle) {
			continue
		}

		// The paths follow the format block, one per line. A commit is reported
		// once per path it changed, so a multi-file commit keeps every file's
		// provenance instead of being attributed to whichever one git printed
		// first.
		for _, p := range strings.Split(fields[4], "\n") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			// With a path filter git already restricted the walk to commits that
			// touched it; this keeps the REPORTED path to the one that was asked
			// about, so a commit that also changed other files does not answer with
			// them.
			if pathFilter != "" && p != pathFilter {
				continue
			}
			if len(entries) >= limit {
				return entries
			}
			entries = append(entries, provider.HistoryEntry{
				Commit:    commit,
				Path:      p,
				Author:    author,
				Timestamp: timestamp,
				Message:   message,
			})
		}
	}
	return entries
}

// normalizeTimestamp re-formats git's author date as RFC3339 in UTC, so the same
// commit reports the same string whatever the reader's timezone is. Output git
// cannot parse is passed through rather than dropped: a malformed date is worth
// showing, and losing the whole entry over it would be worse.
func normalizeTimestamp(raw string) string {
	raw = strings.TrimSpace(raw)
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return raw
	}
	return t.UTC().Format(time.RFC3339)
}

// asVacErr is errors.As for *vacerr.Error, kept local so history's error
// rewriting does not pull a second errors import into this file's surface.
func asVacErr(err error, target **vacerr.Error) bool {
	if ve, ok := err.(*vacerr.Error); ok {
		*target = ve
		return true
	}
	return false
}
