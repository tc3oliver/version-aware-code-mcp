// Package evidence defines the output contract shared by every vacmcp tool.
//
// doc-1 "Tool Contract": a successful tool output always carries the version
// context it was resolved in plus the evidence backing it:
//
//	{"context": {"id": "...", "repository": "...", "branch": "...", "revision": "..."},
//	 "evidence": []}
//
// Returning a bare answer such as {"answer": "..."} is not allowed, so this
// package makes it unrepresentable: [Output] has no exported fields, [New] is
// the only way to build one, and [Output.MarshalJSON] refuses to emit an
// output whose context is incomplete.
package evidence

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrIncompleteContext reports that an output is not scoped to a full version
// context and therefore cannot be emitted as a successful result.
var ErrIncompleteContext = errors.New("evidence: incomplete context")

// Context is the version scope every successful output is bound to. It mirrors
// the id/repository/branch/revision subset of the CodeContext that the config
// package resolves; only these four fields are part of the wire contract.
type Context struct {
	ID         string `json:"id"`
	Repository string `json:"repository"`
	Branch     string `json:"branch"`
	Revision   string `json:"revision"`
}

// Validate reports whether c is complete enough to scope a successful output.
func (c Context) Validate() error {
	for _, field := range []struct{ name, value string }{
		{"id", c.ID},
		{"repository", c.Repository},
		{"branch", c.Branch},
		{"revision", c.Revision},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%w: missing %s", ErrIncompleteContext, field.name)
		}
	}
	return nil
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
// caller outside this package can assemble one without [New]; the zero value
// is the only way around that, and marshalling it fails with
// [ErrIncompleteContext].
type Output struct {
	codeCtx Context
	items   []Evidence
	result  any
}

// New builds a successful output scoped to c and backed by ev. It fails when c
// is missing any part of the version context.
func New(c Context, ev ...Evidence) (Output, error) {
	if err := c.Validate(); err != nil {
		return Output{}, err
	}
	return Output{codeCtx: c, items: ev}, nil
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
	if err := o.codeCtx.Validate(); err != nil {
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

	codeCtx, err := json.Marshal(o.codeCtx)
	if err != nil {
		return nil, err
	}
	items := o.items
	if items == nil {
		items = []Evidence{}
	}
	evidence, err := json.Marshal(items)
	if err != nil {
		return nil, err
	}
	fields["context"] = codeCtx
	fields["evidence"] = evidence

	return json.Marshal(fields)
}
