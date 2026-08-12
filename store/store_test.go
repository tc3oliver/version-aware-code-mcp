package store_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tc3oliver/version-aware-code-mcp/store"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// open returns a store on a fresh data directory.
func open(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	return s
}

// errCode returns the vacerr code err failed with.
func errCode(t *testing.T, err error) vacerr.Code {
	t.Helper()
	if err == nil {
		t.Fatal("got no error, want one")
	}
	var vErr *vacerr.Error
	if !errors.As(err, &vErr) {
		t.Fatalf("error = %v (%T), want *vacerr.Error", err, err)
	}
	return vErr.Code
}

// tree lists every path under root, relative to it, so two calls can be
// compared to prove that an operation touched nothing.
func tree(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(%s): %v", root, err)
	}
	slices.Sort(paths)
	return paths
}

func TestOpenCreatesTheLayout(t *testing.T) {
	s := open(t)

	for _, dir := range []string{"repos", "worktrees", "repositories", "contexts", "zoekt", "runtime"} {
		info, err := os.Stat(filepath.Join(s.Root(), dir))
		if err != nil {
			t.Errorf("Stat(%s): %v", dir, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%s is not a directory", dir)
		}
	}

	// Opening an existing data directory is how every later command starts, so
	// it has to be a no-op rather than a conflict.
	if _, err := store.Open(s.Root()); err != nil {
		t.Errorf("Open() on an existing data directory: %v", err)
	}
}

func TestOpenDefaultsToTheHomeDataDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	s, err := store.Open("")
	if err != nil {
		t.Fatalf("Open(\"\") error = %v", err)
	}
	if want := filepath.Join(home, ".vacmcp"); s.Root() != want {
		t.Errorf("Open(\"\").Root() = %q, want %q", s.Root(), want)
	}

	// An explicit data directory wins over the default, and is absolute
	// whatever was passed in.
	given := t.TempDir()
	override, err := store.Open(given)
	if err != nil {
		t.Fatalf("Open(%s) error = %v", given, err)
	}
	if override.Root() != given {
		t.Errorf("Open(%q).Root() = %q, want %q", given, override.Root(), given)
	}
}

func TestPaths(t *testing.T) {
	s := open(t)

	repoDir, err := s.RepositoryDir("backend")
	if err != nil {
		t.Fatalf("RepositoryDir() error = %v", err)
	}
	if want := filepath.Join(s.Root(), "repos", "backend"); repoDir != want {
		t.Errorf("RepositoryDir() = %q, want %q", repoDir, want)
	}

	worktree, err := s.WorktreeDir("backend", "backend-v2")
	if err != nil {
		t.Fatalf("WorktreeDir() error = %v", err)
	}
	if want := filepath.Join(s.Root(), "worktrees", "backend", "backend-v2"); worktree != want {
		t.Errorf("WorktreeDir() = %q, want %q", worktree, want)
	}

	if want := filepath.Join(s.Root(), "zoekt"); s.ZoektDir() != want {
		t.Errorf("ZoektDir() = %q, want %q", s.ZoektDir(), want)
	}
	if want := filepath.Join(s.Root(), "runtime"); s.RuntimeDir() != want {
		t.Errorf("RuntimeDir() = %q, want %q", s.RuntimeDir(), want)
	}
}

