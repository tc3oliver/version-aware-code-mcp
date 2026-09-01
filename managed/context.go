package managed

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"

	"github.com/tc3oliver/version-aware-code-mcp/store"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// contextLifecycle is the order a context goes through: every state's only
// successor is the next element. ContextReady is last, and ContextFailed and
// ContextRemoving are deliberately not in it at all — it is the path a context
// is built along, and neither of those is a stage of building one.
var contextLifecycle = []string{
	ContextCreating,
	ContextResolving,
	ContextPreparingSource,
	ContextIndexingSearch,
	ContextIndexingGraph,
	ContextVerifying,
	ContextReady,
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

// advances reports whether a context may move from one state to the other.
//
// The machine only goes forwards, one state at a time, and out of READY or
// FAILED it does not go at all: a context that is being served must not be
// walked back into a state where its artifacts are being rebuilt under it, and
// re-running a failed create is [ContextManager.Retry] creating the lifecycle
// again rather than a record quietly reverting.
//
// REMOVING is the exception, and it is one in both directions. Every state
// leads to it, including the terminal ones and itself: a record exists and
// somebody asked for it to be taken away, and what state it stopped in does not
// change that — READY is the ordinary case, a FAILED or half-built context has
// always been removable too, and REMOVING reaching itself is what lets a
// removal that was interrupted be run again rather than refused. Nothing leads
// out of it, which falls out of it not being in contextLifecycle: the next step
// after REMOVING is the record being deleted, not another state.
func advances(from, to string) bool {
	if to == ContextRemoving {
		return true
	}
	at := slices.Index(contextLifecycle, from)
	if at < 0 || from == ContextReady {
		return false
	}
	// Any stage can fail; otherwise the next state is the only move.
	return to == ContextFailed || to == contextLifecycle[at+1]
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
// still listed, still inspectable and still removable, and is one a retry can
// later take over — while the error is still what the caller gets. A store that
// cannot even be written is reported alongside the cause rather than instead of
// it, since the cause is what the user has to read.
func fail(s *store.Store, c *store.Context, cause error) error {
	if err := advance(s, c, ContextFailed); err != nil {
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
	if c.State != ContextReady {
		return store.Context{}, vacerr.New(
			vacerr.ContextNotFound,
			fmt.Sprintf("store: context %q is not managed in %s", id, s.Root()),
			map[string]any{"context": id},
		)
	}
	return c, nil
}

// Create resolves every pin's ref to a commit, records one context pinned to
// all of them and drives it to READY.
//
// Each ref is read exactly once, here. What lands in the record is the full
// commit SHA it resolved to, so the branch it came from can move, be rewritten
// or be deleted afterwards without this context changing.
//
// Resolution is the RESOLVING stage, and it runs before the record exists
// because it is what the record is made of: the pinned revisions and the search
// and graph names derived from them. A ref that does not resolve therefore
// records nothing at all — not even the members whose refs did resolve — which
// is what leaves the same call runnable again with the ref the user meant. A
// FAILED record carrying no revision would only be one they had to remove
// first, and one nothing could ever retry, since the ref that failed is not
// part of a context.
//
// Creating over an existing context ID is refused whatever state it is in. That
// is the same rule [RepositoryManager.Add] follows, and here it is what makes
// immutability a property of the code rather than a promise: no path in this
// file writes a revision into a record that already has one.
func (m *ContextManager) Create(ctx context.Context, id string, pins []Pin) (Context, error) {
	if len(pins) == 0 {
		return Context{}, vacerr.New(
			vacerr.InvalidArgument,
			"context create: a context needs at least one repository and the ref to pin it at",
			map[string]any{"context": id},
		)
	}
	// Every name below becomes a path element. Asking for each member's
	// worktree path is what puts the store's check on all of them in front of
	// everything else, so a name that is not a usable path element is refused
	// before any git runs and leaves nothing behind.
	seen := map[string]bool{}
	for _, pin := range pins {
		if _, err := m.store.WorktreeDir(pin.Repository, id); err != nil {
			return Context{}, err
		}
		// One repository twice in one context is refused rather than
		// deduplicated, exactly as the configuration refuses it: the two entries
		// pin two revisions of it and a context answering from both would report
		// one repository's code twice with no way to say which version each half
		// came from.
		if seen[pin.Repository] {
			return Context{}, vacerr.New(
				vacerr.InvalidArgument,
				fmt.Sprintf("context create: repository %q is given twice; a context pins one revision of each repository it names", pin.Repository),
				map[string]any{"context": id, "repository": pin.Repository},
			)
		}
		seen[pin.Repository] = true
	}
	// The store's allowlist has to admit a dot, and the generated search ref
	// below has to be a legal git ref: ".." is the one sequence that is fine in
	// the first and forbidden in the second.
	if strings.Contains(id, "..") {
		return Context{}, vacerr.New(
			vacerr.InvalidArgument,
			fmt.Sprintf("context create: context name %q cannot contain %q: it becomes part of a git ref", id, ".."),
			map[string]any{"context": id},
		)
	}
	// A managed server serving this data directory is the one thing that stops
	// a context being created here, and it is asked before the repositories'
	// own locks: a create adds a context the running server's snapshot does not
	// have, and once this id exists it can never mean another revision.
	release, err := holdManagementLock(m.store, "context create")
	if err != nil {
		return Context{}, err
	}
	defer release()

	var record store.Context
	// Everything below reads and writes each member's clone, its search index
	// and the records of its other contexts, so it is one operation on every
	// one of those repositories and runs alone on all of them. The refs are
	// resolved inside the locks too: a fetch running underneath one is exactly
	// what would let this pin a commit that is being replaced.
	err = withRepositoryLocks(m.store, pinnedRepositories(pins), func() error {
		if _, err := m.store.Context(id); err == nil {
			return vacerr.New(
				vacerr.InvalidArgument,
				fmt.Sprintf("context create: context %q is already managed in %s; a context is immutable, so another revision is another context name", id, m.store.Root()),
				map[string]any{"context": id},
			)
		}

		members := make([]store.ContextMember, 0, len(pins))
		for _, pin := range pins {
			if _, err := m.store.Repository(pin.Repository); err != nil {
				return err
			}
			repoDir, err := m.store.RepositoryDir(pin.Repository)
			if err != nil {
				return err
			}
			revision, err := resolveRevision(ctx, repoDir, pin.Repository, pin.Ref)
			if err != nil {
				return err
			}
			members = append(members, store.ContextMember{Repository: pin.Repository, Revision: revision})
		}

		// The generated names come after the whole member list, because
		// searchRef reads it: what a member's ref is called depends on whether
		// this context has one repository or several.
		record = store.Context{ID: id, Members: members, State: ContextCreating}
		for i := range record.Members {
			record.Members[i].Branch = searchRef(record, record.Members[i])
			record.Members[i].GraphRef = graphRef(record, record.Members[i])
		}
		// The record first, the artifacts after: a checkout, a graph or a search
		// ref that outlived a failed create would be artifacts no record names
		// and no caller can remove. This way a create that stops half way leaves
		// a context that is still managed, still listed and still removable, in
		// a state that is not one the query plane serves.
		if err := m.store.PutContext(record); err != nil {
			return err
		}

		// RESOLVING is entered and left in the same breath: the revisions were
		// resolved above, before there was a record to write them into. The
		// state exists all the same, because the order of the machine is what
		// makes a stage impossible to skip.
		if err := advance(m.store, &record, ContextResolving); err != nil {
			return err
		}
		return buildContext(ctx, m.store, &record)
	})
	if err != nil {
		return Context{}, err
	}
	return contextOf(record), nil
}

// pinnedRepositories is the repositories a create is about, in the order they
// were given; withRepositoryLocks is what puts them in the order they are
// locked in.
func pinnedRepositories(pins []Pin) []string {
	names := make([]string, 0, len(pins))
	for _, pin := range pins {
		names = append(names, pin.Repository)
	}
	return names
}

// repositoriesOf is the repositories one record's members name, which is the
// set of locks an operation on that context has to hold.
func repositoriesOf(c store.Context) []string {
	names := make([]string, 0, len(c.Members))
	for _, m := range c.Members {
		names = append(names, m.Repository)
	}
	return names
}

// memberPaths is where one member's source lives: the clone its revision is in,
// and the checkout of that revision this context owns.
//
// Both carry the repository as well as the context, so the several members of
// one context land in directories of their own without either name having to
// know that a context can have more than one.
func memberPaths(s *store.Store, id string, m store.ContextMember) (repoDir, worktree string, err error) {
	if repoDir, err = s.RepositoryDir(m.Repository); err != nil {
		return "", "", err
	}
	if worktree, err = s.WorktreeDir(m.Repository, id); err != nil {
		return "", "", err
	}
	return repoDir, worktree, nil
}

// buildContext takes a record that has every member's revision pinned and
// drives it to READY, one stage at a time, each one written down before it runs
// and none of them optional.
//
// It is the whole of what a context is made of, and it is one function because
// a create and a retry build the same context out of the same stages: a retry
// that rebuilt some other way would be a second definition of what READY means,
// and the two would drift.
//
// A stage is over every member before the next one begins, and a member that
// fails any stage fails the whole context. There is deliberately no state in
// which some members are built: READY is the only thing the query plane serves,
// and a workspace answering out of the members that happened to work is exactly
// the partial, silently-wrong answer this server exists to prevent.
//
// The caller holds every member repository's lock and has already advanced c to
// RESOLVING.
func buildContext(ctx context.Context, s *store.Store, c *store.Context) error {
	for _, stage := range []struct {
		state string
		run   func() error
	}{
		{ContextPreparingSource, func() error {
			for _, m := range c.Members {
				repoDir, worktree, err := memberPaths(s, c.ID, m)
				if err != nil {
					return err
				}
				if err := prepareSource(ctx, repoDir, worktree, c.ID, m); err != nil {
					return err
				}
			}
			return nil
		}},
		// Indexing is driven by the records: it creates the search ref of every
		// context the repository has, this one's member now among them, and
		// indexes them together. One call per repository, because one repository
		// is one shard however many contexts reach it.
		{ContextIndexingSearch, func() error {
			for _, repository := range slices.Sorted(slices.Values(repositoriesOf(*c))) {
				if err := indexRepository(ctx, s, repository); err != nil {
					return err
				}
			}
			return nil
		}},
		{ContextIndexingGraph, func() error {
			for _, m := range c.Members {
				_, worktree, err := memberPaths(s, c.ID, m)
				if err != nil {
					return err
				}
				if err := indexGraph(ctx, worktree, c.ID, m); err != nil {
					return err
				}
			}
			return nil
		}},
		{ContextVerifying, func() error { return verifyContext(ctx, s, *c) }},
	} {
		if err := advance(s, c, stage.state); err != nil {
			return err
		}
		if err := stage.run(); err != nil {
			return fail(s, c, err)
		}
	}
	return advance(s, c, ContextReady)
}

// Retry rebuilds the artifacts of a context that did not reach READY.
//
// It is what makes a failure recoverable without an operator going into the
// data directory by hand: whatever the interrupted run left — no worktree, a
// worktree on the wrong commit, a graph that was half indexed, a search ref
// that was never created — is thrown away, for every member and not only the
// one that failed, and made again from the revisions the record already pins.
// No revision is ever re-resolved. A retry fixes a context whose artifacts were
// not built, and there is no ref in it to move a context onto another commit.
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
func (m *ContextManager) Retry(ctx context.Context, id string) (Context, error) {
	// A retry throws this context's artifacts away and builds them again, so a
	// managed server that is serving the data directory refuses it for the same
	// reason it refuses a removal: it would be rebuilding a worktree and a graph
	// underneath answers being given out of them.
	release, err := holdManagementLock(m.store, "context retry")
	if err != nil {
		return Context{}, err
	}
	defer release()

	// Read once outside the repository's lock only to learn which one to take;
	// every decision below is made again from the record as it is under it.
	known, err := m.store.Context(id)
	if err != nil {
		return Context{}, err
	}

	var c store.Context
	err = withRepositoryLocks(m.store, repositoriesOf(known), func() error {
		var err error
		c, err = m.store.Context(id)
		if err != nil {
			return err
		}
		if c.State == ContextReady {
			return vacerr.New(
				vacerr.InvalidArgument,
				fmt.Sprintf("context retry: context %q is already %s; `context verify` re-checks it and `context remove` takes it away, but rebuilding it would take the source away from the answers being given out of it", id, ContextReady),
				map[string]any{"context": id},
			)
		}
		// A context that is being removed is the other thing a retry must not
		// rebuild: REMOVING only ever ends in the record being deleted, and
		// building the artifacts back up would be a retry undoing a removal
		// somebody asked for. Running the removal again is what finishes it.
		if c.State == ContextRemoving {
			return vacerr.New(
				vacerr.InvalidArgument,
				fmt.Sprintf("context retry: context %q is %s; run `vacmcp context remove %s` again to finish taking it away", id, ContextRemoving, id),
				map[string]any{"context": id},
			)
		}
		// Every member's partial artifacts go before anything is built, and the
		// record stays where it is if that fails: a cleanup that could not
		// finish leaves a context that is still not READY and still retryable,
		// rather than one that is being rebuilt on top of the leftovers of the
		// last attempt. All of them go before any of them is rebuilt, and not
		// member by member, so a retry that fails part way through has still
		// thrown away everything the interrupted run left rather than half of
		// it. The search refs need nothing here — indexing re-asserts every
		// record's ref from the record, and the record is immutable.
		for _, member := range c.Members {
			if _, err := m.store.Repository(member.Repository); err != nil {
				return err
			}
			repoDir, worktree, err := memberPaths(m.store, c.ID, member)
			if err != nil {
				return err
			}
			if err := discardSource(ctx, repoDir, worktree, c.ID, member); err != nil {
				return err
			}
		}

		// The one place the machine is re-entered. It is not a transition —
		// advance only ever goes forwards, and out of FAILED it does not go at
		// all — it is this context's lifecycle starting again from the stage
		// after the revision it already pins. The write happens before any
		// artifact is rebuilt, for the same reason every other state does: a
		// retry that is itself interrupted has to be retryable too.
		c.State = ContextResolving
		if err := m.store.PutContext(c); err != nil {
			return err
		}
		return buildContext(ctx, m.store, &c)
	})
	if err != nil {
		return Context{}, err
	}
	return contextOf(c), nil
}

// verifyContext is decision-4's verification: the six checks a context has to
// pass to be READY, and the same six [ContextManager.Verify] re-runs afterwards.
//
// It is one function because it is one question — is every artifact this record
// names really there and really this revision — and asking it in two places
// with two implementations is how the answers drift apart. Nothing here writes:
// verification reports what it found, and a record that disagrees with its
// artifacts is a failure rather than something to quietly adopt.
//
// Every member is checked, and the first one that fails fails the context: a
// workspace is one scope, so a member whose source is not what the record says
// makes the whole of it unanswerable rather than partly answerable.
//
// Every check asks the artifact itself. A create that once succeeded is not
// evidence: a clone can be replaced, a worktree checked out by hand, a shard
// deleted and a graph dropped by anything else driving the same CBM store.
func verifyContext(ctx context.Context, s *store.Store, c store.Context) error {
	for _, m := range c.Members {
		repoDir, worktree, err := memberPaths(s, c.ID, m)
		if err != nil {
			return err
		}
		// The pinned revision is still a commit this repository has, and still
		// the same one. Re-resolving it is what catches a clone that was deleted
		// or replaced under a context.
		found, err := gitOutput(ctx, "-C", repoDir, "rev-parse", "--verify", "--end-of-options", m.Revision+"^{commit}")
		if err != nil {
			return vacerr.New(
				vacerr.RevisionNotFound,
				fmt.Sprintf("context %q pins revision %s, which repository %q cannot resolve: %v", c.ID, m.Revision, m.Repository, err),
				map[string]any{"context": c.ID, "repository": m.Repository, "revision": m.Revision},
			)
		}
		if found != m.Revision {
			return vacerr.NewSourceMismatch(m.Revision, found, map[string]any{"context": c.ID, "repository": m.Repository})
		}

		// The checkout is on the pinned commit. Asked here, after the indexing,
		// this is also the check that the source did not move while it was being
		// indexed: the same worktree answered the same commit on both sides of
		// it, so the index and the graph were built from the revision the record
		// pins.
		if err := verifySource(ctx, worktree, c.ID, m); err != nil {
			return err
		}
		if err := verifySearchRef(ctx, repoDir, s.ZoektDir(), c.ID, m); err != nil {
			return err
		}
		if err := verifyGraph(ctx, c.ID, m); err != nil {
			return err
		}
	}
	return verifyIdentity(c)
}

// verifyIdentity checks that the four fields the query plane resolves each
// member into — repository, branch, revision, graph_ref — are the ones this
// context's own name and that member's revision generate.
//
// They are generated on create and never written again, so this can only fail
// on a record that was edited by hand or written by another version. It is
// checked anyway, and fails closed: a search ref or a graph carrying a
// different revision than the one the record pins is the wrong-version answer
// this server exists to prevent, and it would be served silently.
//
// Member by member, because a context of several repositories has one such
// quadruple per repository: a single-valued check could not say which clone the
// ref it recomputed was supposed to be in.
func verifyIdentity(c store.Context) error {
	for _, m := range c.Members {
		if len(m.Revision) != fullSHA {
			return vacerr.New(
				vacerr.SourceMismatch,
				fmt.Sprintf("context %q pins %q in repository %q, which is not a full commit SHA", c.ID, m.Revision, m.Repository),
				map[string]any{"context": c.ID, "repository": m.Repository, "declared_revision": m.Revision},
			)
		}
		if branch := searchRef(c, m); m.Branch != branch {
			return vacerr.New(
				vacerr.SourceMismatch,
				fmt.Sprintf("context %q is searched in repository %q on branch %q, but its name and revision %s generate %q: the record names another context's source", c.ID, m.Repository, m.Branch, m.Revision, branch),
				map[string]any{"context": c.ID, "repository": m.Repository, "declared_revision": m.Revision, "branch": m.Branch},
			)
		}
		if graph := graphRef(c, m); m.GraphRef != graph {
			return vacerr.New(
				vacerr.SourceMismatch,
				fmt.Sprintf("context %q is traced in graph %q, but its repository %q, name and revision %s generate %q: the record names another context's graph", c.ID, m.GraphRef, m.Repository, m.Revision, graph),
				map[string]any{"context": c.ID, "repository": m.Repository, "declared_revision": m.Revision, "graph_ref": m.GraphRef},
			)
		}
	}
	return nil
}

// List returns every managed context, ordered by ID.
func (m *ContextManager) List() ([]Context, error) {
	records, err := m.store.Contexts()
	if err != nil {
		return nil, err
	}
	contexts := make([]Context, 0, len(records))
	for _, c := range records {
		contexts = append(contexts, contextOf(c))
	}
	return contexts, nil
}

// Status reports one context, including the names it generated for each member
// and where that member's worktree goes. Those are internal — nobody has to type
// them, and the query plane does not show them — but this is the management
// plane, where an operator diagnosing a context needs to know which ref and
// which graph are its, in which repository.
func (m *ContextManager) Status(id string) (ContextStatus, error) {
	c, err := m.store.Context(id)
	if err != nil {
		return ContextStatus{}, err
	}
	artifacts := make([]MemberArtifacts, 0, len(c.Members))
	for _, member := range c.Members {
		worktree, err := m.store.WorktreeDir(member.Repository, c.ID)
		if err != nil {
			return ContextStatus{}, err
		}
		artifacts = append(artifacts, MemberArtifacts{
			Repository: member.Repository,
			Revision:   member.Revision,
			SearchRef:  member.Branch,
			GraphRef:   member.GraphRef,
			Worktree:   worktree,
		})
	}
	return ContextStatus{Context: contextOf(c), Artifacts: artifacts}, nil
}

// Verify re-runs the checks that made a context READY and writes nothing.
//
// It is the same verification the create ran, so what Verify says about a
// context is what READY meant when it was granted, asked again of the artifacts
// as they are now. Not writing is the point: a verification that could update a
// record would be a way for a pinned revision to move, and a verification that
// could grant READY would be a way around the lifecycle. It reports what it
// found and stops.
func (m *ContextManager) Verify(ctx context.Context, id string) (Context, error) {
	c, err := m.store.Context(id)
	if err != nil {
		return Context{}, err
	}
	if err := verifyContext(ctx, m.store, c); err != nil {
		return Context{}, err
	}
	return contextOf(c), nil
}

// Remove forgets a context and deletes every worktree and graph it owns.
//
// Nothing but this context's own record, its own directories and its own graphs
// is touched: a worktree is filed under its repository and its own id, so what
// is deleted is one subtree per member that no other context of the same
// repository is inside, the graphs are the ones its record names, and no other
// record is read or written.
//
// An artifact that is not there is skipped rather than missed — a context whose
// worktree was never created, or whose indexing never finished, still has to be
// removable. That is also what makes this resumable: every step below tolerates
// having already been taken, so a removal killed part way through is finished by
// running it again. It has to be, because a record says the context is being
// removed and never how far that got: a second run re-attempts every member.
func (m *ContextManager) Remove(ctx context.Context, id string) error {
	// The removal decision-6 is written about: a managed server holds the
	// worktree, the graph and the record this is about to delete in a snapshot
	// it cannot be told about, so while one runs this is refused outright.
	release, err := holdManagementLock(m.store, "context remove")
	if err != nil {
		return err
	}
	defer release()

	// Read once outside the repository's lock only to learn which one to take.
	known, err := m.store.Context(id)
	if err != nil {
		return err
	}

	// The removal rebuilds each member repository's search index out of the
	// records it leaves behind, so it is one operation on every one of them,
	// like a create is.
	return withRepositoryLocks(m.store, repositoriesOf(known), func() error {
		c, err := m.store.Context(id)
		if err != nil {
			return err
		}

		// The state before any of it, and written down before any of it: from
		// here the record says its artifacts are being taken apart, which is
		// what makes a removal that is interrupted recognisable afterwards
		// instead of leaving a record still claiming READY over a graph and a
		// worktree that are already gone. REMOVING is not READY, so the query
		// plane stops resolving this context at exactly this line — through the
		// same [readyContext] filter every other non-READY state goes through,
		// as the same CONTEXT_NOT_FOUND.
		if err := advance(m.store, &c, ContextRemoving); err != nil {
			return err
		}

		// The artifacts go first: a failure there leaves the record in place, so
		// the context is still managed and still removable. The other order
		// would leave a checkout and a graph nothing knows about.
		//
		// Then the refs, then the indexes, and the record last of all. The
		// record is what a second run finds the context by, so it is what has to
		// outlive every step that can fail: deleting it before the shards were
		// rebuilt would leave the removed revisions' source in the served index
		// with nothing left to name it, and no way to finish the removal but by
		// hand.
		for _, member := range c.Members {
			repoDir, worktree, err := memberPaths(m.store, c.ID, member)
			if err != nil {
				return err
			}
			if err := discardSource(ctx, repoDir, worktree, c.ID, member); err != nil {
				return err
			}
			if member.Branch == "" {
				continue
			}
			if err := dropSearchRef(ctx, repoDir, member); err != nil {
				return err
			}
		}
		// One rebuild per repository and not one per member, because one
		// repository is one shard: a context with two members in one clone —
		// which a create refuses and a hand-written record can still say — must
		// not have the second rebuild undo what the first left out.
		for _, repository := range slices.Sorted(slices.Values(repositoriesOf(c))) {
			if err := indexRepository(ctx, m.store, repository); err != nil {
				return err
			}
		}
		return m.store.DeleteContext(c.ID)
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

// searchRef is the ref inside m's clone that carries this member's source for
// the search index, and the branch the query plane searches. It is namespaced
// under vacmcp/ so it can never collide with a branch the repository really has,
// and it carries the short SHA so two contexts of one repository are never one
// ref. Nothing else names it: a user creates a context without knowing it
// exists.
//
// A member of a context that names several repositories carries the repository
// too, and one of a context that names a single repository does not. That
// asymmetry is not a style: two members pinned at the same revision would
// otherwise generate the identical name, which is one name for two sources —
// while a context of one repository has to keep generating exactly the name
// v0.4.0 wrote, or every record a v0.4.0 installation left would disagree with
// the formula [verifyIdentity] recomputes and fail closed on an artifact that is
// perfectly good. The count is a property of the record and the record is
// immutable, so a member's name never changes under it.
func searchRef(c store.Context, m store.ContextMember) string {
	if len(c.Members) > 1 {
		return "vacmcp/" + m.Repository + "-" + c.ID + "-" + m.Revision[:shortSHA]
	}
	return "vacmcp/" + c.ID + "-" + m.Revision[:shortSHA]
}

// graphRef is the graph project backing this member. The repository name is in
// it whatever the context looks like, because graph projects share one
// namespace across every repository in a data directory, where search refs are
// scoped to a clone already — which is also what already tells the several
// members of one context apart here.
func graphRef(c store.Context, m store.ContextMember) string {
	return "vacmcp-" + m.Repository + "-" + c.ID + "-" + m.Revision[:shortSHA]
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
