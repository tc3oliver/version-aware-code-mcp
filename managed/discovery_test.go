package managed

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/store"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// How the management plane asks codebase-memory-mcp which graphs it holds.
//
// Two things are pinned here, and both are about not depending on what a
// particular CBM happens to do by default.
//
// The first is the request: JSON is asked for explicitly. CBM 0.11.0 made the
// CLI compact by default and moved JSON behind an opt-in, so a caller that
// parsed 0.10.1's default output gets a formatted table from 0.11.0 and
// reports a healthy graph engine as a broken one.
//
// The second is the answer: it may arrive in pages. A CBM that paginates sends
// the first page and says there are more, and "not on the first page" is not
// "not indexed" — reading it that way would fail a context whose graph is
// there, which is a wrong answer rather than a missing feature. The page size
// is never assumed: what this walks by is what the server sent and what the
// server said about there being more.

// TestVerifyGraphWalksEveryPage is the regression: the graph is on the second
// page, which is the case a single unpaged request gets wrong.
func TestVerifyGraphWalksEveryPage(t *testing.T) {
	// 50 is what CBM 0.11.0 happens to default to, and it is this stub's
	// choice, not the code's: the code under test sends no page size at all.
	requests := pagedCBM(t, 50, "wanted-graph", 120, withNextOffset)

	if err := verifyGraph(context.Background(), "app-v1", member("wanted-graph")); err != nil {
		t.Fatalf("verifyGraph = %v, want the graph on page 2 to be found", err)
	}

	// Walked rather than guessed: the second request has to start where the
	// first page ended, and every request has to have asked for JSON.
	got := requests(t)
	if len(got) < 2 {
		t.Fatalf("made %d requests, want it to have asked for a second page", len(got))
	}
	for i, req := range got {
		if !strings.Contains(req, `"format":"json"`) {
			t.Errorf("request %d did not ask for JSON explicitly: %s", i, req)
		}
	}
	if !strings.Contains(got[1], `"offset":50`) {
		t.Errorf("the second request was %s, want it to continue at the offset the first page ended on", got[1])
	}
}

// TestVerifyGraphAcceptsAnUnpagedAnswer is the other engine: a CBM that sends
// everything at once and says nothing about pages. Its silence must read as
// "that was all", not as "there may be more".
func TestVerifyGraphAcceptsAnUnpagedAnswer(t *testing.T) {
	unpagedCBM(t, "wanted-graph", "other-graph")

	if err := verifyGraph(context.Background(), "app-v1", member("wanted-graph")); err != nil {
		t.Fatalf("verifyGraph = %v, want the graph in a single unpaged answer to be found", err)
	}
}

// TestVerifyGraphStillReportsAGraphThatIsNotThere: paging must not turn a
// genuine absence into a success, whichever shape the answer arrives in.
func TestVerifyGraphStillReportsAGraphThatIsNotThere(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		setup func(*testing.T)
	}{
		{"paged", func(t *testing.T) { pagedCBM(t, 50, "", 120, withNextOffset) }},
		{"unpaged", func(t *testing.T) { unpagedCBM(t, "other-graph") }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			testCase.setup(t)

			err := verifyGraph(context.Background(), "app-v1", member("wanted-graph"))
			var vErr *vacerr.Error
			if !errors.As(err, &vErr) || vErr.Code != vacerr.GraphProviderUnavailable {
				t.Fatalf("verifyGraph = %v, want %s", err, vacerr.GraphProviderUnavailable)
			}
			if !strings.Contains(vErr.Message, "no graph of that name") {
				t.Errorf("message = %q, want it to say the graph is not there", vErr.Message)
			}
		})
	}
}

// TestVerifyGraphDoesNotSpinOnAPageThatNeverArrives: a server promising more
// and sending none would be asked the identical question for ever. The walk
// has to stop and say what it actually knows.
//
// A test that fails by hanging is a poor test, so this one is the exception
// that earns its keep: without the guard it does not fail, it never returns,
// and the package times out — which is the failure being prevented.
func TestVerifyGraphDoesNotSpinOnAPageThatNeverArrives(t *testing.T) {
	alwaysMoreCBM(t)

	err := verifyGraph(context.Background(), "app-v1", member("wanted-graph"))
	var vErr *vacerr.Error
	if !errors.As(err, &vErr) || vErr.Code != vacerr.GraphProviderUnavailable {
		t.Fatalf("verifyGraph = %v, want %s", err, vacerr.GraphProviderUnavailable)
	}
	if !strings.Contains(vErr.Message, "more pages") {
		t.Errorf("message = %q, want it to name what CBM did", vErr.Message)
	}
}

func member(graphRef string) store.ContextMember {
	return store.ContextMember{Repository: "demo", Branch: "main", GraphRef: graphRef}
}

// pagedCBM puts a codebase-memory-mcp on PATH that answers list_projects in
// pages of pageSize out of total projects, with wanted placed past the first
// page. It returns a reader of the requests it was sent.
// Whether the stub names the offset the next page starts at. A real CBM 0.11.0
// does; the walk must also work with a server that only says there is more.
const (
	withNextOffset    = true
	withoutNextOffset = false
)

