// Package vacerr defines the unified error model returned by every vacmcp tool.
//
// An Error always serialises to the wire shape given by the v0.1.0
// specification:
//
//	{"error": {"code": "...", "message": "...", "details": {}}}
//
// The model has no severity or warning concept: every value produced here is a
// Go error and marshals to the shape above. A tool either answers or fails.
package vacerr

import (
	"encoding/json"
	"fmt"
)

// Code is a vacmcp error code. The string values are part of the public tool
// API and must not change.
//
// The ten the v0.1.0 specification fixed are in the block below; the ones added
// after it are declared beneath, each saying which release added it and why.
type Code string

// The ten v0.1.0 error codes. Each one documents where it is produced in
// v0.1.0; a code with no v0.1.0 producer is marked as reserved. A code added
// after v0.1.0 is declared below this block rather than inside it, so what the
// specification fixed stays legible as one set.
const (
	// ContextNotFound is produced by the context resolver whenever a tool is
	// called with a context ID that is not present in the configuration.
	// Used by search_code, trace_calls and get_code. The server never guesses
	// a context.
	ContextNotFound Code = "CONTEXT_NOT_FOUND"

	// ContextAmbiguous is RESERVED: it has no producer in v0.1.0. Contexts are
	// keyed by a unique ID in the configuration, so a lookup can never be
	// ambiguous. It is reserved for later versions that select a context by
	// repository/branch instead of by ID.
	ContextAmbiguous Code = "CONTEXT_AMBIGUOUS"

	// RepositoryNotFound is produced by the git source adapter (and by the
	// validate/doctor commands) when the repository a context points at cannot
	// be resolved on the local machine.
	RepositoryNotFound Code = "REPOSITORY_NOT_FOUND"

	// RevisionNotFound is produced by the git source adapter (and by the
	// validate/doctor commands) when the revision a context declares cannot be
	// resolved inside the repository.
	RevisionNotFound Code = "REVISION_NOT_FOUND"

	// SymbolNotFound is produced by the CBM graph adapter in trace_calls when
	// search_graph returns no match for the requested symbol.
	SymbolNotFound Code = "SYMBOL_NOT_FOUND"

	// SymbolAmbiguous is produced by the CBM graph adapter in trace_calls when
	// search_graph returns more than one match. The adapter reports the
	// candidates instead of picking one.
	SymbolAmbiguous Code = "SYMBOL_AMBIGUOUS"

	// SearchProviderUnavailable is produced by the Zoekt search adapter in
	// search_code when Zoekt cannot be reached or does not serve the query.
	SearchProviderUnavailable Code = "SEARCH_PROVIDER_UNAVAILABLE"

	// GraphProviderUnavailable is produced by the CBM graph adapter in
	// trace_calls when CBM cannot be reached or the graph_ref is not indexed.
	// Only the graph feature degrades; search_code keeps working.
	GraphProviderUnavailable Code = "GRAPH_PROVIDER_UNAVAILABLE"

	// SourceMismatch is produced by the context resolver and the git source
	// adapter (get_code) when the revision a context declares does not match
	// the source actually read. It is fail closed: the tool must return this
	// error and stop, never continue with content from another revision and
	// never downgrade it to a warning. Build it with NewSourceMismatch so the
	// declared and actual revisions travel with the error.
	SourceMismatch Code = "SOURCE_MISMATCH"

	// InvalidArgument is produced by every tool when validating its arguments,
	// for example a missing required field, an illegal get_code line range or
	// a trace_calls direction/depth outside the supported range.
	InvalidArgument Code = "INVALID_ARGUMENT"
)

// SourceDiffUnavailable is produced by the engine in compare_code when the
// configured source provider does not implement the optional
// [github.com/tc3oliver/version-aware-code-mcp/provider.SourceDiffer]
// capability: it reads one version at a time and has no way to compare two, so
// the type assertion for that capability fails and the caller is told this
// server cannot compare code rather than handed an apology shaped like an
// answer.
//
// It is a fact about this server's capability, not about the code asked for:
// nothing is claimed about the file, the revisions or the repository. A server
// built with no source provider at all reports [RepositoryNotFound] instead —
// there is then no repository to read, whatever the contexts declare — and a
// provider that can diff but fails reports its own error unchanged.
//
// It is added after v0.1.0, which is why it is not in the block above:
// comparing two versions is a query v0.1.0 did not have.
const SourceDiffUnavailable Code = "SOURCE_DIFF_UNAVAILABLE"

