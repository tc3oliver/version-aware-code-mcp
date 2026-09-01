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
	record := store.Context{ID: "app", Members: oneMember("demo"), State: ContextCreating}
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
	if after := contextRecord(t, s.Root(), "app"); !sameContext(after, before) {
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
		record := store.Context{ID: "app", Members: oneMember("demo"), State: state}
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
		if err := s.PutContext(store.Context{ID: strings.ToLower(state), Members: oneMember("demo"), State: state}); err != nil {
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

const (
	// The revision the records below pin, and a second one that is not it.
	testRevision  = "0123456789abcdef0123456789abcdef01234567"
	otherRevision = "89abcdef0123456789abcdef0123456789abcdef"
)

// created returns the record a create of these members would have written: the
// generated names filled in from the context they are members of, exactly as
// Create fills them.
func created(id string, members ...store.ContextMember) store.Context {
	c := store.Context{ID: id, Members: members, State: ContextReady}
	for i := range c.Members {
		c.Members[i].Branch = searchRef(c, c.Members[i])
		c.Members[i].GraphRef = graphRef(c, c.Members[i])
	}
	return c
}

// TestVerifyIdentityRefusesARecordThatNamesAnotherContext is the last of the
// six checks, asked of every member: the four fields the query plane resolves a
// member into are the ones this context's own name and that member's revision
// generate, so no member can point at another revision's search ref or graph.
//
// The two-member cases are why the check is per member and not per record: a
// context over two repositories has two such quadruples, and a single-valued
// check could not say which clone the ref it recomputed was supposed to be in.
// Each of them breaks one member and leaves the other sound, so what is caught
// is that member and not the context being broken in general.
func TestVerifyIdentityRefusesARecordThatNamesAnotherContext(t *testing.T) {
	one := created("app", store.ContextMember{Repository: "demo", Revision: testRevision})
	two := created("stack",
		store.ContextMember{Repository: "api", Revision: testRevision},
		store.ContextMember{Repository: "web", Revision: otherRevision},
	)
	for _, sound := range []store.Context{one, two} {
		if err := verifyIdentity(sound); err != nil {
			t.Fatalf("verifyIdentity of %s as created: %v", sound.ID, err)
		}
	}

	for name, broken := range map[string]struct {
		record store.Context
		damage func(c *store.Context)
	}{
		"a revision that is not a full SHA": {one, func(c *store.Context) { c.Members[0].Revision = testRevision[:shortSHA] }},
		"another revision's search ref": {one, func(c *store.Context) {
			c.Members[0].Branch = searchRef(*c, store.ContextMember{Repository: "demo", Revision: otherRevision})
		}},
		"another context's search ref": {one, func(c *store.Context) {
			c.Members[0].Branch = searchRef(store.Context{ID: "other", Members: c.Members}, c.Members[0])
		}},
		"another revision's graph": {one, func(c *store.Context) {
			c.Members[0].GraphRef = graphRef(*c, store.ContextMember{Repository: "demo", Revision: otherRevision})
		}},
		"another repository's graph": {one, func(c *store.Context) {
			c.Members[0].GraphRef = graphRef(*c, store.ContextMember{Repository: "other", Revision: testRevision})
		}},
		// The second member of a two-repository context is checked as closely as
		// the first: a record whose first member is impeccable is not a record
		// that verifies.
		"the second member's search ref": {two, func(c *store.Context) {
			c.Members[1].Branch = searchRef(*c, store.ContextMember{Repository: "web", Revision: testRevision})
		}},
		"the second member's graph": {two, func(c *store.Context) {
			c.Members[1].GraphRef = graphRef(*c, store.ContextMember{Repository: "api", Revision: otherRevision})
		}},
		// A member wearing the name the other member's repository generates is
		// the two of them pointing at one source, which is what the repository in
		// a multi-member name exists to make impossible.
		"one member carrying the other's search ref": {two, func(c *store.Context) {
			c.Members[0].Branch = c.Members[1].Branch
		}},
		// And the single-repository spelling inside a context that has two: the
		// name a v0.4.0 record would carry is the wrong one here, because two
		// members would generate it identically.
		"a member named as if it were the only one": {two, func(c *store.Context) {
			c.Members[0].Branch = "vacmcp/" + c.ID + "-" + c.Members[0].Revision[:shortSHA]
		}},
	} {
		c := store.Context{ID: broken.record.ID, State: broken.record.State, Members: slices.Clone(broken.record.Members)}
		broken.damage(&c)
		if got := codeFor(t, verifyIdentity(c)); got != vacerr.SourceMismatch {
			t.Errorf("%s: code = %q, want %q", name, got, vacerr.SourceMismatch)
		}
	}
}

// TestGeneratedNamesTellEveryMemberApart is what a context's members may never
// share: two of them at one revision would otherwise be one search ref, one
// graph and one checkout, and removing either would take the other's source
// with it.
//
// The pair pinned at the same revision is the case that matters. Nothing else
// distinguishes those two members — same context, same commit — so if the
// repository were not in the generated names, every name below would be equal.
func TestGeneratedNamesTellEveryMemberApart(t *testing.T) {
	s := openStore(t, t.TempDir())
	stack := created("stack",
		store.ContextMember{Repository: "api", Revision: testRevision},
		store.ContextMember{Repository: "web", Revision: testRevision},
	)
	// Another context of the same repositories at the same revision, so what
	// keeps the names apart is asked across contexts as well as within one.
	other := created("other",
		store.ContextMember{Repository: "api", Revision: testRevision},
		store.ContextMember{Repository: "web", Revision: testRevision},
	)

	seen := map[string]string{}
	for _, c := range []store.Context{stack, other} {
		for _, m := range c.Members {
			worktree, err := s.WorktreeDir(m.Repository, c.ID)
			if err != nil {
				t.Fatalf("WorktreeDir(%s, %s): %v", m.Repository, c.ID, err)
			}
			for kind, name := range map[string]string{"search ref": m.Branch, "graph": m.GraphRef, "worktree": worktree} {
				where := c.ID + " " + m.Repository + " " + kind
				if first, taken := seen[name]; taken {
					t.Errorf("%s and %s are both %q", first, where, name)
				}
				seen[name] = where
			}
		}
	}
}

// TestASingleMemberKeepsTheNameV040Generated is the other half of the naming
// rule, and the reason it is a rule and not a formula: a context over one
// repository has to keep generating exactly the name v0.4.0 wrote into its
// records, or verifyIdentity would fail closed on every context an older
// installation created — over artifacts that are perfectly good.
func TestASingleMemberKeepsTheNameV040Generated(t *testing.T) {
	// The two names v0.4.0 generated, spelled out rather than computed: a
	// formula compared against itself would agree however it changed.
	const (
		branch = "vacmcp/app-0123456789ab"
		graph  = "vacmcp-demo-app-0123456789ab"
	)
	v040 := store.Context{
		ID:      "app",
		State:   ContextReady,
		Members: []store.ContextMember{{Repository: "demo", Branch: branch, Revision: testRevision, GraphRef: graph}},
	}
	if err := verifyIdentity(v040); err != nil {
		t.Errorf("verifyIdentity of a record v0.4.0 created: %v", err)
	}
}
