package main

import (
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

// The lifecycle states a repository record carries. A repository whose clone or
// fetch failed keeps a record on purpose: a failure that left no trace would be
// indistinguishable from a repository that was never added, and the user would
// have nothing to list, inspect or remove.
const (
	repoReady  = "READY"
	repoFailed = "FAILED"
)

const repoUsage = `Usage:
  vacmcp repo add NAME --url URL [--data-dir DIR]   clone a repository into the data directory
  vacmcp repo list [--data-dir DIR]                 list the managed repositories
  vacmcp repo status NAME [--data-dir DIR]          report one repository
  vacmcp repo sync NAME|--all [--data-dir DIR]      fetch remote refs
  vacmcp repo remove NAME [--data-dir DIR]          forget a repository and delete its clone`

// repoCommand dispatches `vacmcp repo <subcommand>`.
//
// These commands are the management plane: they are the only place vacmcp
// reaches a network, and they are reachable from the CLI alone. decision-4 keeps
// them off the MCP tool surface so a coding agent cannot make the server clone
// or delete anything.
func repoCommand(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("repo: no subcommand given\n\n" + repoUsage)
	}
	switch subcommand := args[0]; subcommand {
	case "add":
		return repoAdd(args[1:], out)
	case "list":
		return repoList(args[1:], out)
	case "status":
		return repoStatus(args[1:], out)
	case "sync":
		return repoSync(args[1:], out)
	case "remove":
		return repoRemove(args[1:], out)
	default:
		return fmt.Errorf("unknown repo subcommand %q\n\n%s", subcommand, repoUsage)
	}
}

// repoAdd clones a repository into the data directory and records it.
//
// The clone is `--no-checkout`: the source a context serves is read out of the
// object database, and the checkouts contexts do need are worktrees of their
// own. A working tree here would be a second copy of the source that no reader
// wants and every fetch has to be kept consistent with.
//
// A failed clone is recorded as FAILED rather than swallowed, and the command
// still fails. Adding over an existing record is refused whatever state it is
// in: `repo remove` then `repo add` is one obvious way to start again, where an
// add that silently re-cloned would be a way to lose a clone by typo.
func repoAdd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("vacmcp repo add", flag.ExitOnError)
	url := fs.String("url", "", "git remote URL to clone")
	dataDir := fs.String("data-dir", "", "vacmcp data directory (default ~/.vacmcp)")
	name, err := parseRepoFlags(fs, args)
	if err != nil {
		return err
	}
	if name == "" {
		return errors.New("repo add: NAME is required")
	}
	if strings.TrimSpace(*url) == "" {
		return errors.New("repo add: --url is required")
	}
	if embedsCredential(*url) {
		return vacerr.New(
			vacerr.InvalidArgument,
			"repo add: --url must not embed a credential; leave it out of the URL and let system git authenticate through an SSH agent, ~/.ssh/config or a git credential helper",
			map[string]any{"repository": name},
		)
	}

	s, err := store.Open(*dataDir)
	if err != nil {
		return err
	}
	// RepositoryDir validates the name, so nothing below can be reached with a
	// name that is not a single path element inside the data directory.
	path, err := s.RepositoryDir(name)
	if err != nil {
		return err
	}
	if _, err := s.Repository(name); err == nil {
		return vacerr.New(
			vacerr.InvalidArgument,
			fmt.Sprintf("repo add: repository %q is already managed in %s; remove it first to re-add it", name, s.Root()),
			map[string]any{"repository": name},
		)
	}

	record := store.Repository{Name: name, URL: *url, State: repoReady}
	if cloneErr := runGit(context.Background(), "clone", "--no-checkout", "--quiet", "--end-of-options", *url, path); cloneErr != nil {
		record.State = repoFailed
		if err := s.PutRepository(record); err != nil {
			return err
		}
		return fmt.Errorf("repo add: %s: %w", name, cloneErr)
	}
	if err := s.PutRepository(record); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s\t%s\t%s\n", record.Name, record.State, path)
	return err
}

// repoList prints one line per managed repository, ordered by name.
func repoList(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("vacmcp repo list", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "vacmcp data directory (default ~/.vacmcp)")
	_ = fs.Parse(args)

	s, err := store.Open(*dataDir)
	if err != nil {
		return err
	}
	repositories, err := s.Repositories()
	if err != nil {
		return err
	}
	for _, r := range repositories {
		if _, err := fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", r.Name, r.State, r.URL, lastSync(r)); err != nil {
			return err
		}
	}
	return nil
}

