package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/tc3oliver/version-aware-code-mcp/store"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// contextCreating is the state a context is recorded in when it is created.
//
// decision-4 defines the whole machine — CREATING, RESOLVING, PREPARING_SOURCE,
// INDEXING_SEARCH, INDEXING_GRAPH, VERIFYING, READY, and FAILED for any step
// that fails — and driving a context along it is the readiness lifecycle's job,
// not this command's. Creating a context resolves its revision and writes it
// down; the search index and the graph behind it do not exist yet. So this is
// the only state written here: a record that claimed READY would be one the
// query plane serves with nothing behind it.
const contextCreating = "CREATING"

const (
	// fullSHA is the length of the only kind of revision a record may carry. A
	// branch name moves and an abbreviation is ambiguous; a context pins a full
	// commit SHA or it is not a version context.
	fullSHA = 40

	// shortSHA is how much of it goes into the generated names below. They only
	// have to tell one context's artifacts from another's, not identify a
	// commit on their own.
	shortSHA = 12
)

const contextUsage = `Usage:
  vacmcp context create NAME --repo REPO --ref REF [--data-dir DIR]
                                                    pin a repository ref to a new context
  vacmcp context list [--data-dir DIR]              list the managed contexts
  vacmcp context status NAME [--data-dir DIR]       report one context
  vacmcp context verify NAME [--data-dir DIR]       re-check a context without changing it
  vacmcp context remove NAME [--data-dir DIR]       forget a context and delete its worktree`

// contextCommand dispatches `vacmcp context <subcommand>`.
//
// Like the repo commands these are the management plane, reachable from the CLI
// alone: decision-4 keeps them off the MCP tool surface so a coding agent cannot
// make the server create or delete the contexts it is being served.
//
// There is no update subcommand, and no flag anywhere below writes a revision
// into an existing record. A context is immutable: another revision of the same
// repository is another context ID, because every answer already given under
// this one was evidence about the revision it was created with.
func contextCommand(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("context: no subcommand given\n\n" + contextUsage)
	}
	switch subcommand := args[0]; subcommand {
	case "create":
		return contextCreate(args[1:], out)
	case "list":
		return contextList(args[1:], out)
	case "status":
		return contextStatus(args[1:], out)
	case "verify":
		return contextVerify(args[1:], out)
	case "remove":
		return contextRemove(args[1:], out)
	default:
		return fmt.Errorf("unknown context subcommand %q\n\n%s", subcommand, contextUsage)
	}
}

// contextCreate resolves a ref to a commit and records a context pinned to it.
//
// The ref is read exactly once, here. What lands in the record is the full
// commit SHA it resolved to, so the branch it came from can move, be rewritten
// or be deleted afterwards without this context changing.
//
// Creating over an existing context ID is refused whatever state it is in. That
// is the same rule `repo add` follows, and here it is what makes immutability a
// property of the code rather than a promise: no path in this file writes a
// revision into a record that already has one.
func contextCreate(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("vacmcp context create", flag.ExitOnError)
	repository := fs.String("repo", "", "managed repository to pin a revision of")
	ref := fs.String("ref", "", "branch, tag or commit to resolve once and pin")
	dataDir := fs.String("data-dir", "", "vacmcp data directory (default ~/.vacmcp)")
	id, err := parseRepoFlags(fs, args)
	if err != nil {
		return err
	}
	switch {
	case id == "":
		return errors.New("context create: NAME is required")
	case strings.TrimSpace(*repository) == "":
		return errors.New("context create: --repo is required")
	case strings.TrimSpace(*ref) == "":
		return errors.New("context create: --ref is required")
	}

	s, err := store.Open(*dataDir)
	if err != nil {
		return err
	}
	// Both names become path elements. Asking for the worktree path is what
	// puts the store's check on them in front of everything below, so a name
	// that is not a usable path element is refused before any git runs and
	// leaves nothing behind.
	if _, err := s.WorktreeDir(*repository, id); err != nil {
		return err
	}
	// The store's allowlist has to admit a dot, and the generated search ref
	// below has to be a legal git ref: ".." is the one sequence that is fine in
	// the first and forbidden in the second.
	if strings.Contains(id, "..") {
		return vacerr.New(
			vacerr.InvalidArgument,
			fmt.Sprintf("context create: context name %q cannot contain %q: it becomes part of a git ref", id, ".."),
			map[string]any{"context": id},
		)
	}
	if _, err := s.Context(id); err == nil {
		return vacerr.New(
			vacerr.InvalidArgument,
			fmt.Sprintf("context create: context %q is already managed in %s; a context is immutable, so another revision is another context name", id, s.Root()),
			map[string]any{"context": id},
		)
	}
	if _, err := s.Repository(*repository); err != nil {
		return err
	}
	repoDir, err := s.RepositoryDir(*repository)
	if err != nil {
		return err
	}

	revision, err := resolveRevision(context.Background(), repoDir, *repository, *ref)
	if err != nil {
		return err
	}
	record := store.Context{
		ID:         id,
		Repository: *repository,
		Branch:     searchRef(id, revision),
		Revision:   revision,
		GraphRef:   graphRef(*repository, id, revision),
		State:      contextCreating,
	}
	if err := s.PutContext(record); err != nil {
		return err
	}
	// The record goes down first because indexing is driven by the records: it
	// creates the search ref of every context the repository has, this one now
	// among them, and indexes them together.
	//
	// A failure here fails the command, and decision-4 is why: a context whose
	// source is not in the index is not partially usable, it is a context the
	// query plane would search and find nothing in. It stays in CREATING, which
	// is not a state the query plane serves.
	if err := indexRepository(context.Background(), s, record.Repository); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s\t%s\t%s\n", record.ID, record.State, record.Revision)
	return err
}

