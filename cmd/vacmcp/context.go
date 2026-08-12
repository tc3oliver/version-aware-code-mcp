package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/tc3oliver/version-aware-code-mcp/store"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// The states a context record carries, in the order decision-4 gives them.
//
// Each one names what is happening to the context, so a record read while a
// create is running — or left behind by one that was killed — says which stage
// it is in. READY is the only state the query plane serves and the only one a
// context reaches with all of its artifacts built and checked; FAILED is where
// a context that failed any stage stops.
const (
	contextCreating        = "CREATING"
	contextResolving       = "RESOLVING"
	contextPreparingSource = "PREPARING_SOURCE"
	contextIndexingSearch  = "INDEXING_SEARCH"
	contextIndexingGraph   = "INDEXING_GRAPH"
	contextVerifying       = "VERIFYING"
	contextReady           = "READY"
	contextFailed          = "FAILED"
)

// contextLifecycle is the order a context goes through: every state's only
// successor is the next element. READY is last, and FAILED is deliberately not
// in it at all — the two terminal states, which nothing follows.
var contextLifecycle = []string{
	contextCreating,
	contextResolving,
	contextPreparingSource,
	contextIndexingSearch,
	contextIndexingGraph,
	contextVerifying,
	contextReady,
}

// advances reports whether a context may move from one state to the other.
//
// The machine only goes forwards, one state at a time, and out of READY or
// FAILED it does not go at all: a context that is being served must not be
// walked back into a state where its artifacts are being rebuilt under it, and
// re-running a failed create is `context retry` creating the lifecycle again
// rather than a record quietly reverting.
func advances(from, to string) bool {
	at := slices.Index(contextLifecycle, from)
	if at < 0 || from == contextReady {
		return false
	}
	// Any stage can fail; otherwise the next state is the only move.
	return to == contextFailed || to == contextLifecycle[at+1]
}

// advance moves c to the next state and writes it down before the stage it
// names runs.
//
// Persisting first is what makes the record answer where a create is, and where
// one that was killed got to: a state written only after a stage succeeded
// would leave every interrupted context looking like it had never started.
func advance(s *store.Store, c *store.Context, next string) error {
	if !advances(c.State, next) {
		return fmt.Errorf("context %q: cannot move from %s to %s", c.ID, c.State, next)
	}
	c.State = next
	return s.PutContext(*c)
}