// repoStatus reports one repository, including where its clone is and which
// contexts depend on it — the two things a record does not spell out and that
// decide what may be done to it next.
func repoStatus(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("vacmcp repo status", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "vacmcp data directory (default ~/.vacmcp)")
	name, err := parseRepoFlags(fs, args)
	if err != nil {
		return err
	}
	if name == "" {
		return errors.New("repo status: NAME is required")
	}

	s, err := store.Open(*dataDir)
	if err != nil {
		return err
	}
	r, err := s.Repository(name)
	if err != nil {
		return err
	}
	path, err := s.RepositoryDir(name)
	if err != nil {
		return err
	}
	dependents, err := contextsOf(s, name)
	if err != nil {
		return err
	}

	depending := "none"
	if len(dependents) > 0 {
		depending = strings.Join(dependents, ", ")
	}
	for _, row := range [][2]string{
		{"name", r.Name},
		{"url", r.URL},
		{"state", r.State},
		{"path", path},
		{"last sync", lastSync(r)},
		{"contexts", depending},
	} {
		if _, err := fmt.Fprintf(out, "%-12s%s\n", row[0], row[1]); err != nil {
			return err
		}
	}
	return nil
}

// repoSync fetches remote refs for one repository or, with --all, for every
// managed one.
//
// It fetches and does nothing else. A context's pinned revision is a full commit
// SHA in its own record, and no code path here reads or writes a context record:
// syncing brings new commits within reach of the next `context create` and
// leaves every existing context on exactly the revision it was pinned to. That
// is the guarantee this whole project exists for, so it is a property of what
// the command does not do, not of a check it makes afterwards.
//
// --all keeps going after a failure and reports all of them together, for the
// same reason doctor runs every check: a run that stopped at the first
// unreachable remote would hide whether the rest are fine.
func repoSync(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("vacmcp repo sync", flag.ExitOnError)
	all := fs.Bool("all", false, "sync every managed repository")
	dataDir := fs.String("data-dir", "", "vacmcp data directory (default ~/.vacmcp)")
	name, err := parseRepoFlags(fs, args)
	if err != nil {
		return err
	}
	switch {
	case name == "" && !*all:
		return errors.New("repo sync: give either NAME or --all")
	case name != "" && *all:
		return errors.New("repo sync: --all takes no repository name")
	}

	s, err := store.Open(*dataDir)
	if err != nil {
		return err
	}

	var repositories []store.Repository
	if *all {
		if repositories, err = s.Repositories(); err != nil {
			return err
		}
	} else {
		r, err := s.Repository(name)
		if err != nil {
			return err
		}
		repositories = []store.Repository{r}
	}

	var failures []string
	for _, r := range repositories {
		path, err := s.RepositoryDir(r.Name)
		if err != nil {
			return err
		}

		r.State = repoReady
		fetchErr := runGit(context.Background(), "-C", path, "fetch", "--prune", "--tags")
		if fetchErr != nil {
			r.State = repoFailed
		} else {
			r.LastSyncAt = time.Now().UTC()
		}
		if err := s.PutRepository(r); err != nil {
			return err
		}

		if fetchErr != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", r.Name, fetchErr))
			continue
		}
		if _, err := fmt.Fprintf(out, "%s\t%s\t%s\n", r.Name, r.State, lastSync(r)); err != nil {
			return err
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("repo sync: %s", strings.Join(failures, "; "))
	}
	return nil
}

// repoRemove forgets a repository and deletes its clone.
//
// A repository still backing a context is refused, and the blocking context IDs
// are named so the user knows what to remove first. There is no --force: those
// contexts pin revisions that only exist in this clone, so forcing the removal
// would leave contexts that can never be served again — a broken state offered
// as a convenience.
func repoRemove(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("vacmcp repo remove", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "vacmcp data directory (default ~/.vacmcp)")
	name, err := parseRepoFlags(fs, args)
	if err != nil {
		return err
	}
	if name == "" {
		return errors.New("repo remove: NAME is required")
	}

	s, err := store.Open(*dataDir)
	if err != nil {
		return err
	}
	if _, err := s.Repository(name); err != nil {
		return err
	}
	dependents, err := contextsOf(s, name)
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

	path, err := s.RepositoryDir(name)
	if err != nil {
		return err
	}
	// The clone goes first: a failure there leaves the record in place, so the
	// repository is still managed and still removable. The other order would
	// leave an unreferenced clone nothing knows about.
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("repo remove: cannot delete the clone of %q at %s: %w", name, path, err)
	}
	if err := s.DeleteRepository(name); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s\tREMOVED\n", name)
	return err
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

// parseRepoFlags parses the flags of a repo subcommand that takes a repository
// name before them.
//
// The flag package stops at the first non-flag argument, so `repo status NAME
// --data-dir DIR` would leave the flags unparsed. The name is therefore taken
// off the front first. Anything left after the flags is an error rather than
// ignored: a second name is far more likely a typo than something to drop.
func parseRepoFlags(fs *flag.FlagSet, args []string) (string, error) {
	name := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if rest := fs.Args(); len(rest) > 0 {
		return "", fmt.Errorf("%s: unexpected argument %q", fs.Name(), rest[0])
	}
	return name, nil
}

// lastSync renders when a repository's refs were last fetched. A repository that
// has not been synced since it was added says so, rather than reporting the zero
// time as if it were a date.
func lastSync(r store.Repository) string {
	if r.LastSyncAt.IsZero() {
		return "never synced"
	}
	return r.LastSyncAt.Format(time.RFC3339)
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
// derived from a repository name come from the command line, and handing those
// to a shell is how a remote URL becomes a command.
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
// command to branch on.
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