func TestRepositoryRoundTrip(t *testing.T) {
	s := open(t)

	synced := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	want := store.Repository{
		Name:       "backend",
		URL:        "git@example.com:example/backend.git",
		State:      "READY",
		LastSyncAt: synced,
	}
	if err := s.PutRepository(want); err != nil {
		t.Fatalf("PutRepository() error = %v", err)
	}

	got, err := s.Repository("backend")
	if err != nil {
		t.Fatalf("Repository() error = %v", err)
	}
	if got.Name != want.Name || got.URL != want.URL || got.State != want.State || !got.LastSyncAt.Equal(synced) {
		t.Errorf("Repository() = %+v, want %+v", got, want)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("Repository().UpdatedAt is zero, want the write to have stamped it")
	}

	// A second write replaces the record rather than adding one.
	want.State = "FAILED"
	if err := s.PutRepository(want); err != nil {
		t.Fatalf("PutRepository() error = %v", err)
	}
	if err := s.PutRepository(store.Repository{Name: "frontend", State: "READY"}); err != nil {
		t.Fatalf("PutRepository() error = %v", err)
	}

	list, err := s.Repositories()
	if err != nil {
		t.Fatalf("Repositories() error = %v", err)
	}
	names := []string{}
	for _, r := range list {
		names = append(names, r.Name)
	}
	if !slices.Equal(names, []string{"backend", "frontend"}) {
		t.Errorf("Repositories() = %v, want [backend frontend] in name order", names)
	}
	if list[0].State != "FAILED" {
		t.Errorf("Repositories()[0].State = %q, want the second write to have replaced the first", list[0].State)
	}

	if err := s.DeleteRepository("backend"); err != nil {
		t.Fatalf("DeleteRepository() error = %v", err)
	}
	if _, err := s.Repository("backend"); errCode(t, err) != vacerr.RepositoryNotFound {
		t.Errorf("Repository() after delete = %v, want REPOSITORY_NOT_FOUND", err)
	}
	if err := s.DeleteRepository("backend"); errCode(t, err) != vacerr.RepositoryNotFound {
		t.Errorf("DeleteRepository() on an unknown repository = %v, want REPOSITORY_NOT_FOUND", err)
	}
}

func TestContextRoundTrip(t *testing.T) {
	s := open(t)

	// A context exists before it has a revision: the lifecycle fills the
	// resolved fields in later, and state is what says whether it may be used.
	if err := s.PutContext(store.Context{ID: "backend-v2", Repository: "backend", State: "CREATING"}); err != nil {
		t.Fatalf("PutContext() error = %v", err)
	}
	got, err := s.Context("backend-v2")
	if err != nil {
		t.Fatalf("Context() error = %v", err)
	}
	if got.Revision != "" || got.State != "CREATING" {
		t.Errorf("Context() = %+v, want no revision and state CREATING", got)
	}

	want := store.Context{
		ID:         "backend-v2",
		Repository: "backend",
		Branch:     "vacmcp/backend-v2",
		Revision:   "94cb8213d7f2b1c9a06e5d43f8b7c21e0d9a4f65",
		GraphRef:   "backend-v2",
		State:      "READY",
	}
	if err := s.PutContext(want); err != nil {
		t.Fatalf("PutContext() error = %v", err)
	}
	if err := s.PutContext(store.Context{ID: "backend-v1", Repository: "backend", State: "FAILED"}); err != nil {
		t.Fatalf("PutContext() error = %v", err)
	}

	got, err = s.Context("backend-v2")
	if err != nil {
		t.Fatalf("Context() error = %v", err)
	}
	got.UpdatedAt = time.Time{}
	if got != want {
		t.Errorf("Context() = %+v, want %+v", got, want)
	}

	list, err := s.Contexts()
	if err != nil {
		t.Fatalf("Contexts() error = %v", err)
	}
	ids := []string{}
	for _, c := range list {
		ids = append(ids, c.ID)
	}
	if !slices.Equal(ids, []string{"backend-v1", "backend-v2"}) {
		t.Errorf("Contexts() = %v, want [backend-v1 backend-v2] in ID order", ids)
	}

	// Removing one context leaves the other alone.
	if err := s.DeleteContext("backend-v1"); err != nil {
		t.Fatalf("DeleteContext() error = %v", err)
	}
	if _, err := s.Context("backend-v1"); errCode(t, err) != vacerr.ContextNotFound {
		t.Errorf("Context() after delete = %v, want CONTEXT_NOT_FOUND", err)
	}
	if _, err := s.Context("backend-v2"); err != nil {
		t.Errorf("Context(backend-v2) after removing another context: %v", err)
	}
	if err := s.DeleteContext("backend-v1"); errCode(t, err) != vacerr.ContextNotFound {
		t.Errorf("DeleteContext() on an unknown context = %v, want CONTEXT_NOT_FOUND", err)
	}
}

