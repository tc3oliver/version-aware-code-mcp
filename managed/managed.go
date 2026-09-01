// Package managed is the management plane of a vacmcp installation: the
// repositories cloned into a data directory and the version contexts pinned out
// of them.
//
// It is the whole of what `vacmcp repo` and `vacmcp context` do. The CLI parses
// flags and prints rows; every clone, fetch, checkout, index, graph, state
// transition and lock lives here, so another program embedding vacmcp builds the
// same installation the CLI does rather than a second one that drifts from it.
//
// Two types are the surface: a [RepositoryManager] for the repositories and a
// [ContextManager] for the contexts of them. Both are opened on a data
// directory — the same one `--data-dir` names, empty meaning the default
// `~/.vacmcp` — and both hold the same rules the CLI has always held:
//
//   - A context is immutable. Every member's revision is a full commit SHA
//     resolved once, and no method here writes another one into an existing
//     record: another version of a repository is another context.
//   - A context is READY only with every member's artifacts built and checked,
//     and verification asks the artifacts rather than the record. Anything
//     that disagrees fails closed.
//   - One operation per repository at a time. Every method here that changes a
//     repository, a context or an artifact of one runs alone under that
//     repository's lock, against the other methods and against another process
//     running them. A context spanning several repositories is one operation on
//     each of them, and holds all of their locks together, in one order.
//   - Nothing that changes what a managed server is already serving runs while
//     one serves the data directory. [ContextManager.Create],
//     [ContextManager.Retry], [ContextManager.Remove] and
//     [RepositoryManager.Remove] are refused for as long as it runs.
//     [RepositoryManager.Add] and [RepositoryManager.Sync] are not among them:
//     a repository added while a server runs has no contexts, so nothing in the
//     snapshot a server is serving can name it, and a sync only fetches remote
//     refs into a clone and writes its own repository record — no context
//     record, worktree, search ref or graph is touched, so there is nothing in
//     a running server's snapshot for it to invalidate (decision-6).
//
// What is deliberately not on this surface is how any of that is stored. The
// on-disk layout, the file format of a record, the lock files and the way they
// are taken, the generated names of the search ref and the graph, and the paths
// of the clones and worktrees are implementation, and none of them is a type or
// a signature here. The results below carry domain facts — a name, an id, a
// state, a revision, a timestamp — plus, on the two status types alone, the
// paths and identifiers `repo status` and `context status` report to an
// operator, as opaque strings that say where something is rather than how it is
// named or arranged.
//
// What holds those last two rules between processes is a file lock, and there
// is no file lock outside unix. Taking one there is a no-op, so on such a
// platform the first of them holds only between the goroutines of one program
// and the second does not hold at all: nothing there can find out that a server
// is running. A caller embedding this package on one is therefore the only
// thing that may touch its data directory. `vacmcp repo` and `vacmcp context`
// say so when they are run there, and an embedder is told nowhere but here.
package managed

import (
	"time"

	"github.com/tc3oliver/version-aware-code-mcp/store"
)

// The lifecycle states a repository carries. A repository whose clone or fetch
// failed keeps its record on purpose: a failure that left no trace would be
// indistinguishable from a repository that was never added, and there would be
// nothing to list, inspect or remove.
const (
	RepositoryReady  = "READY"
	RepositoryFailed = "FAILED"
)

// The states a context carries, in the order decision-4 gives them.
//
// Each one names what is happening to the context, so a record read while a
// create is running — or left behind by one that was killed — says which stage
// it is in. ContextReady is the only state the query plane serves and the only
// one a context reaches with all of its artifacts built and checked;
// ContextFailed is where a context that failed any stage stops, and
// ContextRemoving is the one state that is not part of building a context at
// all: it says this context's artifacts are being taken apart.
const (
	ContextCreating        = "CREATING"
	ContextResolving       = "RESOLVING"
	ContextPreparingSource = "PREPARING_SOURCE"
	ContextIndexingSearch  = "INDEXING_SEARCH"
	ContextIndexingGraph   = "INDEXING_GRAPH"
	ContextVerifying       = "VERIFYING"
	ContextReady           = "READY"
	ContextFailed          = "FAILED"
	ContextRemoving        = "REMOVING"
)

// Repository is one managed repository.
//
// URL is the git remote it was cloned from; it is a plain remote URL and never
// carries a credential, because one embedded in it is refused by [RepositoryManager.Add].
// LastSyncAt is when its refs were last fetched, and is zero for a repository
// that has not been synced since it was added.
type Repository struct {
	Name       string
	URL        string
	State      string
	LastSyncAt time.Time
}