// contextList prints one line per managed context, ordered by ID.
func contextList(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("vacmcp context list", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "vacmcp data directory (default ~/.vacmcp)")
	_ = fs.Parse(args)

	s, err := store.Open(*dataDir)
	if err != nil {
		return err
	}
	contexts, err := s.Contexts()
	if err != nil {
		return err
	}
	for _, c := range contexts {
		if _, err := fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", c.ID, c.Repository, c.Revision, c.State); err != nil {
			return err
		}
	}
	return nil
}

// contextStatus reports one context, including the names it generated and where
// its worktree goes. Those are internal — nobody has to type them, and the query
// plane does not show them — but this is the management plane, where an operator
// diagnosing a context needs to know which ref and which graph are its.
func contextStatus(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("vacmcp context status", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "vacmcp data directory (default ~/.vacmcp)")
	id, err := parseRepoFlags(fs, args)
	if err != nil {
		return err
	}
	if id == "" {
		return errors.New("context status: NAME is required")
	}

	s, err := store.Open(*dataDir)
	if err != nil {
		return err
	}
	c, err := s.Context(id)
	if err != nil {
		return err
	}
	worktree, err := s.WorktreeDir(c.Repository, c.ID)
	if err != nil {
		return err
	}

	for _, row := range [][2]string{
		{"name", c.ID},
		{"repository", c.Repository},
		{"revision", c.Revision},
		{"state", c.State},
		{"search ref", c.Branch},
		{"graph ref", c.GraphRef},
		{"worktree", worktree},
		{"updated", c.UpdatedAt.Format(time.RFC3339)},
	} {
		if _, err := fmt.Fprintf(out, "%-12s%s\n", row[0], row[1]); err != nil {
			return err
		}
	}
	return nil
}

// contextVerify re-checks a context and writes nothing.
//
// It re-resolves the pinned revision in the repository's object database and
// requires the same commit back, which is what catches a clone that was deleted
// or replaced under a context. Not writing is the point: a verification that
// could update a record would be a way for a pinned revision to move, so it
// reports a disagreement as SOURCE_MISMATCH and stops rather than adopting what
// it found.
//
// This is deliberately a part of decision-4's verification, not all of it. The
// rest of it — the worktree's HEAD, the search ref in the index, the graph — is
// about artifacts that the lifecycle steps creating them are what can check,
// and none of those artifacts exists for a context in CREATING. Reporting on
// what is there is the honest half; reporting a context as verified because the
// checks nobody can run yet did not fail would not be.
func contextVerify(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("vacmcp context verify", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "vacmcp data directory (default ~/.vacmcp)")
	id, err := parseRepoFlags(fs, args)
	if err != nil {
		return err
	}
	if id == "" {
		return errors.New("context verify: NAME is required")
	}

	s, err := store.Open(*dataDir)
	if err != nil {
		return err
	}
	c, err := s.Context(id)
	if err != nil {
		return err
	}
	repoDir, err := s.RepositoryDir(c.Repository)
	if err != nil {
		return err
	}

	found, err := gitOutput(context.Background(), "-C", repoDir, "rev-parse", "--verify", "--end-of-options", c.Revision+"^{commit}")
	if err != nil {
		return vacerr.New(
			vacerr.RevisionNotFound,
			fmt.Sprintf("context verify: context %q pins revision %s, which repository %q cannot resolve: %v", c.ID, c.Revision, c.Repository, err),
			map[string]any{"context": c.ID, "repository": c.Repository, "revision": c.Revision},
		)
	}
	if found != c.Revision {
		return vacerr.NewSourceMismatch(c.Revision, found, map[string]any{"context": c.ID, "repository": c.Repository})
	}
	_, err = fmt.Fprintf(out, "%s\tOK\t%s\n", c.ID, c.Revision)
	return err
}

