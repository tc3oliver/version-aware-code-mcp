package main

import (
	"fmt"
	"os"
	"sync"

	"github.com/tc3oliver/version-aware-code-mcp/store"
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
