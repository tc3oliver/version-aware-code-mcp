package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/evidence"
)

// compareCodeInput is what a caller asks for. The three original fields are
// still required: none of them carries an omitempty, so schema inference marks
// them required and the SDK rejects a call missing one before the handler runs,
// exactly as get_code's does.
//
// The two contexts are the whole scope of the comparison. There is no branch or
// revision field, for the reason no request in this server has one: each side is
// compared at the revision its own context declares, so a caller cannot compare
// a version the configuration never granted it. The path is asked of both sides
// identically — there is deliberately no way to read one file on the from side
// and another on the to side, because a difference produced by reading two
// different files is not a difference between two versions.
//
// Repository is the one thing a request may say about where to look, and it
// widens nothing: it selects a repository both contexts already name, and one
// either of them does not name is refused rather than compared. It is asked of
// both sides identically for the reason the path is — a comparison of two
// repositories is not a comparison of two versions — and it is optional to the
// schema because a context naming one repository never needs it. A context
// naming several has no comparison without it and says so with INVALID_ARGUMENT.
type compareCodeInput struct {
	FromContext string `json:"from_context" jsonschema:"the id of the version context to compare from, as returned by list_contexts; the file is read at the revision that context declares"`
	ToContext   string `json:"to_context" jsonschema:"the id of the version context to compare to, as returned by list_contexts; it must name the same repository as from_context"`
	Repository  string `json:"repository,omitempty" jsonschema:"which repository to compare, as one of the members list_contexts reports for both contexts; required when either context names several, and unnecessary when they name one"`
	Path        string `json:"path" jsonschema:"the file to compare, relative to the repository root; the same path is read on both sides"`
}