// Error is a tool error carrying a Code, a human readable message and
// optional structured details.
type Error struct {
	Code    Code
	Message string
	Details map[string]any
}

// New returns an Error with the given code, message and details. details may
// be nil, in which case it serialises as an empty object.
func New(code Code, message string, details map[string]any) *Error {
	return &Error{Code: code, Message: message, Details: details}
}

// NewSourceMismatch returns a fail-closed SourceMismatch error recording the
// revision the context declared and the revision the source actually has, so
// the mismatch cannot be reported without its evidence.
func NewSourceMismatch(declaredRevision, actualRevision string, details map[string]any) *Error {
	d := map[string]any{
		"declared_revision": declaredRevision,
		"actual_revision":   actualRevision,
	}
	for k, v := range details {
		d[k] = v
	}
	return New(SourceMismatch, fmt.Sprintf("source revision mismatch: context declares %s but source is %s", declaredRevision, actualRevision), d)
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

type wireBody struct {
	Code    Code           `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

// MarshalJSON emits the full wire shape, including the "error" envelope, so no
// caller has to remember to wrap it.
func (e *Error) MarshalJSON() ([]byte, error) {
	details := e.Details
	if details == nil {
		details = map[string]any{}
	}
	return json.Marshal(struct {
		Error wireBody `json:"error"`
	}{wireBody{Code: e.Code, Message: e.Message, Details: details}})
}

// SourceHistoryUnavailable: search_history asked a source provider that does not
// implement [github.com/tc3oliver/version-aware-code-mcp/provider.HistoryProvider],
// so this server can read a revision's bytes but cannot walk its commit history.
//
// It is the history counterpart of [SourceDiffUnavailable], and separate from it
// for the same reason: a caller asking for history is told that this server
// cannot answer that kind of question here, rather than being handed an empty
// history that reads as "this version has no commits". A provider that can walk
// history but fails reports its own error unchanged.
//
// It is added after v0.5.0: searching a version's history is a query v0.5.0 did
// not have.
const SourceHistoryUnavailable Code = "SOURCE_HISTORY_UNAVAILABLE"

// OperationTimeout: an operation budget vacmcp set for itself expired.
//
// It means one thing and nothing else: *this server* decided how long a single
// provider call may take, and the call took longer. It is a wedged-process
// guard — a git that never returns, a codebase-memory-mcp that stopped
// answering, a Zoekt that accepted the connection and went quiet — not a
// performance target, and not a statement that the query was too expensive.
//
// The two things it deliberately does NOT cover both belong to the caller, and
// both propagate as the context error they already are rather than being
// wrapped:
//
//   - the caller cancelled, which is [context.Canceled]. The client stopped
//     waiting and already knows why; giving it a code would put an event the
//     client itself caused into the vocabulary it branches on.
//   - the caller's own deadline expired, which is [context.DeadlineExceeded].
//     That budget is the caller's and reporting it as this server's would be
//     a false attribution — the server would still have been answering.
//
// Telling the third case from the second cannot be done by inspecting the error:
// a context whose deadline passed reports [context.DeadlineExceeded] whether the
// deadline was this server's or the caller's. The producer is the one that knows,
// which is why the budget is attached with a cause and read back through
// [context.Cause] rather than guessed at afterwards.
//
// Details name the operation that ran out of time, the provider it was talking
// to, and the budget in milliseconds — enough for a caller to decide whether to
// retry with a narrower query, and nothing about the repository, the query text
// or the environment.
//
// It is added after v0.6.0: before it, a provider that timed out was reported as
// a provider that was unavailable, which is a different fact about a different
// thing.
const OperationTimeout Code = "OPERATION_TIMEOUT"
