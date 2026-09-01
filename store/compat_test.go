package store_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tc3oliver/version-aware-code-mcp/store"
)

// A data directory outlives the binary that built it, and two versions of
// vacmcp can be pointed at one. What these tests hold is the pair of
// obligations that go with the context record gaining members:
//
//   - a record v0.4.0 wrote is read and written here unchanged, so upgrading is
//     not a conversion;
//   - a record only this version can express is refused by v0.4.0 outright,
//     rather than read as a context with no repository and written back with
//     its members silently gone.
//
// v0.4.0 is reproduced below rather than built and run. Its record type is
// copied verbatim from `git show v0.4.0:store/store.go`, and the two code paths
// that decide both outcomes are three lines long: encoding/json decoding into
// that type, and the repository-name check its PutContext makes before writing.
// Nothing else in that binary is between a record file and either answer, so a
// checked-out build would be exercising these same lines through a git fetch
// and a compiler.

// v040Context is store.Context as v0.4.0 declared it, tags and all.
type v040Context struct {
	ID         string    `json:"id"`
	Repository string    `json:"repository"`
	Branch     string    `json:"branch,omitempty"`
	Revision   string    `json:"revision,omitempty"`
	GraphRef   string    `json:"graph_ref,omitempty"`
	State      string    `json:"state"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// v040NamePattern is the allowlist v0.4.0's validateName applies, copied
// verbatim. It has no way to spell the empty string, which is what makes a
// repository name that got lost in decoding unwritable rather than merely odd.
var v040NamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// v040Read is v0.4.0's readRecord for a context: the file, decoded into the
// type above, with nothing between.
func v040Read(t *testing.T, path string) (v040Context, error) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	var record v040Context
	return record, json.Unmarshal(data, &record)
}

// v040Write is v0.4.0's PutContext: the repository name is checked, and only
// then is the whole struct re-marshalled over the record file.
func v040Write(t *testing.T, path string, c v040Context) error {
	t.Helper()
	if len(c.Repository) > 100 || !v040NamePattern.MatchString(c.Repository) {
		// The vacerr.InvalidArgument v0.4.0's validateName returns. What matters
		// here is that the write is refused, not which error says so.
		return errors.New("store: repository name is not a usable path element")
	}
	c.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent: %v", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
	return nil
}

// contextFile is where the record of id is kept.
func contextFile(s *store.Store, id string) string {
	return filepath.Join(s.Root(), "contexts", id+".json")
}

// TestAV040RecordIsReadWithNoConversion is the upgrade path: a context file
// exactly as v0.4.0 wrote it, dropped into a data directory this version opens,
// is one member with the fields it always had. Nothing rewrites it, and nothing
// has to.
func TestAV040RecordIsReadWithNoConversion(t *testing.T) {
	s := open(t)
	const record = `{
  "id": "backend-v2",
  "repository": "backend",
  "branch": "vacmcp/backend-v2-94cb8213d7f2",
  "revision": "94cb8213d7f2b1c9a06e5d43f8b7c21e0d9a4f65",
  "graph_ref": "vacmcp-backend-backend-v2-94cb8213d7f2",
  "state": "READY",
  "updated_at": "2026-08-12T09:00:00Z"
}
`
	if err := os.WriteFile(contextFile(s, "backend-v2"), []byte(record), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := s.Context("backend-v2")
	if err != nil {
		t.Fatalf("Context(): %v", err)
	}
	want := store.Context{
		ID:    "backend-v2",
		State: "READY",
		Members: []store.ContextMember{{
			Repository: "backend",
			Branch:     "vacmcp/backend-v2-94cb8213d7f2",
			Revision:   "94cb8213d7f2b1c9a06e5d43f8b7c21e0d9a4f65",
			GraphRef:   "vacmcp-backend-backend-v2-94cb8213d7f2",
		}},
		UpdatedAt: time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC),
	}
	if !contextsEqual(got, want) {
		t.Errorf("Context() = %+v, want %+v", got, want)
	}

	// And through the listing the query plane and every command read, not only
	// through the one-record accessor.
	list, err := s.Contexts()
	if err != nil {
		t.Fatalf("Contexts(): %v", err)
	}
	if len(list) != 1 || !contextsEqual(list[0], want) {
		t.Errorf("Contexts() = %+v, want the one v0.4.0 record", list)
	}
}

// TestASingleMemberRecordIsStillTheV040Spelling is the other direction: what
// this version writes for a context over one repository is a record v0.4.0
// reads, field for field. That is what makes the upgrade reversible as well as
// conversion-free — and it is the same rule the wire follows, where a context
// of one repository is written inline and only several become a list.
func TestASingleMemberRecordIsStillTheV040Spelling(t *testing.T) {
	s := open(t)
	member := store.ContextMember{
		Repository: "backend",
		Branch:     "vacmcp/backend-v2-94cb8213d7f2",
		Revision:   "94cb8213d7f2b1c9a06e5d43f8b7c21e0d9a4f65",
		GraphRef:   "vacmcp-backend-backend-v2-94cb8213d7f2",
	}
	if err := s.PutContext(store.Context{ID: "backend-v2", Members: []store.ContextMember{member}, State: "READY"}); err != nil {
		t.Fatalf("PutContext(): %v", err)
	}

	old, err := v040Read(t, contextFile(s, "backend-v2"))
	if err != nil {
		t.Fatalf("v0.4.0 cannot read a record this version wrote for one repository: %v", err)
	}
	if old.ID != "backend-v2" || old.Repository != member.Repository || old.Branch != member.Branch ||
		old.Revision != member.Revision || old.GraphRef != member.GraphRef || old.State != "READY" {
		t.Errorf("v0.4.0 reads %+v, want the record it would have written itself", old)
	}

	// No members key at all, so nothing in the file is invisible to the older
	// reader: the whole record is what it understands.
	body, err := os.ReadFile(contextFile(s, "backend-v2"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(body), "members") {
		t.Errorf("a one-repository record carries a members list:\n%s", body)
	}

	// And byte for byte the file v0.4.0's own writer would have produced from
	// the same values: same fields, same order, same indentation, same trailing
	// newline. Reading the same fields back is not the same statement — a record
	// that decoded identically but was written differently would still be a
	// conversion, and an operator diffing a data directory across the upgrade
	// would see one.
	want, err := json.MarshalIndent(v040Context{
		ID:         old.ID,
		Repository: old.Repository,
		Branch:     old.Branch,
		Revision:   old.Revision,
		GraphRef:   old.GraphRef,
		State:      old.State,
		UpdatedAt:  old.UpdatedAt,
	}, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent: %v", err)
	}
	if string(body) != string(append(want, '\n')) {
		t.Errorf("the record on disk is not the one v0.4.0 would have written:\n got %s\nwant %s", body, want)
	}
}

// TestAV040DecoderRefusesAMultiMemberRecord is the refusal that has to be
// v0.4.0's own, since nothing this version does can reach into a binary that is
// already installed.
//
// Without it that binary reads a context with an empty repository name and
// carries on: `repo remove` decides on repository equality, finds no context
// depending on a clone two members are pinned in, and deletes it. The record
// makes the repository field a type it cannot decode instead, so it fails on
// the record rather than on the consequences.
func TestAV040DecoderRefusesAMultiMemberRecord(t *testing.T) {
	s := open(t)
	const revision = "94cb8213d7f2b1c9a06e5d43f8b7c21e0d9a4f65"
	stack := store.Context{
		ID:    "stack",
		State: "READY",
		Members: []store.ContextMember{
			{Repository: "api", Branch: "vacmcp/api-stack-94cb8213d7f2", Revision: revision, GraphRef: "vacmcp-api-stack-94cb8213d7f2"},
			{Repository: "web", Branch: "vacmcp/web-stack-94cb8213d7f2", Revision: revision, GraphRef: "vacmcp-web-stack-94cb8213d7f2"},
		},
	}
	if err := s.PutContext(stack); err != nil {
		t.Fatalf("PutContext(): %v", err)
	}

	if _, err := v040Read(t, contextFile(s, "stack")); err == nil {
		t.Error("v0.4.0 decoded a two-repository record, want it refused: it would be a context with no repository at all")
	}

	// This version reads the same file as the two members it is.
	got, err := s.Context("stack")
	if err != nil {
		t.Fatalf("Context(): %v", err)
	}
	stack.UpdatedAt = got.UpdatedAt
	if !contextsEqual(got, stack) {
		t.Errorf("Context() = %+v, want %+v", got, stack)
	}
}

// TestAV040WriterRefusesToTruncateAMultiMemberRecord is the write path, which
// is the one that loses data rather than merely misreading it.
//
// v0.4.0 re-marshals the whole struct on every write, so a `context retry` in
// it would replace a two-repository record with what its struct can hold —
// leaving a record with no repository that still parses and still looks like a
// context. The value below is exactly what its decoder produces from such a
// record when nothing stops it, and the check its PutContext makes is what
// turns the truncation into a refusal.
func TestAV040WriterRefusesToTruncateAMultiMemberRecord(t *testing.T) {
	s := open(t)
	path := contextFile(s, "stack")

	// The record as it would be without the type that stops the decoder: the
	// members are there and the repository field is not.
	const untyped = `{
  "id": "stack",
  "members": [
    {"repository": "api", "branch": "vacmcp/api-stack-94cb8213d7f2", "revision": "94cb8213d7f2b1c9a06e5d43f8b7c21e0d9a4f65", "graph_ref": "vacmcp-api-stack-94cb8213d7f2"},
    {"repository": "web", "branch": "vacmcp/web-stack-94cb8213d7f2", "revision": "94cb8213d7f2b1c9a06e5d43f8b7c21e0d9a4f65", "graph_ref": "vacmcp-web-stack-94cb8213d7f2"}
  ],
  "state": "READY",
  "updated_at": "2026-08-12T09:00:00Z"
}
`
	if err := os.WriteFile(path, []byte(untyped), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	truncated, err := v040Read(t, path)
	if err != nil {
		t.Fatalf("the record this test is about did not decode: %v", err)
	}
	if truncated.Repository != "" {
		t.Fatalf("v0.4.0 read repository %q, want the empty one a lost members list leaves", truncated.Repository)
	}

	if err := v040Write(t, path, truncated); err == nil {
		t.Error("v0.4.0 wrote a record whose members it had already lost, want the write refused")
	}

	// And the file is the one that was there: refusing before the write is what
	// makes the members survive a binary that cannot see them.
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(body) != untyped {
		t.Errorf("the record changed under a refused write:\n%s", body)
	}
}

// contextsEqual compares two records, which the member list keeps from being a
// == away.
func contextsEqual(a, b store.Context) bool {
	if a.ID != b.ID || a.State != b.State || !a.UpdatedAt.Equal(b.UpdatedAt) || len(a.Members) != len(b.Members) {
		return false
	}
	for i, m := range a.Members {
		if m != b.Members[i] {
			return false
		}
	}
	return true
}
