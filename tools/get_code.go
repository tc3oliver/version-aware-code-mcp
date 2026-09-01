package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/evidence"
)

// getCodeInput is what a caller asks for. Every field is required: none of them
// carries an omitempty, so schema inference marks all four required and the SDK
// rejects a call missing one before the handler runs.
//
// The repository and revision to read at are deliberately absent. They come
// from the context, so a caller cannot widen or redirect its own scope.
type getCodeInput struct {
	Context   string `json:"context" jsonschema:"the id of the version context to read at, as returned by list_contexts"`
	Path      string `json:"path" jsonschema:"the file to read, relative to the repository root"`
	StartLine int    `json:"start_line" jsonschema:"the first line to read, 1-based and inclusive"`
	EndLine   int    `json:"end_line" jsonschema:"the last line to read, inclusive; must not be before start_line"`
}

// getCodeResult is the tool-specific half of the payload. The other half — the
// repository, branch and revision the lines were read at — is the context block
// [evidence.Output] puts next to it, which is why this struct does not repeat
// them.
type getCodeResult struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Content   string `json:"content"`
}

// AddGetCode registers the get_code tool on srv, answered by eng.
//
// It answers inside one version context, so doc-1's Tool Contract applies in
// full: the result is an [evidence.Output], carrying the context the lines were
// read in and citing where they came from. A bare answer is not representable
// here, and the context's GraphRef never reaches the wire.
//
// The tool holds no version logic of its own, on purpose: resolving the ID,
// reading at the revision and reporting the revision the bytes came from are
// [engine.Engine.GetCode]'s, and the fail-closed SOURCE_MISMATCH check is the
// resolver's, which the git adapter reaches on the one path where the object
// database cannot serve the declared revision's bytes. A second implementation
// of any of them here would be a second thing to keep correct. What this tool
// must guarantee is the other half of failing closed: every error is returned
// as an error result with no content beside it, with its code intact and never
// downgraded to a warning.
func AddGetCode(srv *mcp.Server, eng *engine.Engine) {
	// Out is any and no output schema is declared: the shape on the wire is
	// [evidence.Output]'s to define, and a hand-written mirror of it here would
	// not only drift, it would be enforced — the SDK validates output against a
	// declared schema, so a stale copy would start rejecting valid results.
	mcp.AddTool(srv, &mcp.Tool{
		Name:  "get_code",
		Title: "Read source at a context's revision",
		Description: "Read lines [start_line, end_line] of a file exactly as they are at the revision the given context declares. " +
			"Call list_contexts first to learn which context ids exist. " +
			"The result carries the repository, branch and revision the content was read at, plus the path and line range, so it can be cited. " +
			"It fails instead of answering from another version: an unknown context id is CONTEXT_NOT_FOUND, a path or line range the revision does not have is INVALID_ARGUMENT, " +
			"and source that is not the declared revision is SOURCE_MISMATCH.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in getCodeInput) (*mcp.CallToolResult, any, error) {
		result, err := eng.GetCode(ctx, engine.GetCodeRequest{
			Context:   in.Context,
			Path:      in.Path,
			StartLine: in.StartLine,
			EndLine:   in.EndLine,
		})
		if err != nil {
			return failed(err)
		}

		// The context carries the revision the bytes actually came from, not the
		// spelling the configuration used, which is what lets a caller check the
		// claim instead of trusting it.
		// The workspace a read is answered in has exactly one member — get_code
		// refuses a context naming several — so this marshals to the flat context
		// block it always has.
		out, err := evidence.NewWorkspace(result.Context(), result.Evidence()...)
		if err != nil {
			// The context is missing a field the contract requires, so there is no
			// way to answer without dropping part of it. Failing is the contract
			// working, not a bug to route around.
			return nil, nil, err
		}
		src := result.Source()
		return nil, out.WithResult(getCodeResult{
			Path:      src.Path,
			StartLine: src.StartLine,
			EndLine:   src.EndLine,
			Content:   src.Content,
		}), nil
	})
}
