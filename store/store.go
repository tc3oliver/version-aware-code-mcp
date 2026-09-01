// Package store is the on-disk home of a managed vacmcp installation: the
// clones, worktrees, indexes and metadata behind repositories and contexts that
// were created through the management CLI rather than written by hand.
//
// Everything lives under one data directory, `~/.vacmcp` unless another one is
// given, in a fixed layout:
//
//	repos/         one git clone per repository, named after the repository
//	worktrees/     one checkout per context, filed under its repository
//	repositories/  one JSON record per repository
//	contexts/      one JSON record per context
//	zoekt/         the search index shards
//	runtime/       generated files, such as the configuration a managed server serves
//	locks/         one lock file per repository and one for the data directory
//	               itself, whose content is never read
//
// The records here are the only place a context's pinned revision is written
// down, so two properties are not negotiable:
//
//   - Every record is written atomically: into a temporary file in the same
//     directory, flushed, then renamed over the destination. A process killed
//     mid-write leaves the previous record whole and a stray temporary file
//     that no reader looks at. There is no interleaving that leaves half a
//     record behind.
//   - Repository names and context IDs come from the command line and become
//     path elements, so they are checked against an allowlist before any of
//     them reaches the filesystem. That allowlist has no way to spell "..", an
//     absolute path or a separator of any kind, which is what keeps a name from
//     addressing a file outside the data directory.
//
// A caller-actionable failure is a *[vacerr.Error] with an existing v0.1.0
// code: a rejected name is [vacerr.InvalidArgument], a record that is not there
// is [vacerr.RepositoryNotFound] or [vacerr.ContextNotFound]. A failing disk
// comes back as the wrapped operating system error instead, because it is not
// something a tool caller can be told in terms of the tool API.
//
// The records are metadata only. No credential is stored here: authentication
// is system git's business (SSH agent, ~/.ssh/config, git credential helper).
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// The layout's directory names. They are part of the on-disk contract: an
// operator inspecting a data directory, and every later command reading one,
// finds the same seven.
const (
	reposDir        = "repos"
	worktreesDir    = "worktrees"
	repositoriesDir = "repositories"
	contextsDir     = "contexts"
	zoektDir        = "zoekt"
	runtimeDir      = "runtime"
	locksDir        = "locks"
)

// layout is what Open creates, in one place so the directories cannot drift
// apart from the accessors below.
var layout = []string{reposDir, worktreesDir, repositoriesDir, contextsDir, zoektDir, runtimeDir, locksDir}

// recordExt is the suffix a metadata file has, and the one a temporary file
// deliberately does not: it is how a reader tells a record from the leftovers of
// a write that was killed.
const recordExt = ".json"

// serverLockName is the lock file that stands for the whole data directory
// rather than for one repository in it.
//
// It shares the locks directory with the per-repository files and can never be
// one of them: a repository name has to begin with a letter or a digit, so
// nothing that passes validateName produces a file starting with a dot.
const serverLockName = ".server.lock"