// side is one version's half of a comparison on the wire, shared by both
// comparison tools so the two answer in the same shape.
//
// A present side is an [evidence.Output] carrying nothing but its own context
// and its own citations, which is exactly the {"context": ..., "evidence": ...}
// pair a single-context tool puts at the top level of its result — here nested
// under from and under to instead, so a client reads a shape it already knows
// and reads two of them. Reusing the type rather than mirroring its fields is
// what stops a comparison's context block drifting from every other tool's, and
// is why GraphRef cannot leak from here either: what a context looks like on the
// wire is decided in the evidence package, in one place. The two sides are never
// merged, and no combined context or evidence is offered beside them: a citation
// only means anything at the revision it was read at, so one flattened list
// could not say which version an entry came from.
//
// An absent side — the version that does not have the compared file or symbol —
// is nil, and marshals to JSON null. It is null rather than a {"present": false}
// object because an absent side has nothing to report: [evidence.NewWorkspace]
// refuses to build an output without a complete context, so an object here could
// only be a hollow one, and a "present" field beside it would be a second thing
// that can disagree with the first. null is one value a client cannot mistake
// for a side that has something to say, and both comparison tools emit it.
func side(s engine.ComparisonSide) (*evidence.Output, error) {
	if !s.Present() {
		return nil, nil
	}
	// A present side is one version, which is a workspace of one member: a
	// comparison is answered in one repository per side, so the engine either had
	// one member or was told which one by the request's repository. So this
	// marshals to the flat context block a single-context tool emits, whichever of
	// the two it was.
	out, err := evidence.NewWorkspace(s.Context(), s.Evidence()...)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// diffLine is one line of a hunk: which revisions it belongs to, and its text.
type diffLine struct {
	Kind    string `json:"kind"`
	Content string `json:"content"`
}

// hunk is one changed region in unified diff terms: it begins at line old_start
// of the from revision and spans old_lines of it, and at new_start of the to
// revision spanning new_lines. The field names are spelled out here rather than
// left to the provider's own hunk type, which carries no json tags: the wire
// shape of a result is this package's to decide, and snake_case is what every
// other tool emits.
type hunk struct {
	OldStart int        `json:"old_start"`
	OldLines int        `json:"old_lines"`
	NewStart int        `json:"new_start"`
	NewLines int        `json:"new_lines"`
	Lines    []diffLine `json:"lines"`
}

// compareCodeResult is a code comparison as a client receives it: the two sides
// kept apart, and what happened to the file between them.
//
// It is the whole wire object rather than a payload merged into an
// [evidence.Output], because there is no single context to merge into. The
// context and evidence of each version live on that version's side; change,
// path, binary and hunks are facts about the pair and sit beside them.
type compareCodeResult struct {
	From   *evidence.Output `json:"from"`
	To     *evidence.Output `json:"to"`
	Change string           `json:"change"`
	Path   string           `json:"path"`
	Binary bool             `json:"binary"`
	Hunks  []hunk           `json:"hunks"`
}

// AddCompareCode registers the compare_code tool on srv, answered by eng.
//
// The tool decodes the call, hands it to [engine.Engine.CompareCode] and encodes
// what comes back. Everything the answer is made of is decided before it
// returns: resolving both context IDs, refusing two repositories that have no
// shared history, finding the source backend cannot diff at all, and
// classifying the file as added, removed, modified or unchanged. Nothing here
// compares anything, and a second implementation of any of that would be a
// second thing to keep correct.
//
// Every failure is reported as the error model's wire shape,
// {"error": {"code": ..., "message": ..., "details": {}}}, on a result marked as
// an error and with no content beside it — the same envelope every other tool
// fails with, code intact and never downgraded to a warning.
func AddCompareCode(srv *mcp.Server, eng *engine.Engine) {
	// Out is any and no output schema is declared, as for get_code: the two-sided
	// shape is assembled from [evidence.Output], whose own wire form that package
	// defines, and a hand-written mirror of it here would not only drift, it
	// would be enforced against valid results.
	mcp.AddTool(srv, &mcp.Tool{
		Name:  "compare_code",
		Title: "Compare a file between two version contexts",
		Description: "Compare one file between two version contexts. " +
			"Pass two context ids from list_contexts: the file is read at the revision each one declares, and no branch or revision can be given here. " +
			"Both contexts must name the same repository — two repositories have no shared history to compare, which is INVALID_ARGUMENT. " +
			"A context naming several repositories requires repository, given as one of the members list_contexts reports for it and applied to both sides; leaving it out is INVALID_ARGUMENT naming the side that needs it. " +
			"change is ADDED (only the to context has the file), REMOVED (only the from context has it), MODIFIED or UNCHANGED, with the changed regions as structured hunks; a binary file is marked and has no hunks. " +
			"The from and to sides are reported separately, each with the version context it was read at and the evidence backing it, and the side a version does not have the file in is null. " +
			"A source backend that reads one version at a time and cannot compare two is SOURCE_DIFF_UNAVAILABLE.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in compareCodeInput) (*mcp.CallToolResult, any, error) {
		result, err := eng.CompareCode(ctx, engine.CompareCodeRequest{
			FromContext: in.FromContext,
			ToContext:   in.ToContext,
			Repository:  in.Repository,
			Path:        in.Path,
		})
		if err != nil {
			return failed(err)
		}

		from, err := side(result.From())
		if err != nil {
			// A side is present but its context is missing a field the contract
			// requires, so there is no way to answer without dropping part of it.
			// Failing is the contract working, not a bug to route around.
			return nil, nil, err
		}
		to, err := side(result.To())
		if err != nil {
			return nil, nil, err
		}

		// Built empty rather than nil: a client reading [] learns there is nothing
		// to show line by line, where null leaves it guessing whether anything was
		// compared.
		hunks := make([]hunk, 0, len(result.Hunks()))
		for _, h := range result.Hunks() {
			lines := make([]diffLine, 0, len(h.Lines))
			for _, line := range h.Lines {
				lines = append(lines, diffLine{Kind: string(line.Kind), Content: line.Content})
			}
			hunks = append(hunks, hunk{
				OldStart: h.OldStart,
				OldLines: h.OldLines,
				NewStart: h.NewStart,
				NewLines: h.NewLines,
				Lines:    lines,
			})
		}

		return nil, compareCodeResult{
			From:   from,
			To:     to,
			Change: string(result.Change()),
			Path:   result.Path(),
			Binary: result.Binary(),
			Hunks:  hunks,
		}, nil
	})
}
