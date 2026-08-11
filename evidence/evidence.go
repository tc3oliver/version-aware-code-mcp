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
//
// An output is scoped by a [vacctx.CodeContext], of which only the four fields
// above reach the client: GraphRef is the CBM project backing the context and
// stays internal.
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

// wireContext is the only shape of a context a client ever sees. It exists so
// that adding a field to [vacctx.CodeContext] cannot leak it into tool output.
type wireContext struct {
	ID         string `json:"id"`
	Repository string `json:"repository"`
	Branch     string `json:"branch"`
	Revision   string `json:"revision"`
}

// validate reports whether c is complete enough to scope a successful output.
// Only the four fields on the wire are required: an output does not carry the
// graph reference, so it does not need one.
func validate(c vacctx.CodeContext) error {
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
	codeCtx vacctx.CodeContext
	items   []Evidence
	result  any
}

// New builds a successful output scoped to c and backed by ev. It fails when c
// is missing any part of the version context.
func New(c vacctx.CodeContext, ev ...Evidence) (Output, error) {
	if err := validate(c); err != nil {
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
	if err := validate(o.codeCtx); err != nil {
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

	codeCtx, err := json.Marshal(wireContext{
		ID:         o.codeCtx.ID,
		Repository: o.codeCtx.Repository,
		Branch:     o.codeCtx.Branch,
		Revision:   o.codeCtx.Revision,
	})
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
