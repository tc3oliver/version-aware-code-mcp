package managed

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/store"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// The context lifecycle's own rules — which state may follow which, what a
// record carries while it is being built, and which records the query plane may
// read — are decided by this package and by nothing outside it, so they are
// checked against a store and no engine at all. The commands themselves are in
// cmd/vacmcp's context_integration_test.go, against real git, Zoekt and CBM:
// what a create resolves and what a removal takes apart are answers only a real
// object database, index and graph can give.

// contextRecord returns the stored record of id.
func contextRecord(t *testing.T, dataDir, id string) store.Context {
	t.Helper()
	c, err := openStore(t, dataDir).Context(id)
	if err != nil {
		t.Fatalf("Context(%s): %v", id, err)
	}
	return c
}

// TestContextStateMachineOnlyGoesForwards is AC #1's transition rule, one case
// per condition: the successor of every state is allowed, FAILED is allowed out
// of every state that is not terminal, and nothing else is — not a skipped
// stage, not a step back, and nothing at all out of READY or FAILED.
func TestContextStateMachineOnlyGoesForwards(t *testing.T) {
	order := []string{
		ContextCreating,
		ContextResolving,
		ContextPreparingSource,
		ContextIndexingSearch,
		ContextIndexingGraph,
		ContextVerifying,
		ContextReady,
	}

	for i, from := range order {
		for j, to := range order {
			want := j == i+1
			if got := advances(from, to); got != want {
				t.Errorf("advances(%s, %s) = %v, want %v", from, to, got, want)
			}
		}
		// Every stage can fail, and READY, the last of them, cannot: a context
		// the query plane is serving must not be walked back into a state where
		// its artifacts are being rebuilt under it.
		if got, want := advances(from, ContextFailed), from != ContextReady; got != want {
			t.Errorf("advances(%s, %s) = %v, want %v", from, ContextFailed, got, want)
		}
		// And nothing leads back out of FAILED: re-running a failed create is
		// creating the lifecycle again, not a record reverting.
		if advances(ContextFailed, from) {
			t.Errorf("advances(%s, %s) = true, want false", ContextFailed, from)
		}
	}
	if advances(ContextFailed, ContextFailed) {
		t.Error("advances(FAILED, FAILED) = true, want false")
	}
	if advances("", ContextResolving) || advances(ContextCreating, "SOMETHING_ELSE") {
		t.Error("advances admitted a state that is not in the machine")
	}

	// REMOVING is reachable from wherever a context got to — one being served,
	// one that failed, one still being built, and one whose removal was already
	// interrupted — because every one of them is a record somebody asked to be
	// taken away. Nothing leads back out of it: what follows REMOVING is the
	// record being deleted, not another state.
	for _, from := range append(slices.Clone(order), ContextFailed, ContextRemoving) {
		if !advances(from, ContextRemoving) {
			t.Errorf("advances(%s, %s) = false, want true", from, ContextRemoving)
		}
		if from != ContextRemoving && advances(ContextRemoving, from) {
			t.Errorf("advances(%s, %s) = true, want false", ContextRemoving, from)
		}
	}
}

// TestAdvanceWritesEveryStateItPassesThrough is the other half of AC #1: the
// record carries the stage the context is in while it is in it, which is what a
// context killed half way through a create leaves behind, and a refused
// transition changes nothing.
func TestAdvanceWritesEveryStateItPassesThrough(t *testing.T) {
	s := openStore(t, t.TempDir())
	record := store.Context{ID: "app", Repository: "demo", State: ContextCreating}
	if err := s.PutContext(record); err != nil {
		t.Fatalf("PutContext: %v", err)
	}

	for _, state := range []string{
		ContextResolving,
		ContextPreparingSource,
		ContextIndexingSearch,
		ContextIndexingGraph,
		ContextVerifying,
		ContextReady,
	} {
		if err := advance(s, &record, state); err != nil {
			t.Fatalf("advance to %s: %v", state, err)
		}
		if record.State != state {
			t.Errorf("in-memory state = %q, want %q", record.State, state)
		}
		if stored := contextRecord(t, s.Root(), "app"); stored.State != state {
			t.Errorf("stored state = %q, want %q", stored.State, state)
		}
	}

	// READY is terminal, and a refused transition leaves both the record and
	// the file exactly as they were.
	before := contextRecord(t, s.Root(), "app")
	if err := advance(s, &record, ContextVerifying); err == nil {
		t.Error("advance from READY back to VERIFYING returned nil, want an error")
	}
	if record.State != ContextReady {
		t.Errorf("in-memory state = %q after a refused transition, want %q", record.State, ContextReady)
	}
	if after := contextRecord(t, s.Root(), "app"); after != before {
		t.Errorf("a refused transition rewrote the record:\n before %+v\n  after %+v", before, after)
	}
}

