package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// The history fixture is a repository whose worktree deliberately sits on a
// commit LATER than the one a context pins, because that is the only shape in
// which "walked from the pinned commit" and "walked from the checkout" give
// different answers. A test that checked the worktree back out to the pinned
// commit would pass either way and prove nothing.

const afterPinToken = "AFTER_PIN_ONLY"

// historyRepo is a five-commit repository:
//
//	A  add watchdog.c            (ResetWatchdog appears)
//	B  fix watchdog timeout      (touches watchdog.c and network.c)
//	C  add network retry         ← contexts pin here
//	D  post-pin change           (contains AFTER_PIN_ONLY)
//	E  another post-pin change   ← HEAD stays here
type historyRepo struct {
	path                   string
	a, b, c, d, head       string
	authorName, authorMail string
}

func newHistoryRepo(t *testing.T) historyRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	r := historyRepo{
		path:       t.TempDir(),
		authorName: "vacmcp",
		authorMail: "vacmcp@example.invalid",
	}

	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", r.path}, args...)...)
		// The machine's own git configuration must not change what the test sees.
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(rel, body string) {
		t.Helper()
		full := filepath.Join(r.path, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	// Author and committer dates are fixed so the metadata assertions compare
	// against a known value rather than against "whatever now was".
	commit := func(message, when string) string {
		t.Helper()
		run("add", ".")
		run(
			"-c", "user.name="+r.authorName, "-c", "user.email="+r.authorMail,
			"commit", "--no-verify", "--date="+when, "-m", message,
		)
		return run("rev-parse", "HEAD")
	}

	run("init")
	run("config", "user.name", r.authorName)
	run("config", "user.email", r.authorMail)
	t.Setenv("GIT_COMMITTER_DATE", "2026-01-01T00:00:00Z")

	write("src/watchdog.c", "void ResetWatchdog(void) { /* v1 */ }\n")
	r.a = commit("add watchdog", "2026-01-01T00:00:00Z")

	write("src/watchdog.c", "void ResetWatchdog(void) { /* v2 */ }\n")
	write("src/network.c", "void Connect(void) {}\n")
	r.b = commit("fix watchdog timeout", "2026-01-02T00:00:00Z")

	write("src/network.c", "void Connect(void) { retry(); }\n")
	r.c = commit("add network retry", "2026-01-03T00:00:00Z")

	write("src/network.c", "void Connect(void) { /* "+afterPinToken+" */ }\n")
	r.d = commit("post-pin change", "2026-01-04T00:00:00Z")

	write("src/watchdog.c", "void ResetWatchdog(void) { /* "+afterPinToken+" */ }\n")
	r.head = commit("another post-pin change", "2026-01-05T00:00:00Z")

	for _, pair := range [][2]string{{r.a, r.b}, {r.b, r.c}, {r.c, r.d}, {r.d, r.head}} {
		if pair[0] == pair[1] {
			t.Fatalf("the repository never moved: %s", pair[0])
		}
	}
	return r
}

// historyProvider builds a Provider over the fixture, and a CodeContext pinned to
// revision. The worktree is left where it is.
func historyProvider(t *testing.T, r historyRepo, ctxID, revision string) (*Provider, vacctx.CodeContext) {
	t.Helper()
	cfg := &config.Config{Repositories: map[string]config.Repository{
		"repo-a": {Path: r.path},
	}}
	return New(cfg), vacctx.CodeContext{
		ID: ctxID, Repository: "repo-a", Branch: "main", Revision: revision,
	}
}

// assertHeadUnmoved proves the test never cheated by checking the worktree out to
// the pinned commit.
func assertHeadUnmoved(t *testing.T, r historyRepo) {
	t.Helper()
	out, err := exec.Command("git", "-C", r.path, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != r.head {
		t.Fatalf("HEAD moved to %s; it must stay at %s or the version-scope assertion proves nothing", got, r.head)
	}
}

func commitSet(entries []provider.HistoryEntry) map[string]bool {
	out := map[string]bool{}
	for _, e := range entries {
		out[e.Commit] = true
	}
	return out
}

// --- Version scope: the core correctness requirement -------------------------

// TestHistoryIsScopedToThePinnedRevision proves the walk starts at the commit
// the context pins, not at HEAD.
//
// Commits D and E exist in the repository and are what the checkout is sitting
// on, so a walk from HEAD would report them. A context pinned to C must not see
// them at all — that is the difference between a version-aware history and a
// repository-wide one.
func TestHistoryIsScopedToThePinnedRevision(t *testing.T) {
	r := newHistoryRepo(t)
	p, codeCtx := historyProvider(t, r, "ctx-old", r.c)

	entries, err := p.SearchHistory(context.Background(), codeCtx, provider.HistoryQuery{})
	if err != nil {
		t.Fatalf("SearchHistory: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("a context pinned to C must still see A..C")
	}

	seen := commitSet(entries)
	for name, sha := range map[string]string{"A": r.a, "B": r.b, "C": r.c} {
		if !seen[sha] {
			t.Errorf("commit %s (%s) is in the pinned lineage but was not reported", name, sha)
		}
	}
	for name, sha := range map[string]string{"D": r.d, "E (HEAD)": r.head} {
		if seen[sha] {
			t.Errorf("commit %s (%s) is AFTER the pinned revision and must not be reported", name, sha)
		}
	}
	// The token only exists in D and E, so its presence would betray a leak even
	// if the commit ids somehow matched.
	for _, e := range entries {
		if strings.Contains(e.Message, afterPinToken) {
			t.Errorf("an after-pin commit leaked into the history: %+v", e)
		}
	}
	assertHeadUnmoved(t, r)
}

// TestTwoContextsSeeDifferentHistories is the same guarantee from the other
// side: the newer context sees the commits the older one cannot.
func TestTwoContextsSeeDifferentHistories(t *testing.T) {
	r := newHistoryRepo(t)

	pOld, ctxOld := historyProvider(t, r, "ctx-old", r.c)
	oldEntries, err := pOld.SearchHistory(context.Background(), ctxOld, provider.HistoryQuery{})
	if err != nil {
		t.Fatalf("old context: %v", err)
	}
	pNew, ctxNew := historyProvider(t, r, "ctx-new", r.head)
	newEntries, nerr := pNew.SearchHistory(context.Background(), ctxNew, provider.HistoryQuery{})
	if nerr != nil {
		t.Fatalf("new context: %v", nerr)
	}

	if commitSet(oldEntries)[r.d] {
		t.Error("the old context must not see commit D")
	}
	if !commitSet(newEntries)[r.d] {
		t.Error("the new context must see commit D")
	}
	if len(newEntries) <= len(oldEntries) {
		t.Errorf("the newer context should see more history: old=%d new=%d", len(oldEntries), len(newEntries))
	}
	assertHeadUnmoved(t, r)
}

// --- Filters: hit and miss ---------------------------------------------------

func TestHistoryQueryFilter(t *testing.T) {
	r := newHistoryRepo(t)
	p, codeCtx := historyProvider(t, r, "ctx", r.c)

	hit, err := p.SearchHistory(context.Background(), codeCtx, provider.HistoryQuery{Query: "watchdog"})
	if err != nil {
		t.Fatalf("hit: %v", err)
	}
	if len(hit) == 0 {
		t.Fatal(`"watchdog" should match the "fix watchdog timeout" commit`)
	}
	for _, e := range hit {
		if !strings.Contains(strings.ToLower(e.Message), "watchdog") {
			t.Errorf("entry does not match the query: %+v", e)
		}
	}

	miss, merr := p.SearchHistory(context.Background(), codeCtx, provider.HistoryQuery{Query: "nonexistent-message"})
	if merr != nil {
		t.Fatalf("miss: %v", merr)
	}
	if len(miss) != 0 {
		t.Errorf("a query matching nothing must return nothing, not fall back: %+v", miss)
	}
}

func TestHistorySymbolPickaxeFilter(t *testing.T) {
	r := newHistoryRepo(t)
	p, codeCtx := historyProvider(t, r, "ctx", r.c)

	hit, err := p.SearchHistory(context.Background(), codeCtx, provider.HistoryQuery{Symbol: "ResetWatchdog"})
	if err != nil {
		t.Fatalf("hit: %v", err)
	}
	if !commitSet(hit)[r.a] {
		t.Errorf("commit A introduced ResetWatchdog and should be reported: %+v", hit)
	}

	miss, merr := p.SearchHistory(context.Background(), codeCtx, provider.HistoryQuery{Symbol: "NotPresentSymbol"})
	if merr != nil {
		t.Fatalf("miss: %v", merr)
	}
	if len(miss) != 0 {
		t.Errorf("a symbol that never occurred must return nothing: %+v", miss)
	}
}

func TestHistoryPathFilter(t *testing.T) {
	r := newHistoryRepo(t)
	p, codeCtx := historyProvider(t, r, "ctx", r.c)

	hit, err := p.SearchHistory(context.Background(), codeCtx, provider.HistoryQuery{Path: "src/watchdog.c"})
	if err != nil {
		t.Fatalf("hit: %v", err)
	}
	if len(hit) == 0 {
		t.Fatal("src/watchdog.c has history and should report it")
	}
	// Only the asked-for path is reported, even though commit B also touched
	// src/network.c.
	for _, e := range hit {
		if e.Path != "src/watchdog.c" {
			t.Errorf("path filter leaked another path: %+v", e)
		}
	}

	miss, merr := p.SearchHistory(context.Background(), codeCtx, provider.HistoryQuery{Path: "does/not/exist.c"})
	if merr != nil {
		t.Fatalf("miss: %v", merr)
	}
	if len(miss) != 0 {
		t.Errorf("a path with no history must return nothing, not the whole repository: %+v", miss)
	}
}

// TestHistoryFiltersCombineWithAND proves the three filters intersect: only a
// commit-path entry satisfying all of them is reported, and dropping any one of
// them changes the answer.
func TestHistoryFiltersCombineWithAND(t *testing.T) {
	r := newHistoryRepo(t)
	p, codeCtx := historyProvider(t, r, "ctx", r.c)
	ctx := context.Background()

	// Commit A is the only entry satisfying all three: its message contains
	// "watchdog", it is where ResetWatchdog's occurrence count changed (-S
	// pickaxe: commit B only edits a comment on that line, so the count is
	// unchanged and B is correctly NOT a pickaxe match), and it touched
	// src/watchdog.c.
	all := provider.HistoryQuery{Query: "watchdog", Symbol: "ResetWatchdog", Path: "src/watchdog.c"}
	got, err := p.SearchHistory(ctx, codeCtx, all)
	if err != nil {
		t.Fatalf("combined: %v", err)
	}
	if len(got) != 1 || got[0].Commit != r.a || got[0].Path != "src/watchdog.c" {
		t.Fatalf("want exactly commit A at src/watchdog.c, got %+v", got)
	}

	// Dropping the symbol filter admits commit B, whose message also matches and
	// which also touched that path — so the pickaxe is doing work rather than
	// being redundant with the other two filters.
	wider, werr := p.SearchHistory(ctx, codeCtx, provider.HistoryQuery{Query: "watchdog", Path: "src/watchdog.c"})
	if werr != nil {
		t.Fatalf("without the symbol filter: %v", werr)
	}
	if !commitSet(wider)[r.b] {
		t.Errorf("dropping the symbol filter should admit commit B: %+v", wider)
	}

	// A filter that cannot be satisfied together with the others yields nothing
	// rather than being relaxed.
	for _, tc := range []struct {
		name string
		q    provider.HistoryQuery
	}{
		{"unsatisfiable message", provider.HistoryQuery{Query: "nonexistent-message", Symbol: "ResetWatchdog", Path: "src/watchdog.c"}},
		{"unsatisfiable symbol", provider.HistoryQuery{Query: "watchdog", Symbol: "NotPresentSymbol", Path: "src/watchdog.c"}},
		{"unsatisfiable path", provider.HistoryQuery{Query: "watchdog", Symbol: "ResetWatchdog", Path: "src/network.c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, rerr := p.SearchHistory(ctx, codeCtx, tc.q)
			if rerr != nil {
				t.Fatalf("SearchHistory: %v", rerr)
			}
			if len(res) != 0 {
				t.Fatalf("filters must AND, not fall back: %+v", res)
			}
		})
	}
}

// --- Limit -------------------------------------------------------------------

// TestHistoryLimitBoundaries pins the limit contract on a deterministic order.
func TestHistoryLimitBoundaries(t *testing.T) {
	r := newHistoryRepo(t)
	p, codeCtx := historyProvider(t, r, "ctx", r.c)
	ctx := context.Background()

	full, err := p.SearchHistory(ctx, codeCtx, provider.HistoryQuery{})
	if err != nil {
		t.Fatalf("unlimited: %v", err)
	}
	total := len(full)
	if total < 2 {
		t.Fatalf("the fixture should produce several entries, got %d", total)
	}

	for _, tc := range []struct {
		name  string
		limit int
		want  int
	}{
		{"limit 1", 1, 1},
		{"limit equals the result count", total, total},
		{"limit above the result count", total + 10, total},
		{"limit 0 means the default bound", 0, total},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, gerr := p.SearchHistory(ctx, codeCtx, provider.HistoryQuery{Limit: tc.limit})
			if gerr != nil {
				t.Fatalf("SearchHistory: %v", gerr)
			}
			if len(got) != tc.want {
				t.Fatalf("limit %d: want %d entries, got %d", tc.limit, tc.want, len(got))
			}
			// The cap is applied to a stable order, so a limited answer is the
			// prefix of the unlimited one rather than an arbitrary subset.
			for i := range got {
				if got[i] != full[i] {
					t.Fatalf("entry %d differs from the unlimited order:\n got %+v\nwant %+v", i, got[i], full[i])
				}
			}
		})
	}

	if _, nerr := p.SearchHistory(ctx, codeCtx, provider.HistoryQuery{Limit: -1}); nerr == nil {
		t.Fatal("a negative limit must be refused, not treated as unbounded")
	} else if !strings.Contains(nerr.Error(), string(vacerr.InvalidArgument)) {
		t.Errorf("a negative limit should be INVALID_ARGUMENT, got %v", nerr)
	}
}

// --- Commit metadata ---------------------------------------------------------

// TestHistoryMetadataMatchesGit asserts each field against the value git itself
// records, rather than merely checking the field is non-empty.
func TestHistoryMetadataMatchesGit(t *testing.T) {
	r := newHistoryRepo(t)
	p, codeCtx := historyProvider(t, r, "ctx", r.c)

	entries, err := p.SearchHistory(context.Background(), codeCtx, provider.HistoryQuery{Query: "fix watchdog timeout"})
	if err != nil {
		t.Fatalf("SearchHistory: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("commit B should be reported")
	}

	var got *provider.HistoryEntry
	for i := range entries {
		if entries[i].Path == "src/watchdog.c" {
			got = &entries[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("commit B changed src/watchdog.c, so that occurrence should exist: %+v", entries)
	}

	if got.Commit != r.b {
		t.Errorf("commit = %q, want %q", got.Commit, r.b)
	}
	if len(got.Commit) != 40 {
		t.Errorf("commit must be the full immutable id, got %q", got.Commit)
	}
	wantAuthor := r.authorName + " <" + r.authorMail + ">"
	if got.Author != wantAuthor {
		t.Errorf("author = %q, want %q", got.Author, wantAuthor)
	}
	if strings.TrimSpace(got.Message) != "fix watchdog timeout" {
		t.Errorf("message = %q, want %q", got.Message, "fix watchdog timeout")
	}
	// The fixture pinned this commit's author date, so the timestamp is a known
	// value rather than "whatever now was".
	if _, perr := time.Parse(time.RFC3339, got.Timestamp); perr != nil {
		t.Errorf("timestamp %q is not RFC3339: %v", got.Timestamp, perr)
	}
	if !strings.HasPrefix(got.Timestamp, "2026-01-02") {
		t.Errorf("timestamp = %q, want the fixture's 2026-01-02 author date", got.Timestamp)
	}
}

// TestHistoryReportsOneEntryPerChangedPath proves a multi-file commit keeps every
// file's provenance instead of being attributed to an arbitrary one.
func TestHistoryReportsOneEntryPerChangedPath(t *testing.T) {
	r := newHistoryRepo(t)
	p, codeCtx := historyProvider(t, r, "ctx", r.c)

	entries, err := p.SearchHistory(context.Background(), codeCtx, provider.HistoryQuery{Query: "fix watchdog timeout"})
	if err != nil {
		t.Fatalf("SearchHistory: %v", err)
	}
	paths := map[string]bool{}
	for _, e := range entries {
		if e.Commit != r.b {
			t.Fatalf("the query should only match commit B: %+v", e)
		}
		paths[e.Path] = true
	}
	// Commit B touched both files; both occurrences must be reported.
	for _, want := range []string{"src/watchdog.c", "src/network.c"} {
		if !paths[want] {
			t.Errorf("commit B changed %s, but no entry reports it: %+v", want, entries)
		}
	}
}

// --- Git command safety ------------------------------------------------------

// TestHistoryRefusesOptionInjection proves request fields cannot be read as git
// options and cannot widen the walk beyond the pinned revision.
func TestHistoryRefusesOptionInjection(t *testing.T) {
	r := newHistoryRepo(t)
	p, codeCtx := historyProvider(t, r, "ctx", r.c)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		q    provider.HistoryQuery
	}{
		{"path that looks like an option", provider.HistoryQuery{Path: "-n"}},
		{"query that looks like an option", provider.HistoryQuery{Query: "--all"}},
		{"symbol that looks like an option", provider.HistoryQuery{Symbol: "--branches"}},
		{"path that escapes the repository", provider.HistoryQuery{Path: "../etc/passwd"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries, err := p.SearchHistory(ctx, codeCtx, tc.q)
			if err != nil {
				// Refusing is a fine outcome; what matters is that it never widens.
				return
			}
			// If it answered, it must not have escaped the pinned revision.
			for _, e := range entries {
				if e.Commit == r.d || e.Commit == r.head {
					t.Fatalf("%s widened the walk past the pinned revision: %+v", tc.name, e)
				}
			}
		})
	}
}

// --- Cancellation ------------------------------------------------------------

// TestHistoryHonoursContextCancellation proves the git subprocess is bound to the
// caller's context rather than outliving it.
func TestHistoryHonoursContextCancellation(t *testing.T) {
	r := newHistoryRepo(t)
	p, codeCtx := historyProvider(t, r, "ctx", r.c)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the walk starts

	if _, err := p.SearchHistory(ctx, codeCtx, provider.HistoryQuery{}); err == nil {
		t.Fatal("a cancelled context must not produce a history answer")
	} else if !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("the cancellation should surface as itself, got %v", err)
	}
}

// --- Repository errors -------------------------------------------------------

func TestHistoryUnconfiguredRepositoryFailsClosed(t *testing.T) {
	r := newHistoryRepo(t)
	p, _ := historyProvider(t, r, "ctx", r.c)

	_, err := p.SearchHistory(context.Background(), vacctx.CodeContext{
		ID: "ctx", Repository: "not-configured", Revision: r.c,
	}, provider.HistoryQuery{})
	if err == nil {
		t.Fatal("an unconfigured repository must fail closed")
	}
	if !strings.Contains(err.Error(), string(vacerr.RepositoryNotFound)) {
		t.Errorf("want REPOSITORY_NOT_FOUND, got %v", err)
	}
}

func TestHistoryUnknownRevisionFailsClosed(t *testing.T) {
	r := newHistoryRepo(t)
	p, _ := historyProvider(t, r, "ctx", "0000000000000000000000000000000000000000")

	_, err := p.SearchHistory(context.Background(), vacctx.CodeContext{
		ID: "ctx", Repository: "repo-a", Revision: "0000000000000000000000000000000000000000",
	}, provider.HistoryQuery{})
	if err == nil {
		t.Fatal("an unknown revision must fail closed")
	}
	if !strings.Contains(err.Error(), string(vacerr.RevisionNotFound)) {
		t.Errorf("want REVISION_NOT_FOUND, got %v", err)
	}
}

// TestHistoryEntryCountIsExact pins the number of entries a known fixture
// produces, so a change in what counts as an entry is caught rather than
// absorbed by a "non-empty" assertion.
//
// Pinned at commit C, the reachable commits are A, B and C, and the entries are
// their commit-path occurrences:
//
//	A  src/watchdog.c                     1
//	B  src/watchdog.c + src/network.c     2
//	C  src/network.c                      1
func TestHistoryEntryCountIsExact(t *testing.T) {
	r := newHistoryRepo(t)
	p, codeCtx := historyProvider(t, r, "ctx", r.c)

	entries, err := p.SearchHistory(context.Background(), codeCtx, provider.HistoryQuery{})
	if err != nil {
		t.Fatalf("SearchHistory: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("want 4 commit-path entries for A..C, got %d: %+v", len(entries), entries)
	}

	// Newest first, which is git's own order and the one Limit is applied to.
	wantOrder := []struct{ commit, path string }{
		{r.c, "src/network.c"},
		{r.b, "src/network.c"},
		{r.b, "src/watchdog.c"},
		{r.a, "src/watchdog.c"},
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Commit+"|"+e.Path] = true
	}
	for _, w := range wantOrder {
		if !got[w.commit+"|"+w.path] {
			t.Errorf("missing entry %s at %s", w.commit, w.path)
		}
	}
	// Newest-first: the first entry is commit C's.
	if entries[0].Commit != r.c {
		t.Errorf("entries should be newest first, got %s first", entries[0].Commit)
	}
	if entries[len(entries)-1].Commit != r.a {
		t.Errorf("entries should end at the oldest commit, got %s", entries[len(entries)-1].Commit)
	}
}
