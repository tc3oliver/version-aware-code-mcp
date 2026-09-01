// Package evidence defines the output contract shared by every vacmcp tool.
//
// doc-1 "Tool Contract": a successful tool output always carries the version
// context it was resolved in plus the evidence backing it:
//
//	{"context": {"id": "...", "repository": "...", "branch": "...", "revision": "..."},
//	 "evidence": []}
//
// Returning a bare answer such as {"answer": "..."} is not allowed, so this
// package makes it unrepresentable: [Output] has no exported fields, [New] and
// [NewWorkspace] are the only ways to build one, and [Output.MarshalJSON]
// refuses to emit an output whose context is incomplete.
//
// An output is scoped by a [vacctx.Workspace], and how many members that
// workspace has decides the shape on the wire. One member is the flat context
// above, exactly as v0.4.0 emitted it. Several members is:
//
//	{"context": {"id": "...", "members": [{"repository": "...", "branch": "...", "revision": "..."}]},
//	 "evidence": [{"location": {...}, "repository": "...", "revision": "..."}]}
//
// The member count is the same boundary that decides whether a request has to
// name a repository, and the shape follows it rather than being emitted always.
// A members array on every output would break every client reading
// context.revision, and members beside the old fields would put one fact in two
// places on the wire, where they can disagree — which is the failure this
// package exists to prevent, not one to introduce in its own schema.
//
// Only the fields above reach a client: [vacctx.CodeContext.GraphRef] is the CBM
// project backing a member and stays internal in both shapes.
package evidence

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
)

// ErrIncompleteContext reports that an output is not scoped to a full version
// context and therefore cannot be emitted as a successful result.
var ErrIncompleteContext = errors.New("evidence: incomplete context")

// ErrUnattributedEvidence reports that an output's citations do not line up with
// the members of its workspace: some member has no list of its own, or there is
// a list belonging to no member. Either way at least one citation would go out
// unable to say which repository it came from, which is the one thing a
// multi-repository answer must never do.
var ErrUnattributedEvidence = errors.New("evidence: evidence is not attributed to the workspace's members")

// wireContext is the shape a client sees when the workspace has one member. It
// exists so that adding a field to [vacctx.CodeContext] cannot leak it into tool
// output.
type wireContext struct {
	ID         string `json:"id"`
	Repository string `json:"repository"`
	Branch     string `json:"branch"`
	Revision   string `json:"revision"`
}

// wireWorkspace is the shape a client sees when the workspace has several
// members, and wireMember one repository inside it. They exist for the same
// reason [wireContext] does, and a member deliberately has no id of its own: the
// workspace's id names the whole set and a member is not separately addressable,
// so a per-member id could only be that same string repeated.
type wireWorkspace struct {
	ID      string       `json:"id"`
	Members []wireMember `json:"members"`
}

type wireMember struct {
	Repository string `json:"repository"`
	Branch     string `json:"branch"`
	Revision   string `json:"revision"`
}

// wireEvidence is a citation in the several-member shape: the citation itself
// plus the member it was found in.
//
// The two extra fields exist only here. In the one-member shape a citation is
// marshalled as a plain [Evidence] and gains nothing, because there is exactly
// one repository and one revision the answer can be about and they are already
// in the context block — repeating them per item would be the same fact in two
// places, and it is also the field a v0.4.0 client would not expect.
type wireEvidence struct {
	Location   Location `json:"location"`
	Snippet    string   `json:"snippet,omitempty"`
	Repository string   `json:"repository"`
	Revision   string   `json:"revision"`
}

// Location points at a line range of one file, read at the context's revision.
type Location struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

// Evidence is a single citation backing a tool result.
type Evidence struct {
	Location Location `json:"location"`
	Snippet  string   `json:"snippet,omitempty"`
}

// At builds an Evidence for lines [start, end] of path.
func At(path string, start, end int, snippet string) Evidence {
	return Evidence{
		Location: Location{Path: path, StartLine: start, EndLine: end},
		Snippet:  snippet,
	}
}

// Output is a successful tool result. Its fields are unexported so that no
// caller outside this package can assemble one without [New] or
// [NewWorkspace]; the zero value is the only way around that, and marshalling it
// fails with [ErrIncompleteContext].
//
// cited is one list of citations per member of workspace, in the same order:
// cited[i] is what was found in workspace.Members[i]. A citation's provenance is
// therefore its position, which is what makes it impossible to build an output
// whose citations are attributed to a repository the workspace does not have —
// there is no repository name for a caller to write down, only the members it
// passed in — and impossible to leave one unattributed, because there is nowhere
// to put a citation except inside some member's list.
type Output struct {
	workspace vacctx.Workspace
	cited     [][]Evidence
	result    any
}