// TestFailRecordsWhereAContextStopped covers the failure edge of every stage:
// the cause is what the caller gets, and the record is FAILED so the failure can
// be seen, listed and later retried rather than looking like a create that never
// ran.
func TestFailRecordsWhereAContextStopped(t *testing.T) {
	s := openStore(t, t.TempDir())
	for _, state := range []string{
		ContextCreating,
		ContextResolving,
		ContextPreparingSource,
		ContextIndexingSearch,
		ContextIndexingGraph,
		ContextVerifying,
	} {
		record := store.Context{ID: "app", Repository: "demo", State: state}
		if err := s.PutContext(record); err != nil {
			t.Fatalf("PutContext: %v", err)
		}

		cause := vacerr.New(vacerr.GraphProviderUnavailable, "the graph engine fell over", nil)
		if err := fail(s, &record, cause); !errors.Is(err, cause) {
			t.Errorf("fail in %s returned %v, want the cause", state, err)
		}
		if stored := contextRecord(t, s.Root(), "app"); stored.State != ContextFailed {
			t.Errorf("state after a failure in %s = %q, want %s", state, stored.State, ContextFailed)
		}
	}
}

// TestReadyContextIsTheOnlyWayIn is AC #3 and AC #4: the contexts the query
// plane can read are exactly the READY ones, and every other id — one that
// failed, one still being built, one that was never managed at all — comes back
// as the same CONTEXT_NOT_FOUND, down to the message and the details.
func TestReadyContextIsTheOnlyWayIn(t *testing.T) {
	s := openStore(t, t.TempDir())
	for _, state := range []string{
		ContextCreating,
		ContextResolving,
		ContextPreparingSource,
		ContextIndexingSearch,
		ContextIndexingGraph,
		ContextVerifying,
		ContextReady,
		ContextFailed,
		ContextRemoving,
	} {
		if err := s.PutContext(store.Context{ID: strings.ToLower(state), Repository: "demo", State: state}); err != nil {
			t.Fatalf("PutContext(%s): %v", state, err)
		}
	}

	// The registry the query plane reads is what this primitive admits, and
	// READY is the whole of it.
	contexts, err := s.Contexts()
	if err != nil {
		t.Fatalf("Contexts(): %v", err)
	}
	var readable []string
	for _, c := range contexts {
		if _, err := readyContext(s, c.ID); err == nil {
			readable = append(readable, c.ID)
		}
	}
	if want := []string{strings.ToLower(ContextReady)}; !slices.Equal(readable, want) {
		t.Errorf("the query plane can read %v, want only %v", readable, want)
	}

	// AC #4. A context that is not READY and a context that does not exist are
	// the same answer: not the same code with a different message, the same
	// error. The comparison is the same id twice — once hidden by its state,
	// once really gone — so nothing but the state can account for a difference.
	for _, id := range []string{strings.ToLower(ContextFailed), strings.ToLower(ContextIndexingGraph), strings.ToLower(ContextRemoving)} {
		hidden := errorFor(t, func() error { _, err := readyContext(s, id); return err })
		if hidden.Code != vacerr.ContextNotFound {
			t.Errorf("readyContext(%s) code = %q, want %q", id, hidden.Code, vacerr.ContextNotFound)
		}
		if err := s.DeleteContext(id); err != nil {
			t.Fatalf("DeleteContext(%s): %v", id, err)
		}
		absent := errorFor(t, func() error { _, err := readyContext(s, id); return err })
		if !reflect.DeepEqual(hidden, absent) {
			t.Errorf("readyContext(%s) in %s = %v, want the error an unmanaged context produces, %v", id, strings.ToUpper(id), hidden, absent)
		}
	}
}

// TestVerifyIdentityRefusesARecordThatNamesAnotherContext is the last of the
// six checks: the four fields the query plane resolves a context into are the
// ones this context's own name and revision generate, so a record cannot point
// at another revision's search ref or graph.
func TestVerifyIdentityRefusesARecordThatNamesAnotherContext(t *testing.T) {
	const revision = "0123456789abcdef0123456789abcdef01234567"
	sound := store.Context{
		ID:         "app",
		Repository: "demo",
		Branch:     searchRef("app", revision),
		Revision:   revision,
		GraphRef:   graphRef("demo", "app", revision),
		State:      ContextReady,
	}
	if err := verifyIdentity(sound); err != nil {
		t.Fatalf("verifyIdentity of a record as created: %v", err)
	}

	other := "89abcdef0123456789abcdef0123456789abcdef"
	for name, broken := range map[string]func(c *store.Context){
		"a revision that is not a full SHA": func(c *store.Context) { c.Revision = revision[:shortSHA] },
		"another revision's search ref":     func(c *store.Context) { c.Branch = searchRef("app", other) },
		"another context's search ref":      func(c *store.Context) { c.Branch = searchRef("other", revision) },
		"another revision's graph":          func(c *store.Context) { c.GraphRef = graphRef("demo", "app", other) },
		"another repository's graph":        func(c *store.Context) { c.GraphRef = graphRef("other", "app", revision) },
	} {
		c := sound
		broken(&c)
		if got := codeFor(t, verifyIdentity(c)); got != vacerr.SourceMismatch {
			t.Errorf("%s: code = %q, want %q", name, got, vacerr.SourceMismatch)
		}
	}
}