// contextRemove forgets a context and deletes the worktree it owns.
//
// Nothing but this context's own record and its own directory is touched: a
// worktree lives at worktrees/<repository>/<context>, so what is deleted is one
// subtree that no other context of the same repository is inside, and no other
// record is read or written.
//
// The directory is removed if it is there and skipped if it is not, which is
// what os.RemoveAll does on its own — a context whose worktree was never
// created, or was already cleaned up, still has to be removable.
func contextRemove(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("vacmcp context remove", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "vacmcp data directory (default ~/.vacmcp)")
	id, err := parseRepoFlags(fs, args)
	if err != nil {
		return err
	}
	if id == "" {
		return errors.New("context remove: NAME is required")
	}

	s, err := store.Open(*dataDir)
	if err != nil {
		return err
	}
	c, err := s.Context(id)
	if err != nil {
		return err
	}
	worktree, err := s.WorktreeDir(c.Repository, c.ID)
	if err != nil {
		return err
	}
	repoDir, err := s.RepositoryDir(c.Repository)
	if err != nil {
		return err
	}

	// The worktree goes first: a failure there leaves the record in place, so
	// the context is still managed and still removable. The other order would
	// leave a checkout nothing knows about.
	if err := os.RemoveAll(worktree); err != nil {
		return fmt.Errorf("context remove: cannot delete the worktree of %q at %s: %w", c.ID, worktree, err)
	}
	// Then the ref, then the record, then the index, and that order is what
	// keeps a half-finished removal safe: until the record is gone the context
	// is still wholly there, and once it is gone the query plane has no context
	// to search whatever the index still holds. A rebuild that fails after that
	// is reported rather than swallowed, because what it leaves behind is the
	// removed revision's source still in the shard.
	if c.Branch != "" {
		if err := dropSearchRef(context.Background(), repoDir, c); err != nil {
			return err
		}
	}
	if err := s.DeleteContext(c.ID); err != nil {
		return err
	}
	if err := indexRepository(context.Background(), s, c.Repository); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s\tREMOVED\n", c.ID)
	return err
}

// resolveRevision turns a ref into the full commit SHA a context pins forever.
//
// The clone a repository is added as has no working tree, so every branch but
// the one the remote's HEAD points at is in it as a remote-tracking ref only:
// git resolves `main` but not `release/2.x`. The ref as given is therefore tried
// first — which is what makes a tag, an abbreviated SHA and a full SHA resolve
// to themselves — and only when that fails is origin/<ref> tried, where it can
// no longer shadow anything the user might have meant literally.
func resolveRevision(ctx context.Context, repoDir, repository, ref string) (string, error) {
	for _, candidate := range []string{ref, "origin/" + ref} {
		// --end-of-options keeps a ref beginning with a dash a revision git
		// fails to find rather than an option git obeys, and ^{commit} both
		// peels a tag to its commit and refuses anything that is not one, so a
		// tree or a blob cannot become a context's revision.
		revision, err := gitOutput(ctx, "-C", repoDir, "rev-parse", "--verify", "--end-of-options", candidate+"^{commit}")
		if err != nil {
			continue
		}
		if len(revision) != fullSHA {
			return "", fmt.Errorf("context create: git resolved %q in repository %q to %q, which is not a full commit SHA", ref, repository, revision)
		}
		return revision, nil
	}
	return "", vacerr.New(
		vacerr.RevisionNotFound,
		fmt.Sprintf("context create: cannot resolve ref %q in repository %q: no commit, tag or branch of that name", ref, repository),
		map[string]any{"repository": repository, "ref": ref},
	)
}

// searchRef is the ref inside the clone that carries this context's source for
// the search index, and the branch the query plane searches. It is namespaced
// under vacmcp/ so it can never collide with a branch the repository really has,
// and it carries the short SHA so two contexts of one repository are never one
// ref. Nothing else names it: a user creates a context without knowing it
// exists.
func searchRef(contextID, revision string) string {
	return "vacmcp/" + contextID + "-" + revision[:shortSHA]
}

// graphRef is the graph project backing this context. The repository name is in
// it because graph projects share one namespace across every repository in a
// data directory, where search refs are scoped to a clone already.
func graphRef(repository, contextID, revision string) string {
	return "vacmcp-" + repository + "-" + contextID + "-" + revision[:shortSHA]
}

// gitOutput runs one git command and returns its standard output, trimmed. It
// is runGit for the commands whose answer, rather than their success, is what is
// wanted.
//
// Standard error is kept separate rather than combined: the answer here becomes
// a context's pinned revision, and a warning git prints — an ambiguous ref
// name, a hint — must not be able to end up inside it.
func gitOutput(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err == nil {
		return strings.TrimSpace(string(out)), nil
	}
	if message := strings.TrimSpace(stderr.String()); message != "" {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, message)
	}
	return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
}