// Repository is the record of one repository managed in this data directory.
//
// URL is the git remote it was cloned from; it is a plain remote URL and never
// carries a credential. State is the lifecycle state the repository commands
// maintain, and LastSyncAt is when its refs were last fetched — zero for a
// repository that has not been synced since it was added. UpdatedAt is set on
// every write, so a record's age is readable without stat'ing the file.
type Repository struct {
	Name       string    `json:"name"`
	URL        string    `json:"url"`
	State      string    `json:"state"`
	LastSyncAt time.Time `json:"last_sync_at,omitzero"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Context is the record of one version context managed in this data directory.
//
// A context is one or more repositories, each pinned to its own revision, and
// one State for all of them: a context is built or it is not, and there is no
// state in which some of its members may be answered from. Members is therefore
// a list at every length, exactly as [github.com/tc3oliver/version-aware-code-mcp/vacctx.Workspace]
// is — one member and several are the same value, so no code above here has a
// single-repository path to keep correct beside a multi-repository one.
type Context struct {
	ID        string
	Members   []ContextMember
	State     string
	UpdatedAt time.Time
}

// ContextMember is one repository of a context, pinned to one revision of it.
//
// Revision is the full commit SHA, and it is immutable once resolved: another
// version of the same repository is another context, not an edit of this one.
// Branch is the ref inside that repository's clone whose search index serves
// this member, and GraphRef is the graph project backing it; both are
// generated, never typed by a user. The three are empty until the context's
// lifecycle has got that far, which is why State, not the presence of a field,
// decides whether a context may be served.
type ContextMember struct {
	Repository string `json:"repository"`
	Branch     string `json:"branch,omitempty"`
	Revision   string `json:"revision,omitempty"`
	GraphRef   string `json:"graph_ref,omitempty"`
}

// contextRecord is a context as it is written down, which is not quite the
// context as used: there are two spellings and one meaning, the same way the
// configuration file has two.
//
// A context over a single repository writes that repository's fields directly
// under the record — the only spelling v0.4.0 ever wrote, and still what almost
// every record says — and a context over several writes them as members
// instead. That is what lets a data directory built by v0.4.0 be read and
// served here with no conversion of any kind: a record with one member is
// byte-for-byte the record v0.4.0 wrote.
//
// The repository field is a JSON string in the first spelling and the empty
// JSON array in the second, and that difference is deliberate rather than
// decorative. A v0.4.0 binary decodes this field into a string; handed a
// multi-member record it would otherwise read a context with no repository at
// all, keep it, and hand it to code that decides on repository equality —
// `repo remove` would then find no context depending on a clone two members are
// pinned in and delete it. Making the field an array means such a binary fails
// on the type instead, loudly, before the record is anything to it. The array
// is always empty, so it carries nothing that could disagree with members.
type contextRecord struct {
	ID string `json:"id"`
	// Raw, because its type is what says which spelling this is.
	Repository json.RawMessage `json:"repository"`
	Branch     string          `json:"branch,omitempty"`
	Revision   string          `json:"revision,omitempty"`
	GraphRef   string          `json:"graph_ref,omitempty"`
	Members    []ContextMember `json:"members,omitempty"`
	State      string          `json:"state"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

// noRepository is what the repository field of a multi-member record holds: not
// one repository, and nothing an older decoder can read as one.
const noRepository = "[]"

// MarshalJSON writes the spelling this context's member count decides.
func (c Context) MarshalJSON() ([]byte, error) {
	record := contextRecord{ID: c.ID, State: c.State, UpdatedAt: c.UpdatedAt}
	if len(c.Members) == 1 {
		m := c.Members[0]
		name, err := json.Marshal(m.Repository)
		if err != nil {
			return nil, fmt.Errorf("store: cannot encode the repository of context %q: %w", c.ID, err)
		}
		record.Repository = name
		record.Branch, record.Revision, record.GraphRef = m.Branch, m.Revision, m.GraphRef
	} else {
		record.Repository = json.RawMessage(noRepository)
		record.Members = c.Members
	}
	return json.Marshal(record)
}

// UnmarshalJSON reads either spelling into the one member list everything above
// works on.
//
// A record that uses both at once is refused rather than merged: it is not
// something any version of vacmcp writes, so it was edited by hand or written
// by something that is not vacmcp, and guessing which half to believe is
// guessing which revision to answer from.
func (c *Context) UnmarshalJSON(data []byte) error {
	var record contextRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return err
	}
	c.ID, c.State, c.UpdatedAt = record.ID, record.State, record.UpdatedAt

	if len(record.Members) > 0 {
		var none []string
		if err := json.Unmarshal(record.Repository, &none); err != nil || len(none) != 0 ||
			record.Branch != "" || record.Revision != "" || record.GraphRef != "" {
			return fmt.Errorf("store: context %q is written as one repository and as a members list at once", record.ID)
		}
		c.Members = record.Members
		return nil
	}

	if len(record.Repository) == 0 {
		return fmt.Errorf("store: context %q names no repository", record.ID)
	}
	var repository string
	if err := json.Unmarshal(record.Repository, &repository); err != nil {
		return fmt.Errorf("store: context %q: %w", record.ID, err)
	}
	c.Members = []ContextMember{{
		Repository: repository,
		Branch:     record.Branch,
		Revision:   record.Revision,
		GraphRef:   record.GraphRef,
	}}
	return nil
}