// TestRejectsUnsafeNames is the path traversal defence: a name that is not a
// plain path element is refused before anything is written, and refusing it
// leaves the data directory byte for byte as it was.
func TestRejectsUnsafeNames(t *testing.T) {
	names := []struct{ label, name string }{
		{"parent directory", ".."},
		{"traversal", "../evil"},
		{"deep traversal", "../../etc/passwd"},
		{"traversal in the middle", "backend/../../etc"},
		{"absolute path", "/etc/passwd"},
		{"absolute windows path", `C:\Windows`},
		{"separator", "example/backend"},
		{"backslash separator", `example\backend`},
		{"current directory", "."},
		{"hidden", ".ssh"},
		{"empty", ""},
		{"blank", " "},
		{"leading dash", "--upload-pack=evil"},
		{"newline", "backend\n../evil"},
		{"nul byte", "backend\x00"},
		{"home shorthand", "~"},
		{"too long", strings.Repeat("a", 101)},
	}

	for _, unsafe := range names {
		t.Run(unsafe.label, func(t *testing.T) {
			s := open(t)
			before := tree(t, s.Root())

			_, readRepoErr := s.Repository(unsafe.name)
			_, readContextErr := s.Context(unsafe.name)
			_, repositoryDirErr := s.RepositoryDir(unsafe.name)
			_, worktreeRepoErr := s.WorktreeDir(unsafe.name, "backend-v2")
			_, worktreeContextErr := s.WorktreeDir("backend", unsafe.name)

			calls := []struct {
				name string
				err  error
			}{
				{"PutRepository", s.PutRepository(store.Repository{Name: unsafe.name, State: "READY"})},
				{"DeleteRepository", s.DeleteRepository(unsafe.name)},
				{"PutContext", s.PutContext(store.Context{ID: unsafe.name, Repository: "backend", State: "CREATING"})},
				{"PutContext repository", s.PutContext(store.Context{ID: "backend-v2", Repository: unsafe.name, State: "CREATING"})},
				{"DeleteContext", s.DeleteContext(unsafe.name)},
				{"Repository", readRepoErr},
				{"Context", readContextErr},
				{"RepositoryDir", repositoryDirErr},
				{"WorktreeDir repository", worktreeRepoErr},
				{"WorktreeDir context", worktreeContextErr},
			}

			for _, call := range calls {
				if code := errCode(t, call.err); code != vacerr.InvalidArgument {
					t.Errorf("%s(%q) error code = %s, want INVALID_ARGUMENT", call.name, unsafe.name, code)
				}
			}

			if after := tree(t, s.Root()); !slices.Equal(before, after) {
				t.Errorf("the data directory changed after rejecting %q:\nbefore %v\nafter  %v", unsafe.name, before, after)
			}
		})
	}
}

func TestAcceptsOrdinaryNames(t *testing.T) {
	s := open(t)
	for _, name := range []string{"a", "backend", "Backend-2", "backend_v2", "release.1.x", strings.Repeat("a", 100)} {
		if err := s.PutRepository(store.Repository{Name: name, State: "READY"}); err != nil {
			t.Errorf("PutRepository(%q) error = %v", name, err)
		}
	}
}

