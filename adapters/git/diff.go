package git

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// Diff compares one file between the revisions from and to declare, which is
// [provider.SourceDiffer] on top of the reading this adapter already does. Both
// revisions are pinned to their full SHA first, by the same resolve the read
// path uses, so the comparison cannot straddle a ref that moved between the two
// sides being looked up — and so a repository or revision that is not there
// fails exactly as it does for a read.
//
// Both contexts must name the same repository. Two repositories have no common
// history, so a diff across them is not a comparison that means anything; it is
// [vacerr.InvalidArgument] rather than a diff of unrelated bytes.
//
// The result is structured, never diff text, and it is one file's: a path
// covering several changed files is refused rather than reported as though the
// first of them were the answer.
func (p *Provider) Diff(ctx context.Context, from, to vacctx.CodeContext, req provider.SourceDiffRequest) (*provider.SourceDiff, error) {
	cleanPath, err := validatePath(req.Path)
	if err != nil {
		return nil, err
	}
	if from.Repository != to.Repository {
		return nil, invalid(
			fmt.Sprintf("compare_code: contexts %q and %q name different repositories (%q and %q), which have no shared history to compare", from.ID, to.ID, from.Repository, to.Repository),
			map[string]any{"from_context": from.ID, "to_context": to.ID, "from_repository": from.Repository, "to_repository": to.Repository},
		)
	}

	repo, ok := p.repositories[from.Repository]
	if !ok {
		return nil, vacerr.New(
			vacerr.RepositoryNotFound,
			fmt.Sprintf("contexts %q and %q reference repository %q, which is not configured", from.ID, to.ID, from.Repository),
			map[string]any{"from_context": from.ID, "to_context": to.ID, "repository": from.Repository},
		)
	}

	fromRevision, err := p.resolve(ctx, from, repo.Path)
	if err != nil {
		return nil, err
	}
	toRevision, err := p.resolve(ctx, to, repo.Path)
	if err != nil {
		return nil, err
	}

	// Both revisions are full SHAs by now, so they start with a hex digit and
	// need no protection against being read as a flag; the path is separated by
	// "--" anyway. --no-renames is deliberate: a file that exists on one side
	// only is that path being added or removed, not the same content wearing
	// another name, and rename detection would report the compared path as
	// unchanged elsewhere. --no-ext-diff, --no-textconv and --no-color keep the
	// machine's git configuration from replacing the output with a program's,
	// a filter's, or the same output wrapped in escape sequences.
	out, err := gitOutput(ctx, repo.Path, "diff",
		"--no-color", "--no-ext-diff", "--no-textconv", "--no-renames",
		fromRevision, toRevision, "--", cleanPath)
	if err != nil {
		return nil, vacerr.New(
			vacerr.RepositoryNotFound,
			fmt.Sprintf("compare_code: repository %q cannot compare %s between %s and %s: %v", from.Repository, cleanPath, fromRevision, toRevision, err),
			map[string]any{"repository": from.Repository, "path": cleanPath, "from_revision": fromRevision, "to_revision": toRevision},
		)
	}
	if out == "" {
		return p.emptyDiff(ctx, repo.Path, cleanPath, fromRevision, toRevision)
	}
	return parseDiff(out, cleanPath, fromRevision, toRevision)
}

// emptyDiff answers the case git says nothing about. It prints nothing both
// when the two revisions hold identical content for the path and when neither
// revision has the path at all, and those are not the same answer: one is
// UNCHANGED, the other is a comparison of a file that was never there, which is
// [vacerr.InvalidArgument] rather than a reassuring "nothing changed".
//
// One ls-tree on the from side settles it, as it does for a failed read: if the
// path is absent there and the diff is empty, it is absent on the to side too —
// a path present only on the to side is an added file, which git does print.
func (p *Provider) emptyDiff(ctx context.Context, repoPath, filePath, fromRevision, toRevision string) (*provider.SourceDiff, error) {
	entry, err := gitLine(ctx, repoPath, "ls-tree", "--name-only", fromRevision, "--", filePath)
	if err != nil || entry == "" {
		return nil, invalid(
			fmt.Sprintf("compare_code: neither revision %s nor %s has file %s", fromRevision, toRevision, filePath),
			map[string]any{"path": filePath, "from_revision": fromRevision, "to_revision": toRevision},
		)
	}
	return &provider.SourceDiff{Path: filePath, Change: provider.ChangeUnchanged}, nil
}