// Store is one data directory.
type Store struct {
	root string
}

// Open returns the store rooted at dir, creating the layout if it is not there
// yet. An empty dir means the default location, `~/.vacmcp`: the caller passes
// what --data-dir was given without having to spell the default itself.
//
// The path is made absolute up front, so a store keeps addressing the same
// directory no matter what the process's working directory does afterwards.
func Open(dir string) (*Store, error) {
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("store: no data directory given and no home directory to fall back on: %w", err)
		}
		dir = filepath.Join(home, ".vacmcp")
	}

	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("store: %s: %w", dir, err)
	}

	// 0o700 throughout: a data directory names private repositories and holds
	// their source, and nothing outside this user's account has business
	// reading it.
	for _, name := range layout {
		if err := os.MkdirAll(filepath.Join(root, name), 0o700); err != nil {
			return nil, fmt.Errorf("store: cannot create the data directory layout under %s: %w", root, err)
		}
	}
	return &Store{root: root}, nil
}

// Root is the absolute path of the data directory.
func (s *Store) Root() string { return s.root }

// RepositoryDir is where the clone of the named repository lives.
func (s *Store) RepositoryDir(name string) (string, error) {
	if err := validateName("repository", name); err != nil {
		return "", err
	}
	return filepath.Join(s.root, reposDir, name), nil
}

// WorktreeDir is where the checkout of one context of one repository lives. The
// repository name is a directory of its own, so removing a repository's
// worktrees is removing one subtree.
func (s *Store) WorktreeDir(repository, contextID string) (string, error) {
	if err := validateName("repository", repository); err != nil {
		return "", err
	}
	if err := validateName("context", contextID); err != nil {
		return "", err
	}
	return filepath.Join(s.root, worktreesDir, repository, contextID), nil
}

// LockFile is the file whose lock an operation on the named repository holds.
//
// It is a file of its own rather than one of the records, because a record is
// replaced by a rename: a lock taken on it would be a lock on an inode that the
// next write leaves nobody holding. Nothing ever reads the content, and nothing
// deletes the file — a lock file removed while it is held is one that another
// waiter can create and take again.
func (s *Store) LockFile(repository string) (string, error) {
	if err := validateName("repository", repository); err != nil {
		return "", err
	}
	return filepath.Join(s.root, locksDir, repository+".lock"), nil
}

// ServerLockFile is the file whose lock says a managed server is running on
// this data directory.
//
// It takes no name and validates none: this lock is the data directory itself,
// not anything a user typed, which is also why it is one file rather than one
// per repository. A managed server serves the contexts of the whole directory,
// so what it has to be held against is every command that would take one of
// them apart.
func (s *Store) ServerLockFile() string {
	return filepath.Join(s.root, locksDir, serverLockName)
}

// ZoektDir is where the search index shards live.
func (s *Store) ZoektDir() string { return filepath.Join(s.root, zoektDir) }

// RuntimeDir is where generated files live. Nothing in it is a source of
// truth — it can be deleted and regenerated from the records.
func (s *Store) RuntimeDir() string { return filepath.Join(s.root, runtimeDir) }

// PutRepository writes r, replacing any record filed under the same name.
// UpdatedAt is stamped here rather than by the caller, so no write can forget
// it.
func (s *Store) PutRepository(r Repository) error {
	path, err := s.recordPath(repositoriesDir, "repository", r.Name)
	if err != nil {
		return err
	}
	r.UpdatedAt = time.Now().UTC()
	return writeRecord(path, r)
}