// New builds a successful output scoped to the single-repository context c and
// backed by ev, which is the shape every tool answering in one version uses. It
// fails when c is missing any part of the version context.
//
// It is [NewWorkspace] over a workspace of one member, and not a second code
// path: one member and several are the same value at different lengths, as
// [vacctx.Workspace] says, so a one-repository answer is not a special case with
// its own contract to keep correct beside the general one.
func New(c vacctx.CodeContext, ev ...Evidence) (Output, error) {
	return NewWorkspace(vacctx.Workspace{ID: c.ID, Members: []vacctx.CodeContext{c}}, ev)
}

// NewWorkspace builds a successful output scoped to the whole of w, where
// cited[i] is the evidence found in w.Members[i]. It fails when any member is
// missing part of its version context, and when there is not exactly one list of
// citations per member — a member that found nothing passes a nil or empty list
// and says so, rather than being left out and leaving the count to be guessed
// at.
//
// A member's citations keep the order they are given in, and the members keep
// the order w declares them in; on the wire the lists are concatenated in that
// order. Evidence is therefore grouped by member, which is what a tool that
// queries each member in turn produces anyway. A tool that wants one ranked list
// across every repository cannot express it here, and that is the deliberate
// ceiling of attributing a citation by its position rather than by carrying a
// member on every item.
func NewWorkspace(w vacctx.Workspace, cited ...[]Evidence) (Output, error) {
	out := Output{workspace: w, cited: cited}
	if err := out.validate(); err != nil {
		return Output{}, err
	}
	return out, nil
}

// validate reports whether o is scoped well enough to be emitted as a successful
// result. It is checked at construction and again at marshalling, so the one way
// around [New] and [NewWorkspace] — the zero [Output] — cannot serialize into
// something a client could read as an answer.
//
// Only the fields that reach the wire are required: an output does not carry a
// member's graph reference, so it does not need one.
func (o Output) validate() error {
	if strings.TrimSpace(o.workspace.ID) == "" {
		return fmt.Errorf("%w: missing id", ErrIncompleteContext)
	}
	if len(o.workspace.Members) == 0 {
		return fmt.Errorf("%w: no members", ErrIncompleteContext)
	}
	for i, member := range o.workspace.Members {
		for _, field := range []struct{ name, value string }{
			{"repository", member.Repository},
			{"branch", member.Branch},
			{"revision", member.Revision},
		} {
			if strings.TrimSpace(field.value) == "" {
				return fmt.Errorf("%w: member %d is missing %s", ErrIncompleteContext, i+1, field.name)
			}
		}
	}
	if len(o.cited) != len(o.workspace.Members) {
		return fmt.Errorf("%w: %d evidence lists for %d members",
			ErrUnattributedEvidence, len(o.cited), len(o.workspace.Members))
	}
	return nil
}

// WithResult attaches the tool-specific payload (search matches, call graph,
// source content, ...), whose fields are merged into the top-level JSON object
// next to context and evidence. data must marshal to a JSON object.
func (o Output) WithResult(data any) Output {
	o.result = data
	return o
}

// MarshalJSON implements [json.Marshaler]. context and evidence always win over
// payload fields of the same name, and evidence is emitted as [] when empty.
func (o Output) MarshalJSON() ([]byte, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}

	fields := map[string]json.RawMessage{}
	if o.result != nil {
		raw, err := json.Marshal(o.result)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, fmt.Errorf("evidence: result must marshal to a JSON object: %w", err)
		}
	}

	codeCtx, items := o.wire()
	marshalled, err := json.Marshal(codeCtx)
	if err != nil {
		return nil, err
	}
	evidence, err := json.Marshal(items)
	if err != nil {
		return nil, err
	}
	fields["context"] = marshalled
	fields["evidence"] = evidence

	return json.Marshal(fields)
}

// wire returns the context block and the evidence list in the shape this
// output's member count calls for. It runs after [Output.validate], so there is
// a list of citations for every member and indexing one by the other is sound.
//
// Both lists are built empty rather than nil in either shape: [] and null are
// different answers, and "this version has no such code" is an answer.
func (o Output) wire() (any, any) {
	if len(o.workspace.Members) == 1 {
		member := o.workspace.Members[0]
		items := o.cited[0]
		if items == nil {
			items = []Evidence{}
		}
		return wireContext{
			ID:         o.workspace.ID,
			Repository: member.Repository,
			Branch:     member.Branch,
			Revision:   member.Revision,
		}, items
	}

	members := make([]wireMember, 0, len(o.workspace.Members))
	items := []wireEvidence{}
	for i, member := range o.workspace.Members {
		members = append(members, wireMember{
			Repository: member.Repository,
			Branch:     member.Branch,
			Revision:   member.Revision,
		})
		for _, item := range o.cited[i] {
			// The repository and revision are the member's own and are read
			// here rather than taken from the item, because an item has none to
			// give: where a citation came from is decided by the code that put
			// it in this member's list, never inferred at marshalling time,
			// where the only thing left to go on would be a guess.
			items = append(items, wireEvidence{
				Location:   item.Location,
				Snippet:    item.Snippet,
				Repository: member.Repository,
				Revision:   member.Revision,
			})
		}
	}
	return wireWorkspace{ID: o.workspace.ID, Members: members}, items
}