// parseDiff turns git's unified diff into the structured one. The header lines
// say whether the path was added, deleted or is binary, and everything after a
// hunk header belongs to that hunk until the next one.
//
// Only column zero is read as diff structure, which is unambiguous: every line
// of a hunk's content carries a marker character first, so a source line that
// happens to read "diff --git" or "@@" arrives with a "+", "-" or " " in front
// of it and cannot be mistaken for the diff's own syntax.
func parseDiff(out, filePath, fromRevision, toRevision string) (*provider.SourceDiff, error) {
	diff := &provider.SourceDiff{Path: filePath, Change: provider.ChangeModified}
	files, inHunk := 0, false

	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			files++
			inHunk = false
		case strings.HasPrefix(line, "@@"):
			hunk, err := parseHunkHeader(line, filePath)
			if err != nil {
				return nil, err
			}
			diff.Hunks = append(diff.Hunks, hunk)
			inHunk = true
		case inHunk:
			// "\ No newline at end of file" is a note about the line above it,
			// not a line of either revision.
			if strings.HasPrefix(line, `\`) {
				continue
			}
			hunk := &diff.Hunks[len(diff.Hunks)-1]
			hunk.Lines = append(hunk.Lines, parseDiffLine(line))
		case strings.HasPrefix(line, "new file mode "):
			diff.Change = provider.ChangeAdded
		case strings.HasPrefix(line, "deleted file mode "):
			diff.Change = provider.ChangeRemoved
		case strings.HasPrefix(line, "Binary files "):
			// Binary content has no lines to report, and git prints no hunks
			// for it, so there is nothing here to parse as text.
			diff.Binary = true
		}
	}

	if files != 1 {
		return nil, invalid(
			fmt.Sprintf("compare_code: path %q covers %d changed files between %s and %s, compare one file at a time", filePath, files, fromRevision, toRevision),
			map[string]any{"path": filePath, "changed_files": files, "from_revision": fromRevision, "to_revision": toRevision},
		)
	}
	return diff, nil
}

// parseHunkHeader reads "@@ -oldStart,oldLines +newStart,newLines @@" and the
// optional heading git puts after it.
func parseHunkHeader(line, filePath string) (provider.DiffHunk, error) {
	ranges, _, ok := strings.Cut(strings.TrimPrefix(line, "@@ "), " @@")
	if !ok {
		return provider.DiffHunk{}, unreadableDiff(line, filePath)
	}
	oldRange, newRange, ok := strings.Cut(ranges, " +")
	if !ok || !strings.HasPrefix(oldRange, "-") {
		return provider.DiffHunk{}, unreadableDiff(line, filePath)
	}

	oldStart, oldLines, err := parseHunkRange(strings.TrimPrefix(oldRange, "-"))
	if err != nil {
		return provider.DiffHunk{}, unreadableDiff(line, filePath)
	}
	newStart, newLines, err := parseHunkRange(newRange)
	if err != nil {
		return provider.DiffHunk{}, unreadableDiff(line, filePath)
	}
	return provider.DiffHunk{OldStart: oldStart, OldLines: oldLines, NewStart: newStart, NewLines: newLines}, nil
}

// parseHunkRange reads one side of a hunk header. git writes a side spanning a
// single line as just its start, with no count.
func parseHunkRange(text string) (start, lines int, err error) {
	startText, linesText, hasCount := strings.Cut(text, ",")
	if start, err = strconv.Atoi(startText); err != nil {
		return 0, 0, err
	}
	if !hasCount {
		return start, 1, nil
	}
	if lines, err = strconv.Atoi(linesText); err != nil {
		return 0, 0, err
	}
	return start, lines, nil
}

// parseDiffLine reads one line of a hunk, stripping the single marker character
// that says which revisions it belongs to.
func parseDiffLine(line string) provider.DiffLine {
	switch {
	case strings.HasPrefix(line, "+"):
		return provider.DiffLine{Kind: provider.LineAdded, Content: line[1:]}
	case strings.HasPrefix(line, "-"):
		return provider.DiffLine{Kind: provider.LineRemoved, Content: line[1:]}
	default:
		// An unchanged line is written with a leading space, and an unchanged
		// empty line is that space alone.
		return provider.DiffLine{Kind: provider.LineContext, Content: strings.TrimPrefix(line, " ")}
	}
}

// unreadableDiff is the adapter admitting it cannot read what git handed back.
// Guessing at a hunk header would put line numbers on the result that the caller
// would go on to cite, so the comparison fails instead.
func unreadableDiff(line, filePath string) *vacerr.Error {
	return vacerr.New(
		vacerr.RepositoryNotFound,
		fmt.Sprintf("compare_code: git diff of %s produced a hunk header this adapter cannot read: %q", filePath, line),
		map[string]any{"path": filePath, "line": line},
	)
}
