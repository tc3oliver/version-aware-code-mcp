package main

import (
	"fmt"
	"os"
	"sync"

	"github.com/tc3oliver/version-aware-code-mcp/store"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// The per-repository lock decision-4 calls for, and the whole of it.
//
// A repository's git clone, its Zoekt shard, the graphs of its contexts and the
// records that name all three are four things that have to agree with each
// other. Nothing but running one operation on a repository at a time keeps them
// agreeing: two creates that both rebuild the repository's index from the
// records can have the one that read the older records finish last, leaving a
// shard that is missing a context the registry says is READY — a context the
// query plane would then serve out of no source at all.
//
// The lock is per repository and never wider. Two repositories share no clone,
// no shard and no worktree directory, so serialising them would only be slower.

// repositoryLocks is the in-process half, keyed by lock file path so two data
// directories that happen to manage a repository of the same name are still two
// locks.
var repositoryLocks sync.Map

// withRepositoryLock runs fn as the only operation on the named repository.
//
// It is held two ways over, because one way is not enough. The mutex is what
// makes two goroutines of one process take turns. The file lock is what makes
// two vacmcp processes take turns, and that is the case that actually happens:
// decision-4 keeps repository and context mutation off the MCP tool surface, so
// every one of these operations is a separate run of the CLI — a cron'd `repo
// sync --all` and an operator's `context create` are two processes, and an
// in-process mutex would not be between them at all.
//
// The file lock is advisory and released by the kernel when the file descriptor
// closes, which a dying process does too: a killed vacmcp leaves a retryable
// context behind (see contextRetry) and never a repository nothing can lock
// again.
//
// Callers are the CLI subcommands, and only they: nothing this calls takes the
// lock again, which is what keeps a re-entrant acquisition — the one way this
// deadlocks — out of the code rather than out of a convention.
func withRepositoryLock(s *store.Store, repository string, fn func() error) error {
	path, err := s.LockFile(repository)
	if err != nil {
		return err
	}

	gate, _ := repositoryLocks.LoadOrStore(path, &sync.Mutex{})
	mu := gate.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("cannot open the lock of repository %q at %s: %w", repository, path, err)
	}
	defer func() { _ = f.Close() }()

	if err := lockExclusive(f); err != nil {
		return fmt.Errorf("cannot lock repository %q at %s: %w", repository, path, err)
	}
	return fn()
}

// The server lock decision-6 calls for, which is a different lock from the one
// above and not a wider version of it.
//
// A `serve --managed` reads the READY contexts once, at startup, and serves
// that snapshot for as long as it runs: v0.2.0 has no live reload. So the
// contexts it is serving must not be taken apart underneath it — a `context
// remove` while it runs deletes a worktree and a graph the server still
// believes in, and once the record is gone the same context id can be created
// again on another revision, which is the one thing this project exists to
// make impossible.
//
// The two sides take the same file differently, and that asymmetry is the
// whole mechanism:
//
//   - a managed server takes it exclusively, and holds it for its whole run;
//   - a command that changes an artifact or a record takes it shared, which
//     succeeds only when no server holds it exclusively.
//
// Shared rather than exclusive on that side is what keeps decision-4's
// per-repository concurrency: two management commands on two repositories
// still run at once, because they do not exclude each other here — only a
// server excludes them. An exclusive lock would serialise the whole data
// directory and, worse, would make the second command report a running server
// that is not there.
//
// There is no in-process mutex to go with the file lock, unlike the
// per-repository lock above. The two sides of this one are always two
// processes — decision-4 keeps the management plane off the MCP tool surface,
// so a server and a `context remove` are never one program — and a mutex
// between goroutines would be a lock between things that never meet.

// holdServerLock takes the data directory's server lock for a managed server
// and returns the release.
//
// It waits rather than failing: what it waits for is a management command
// holding it shared, which is a create or a removal that will be done in a
// moment, and a server that refused to start because one was running would be
// a server that has to be started again by hand.
//
// The file is kept open by the returned release and closed by it, which is
// what makes the hold last exactly as long as the caller keeps it: a process
// that dies instead has the kernel drop the lock with the descriptor, so a
// killed server never leaves a data directory nothing may change again.
func holdServerLock(s *store.Store) (func(), error) {
	path := s.ServerLockFile()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("cannot open the server lock at %s: %w", path, err)
	}
	if err := lockExclusive(f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("cannot take the server lock at %s: %w", path, err)
	}
	return func() { _ = f.Close() }, nil
}

// holdManagementLock takes the server lock shared for the length of one
// management command, or refuses the command because a managed server is
// running on the data directory.
//
// The lock is held for the command rather than merely asked about before it:
// a check that let go of the lock would leave the window it exists to close —
// a server starting between the check and the deletion reads a snapshot of
// contexts this command is about to take apart.
//
// command is the command being refused, so the message reads as its own
// failure rather than as a lock nobody asked for. The code is INVALID_ARGUMENT
// because doc-1's error model gains nothing here: what happened is that this
// command cannot be run in the state the data directory is in, and the message
// says what to do about it.
func holdManagementLock(s *store.Store, command string) (func(), error) {
	path := s.ServerLockFile()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("cannot open the server lock at %s: %w", path, err)
	}
	held, err := tryLockShared(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("cannot take the server lock at %s: %w", path, err)
	}
	if !held {
		_ = f.Close()
		return nil, vacerr.New(
			vacerr.InvalidArgument,
			fmt.Sprintf("%s: a managed server is running on %s, and it serves the contexts it read when it started; stop `vacmcp serve --managed`, run this command, then start the server again", command, s.Root()),
			map[string]any{"data_dir": s.Root()},
		)
	}
	return func() { _ = f.Close() }, nil
}