// RepositoryStatus is a repository with the two things its record does not
// spell out and that decide what may be done to it next: where its clone is,
// and which contexts depend on it.
//
// Path is where the clone can be found, for reporting it to an operator. It is
// not a way to address the data directory: nothing here turns a name into a
// path for a caller, and the layout Path happens to sit in is not part of this
// package's contract.
type RepositoryStatus struct {
	Repository
	Path     string
	Contexts []string
}

// Context is one managed version context: the repositories it names, each
// pinned to its own revision, and the state its lifecycle has reached.
//
// There is one state for the whole context and not one per member. READY means
// every member's artifacts were built and checked; anything less is a context
// the query plane does not serve at all, because half a workspace answering
// about half a version is the wrong-version answer this server exists to
// prevent.
type Context struct {
	ID        string
	Members   []Member
	State     string
	UpdatedAt time.Time
}

// Member is one repository of a managed context.
//
// Revision is the full commit SHA it is pinned to, and it is immutable once
// resolved. It is empty only for a context that failed before it got that far.
type Member struct {
	Repository string
	Revision   string
}

// Pin is one repository [ContextManager.Create] is asked to add to a context,
// and the ref to resolve once and pin it at. The ref is read exactly once, at
// creation, and is not kept: what the context carries afterwards is the commit
// it resolved to.
type Pin struct {
	Repository string
	Ref        string
}

// ContextStatus is a context with the artifacts each of its members owns, for
// an operator diagnosing one.
//
// SearchRef, GraphRef and Worktree are opaque: they say which ref, which graph
// and which directory that member's answers come out of, so a person can go and
// look at them. How any of the three is generated or arranged is not part of
// this contract, and nothing here derives one from a context's name.
type ContextStatus struct {
	Context
	Artifacts []MemberArtifacts
}

// MemberArtifacts is one member and where its answers come out of, in the order
// the members of its context are declared. It repeats the revision so a row of
// a status report is one value rather than two slices a caller has to line up.
type MemberArtifacts struct {
	Repository string
	Revision   string
	SearchRef  string
	GraphRef   string
	Worktree   string
}

// RepositoryManager manages the repositories of one data directory.
type RepositoryManager struct {
	store *store.Store
}

// ContextManager manages the contexts of one data directory.
type ContextManager struct {
	store *store.Store
}

// NewRepositoryManager opens the data directory dir, creating it if it is not
// there yet. An empty dir means the default location, `~/.vacmcp`.
func NewRepositoryManager(dir string) (*RepositoryManager, error) {
	s, err := store.Open(dir)
	if err != nil {
		return nil, err
	}
	return &RepositoryManager{store: s}, nil
}

// NewContextManager opens the data directory dir, creating it if it is not
// there yet. An empty dir means the default location, `~/.vacmcp`.
func NewContextManager(dir string) (*ContextManager, error) {
	s, err := store.Open(dir)
	if err != nil {
		return nil, err
	}
	return &ContextManager{store: s}, nil
}

// HoldServerLock says a server is serving the contexts of the data directory
// dir, and returns the release that ends the claim.
//
// While it is held, every method here that would change what such a server is
// serving is refused: [ContextManager.Create], [ContextManager.Retry],
// [ContextManager.Remove] and [RepositoryManager.Remove]. A server reads the
// contexts it serves once and serves that snapshot for its whole run, so
// taking one apart underneath it would be serving a version that is no longer
// there. [RepositoryManager.Add] is not refused: it can only add a repository
// no context of that snapshot names. [RepositoryManager.Sync] is not refused
// either: it only fetches remote refs into a clone and writes its own
// repository record, so it changes nothing a running snapshot reads (decision-6,
// validated in TASK-40's TestRepoSyncRunsBesideARunningManagedServer). It waits
// for the management commands already running rather than refusing to start
// behind them, and a process that dies instead of releasing has the kernel drop
// the claim with it.
func HoldServerLock(dir string) (func(), error) {
	s, err := store.Open(dir)
	if err != nil {
		return nil, err
	}
	return holdServerLock(s)
}

// repositoryOf and contextOf are the one place a record becomes a result. Every
// method returns through them, so a field added to a record is not a field this
// package starts reporting by accident.
func repositoryOf(r store.Repository) Repository {
	return Repository{Name: r.Name, URL: r.URL, State: r.State, LastSyncAt: r.LastSyncAt}
}

func contextOf(c store.Context) Context {
	members := make([]Member, 0, len(c.Members))
	for _, m := range c.Members {
		members = append(members, Member{Repository: m.Repository, Revision: m.Revision})
	}
	return Context{ID: c.ID, Members: members, State: c.State, UpdatedAt: c.UpdatedAt}
}
