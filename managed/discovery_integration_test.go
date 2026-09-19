//go:build integration

package managed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/store"
)

// pagedProjects is how many graphs this test puts in front of the one it is
// looking for.
//
// It is one more than codebase-memory-mcp 0.11.0's default page size, which is
// the only number that matters: the graph being looked for has to fall outside
// the first page the server chooses to send. It is written here, in the test
// that needs the server to paginate, and nowhere in the code under test — what
// verifyGraph walks by is what the server sends, not a page size of its own.
const pagedProjects = 51

// TestVerifyGraphFindsAGraphPastTheFirstPageOfARealCBM is the paging
// regression against the engine itself.
//
// managed/discovery_test.go proves the walk with a stub, deterministically and
// in milliseconds. This proves the thing the stub cannot: that a real
// codebase-memory-mcp, asked the way this plane asks it, really does answer in
// pages and really does describe them the way the walk reads. Against a CBM
// that does not paginate at all it is a slower way of passing, which is the
// honest state of affairs while that is the version this repository pins.
func TestVerifyGraphFindsAGraphPastTheFirstPageOfARealCBM(t *testing.T) {
	if _, err := exec.LookPath(CBMCommand); err != nil {
		t.Skipf("%s is not on PATH: %v", CBMCommand, err)
	}

	// The ambient store, deliberately, and not one of this test's own.
	// codebase-memory-mcp allows one cache directory per account while a
	// daemon is running — a second one is refused outright:
	//
	//	CBM could not start because the active account daemon uses a different
	//	cache directory ... retry with one consistent CBM_CACHE_DIR
	//
	// CI keeps a daemon warm on the fixture's store, so a test that insisted on
	// its own would fail there while passing on a developer machine with no
	// daemon. The graphs below are therefore built in the shared store and
	// removed again when the test ends.
	repo := tinyRepository(t)

	// Named so the one being looked for sorts last: CBM returns projects in
	// name order, so this is what puts it past the first page rather than
	// hoping it lands there.
	const wanted = "zzz-the-graph-being-looked-for"
	ctx := context.Background()

	names := make([]string, 0, pagedProjects+1)
	for i := range pagedProjects {
		names = append(names, fmt.Sprintf("vacmcp-paging-filler-%03d", i))
	}
	names = append(names, wanted)

	// Built and taken down concurrently, which this package already relies on
	// elsewhere: source.go records that CBM indexes different projects at the
	// same time without interfering, measured rather than assumed, which is why
	// there is no semaphore around the real indexing either. Serially this test
	// is five minutes of process startup; this way it is under one.
	t.Cleanup(func() { eachProject(t, names, func(name string) { deleteProject(ctx, name) }) })
	eachProject(t, names, func(name string) { indexProject(t, ctx, repo, name) })

	if err := verifyGraph(ctx, "app-v1", store.ContextMember{
		Repository: "demo", Branch: "main", GraphRef: wanted,
	}); err != nil {
		t.Fatalf("verifyGraph = %v, want the graph found past the first page", err)
	}

	// The negative, on the same store: a name that is on no page at all must
	// still be reported as missing rather than swallowed by the walk.
	if err := verifyGraph(ctx, "app-v1", store.ContextMember{
		Repository: "demo", Branch: "main", GraphRef: "no-such-graph",
	}); err == nil {
		t.Error("verifyGraph accepted a graph that is on no page")
	}
}

// tinyRepository is the smallest thing CBM will index. The content is
// irrelevant — what this test needs is many projects, not large ones — so it is
// kept to one file to pay the least indexing per project.
func tinyRepository(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tiny.go"), []byte("package tiny\n\nfunc F() {}\n"), 0o600); err != nil {
		t.Fatalf("writing the repository: %v", err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.email=fixture@example.invalid", "-c", "user.name=fixture", "add", "-A"},
		{"-c", "user.email=fixture@example.invalid", "-c", "user.name=fixture", "commit", "-qm", "tiny"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		var out bytes.Buffer
		cmd.Stderr = &out
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out.String())
		}
	}
	return dir
}

// eachProject runs work over every name, a few at a time. The bound is small
// and fixed: the point is to stop paying process startup one at a time, not to
// find out how many codebase-memory-mcp processes this machine will tolerate.
func eachProject(t *testing.T, names []string, work func(string)) {
	t.Helper()

	var group sync.WaitGroup
	tokens := make(chan struct{}, 8)
	for _, name := range names {
		group.Add(1)
		go func() {
			defer group.Done()
			tokens <- struct{}{}
			defer func() { <-tokens }()
			work(name)
		}()
	}
	group.Wait()
}

// deleteProject takes one graph back out of the shared store. A failure is not
// reported: this runs in cleanup, after the assertions, and a graph that could
// not be removed is worth neither failing a passing test nor hiding a real one.
func deleteProject(ctx context.Context, name string) {
	_, _, _ = cbmCLI(ctx, "delete_project", map[string]any{"project": name})
}

// indexProject builds one graph, through the same piped-JSON call the code
// under test uses, so the test cannot pass by asking CBM a way the code does
// not.
//
// It is called from several goroutines, so it reports through t.Error rather
// than t.Fatal: only the goroutine running the test may call Fatal.
func indexProject(t *testing.T, ctx context.Context, repo, name string) {
	t.Helper()

	out, stderr, err := cbmCLI(ctx, "index_repository", map[string]any{"repo_path": repo, "name": name})
	if err != nil {
		t.Errorf("indexing %s: %v: %s", name, err, lastLine(stderr))
		return
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(out, &body); err != nil {
		t.Errorf("indexing %s: %v", name, err)
		return
	}
	if body.Status != "indexed" {
		t.Errorf("indexing %s reported %q", name, body.Status)
	}
}
