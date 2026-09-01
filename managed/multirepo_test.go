package managed

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/store"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// What a context spanning several repositories does to the management plane,
// asked of real git clones and no other engine. Indexing a context needs Zoekt
// and CBM, so a create that reaches READY is in cmd/vacmcp's
// context_integration_test.go; what is here is everything that is decided
// before or beside them — which refs are resolved, which locks are held, which
// records a repository still depends on, and what a fetch may not touch.

// sourceWith creates a git repository with one commit and returns its path,
// standing in for the remote a user would add. The content is the caller's, so
// two sources are two different commits rather than two that happen to hash the
// same.
func sourceWith(t *testing.T, name, content string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	mustGit(t, "init", "-q", "-b", "main", dir)
	if err := os.WriteFile(filepath.Join(dir, "one.txt"), []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mustGit(t, "-C", dir, "add", "one.txt")
	mustGit(t, "-C", dir,
		"-c", "user.name=vacmcp test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false",
		"commit", "--no-verify", "-q", "-m", name)
	return dir
}

// twoRepositories returns a data directory with "api" and "web" cloned into it,
// and the commit each of their mains points at. The two commits are different,
// so a test can tell which member pinned which.
func twoRepositories(t *testing.T) (data, apiSHA, webSHA string) {
	t.Helper()
	data = t.TempDir()
	m, err := NewRepositoryManager(data)
	if err != nil {
		t.Fatalf("NewRepositoryManager: %v", err)
	}
	shas := map[string]string{}
	for _, name := range []string{"api", "web"} {
		source := sourceWith(t, name, name+"\n")
		if _, err := m.Add(context.Background(), name, source); err != nil {
			t.Fatalf("repo add %s: %v", name, err)
		}
		shas[name] = strings.TrimSpace(gitOut(t, "-C", source, "rev-parse", "HEAD"))
	}
	if shas["api"] == shas["web"] {
		t.Fatal("the two sources are the same commit, so nothing below could tell the members apart")
	}
	return data, shas["api"], shas["web"]
}

// gitOut runs one git command and returns its output.
func gitOut(t *testing.T, args ...string) string {
	t.Helper()
	out, err := gitOutput(t.Context(), args...)
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return out
}

// stack is the two-member record a create of api and web would leave, with the
// names it generates, so a test that is not about building artifacts does not
// have to build them.
func stack(t *testing.T, id, apiSHA, webSHA, state string) store.Context {
	t.Helper()
	c := store.Context{
		ID:    id,
		State: state,
		Members: []store.ContextMember{
			{Repository: "api", Revision: apiSHA},
			{Repository: "web", Revision: webSHA},
		},
	}
	for i := range c.Members {
		c.Members[i].Branch = searchRef(c, c.Members[i])
		c.Members[i].GraphRef = graphRef(c, c.Members[i])
	}
	return c
}

// TestCreateRecordsNothingWhenOneMembersRefDoesNotResolve is the create that
// has to leave no trace: the first repository's ref is perfectly good and the
// second one's is not, so a context that recorded what it had resolved would be
// a half-pinned record an operator has to remove before they can run the
// command again with the ref they meant.
func TestCreateRecordsNothingWhenOneMembersRefDoesNotResolve(t *testing.T) {
	data, _, _ := twoRepositories(t)
	m, err := NewContextManager(data)
	if err != nil {
		t.Fatalf("NewContextManager: %v", err)
	}

	for _, pins := range [][]Pin{
		{{Repository: "api", Ref: "main"}, {Repository: "web", Ref: "no-such-branch"}},
		// The other order, so what is being shown is that no member's
		// resolution is kept rather than that the last one is not.
		{{Repository: "api", Ref: "no-such-branch"}, {Repository: "web", Ref: "main"}},
		// A repository that is not managed at all fails the same way.
		{{Repository: "api", Ref: "main"}, {Repository: "absent", Ref: "main"}},
	} {
		if _, err := m.Create(context.Background(), "stack", pins); err == nil {
			t.Fatalf("Create(%+v) returned nil, want an error", pins)
		}
		contexts, err := openStore(t, data).Contexts()
		if err != nil {
			t.Fatalf("Contexts(): %v", err)
		}
		if len(contexts) != 0 {
			t.Fatalf("contexts = %+v, want a refused create to record nothing", contexts)
		}
	}

	// That the same command is runnable again with the ref the user meant —
	// which is what recording nothing was for — is in
	// TestCreatePinsEveryMemberBeforeAnyArtifactIsBuilt, where the indexer is a
	// stub and the run therefore stops in a defined place.
}

// TestCreateRefusesOneRepositoryTwice keeps a context from naming two revisions
// of one repository, which the configuration refuses too: a workspace answering
// from both would report one repository's code twice with no way to say which
// version each half came from.
func TestCreateRefusesOneRepositoryTwice(t *testing.T) {
	data, _, _ := twoRepositories(t)
	m, err := NewContextManager(data)
	if err != nil {
		t.Fatalf("NewContextManager: %v", err)
	}

	_, err = m.Create(context.Background(), "stack", []Pin{
		{Repository: "api", Ref: "main"},
		{Repository: "api", Ref: "main"},
	})
	if got := codeFor(t, err); got != vacerr.InvalidArgument {
		t.Errorf("Create with one repository twice = %q, want %q", got, vacerr.InvalidArgument)
	}
	if _, err := m.Create(context.Background(), "stack", nil); codeFor(t, err) != vacerr.InvalidArgument {
		t.Errorf("Create with no repository at all = %v, want %q", err, vacerr.InvalidArgument)
	}
	if contexts, err := openStore(t, data).Contexts(); err != nil || len(contexts) != 0 {
		t.Errorf("contexts = %+v (err %v), want a refused create to record nothing", contexts, err)
	}
}

// TestRepoRemoveRefusesARepositoryAContextIsAMemberOf is the dependency check
// after a context can name several repositories: what blocks a removal is
// membership, so either of a two-repository context's clones is one that
// context still pins a revision in.
//
// Forcing it would leave a context that can never be served again: half its
// members would have no source, and there is no such thing as half a workspace.
func TestRepoRemoveRefusesARepositoryAContextIsAMemberOf(t *testing.T) {
	data, apiSHA, webSHA := twoRepositories(t)
	s := openStore(t, data)
	if err := s.PutContext(stack(t, "stack", apiSHA, webSHA, ContextReady)); err != nil {
		t.Fatalf("PutContext: %v", err)
	}

	m, err := NewRepositoryManager(data)
	if err != nil {
		t.Fatalf("NewRepositoryManager: %v", err)
	}
	for _, name := range []string{"api", "web"} {
		err := m.Remove(name)
		if got := codeFor(t, err); got != vacerr.InvalidArgument {
			t.Errorf("repo remove %s = %q, want %q", name, got, vacerr.InvalidArgument)
		}
		// The blocking context is named, or the operator is told to remove
		// something without being told what.
		if !strings.Contains(err.Error(), "stack") {
			t.Errorf("repo remove %s said %q, want it to name the context that depends on it", name, err)
		}
		if _, err := os.Stat(filepath.Join(data, "repos", name)); err != nil {
			t.Errorf("the clone of %s was deleted by a refused removal: %v", name, err)
		}
	}

	// And each of them is named once, however many members reach it, and only
	// through membership: status reports the dependency the removal refuses on.
	status, err := m.Status("web")
	if err != nil {
		t.Fatalf("Status(web): %v", err)
	}
	if len(status.Contexts) != 1 || status.Contexts[0] != "stack" {
		t.Errorf("repo status web reports contexts %v, want [stack]", status.Contexts)
	}

	// Removing the context releases both, which is what makes the refusal a
	// step in an order rather than a dead end.
	if err := s.DeleteContext("stack"); err != nil {
		t.Fatalf("DeleteContext: %v", err)
	}
	for _, name := range []string{"api", "web"} {
		if err := m.Remove(name); err != nil {
			t.Errorf("repo remove %s after the context was gone: %v", name, err)
		}
	}
}

// TestSyncMovesNoMembersPinnedRevision is the invariant this project exists
// for, asked of a context over two repositories: a fetch brings new commits
// within reach of the next create and leaves every member of every existing
// context on exactly the revision it was pinned to.
//
// It is a property of what Sync does not do — it reads and writes no context
// record at all — so what this checks is the record, byte for byte, across a
// sync of both repositories that really moved.
func TestSyncMovesNoMembersPinnedRevision(t *testing.T) {
	data, apiSHA, webSHA := twoRepositories(t)
	s := openStore(t, data)
	if err := s.PutContext(stack(t, "stack", apiSHA, webSHA, ContextReady)); err != nil {
		t.Fatalf("PutContext: %v", err)
	}
	before, err := s.Context("stack")
	if err != nil {
		t.Fatalf("Context(stack): %v", err)
	}

	// Both sources move on, which is the only thing that could take a member
	// with it.
	moved := map[string]string{}
	for _, name := range []string{"api", "web"} {
		source := gitOut(t, "-C", filepath.Join(data, "repos", name), "remote", "get-url", "origin")
		if err := os.WriteFile(filepath.Join(source, "two.txt"), []byte("two\n"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		mustGit(t, "-C", source, "add", "two.txt")
		mustGit(t, "-C", source,
			"-c", "user.name=vacmcp test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false",
			"commit", "--no-verify", "-q", "-m", "two")
		moved[name] = gitOut(t, "-C", source, "rev-parse", "HEAD")
	}

	repositories, err := NewRepositoryManager(data)
	if err != nil {
		t.Fatalf("NewRepositoryManager: %v", err)
	}
	if _, err := repositories.Sync(context.Background(), nil); err != nil {
		t.Fatalf("repo sync --all: %v", err)
	}

	// The fetch happened: both new commits are in their clones.
	for name, sha := range moved {
		if got := gitOut(t, "-C", filepath.Join(data, "repos", name), "rev-parse", "--verify", sha+"^{commit}"); got != sha {
			t.Errorf("the clone of %s resolves %s to %q after a sync, want the new commit fetched", name, sha, got)
		}
	}

	after, err := s.Context("stack")
	if err != nil {
		t.Fatalf("Context(stack) after the sync: %v", err)
	}
	if !sameContext(after, before) {
		t.Errorf("a sync changed the record:\n before %+v\n  after %+v", before, after)
	}
	for i, member := range after.Members {
		if member.Revision != before.Members[i].Revision || member.Revision == moved[member.Repository] {
			t.Errorf("member %s pins %q, want the original %q and not the fetched %q",
				member.Repository, member.Revision, before.Members[i].Revision, moved[member.Repository])
		}
	}
}
