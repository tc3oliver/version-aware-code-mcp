package managed

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/tc3oliver/version-aware-code-mcp/store"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// The repository operations are the management plane: they are the only place
// vacmcp reaches a network, and decision-4 keeps them off the MCP tool surface
// so a coding agent cannot make the server clone or delete anything.

// Add clones a repository into the data directory and records it.
//
// The clone is `--no-checkout`: the source a context serves is read out of the
// object database, and the checkouts contexts do need are worktrees of their
// own. A working tree here would be a second copy of the source that no reader
// wants and every fetch has to be kept consistent with.
//
// A failed clone is recorded as FAILED rather than swallowed, and Add still
// fails. Adding over an existing record is refused whatever state it is in:
// [RepositoryManager.Remove] then Add is one obvious way to start again, where
// an add that silently re-cloned would be a way to lose a clone by typo.
func (m *RepositoryManager) Add(ctx context.Context, name, url string) (RepositoryStatus, error) {
	if embedsCredential(url) {
		return RepositoryStatus{}, vacerr.New(
			vacerr.InvalidArgument,
			"repo add: --url must not embed a credential; leave it out of the URL and let system git authenticate through an SSH agent, ~/.ssh/config or a git credential helper",
			map[string]any{"repository": name},
		)
	}

	// RepositoryDir validates the name, so nothing below can be reached with a
	// name that is not a single path element inside the data directory.
	path, err := m.store.RepositoryDir(name)
	if err != nil {
		return RepositoryStatus{}, err
	}
	if _, err := m.store.Repository(name); err == nil {
		return RepositoryStatus{}, vacerr.New(
			vacerr.InvalidArgument,
			fmt.Sprintf("repo add: repository %q is already managed in %s; remove it first to re-add it", name, m.store.Root()),
			map[string]any{"repository": name},
		)
	}

	record := store.Repository{Name: name, URL: url, State: RepositoryReady}
	if cloneErr := runGit(ctx, "clone", "--no-checkout", "--quiet", "--end-of-options", url, path); cloneErr != nil {
		record.State = RepositoryFailed
		if err := m.store.PutRepository(record); err != nil {
			return RepositoryStatus{}, err
		}
		return RepositoryStatus{}, fmt.Errorf("repo add: %s: %w", name, cloneErr)
	}
	if err := m.store.PutRepository(record); err != nil {
		return RepositoryStatus{}, err
	}
	return RepositoryStatus{Repository: repositoryOf(record), Path: path}, nil
}

// List returns every managed repository, ordered by name.
func (m *RepositoryManager) List() ([]Repository, error) {
	records, err := m.store.Repositories()
	if err != nil {
		return nil, err
	}
	repositories := make([]Repository, 0, len(records))
	for _, r := range records {
		repositories = append(repositories, repositoryOf(r))
	}
	return repositories, nil
}

// Status reports one repository, including where its clone is and which
// contexts depend on it.
func (m *RepositoryManager) Status(name string) (RepositoryStatus, error) {
	r, err := m.store.Repository(name)
	if err != nil {
		return RepositoryStatus{}, err
	}
	path, err := m.store.RepositoryDir(name)
	if err != nil {
		return RepositoryStatus{}, err
	}
	dependents, err := contextsOf(m.store, name)
	if err != nil {
		return RepositoryStatus{}, err
	}
	return RepositoryStatus{Repository: repositoryOf(r), Path: path, Contexts: dependents}, nil
}

// Sync fetches remote refs for the named repositories, or for every managed one
// when names is empty. It returns the repositories it synced, which on a
// failure is the ones it got through before it.
//
// It fetches and does nothing else. A context's pinned revision is a full commit
// SHA in its own record, and no code path here reads or writes a context record:
// syncing brings new commits within reach of the next create and leaves every
// existing context on exactly the revision it was pinned to. That is the
// guarantee this whole project exists for, so it is a property of what this does
// not do, not of a check it makes afterwards.
//
// A fetch that fails does not stop the ones after it, and all of them are
// reported together, for the same reason doctor runs every check: a run that
// stopped at the first unreachable remote would hide whether the rest are fine.
func (m *RepositoryManager) Sync(ctx context.Context, names []string) ([]Repository, error) {
	var records []store.Repository
	if len(names) == 0 {
		var err error
		if records, err = m.store.Repositories(); err != nil {
			return nil, err
		}
	} else {
		for _, name := range names {
			r, err := m.store.Repository(name)
			if err != nil {
				return nil, err
			}
			records = append(records, r)
		}
	}

	var synced []Repository
	var failures []string
	for _, r := range records {
		path, err := m.store.RepositoryDir(r.Name)
		if err != nil {
			return synced, err
		}

		// One repository's lock at a time, taken and released inside the loop:
		// every repository is synced without ever holding two of them, which is
		// what keeps a slow fetch from blocking work on the rest. A fetch
		// rewrites the refs a create resolves against, so it is not something to
		// run beside one on the same clone.
		var fetchErr error
		err = withRepositoryLock(m.store, r.Name, func() error {
			r.State = RepositoryReady
			fetchErr = runGit(ctx, "-C", path, "fetch", "--prune", "--tags")
			if fetchErr != nil {
				r.State = RepositoryFailed
			} else {
				r.LastSyncAt = time.Now().UTC()
			}
			return m.store.PutRepository(r)
		})
		if err != nil {
			return synced, err
		}

		if fetchErr != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", r.Name, fetchErr))
			continue
		}
		synced = append(synced, repositoryOf(r))
	}
	if len(failures) > 0 {
		return synced, fmt.Errorf("repo sync: %s", strings.Join(failures, "; "))
	}
	return synced, nil
}