// TestLeftoverTemporaryFileIsNotARecord covers what a write killed halfway
// leaves behind: the temporary file it was still writing into. It must be
// invisible to every reader, whatever is in it.
func TestLeftoverTemporaryFileIsNotARecord(t *testing.T) {
	s := open(t)
	if err := s.PutRepository(store.Repository{Name: "backend", URL: "git@example.com:example/backend.git", State: "READY"}); err != nil {
		t.Fatalf("PutRepository() error = %v", err)
	}

	half := filepath.Join(s.Root(), "repositories", ".tmp-641992370")
	if err := os.WriteFile(half, []byte(`{"name":"backend","url":"git@exam`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := s.Repository("backend")
	if err != nil {
		t.Fatalf("Repository() error = %v", err)
	}
	if got.URL != "git@example.com:example/backend.git" {
		t.Errorf("Repository().URL = %q, want the complete record", got.URL)
	}
	list, err := s.Repositories()
	if err != nil {
		t.Fatalf("Repositories() error = %v", err)
	}
	if len(list) != 1 {
		t.Errorf("Repositories() = %d records, want 1: a leftover temporary file is not a record", len(list))
	}
}

// crashDirEnv carries the data directory to the child process of
// TestKilledWriteLeavesNoHalfWrittenRecord, and is what tells that process it is
// the child.
const crashDirEnv = "VACMCP_STORE_CRASH_DIR"

const (
	crashRecordName = "backend"
	// Big enough that a write takes long enough to be killed in the middle of.
	crashURLLength = 1 << 18
	// recordSuffix is what store names a record file, spelled out here because
	// this test reaches for one by path.
	recordSuffix = ".json"
)

// TestKilledWriteLeavesNoHalfWrittenRecord kills a process while it is writing
// records and checks that what it left on disk is still a whole record. This is
// the interruption the atomic write exists for, and nothing short of killing a
// real process proves it.
//
// Where the kill lands is a race with the writer, so it is tried until it
// demonstrably landed inside a write — the temporary file the killed process
// could no longer clean up is the proof — while the record is checked after
// every kill either way.
func TestKilledWriteLeavesNoHalfWrittenRecord(t *testing.T) {
	for attempt := 1; attempt <= 8; attempt++ {
		if killWriter(t) {
			return
		}
	}
	t.Log("no kill landed inside a write; the record was whole after every one of them anyway")
}

// killWriter starts a process that rewrites one record until it dies, kills it,
// and reports whether the kill landed inside a write. It fails the test if what
// the killed process left behind is not exactly one whole record.
func killWriter(t *testing.T) bool {
	t.Helper()

	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	child := exec.Command(os.Args[0], "-test.run=^TestCrashWriterChild$")
	child.Env = append(os.Environ(), crashDirEnv+"="+dir)
	if err := child.Start(); err != nil {
		t.Fatalf("starting the writer: %v", err)
	}
	defer func() { _ = child.Wait() }()

	// Wait until it has completed a write, so the kill has a write loop to land
	// in rather than a process that has not started writing yet.
	record := filepath.Join(dir, "repositories", crashRecordName+recordSuffix)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(record); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = child.Process.Kill()
			t.Fatalf("the writer never wrote %s", record)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatalf("killing the writer: %v", err)
	}
	_ = child.Wait()

	got, err := s.Repository(crashRecordName)
	if err != nil {
		t.Fatalf("Repository() after the writer was killed: %v", err)
	}
	if len(got.URL) != crashURLLength {
		t.Errorf("Repository().URL is %d bytes, want the %d bytes of a complete record", len(got.URL), crashURLLength)
	}
	if got.State != "READY" {
		t.Errorf("Repository().State = %q, want READY", got.State)
	}

	list, err := s.Repositories()
	if err != nil {
		t.Fatalf("Repositories() after the writer was killed: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("Repositories() = %d records, want 1: whatever the killed write left behind is not a record", len(list))
	}

	// A leftover temporary file is a write that was cut off partway: the record
	// above was read while a half-written one sat next to it.
	left := tree(t, s.Root())
	for _, path := range left {
		if strings.HasPrefix(filepath.Base(path), ".tmp-") {
			t.Logf("the kill landed inside a write, leaving %s; the record was still whole", path)
			return true
		}
	}
	return false
}

// TestCrashWriterChild is not a test of its own: it is the body of the process
// TestKilledWriteLeavesNoHalfWrittenRecord starts and kills. It rewrites one
// record until it is killed.
func TestCrashWriterChild(t *testing.T) {
	dir := os.Getenv(crashDirEnv)
	if dir == "" {
		t.Skip("runs only as the child of TestKilledWriteLeavesNoHalfWrittenRecord")
	}

	s, err := store.Open(dir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	record := store.Repository{
		Name:  crashRecordName,
		URL:   strings.Repeat("u", crashURLLength),
		State: "READY",
	}
	for {
		if err := s.PutRepository(record); err != nil {
			t.Fatalf("PutRepository() error = %v", err)
		}
	}
}