// Repository returns the record filed under name, or [vacerr.RepositoryNotFound]
// if this data directory does not manage it.
func (s *Store) Repository(name string) (Repository, error) {
	path, err := s.recordPath(repositoriesDir, "repository", name)
	if err != nil {
		return Repository{}, err
	}
	r, err := readRecord[Repository](path)
	if errors.Is(err, fs.ErrNotExist) {
		return Repository{}, vacerr.New(
			vacerr.RepositoryNotFound,
			fmt.Sprintf("store: repository %q is not managed in %s", name, s.root),
			map[string]any{"repository": name},
		)
	}
	return r, err
}

// Repositories returns every repository record, ordered by name.
func (s *Store) Repositories() ([]Repository, error) {
	return listRecords[Repository](filepath.Join(s.root, repositoriesDir))
}

// DeleteRepository removes the record filed under name. Whether a repository
// may be removed at all — it may still have contexts — is the caller's
// question; this only forgets the record.
func (s *Store) DeleteRepository(name string) error {
	path, err := s.recordPath(repositoriesDir, "repository", name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return vacerr.New(
				vacerr.RepositoryNotFound,
				fmt.Sprintf("store: repository %q is not managed in %s", name, s.root),
				map[string]any{"repository": name},
			)
		}
		return fmt.Errorf("store: cannot remove the record of repository %q: %w", name, err)
	}
	return nil
}

// PutContext writes c, replacing any record filed under the same ID. Both the
// context ID and the repository every member names are validated: a repository
// name is a path element everywhere else in the layout, so a record may not
// carry one that could not be written down safely.
//
// A context with no member is refused for the same reason the configuration
// refuses one: it is not a narrower scope, it is no scope, and every artifact
// this record would own hangs off a repository it does not name.
func (s *Store) PutContext(c Context) error {
	path, err := s.recordPath(contextsDir, "context", c.ID)
	if err != nil {
		return err
	}
	if len(c.Members) == 0 {
		return vacerr.New(
			vacerr.InvalidArgument,
			fmt.Sprintf("store: context %q names no repository", c.ID),
			map[string]any{"context": c.ID},
		)
	}
	for _, m := range c.Members {
		if err := validateName("repository", m.Repository); err != nil {
			return err
		}
	}
	c.UpdatedAt = time.Now().UTC()
	return writeRecord(path, c)
}

// Context returns the record filed under id, or [vacerr.ContextNotFound] if this
// data directory does not manage it.
func (s *Store) Context(id string) (Context, error) {
	path, err := s.recordPath(contextsDir, "context", id)
	if err != nil {
		return Context{}, err
	}
	c, err := readRecord[Context](path)
	if errors.Is(err, fs.ErrNotExist) {
		return Context{}, vacerr.New(
			vacerr.ContextNotFound,
			fmt.Sprintf("store: context %q is not managed in %s", id, s.root),
			map[string]any{"context": id},
		)
	}
	return c, err
}

// Contexts returns every context record, ordered by ID.
func (s *Store) Contexts() ([]Context, error) {
	return listRecords[Context](filepath.Join(s.root, contextsDir))
}

// DeleteContext removes the record filed under id. The artifacts the context
// owns — worktree, search ref, graph — are cleaned up by the caller that knows
// about them.
func (s *Store) DeleteContext(id string) error {
	path, err := s.recordPath(contextsDir, "context", id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return vacerr.New(
				vacerr.ContextNotFound,
				fmt.Sprintf("store: context %q is not managed in %s", id, s.root),
				map[string]any{"context": id},
			)
		}
		return fmt.Errorf("store: cannot remove the record of context %q: %w", id, err)
	}
	return nil
}

// recordPath is the file a record is kept in. It is the single place a
// user-supplied name turns into a path, which is what makes the validation
// below unbypassable: every read, write and delete goes through here.
func (s *Store) recordPath(dir, kind, name string) (string, error) {
	if err := validateName(kind, name); err != nil {
		return "", err
	}
	return filepath.Join(s.root, dir, name+recordExt), nil
}