// fail records that the context stopped in the stage it is in and returns the
// cause it stopped for.
//
// The record is what keeps a failure observable — a context nothing built is
// still listed, still inspectable and still removable, and is one `context
// retry` can later take over — while the error is still what the command exits
// with. A store that cannot even be written is reported alongside the cause
// rather than instead of it, since the cause is what the user has to read.
func fail(s *store.Store, c *store.Context, cause error) error {
	if err := advance(s, c, contextFailed); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// readyContext returns the context id only if it is READY.
//
// This is the whole of what the query plane may read: a context that is being
// built, or one that failed, is not half a context to be served with whatever
// of it exists — decision-4 makes it indistinguishable from a context that is
// not there, down to the error. So a non-READY id comes back as exactly the
// [vacerr.ContextNotFound] an unknown one does, with no new code and nothing in
// it that says the difference.
func readyContext(s *store.Store, id string) (store.Context, error) {
	c, err := s.Context(id)
	if err != nil {
		return store.Context{}, err
	}
	if c.State != contextReady {
		return store.Context{}, vacerr.New(
			vacerr.ContextNotFound,
			fmt.Sprintf("store: context %q is not managed in %s", id, s.Root()),
			map[string]any{"context": id},
		)
	}
	return c, nil
}

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
  vacmcp context retry NAME [--data-dir DIR]        rebuild a context that did not reach READY
  vacmcp context remove NAME [--data-dir DIR]       forget a context, its worktree and its graph`

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
	case "retry":
		return contextRetry(args[1:], out)
	case "remove":
		return contextRemove(args[1:], out)
	default:
		return fmt.Errorf("unknown context subcommand %q\n\n%s", subcommand, contextUsage)
	}
}

// contextCreate resolves a ref to a commit, records a context pinned to it and
// drives it to READY.
//
// The ref is read exactly once, here. What lands in the record is the full
// commit SHA it resolved to, so the branch it came from can move, be rewritten
// or be deleted afterwards without this context changing.
//
// Resolution is the RESOLVING stage, and it runs before the record exists
// because it is what the record is made of: the pinned revision and the search
// and graph names derived from it. A ref that does not resolve therefore
// records nothing at all, which is what leaves the same command runnable again
// with the ref the user meant — a FAILED record carrying no revision would only
// be one they had to remove first, and one nothing could ever retry, since the
// ref that failed is not part of a context.
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
	worktree, err := s.WorktreeDir(*repository, id)
	if err != nil {
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
	// Everything below reads and writes the repository's clone, its search
	// index and the records of its other contexts, so it is one operation on
	// that repository and runs alone. The ref is resolved inside the lock too: a
	// fetch running underneath it is exactly what would let this pin a commit
	// that is being replaced.
	return withRepositoryLock(s, *repository, func() error {
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

		ctx := context.Background()
		revision, err := resolveRevision(ctx, repoDir, *repository, *ref)
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
		// The record first, the artifacts after: a checkout, a graph or a search
		// ref that outlived a failed create would be artifacts no record names
		// and no command can remove. This way a create that stops half way
		// leaves a context that is still managed, still listed and still
		// removable, in a state that is not one the query plane serves.
		if err := s.PutContext(record); err != nil {
			return err
		}

		// RESOLVING is entered and left in the same breath: the revision was
		// resolved above, before there was a record to write it into. The state
		// exists all the same, because the order of the machine is what makes a
		// stage impossible to skip.
		if err := advance(s, &record, contextResolving); err != nil {
			return err
		}
		if err := buildContext(ctx, s, &record, repoDir, worktree); err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "%s\t%s\t%s\n", record.ID, record.State, record.Revision)
		return err
	})
}

// buildContext takes a record that has its revision pinned and drives it to
// READY, one stage at a time, each one written down before it runs and none of
// them optional.
//
// It is the whole of what a context is made of, and it is one function because
// `context create` and `context retry` build the same context out of the same
// stages: a retry that rebuilt some other way would be a second definition of
// what READY means, and the two would drift.
//
// The caller holds the repository's lock and has already advanced c to
// RESOLVING. A context that fails any stage is FAILED rather than a context with
// some of its artifacts that the query plane would serve anyway.
func buildContext(ctx context.Context, s *store.Store, c *store.Context, repoDir, worktree string) error {
	for _, stage := range []struct {
		state string
		run   func() error
	}{
		{contextPreparingSource, func() error { return prepareSource(ctx, repoDir, worktree, *c) }},
		// Indexing is driven by the records: it creates the search ref of every
		// context the repository has, this one now among them, and indexes them
		// together.
		{contextIndexingSearch, func() error { return indexRepository(ctx, s, c.Repository) }},
		{contextIndexingGraph, func() error { return indexGraph(ctx, worktree, *c) }},
		{contextVerifying, func() error { return verifyContext(ctx, s, *c) }},
	} {
		if err := advance(s, c, stage.state); err != nil {
			return err
		}
		if err := stage.run(); err != nil {
			return fail(s, c, err)
		}
	}
	return advance(s, c, contextReady)
}

// contextRetry rebuilds the artifacts of a context that did not reach READY.
//
// It is what makes a failure recoverable without an operator going into the
// data directory by hand: whatever the interrupted run left — no worktree, a
// worktree on the wrong commit, a graph that was half indexed, a search ref
// that was never created — is thrown away and made again from the revision the
// record already pins. The revision is never re-resolved. A retry fixes a
// context whose artifacts were not built, and there is no ref in it to move a
// context onto another commit.
//
// Rebuilding from the first stage rather than resuming where it stopped is
// deliberate. The states say which stage a run was in, not how far into it it
// got, so telling a half-written worktree or a half-indexed graph from a whole
// one would mean asking every artifact a question the lifecycle already answers
// by making it again — and the answer would have to be right every time, where
// rebuilding is right by construction.
//
// Any state but READY is retried, which is what recovers a context whose
// process was killed mid-stage: the record it left says PREPARING_SOURCE or
// INDEXING_GRAPH rather than FAILED, and it is stuck there forever otherwise.
// READY is refused, because rebuilding a context the query plane is serving
// would take its source out from under the answers being given from it. Holding
// the repository's lock is what makes that check mean something: a retry
// started while a create is still running waits for it, and then finds the
// READY it produced.
func contextRetry(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("vacmcp context retry", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "vacmcp data directory (default ~/.vacmcp)")
	id, err := parseRepoFlags(fs, args)
	if err != nil {
		return err
	}
	if id == "" {
		return errors.New("context retry: NAME is required")
	}

	s, err := store.Open(*dataDir)
	if err != nil {
		return err
	}
	// Read once outside the lock only to learn which repository's lock to take;
	// every decision below is made again from the record as it is under it.
	known, err := s.Context(id)
	if err != nil {
		return err
	}

	return withRepositoryLock(s, known.Repository, func() error {
		c, err := s.Context(id)
		if err != nil {
			return err
		}
		if c.State == contextReady {
			return vacerr.New(
				vacerr.InvalidArgument,
				fmt.Sprintf("context retry: context %q is already %s; `context verify` re-checks it and `context remove` takes it away, but rebuilding it would take the source away from the answers being given out of it", id, contextReady),
				map[string]any{"context": id},
			)
		}
		worktree, err := s.WorktreeDir(c.Repository, c.ID)
		if err != nil {
			return err
		}
		repoDir, err := s.RepositoryDir(c.Repository)
		if err != nil {
			return err
		}
		if _, err := s.Repository(c.Repository); err != nil {
			return err
		}

		ctx := context.Background()
		// The partial artifacts go before anything is built, and the record
		// stays where it is if that fails: a cleanup that could not finish
		// leaves a context that is still not READY and still retryable, rather
		// than one that is being rebuilt on top of the leftovers of the last
		// attempt. The search ref needs nothing here — indexing re-asserts every
		// record's ref from the record, and the record is immutable.
		if err := discardSource(ctx, repoDir, worktree, c); err != nil {
			return err
		}

		// The one place the machine is re-entered. It is not a transition —
		// advance only ever goes forwards, and out of FAILED it does not go at
		// all — it is this context's lifecycle starting again from the stage
		// after the revision it already pins. The write happens before any
		// artifact is rebuilt, for the same reason every other state does: a
		// retry that is itself interrupted has to be retryable too.
		c.State = contextResolving
		if err := s.PutContext(c); err != nil {
			return err
		}
		if err := buildContext(ctx, s, &c, repoDir, worktree); err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "%s\t%s\t%s\n", c.ID, c.State, c.Revision)
		return err
	})
}

// verifyContext is decision-4's verification: the six checks a context has to
// pass to be READY, and the same six `context verify` re-runs afterwards.
//
// It is one function because it is one question — is every artifact this record
// names really there and really this revision — and asking it in two places
// with two implementations is how the answers drift apart. Nothing here writes:
// verification reports what it found, and a record that disagrees with its
// artifacts is a failure rather than something to quietly adopt.
//
// Every check asks the artifact itself. A create that once succeeded is not
// evidence: a clone can be replaced, a worktree checked out by hand, a shard
// deleted and a graph dropped by anything else driving the same CBM store.
func verifyContext(ctx context.Context, s *store.Store, c store.Context) error {
	repoDir, err := s.RepositoryDir(c.Repository)
	if err != nil {
		return err
	}
	// The pinned revision is still a commit this repository has, and still the
	// same one. Re-resolving it is what catches a clone that was deleted or
	// replaced under a context.
	found, err := gitOutput(ctx, "-C", repoDir, "rev-parse", "--verify", "--end-of-options", c.Revision+"^{commit}")
	if err != nil {
		return vacerr.New(
			vacerr.RevisionNotFound,
			fmt.Sprintf("context %q pins revision %s, which repository %q cannot resolve: %v", c.ID, c.Revision, c.Repository, err),
			map[string]any{"context": c.ID, "repository": c.Repository, "revision": c.Revision},
		)
	}
	if found != c.Revision {
		return vacerr.NewSourceMismatch(c.Revision, found, map[string]any{"context": c.ID, "repository": c.Repository})
	}

	// The checkout is on the pinned commit. Asked here, after the indexing,
	// this is also the check that the source did not move while it was being
	// indexed: the same worktree answered the same commit on both sides of it,
	// so the index and the graph were built from the revision the record pins.
	worktree, err := s.WorktreeDir(c.Repository, c.ID)
	if err != nil {
		return err
	}
	if err := verifySource(ctx, worktree, c); err != nil {
		return err
	}
	if err := verifySearchRef(ctx, repoDir, s.ZoektDir(), c); err != nil {
		return err
	}
	if err := verifyGraph(ctx, c); err != nil {
		return err
	}
	return verifyIdentity(c)
}

// verifyIdentity checks that the four fields the query plane resolves a context
// into — repository, branch, revision, graph_ref — are the ones this context's
// own name and revision generate.
//
// They are generated on create and never written again, so this can only fail
// on a record that was edited by hand or written by another version. It is
// checked anyway, and fails closed: a search ref or a graph carrying a
// different revision than the one the record pins is the wrong-version answer
// this server exists to prevent, and it would be served silently.
func verifyIdentity(c store.Context) error {
	if len(c.Revision) != fullSHA {
		return vacerr.New(
			vacerr.SourceMismatch,
			fmt.Sprintf("context %q pins %q, which is not a full commit SHA", c.ID, c.Revision),
			map[string]any{"context": c.ID, "repository": c.Repository, "declared_revision": c.Revision},
		)
	}
	if branch := searchRef(c.ID, c.Revision); c.Branch != branch {
		return vacerr.New(
			vacerr.SourceMismatch,
			fmt.Sprintf("context %q is searched on branch %q, but its name and revision %s generate %q: the record names another context's source", c.ID, c.Branch, c.Revision, branch),
			map[string]any{"context": c.ID, "repository": c.Repository, "declared_revision": c.Revision, "branch": c.Branch},
		)
	}
	if graph := graphRef(c.Repository, c.ID, c.Revision); c.GraphRef != graph {
		return vacerr.New(
			vacerr.SourceMismatch,
			fmt.Sprintf("context %q is traced in graph %q, but its repository, name and revision %s generate %q: the record names another context's graph", c.ID, c.GraphRef, c.Revision, graph),
			map[string]any{"context": c.ID, "repository": c.Repository, "declared_revision": c.Revision, "graph_ref": c.GraphRef},
		)
	}
	return nil
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
		{"retry", retryHint(c)},
		{"updated", c.UpdatedAt.Format(time.RFC3339)},
	} {
		if _, err := fmt.Fprintf(out, "%-12s%s\n", row[0], row[1]); err != nil {
			return err
		}
	}
	return nil
}

// retryHint says what to do about the state a context is in.
//
// A context is READY or it is one that needs rebuilding, and the states in
// between do not divide any finer than that: a record left in INDEXING_GRAPH by
// a process that was killed and one being written by a create that is running
// right now look the same on disk, and nothing an operator can read tells them
// apart. This says the one thing that is true of both, and `context retry` is
// what resolves the difference — it waits for the running create, if there is
// one, and then finds the READY it produced.
func retryHint(c store.Context) string {
	if c.State == contextReady {
		return "not needed"
	}
	return "needed: vacmcp context retry " + c.ID
}

// contextVerify re-runs the checks that made a context READY and writes
// nothing.
//
// It is the same [verifyContext] the create ran, so what `context verify` says
// about a context is what READY meant when it was granted, asked again of the
// artifacts as they are now. Not writing is the point: a verification that
// could update a record would be a way for a pinned revision to move, and a
// verification that could grant READY would be a way around the lifecycle. It
// reports what it found and stops.
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
	if err := verifyContext(context.Background(), s, c); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s\tOK\t%s\n", c.ID, c.Revision)
	return err
}

// contextRemove forgets a context and deletes the worktree and the graph it
// owns.
//
// Nothing but this context's own record, its own directory and its own graph is
// touched: a worktree lives at worktrees/<repository>/<context>, so what is
// deleted is one subtree that no other context of the same repository is
// inside, the graph is the one its record names, and no other record is read or
// written.
//
// An artifact that is not there is skipped rather than missed — a context whose
// worktree was never created, or whose indexing never finished, still has to be
// removable.
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
	// Read once outside the lock only to learn which repository's lock to take.
	known, err := s.Context(id)
	if err != nil {
		return err
	}

	// The removal rebuilds the repository's search index out of the records it
	// leaves behind, so it is one operation on that repository like a create is.
	return withRepositoryLock(s, known.Repository, func() error {
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

		// The artifacts go first: a failure there leaves the record in place, so
		// the context is still managed and still removable. The other order
		// would leave a checkout and a graph nothing knows about.
		if err := discardSource(context.Background(), repoDir, worktree, c); err != nil {
			return err
		}
		// Then the ref, then the record, then the index, and that order is what
		// keeps a half-finished removal safe: until the record is gone the
		// context is still wholly there, and once it is gone the query plane has
		// no context to search whatever the index still holds. A rebuild that
		// fails after that is reported rather than swallowed, because what it
		// leaves behind is the removed revision's source still in the shard.
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
	})
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