// Remove forgets a repository and deletes its clone.
//
// A repository still backing a context is refused, and the blocking context IDs
// are named so the caller knows what to remove first. There is no force: those
// contexts pin revisions that only exist in this clone, so forcing the removal
// would leave contexts that can never be served again — a broken state offered
// as a convenience.
func (m *RepositoryManager) Remove(name string) error {
	path, err := m.store.RepositoryDir(name)
	if err != nil {
		return err
	}

	// A managed server resolves its contexts to paths inside this clone, so
	// deleting it while one runs would leave the source of contexts it is
	// serving gone. Asked before the repository's own lock, as decision-6 has
	// it.
	release, err := holdManagementLock(m.store, "repo remove")
	if err != nil {
		return err
	}
	defer release()

	// The check that no context depends on this repository and the deletion it
	// permits are one operation: without the lock a create could land between
	// them and leave a context pinned to a clone that is being deleted.
	return withRepositoryLock(m.store, name, func() error {
		if _, err := m.store.Repository(name); err != nil {
			return err
		}
		dependents, err := contextsOf(m.store, name)
		if err != nil {
			return err
		}
		if len(dependents) > 0 {
			return vacerr.New(
				vacerr.InvalidArgument,
				fmt.Sprintf("repo remove: repository %q still has %d context(s): %s; remove them first", name, len(dependents), strings.Join(dependents, ", ")),
				map[string]any{"repository": name, "contexts": dependents},
			)
		}

		// The clone goes first: a failure there leaves the record in place, so
		// the repository is still managed and still removable. The other order
		// would leave an unreferenced clone nothing knows about.
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("repo remove: cannot delete the clone of %q at %s: %w", name, path, err)
		}
		return m.store.DeleteRepository(name)
	})
}

// contextsOf returns the IDs of the contexts backed by the named repository, in
// the order the store lists them, which is by ID.
func contextsOf(s *store.Store, repository string) ([]string, error) {
	contexts, err := s.Contexts()
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, c := range contexts {
		if c.Repository == repository {
			ids = append(ids, c.ID)
		}
	}
	return ids, nil
}

// embedsCredential reports whether a git remote URL carries a secret in its
// userinfo component.
//
// Such a URL is refused rather than masked, because masking cannot hold: git
// writes the URL it was given into remote.origin.url of the clone, so a
// credential vacmcp redacted in its own record and output would still be sitting
// in plain text in .git/config inside the data directory. The only place the
// leak can be stopped is before git is ever handed the URL.
//
// A password is a secret under any transport, so a userinfo with a colon in it
// is refused whatever the scheme. Over HTTP the whole userinfo is the
// credential — basic auth, and a forge token is routinely put in the username
// field, where nothing can tell it from a login name — so any userinfo at all is
// refused there. `ssh://git@host` and `git@host:path` are left alone: that
// userinfo is an SSH login name, the secret behind it lives in the agent or the
// key file, and it is the form decision-4 expects people to use.
func embedsCredential(remote string) bool {
	// The authority is what precedes the first slash, with any scheme taken off
	// first: that holds for scp-like syntax too, whose [user@]host:path has no
	// slash before the path.
	scheme, rest, hasScheme := strings.Cut(remote, "://")
	if !hasScheme {
		rest = remote
	}
	authority, _, _ := strings.Cut(rest, "/")

	at := strings.LastIndex(authority, "@")
	if at < 0 {
		return false
	}
	if strings.Contains(authority[:at], ":") {
		return true
	}
	return hasScheme && (strings.EqualFold(scheme, "http") || strings.EqualFold(scheme, "https"))
}

// runGit runs one git command, with every argument passed as its own element of
// argv. No part of it is ever a shell string: the repository URL and the paths
// derived from a repository name come from a caller, and handing those to a
// shell is how a remote URL becomes a command.
//
// The URL is additionally separated from the options by --end-of-options, so one
// that begins with a dash is a URL git fails to clone rather than an option git
// obeys.
//
// Avoiding the shell is not enough on its own: git's remote helpers are a shell
// of their own. `ext::sh -c ...` runs a command by design, and a user who has
// enabled it in their own git configuration would be running it from a URL
// vacmcp passed on. GIT_PROTOCOL_FROM_USER=0 is git's own way to say this URL
// did not come from someone typing a git command, which turns every transport
// enabled only at the "user" level back off. That includes the local file
// transport, which vacmcp does support as a remote, so it is re-enabled
// explicitly and only it.
//
// The environment is otherwise inherited whole, because that is where
// authentication lives: SSH_AUTH_SOCK, GIT_SSH_COMMAND, HOME and everything git
// reads a credential helper out of. vacmcp holds no credential of its own.
//
// git's messages are localised, so its output is passed through verbatim and
// never parsed: the reason a fetch failed is for the user to read, not for this
// package to branch on.
func runGit(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "protocol.file.allow=always"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_PROTOCOL_FROM_USER=0")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if message := strings.TrimSpace(string(out)); message != "" {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, message)
	}
	return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
}