// namePattern is the allowlist every repository name and context ID has to
// match. It is an allowlist rather than a list of forbidden sequences because
// the forbidden ones are open-ended — "..", "/", "\", "", a leading dash that
// git would read as a flag, a NUL byte, a Unicode separator lookalike — while
// what a name legitimately needs is small and closed. A name matching this
// cannot be anything but a single path element inside the directory it is
// joined to.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// maxNameLength keeps a name inside the 255-byte file name limit of every
// filesystem this runs on, with room for the suffix and for being nested under
// another name in worktrees/.
const maxNameLength = 100

// validateName rejects a name that cannot be used as a path element. kind is
// "repository" or "context" and names both the wording and the details key of
// the error, so the caller learns which of the two arguments was refused.
func validateName(kind, name string) error {
	if len(name) > maxNameLength || !namePattern.MatchString(name) {
		return vacerr.New(
			vacerr.InvalidArgument,
			fmt.Sprintf("store: %s name %q is not a usable path element: use 1 to %d characters from A-Z a-z 0-9 . _ - starting with a letter or digit", kind, name, maxNameLength),
			map[string]any{kind: name},
		)
	}
	return nil
}

// writeRecord serialises record and writes it atomically to path.
func writeRecord(path string, record any) error {
	// Indented, with a trailing newline: these files are read by people
	// debugging a data directory as often as by the commands that wrote them.
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("store: cannot encode the record for %s: %w", path, err)
	}
	return atomicWrite(path, append(data, '\n'))
}

// readRecord decodes the record in path. A missing file comes back as
// [fs.ErrNotExist] unwrapped enough for the caller to turn it into the right
// not-found code; anything else is reported with the path that failed.
func readRecord[T any](path string) (T, error) {
	var record T
	data, err := os.ReadFile(path)
	if err != nil {
		return record, err
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return record, fmt.Errorf("store: %s is not a valid record: %w", path, err)
	}
	return record, nil
}

// listRecords decodes every record in dir, in the order [os.ReadDir] returns
// them, which is by file name and therefore by record name.
//
// Only files with the record suffix are read. That is what makes a temporary
// file left behind by a killed write invisible: it never had the suffix, so no
// listing has ever seen it as a record.
func listRecords[T any](dir string) ([]T, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("store: cannot list %s: %w", dir, err)
	}
	records := make([]T, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), recordExt) {
			continue
		}
		record, err := readRecord[T](filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

// atomicWrite replaces path with data, or leaves path exactly as it was.
//
// The temporary file is created in the destination's own directory so the
// rename stays within one filesystem, where it is atomic: a reader sees either
// the old file or the new one, never a partial one. The flush before the rename
// is what makes that true after a power loss too — renaming first and flushing
// later can leave the name pointing at a file whose content never reached the
// disk. The directory is flushed after the rename for the same reason: the
// rename itself has to survive.
//
// A killed process cannot run the cleanup below, so it leaves the temporary
// file behind. That is the intended trade: a stray file that no reader looks at
// is harmless, a truncated record is not.
func atomicWrite(path string, data []byte) (err error) {
	dir := filepath.Dir(path)

	// The pattern deliberately produces no ".json" suffix, so a leftover is
	// never mistaken for a record.
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("store: cannot create a temporary file in %s: %w", dir, err)
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()

	if _, err = tmp.Write(data); err != nil {
		return fmt.Errorf("store: cannot write %s: %w", tmp.Name(), err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("store: cannot flush %s: %w", tmp.Name(), err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("store: cannot close %s: %w", tmp.Name(), err)
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("store: cannot replace %s: %w", path, err)
	}
	return syncDir(dir)
}

// syncDir flushes a directory entry, which is what makes a completed rename
// durable rather than merely visible to this machine until it loses power.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("store: cannot open %s: %w", dir, err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("store: cannot flush %s: %w", dir, err)
	}
	return nil
}