func pagedCBM(t *testing.T, pageSize int, wanted string, total int, nextOffset bool) func(*testing.T) []string {
	t.Helper()

	dir := t.TempDir()
	requests := filepath.Join(dir, "requests")
	// The stub reads the arguments off standard input the way the real CLI
	// does, and answers from the offset it was given. sh and sed only: a stub
	// for the fast tier may not need an interpreter the tier does not already
	// require.
	script := fmt.Sprintf(`#!/bin/sh
args=$(cat)
printf '%%s\n' "$args" >>%[1]q
offset=$(printf '%%s' "$args" | sed -n 's/.*"offset":\([0-9]*\).*/\1/p')
[ -n "$offset" ] || offset=0
total=%[2]d
page=%[3]d
printf '{"projects":['
i=$offset
n=0
sep=
while [ "$i" -lt "$total" ] && [ "$n" -lt "$page" ]; do
	printf '%%s{"name":"filler-%%s"}' "$sep" "$i"
	sep=,
	i=$((i + 1))
	n=$((n + 1))
done
`, requests, total, pageSize)
	// The wanted graph is appended to whichever page covers it, so it is only
	// reachable by continuing past the first.
	if wanted != "" {
		script += fmt.Sprintf(`if [ "$offset" -ge "$page" ]; then printf '%%s{"name":"%s"}' "$sep"; fi
`, wanted)
	}
	script += `more=false
[ "$i" -lt "$total" ] && more=true
printf '],"total":%s,"offset":%s,"returned":%s,"has_more":%s' "$total" "$offset" "$n" "$more"
`
	if nextOffset {
		script += `printf ',"next_offset":%s' "$i"
`
	}
	script += `printf '}\n'
`
	writeCBMStub(t, dir, script)
	return func(t *testing.T) []string {
		t.Helper()
		raw, err := os.ReadFile(requests)
		if err != nil {
			t.Fatalf("the stub recorded no request: %v", err)
		}
		return strings.Split(strings.TrimSpace(string(raw)), "\n")
	}
}

// unpagedCBM is a codebase-memory-mcp of the kind that sends every project at
// once and carries no pagination metadata at all.
func unpagedCBM(t *testing.T, names ...string) {
	t.Helper()

	entries := make([]string, 0, len(names))
	for _, name := range names {
		entries = append(entries, fmt.Sprintf(`{"name":"%s"}`, name))
	}
	dir := t.TempDir()
	writeCBMStub(t, dir, fmt.Sprintf(`#!/bin/sh
cat >/dev/null
printf '{"projects":[%s]}\n'
`, strings.Join(entries, ",")))
}

// TestVerifyGraphWalksEveryPageWithoutANextOffset covers the other paging
// contract: has_more without a next_offset. The walk continues from where the
// page it was sent ended, rather than refusing to continue because the server
// did not spell the offset out.
func TestVerifyGraphWalksEveryPageWithoutANextOffset(t *testing.T) {
	requests := pagedCBM(t, 50, "wanted-graph", 120, withoutNextOffset)

	if err := verifyGraph(context.Background(), "app-v1", member("wanted-graph")); err != nil {
		t.Fatalf("verifyGraph = %v, want the graph found without the server naming the next offset", err)
	}
	if got := requests(t); len(got) < 2 || !strings.Contains(got[1], `"offset":50`) {
		t.Errorf("requests were %v, want the second to continue where the first page ended", got)
	}
}

// TestVerifyGraphRefusesAContinuationThatDoesNotContinue: a next_offset that
// points at the page just read is a loop, and the walk must say so rather than
// run for ever.
func TestVerifyGraphRefusesAContinuationThatDoesNotContinue(t *testing.T) {
	stuckCBM(t)

	err := verifyGraph(context.Background(), "app-v1", member("wanted-graph"))
	var vErr *vacerr.Error
	if !errors.As(err, &vErr) || vErr.Code != vacerr.GraphProviderUnavailable {
		t.Fatalf("verifyGraph = %v, want %s", err, vacerr.GraphProviderUnavailable)
	}
	if !strings.Contains(vErr.Message, "does not advance") {
		t.Errorf("message = %q, want it to name what CBM did", vErr.Message)
	}
}

// stuckCBM promises more pages and keeps pointing at the one already read.
func stuckCBM(t *testing.T) {
	t.Helper()

	writeCBMStub(t, t.TempDir(), `#!/bin/sh
cat >/dev/null
printf '{"projects":[{"name":"filler"}],"has_more":true,"next_offset":0}\n'
`)
}

// alwaysMoreCBM promises another page and never sends one.
func alwaysMoreCBM(t *testing.T) {
	t.Helper()

	writeCBMStub(t, t.TempDir(), `#!/bin/sh
cat >/dev/null
printf '{"projects":[],"has_more":true}\n'
`)
}

func writeCBMStub(t *testing.T, dir, script string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, CBMCommand), []byte(script), 0o700); err != nil {
		t.Fatalf("write the CBM stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
