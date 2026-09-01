//go:build integration

package tools

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	cbmadapter "github.com/tc3oliver/version-aware-code-mcp/adapters/cbm"
	gitadapter "github.com/tc3oliver/version-aware-code-mcp/adapters/git"
	zoektadapter "github.com/tc3oliver/version-aware-code-mcp/adapters/zoekt"
	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/evidence"
	"github.com/tc3oliver/version-aware-code-mcp/internal/demorepo"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/resolver"
	"github.com/tc3oliver/version-aware-code-mcp/server"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// The engine is what vacmcp answers and MCP is one way of asking. This file is
// the evidence for the second half of that sentence: over one fixture, an
// engine call and the matching tool call return the same context, the same
// evidence, the same payload, the same error code and the same version
// isolation.
//
// Both sides are real and independent — two engines over one *config.Config,
// each with its own Zoekt, CBM and git providers — so agreement here is two
// separately built stacks reaching the same answer rather than one value being
// read twice. Nothing is faked: what a fake would prove is that this package
// agrees with our own idea of an engine.

// parityWire is a tool result as a client receives it, wide enough for all
// three tools. Fields a given tool does not emit stay at their zero value on
// both sides of the comparison, so one type serves for all of them rather than
// three near-identical ones. Decoding into it is also what pins the field names:
// a wrong json tag leaves the field it should have filled empty.
//
// Context and Evidence are each wide enough for both member-count shapes
// rather than the flat one alone: [listedContext] already carries the members
// array decision-11 §5 adds, and [parityEvidence] is evidence.wireEvidence's
// two extra fields decoded, so a search across demo-multi's members compares
// on the repository and revision each citation and match carries and not only
// on the fields every single-repository answer already had.
type parityWire struct {
	// doc-1's Tool Contract half, on every successful result.
	Context  listedContext    `json:"context"`
	Evidence []parityEvidence `json:"evidence"`

	// search_code's payload.
	Matches []searchMatch `json:"matches"`

	// trace_calls's payload. Direction and depth are deliberately absent: the
	// tool echoes back what was asked, which is a property of the request rather
	// than an answer the engine produced, so there is nothing to compare them to.
	Symbol string `json:"symbol"`
	Calls  []call `json:"calls"`

	// get_code's payload.
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Content   string `json:"content"`
}

// parityEvidence is one citation as a client receives it, in whichever of
// evidence.Output's two shapes the answer's member count produced: Repository
// and Revision stay at their zero value when the context named one repository
// — that fact already lives in the context block once — and carry the member
// each citation was found in when it named several. The old plain
// []evidence.Evidence this field used to be had nowhere to put those two: a
// wrong or absent json tag on them would leave both sides of the comparison
// having silently dropped the same fact, agreeing with each other about
// nothing.
type parityEvidence struct {
	Location   evidence.Location `json:"location"`
	Snippet    string            `json:"snippet,omitempty"`
	Repository string            `json:"repository,omitempty"`
	Revision   string            `json:"revision,omitempty"`
}

// evidenceOnTheWire projects the engine's citations — one list per member of
// workspace, in [engine.answer.Evidence]'s own pairing — onto the flat,
// attributed list [evidence.Output] emits: unattributed when workspace names
// one member, exactly as [evidence.Evidence] always was, and carrying that
// member's own repository and revision on every item when it names several.
//
// It is written out here rather than read off evidence.Output's own
// projection (unexported, and the encoder under test) for the reason this
// file's own doc comment gives for the rest of it: an encoder cannot also be
// the source of the expectation it is compared to.
func evidenceOnTheWire(t *testing.T, workspace vacctx.Workspace, cited [][]evidence.Evidence) []parityEvidence {
	t.Helper()
	if len(cited) != len(workspace.Members) {
		t.Fatalf("the engine cited %d repositories for %d members", len(cited), len(workspace.Members))
	}
	attributed := len(workspace.Members) > 1
	items := make([]parityEvidence, 0)
	for i, member := range workspace.Members {
		for _, item := range cited[i] {
			found := parityEvidence{Location: item.Location, Snippet: item.Snippet}
			if attributed {
				found.Repository, found.Revision = member.Repository, member.Revision
			}
			items = append(items, found)
		}
	}
	return items
}

// The two versions of the demo repository, and the handler that exists in each.
// Process calls LegacyHandler on release/v1 and NewHandler on release/v2, so
// every "other" below is a symbol that really exists — in the other version.
// An answer that crossed contexts shows up as the wrong handler rather than as
// a difference in a number.
var parityVersions = []struct{ id, own, other string }{
	{v1, "LegacyHandler", "NewHandler"},
	{v2, "NewHandler", "LegacyHandler"},
}

// TestEngineAndMCPAnswerIdentically is decision-8's release gate: the protocol
// adapter adds no answer of its own and drops none of the engine's.
//
// It matters because every version guarantee this project makes is tested at
// the engine, where it needs no server and no wire. That is only worth
// something if the tool an agent actually calls returns what the engine
// decided — including the negative answers, which is where a leak would hide:
// an empty result and a wrong-version result look alike until you ask both
// sides the same question.
func TestEngineAndMCPAnswerIdentically(t *testing.T) {
	cfg := parityFixture(t)
	eng := parityEngine(t, cfg)
	session := paritySession(t, cfg)

	// list_contexts first, for the reason an agent calls it first: it is the
	// only tool whose answer is the set of versions rather than something inside
	// one, so if the two sides disagree here they are not even talking about the
	// same configuration.
	t.Run("list_contexts", func(t *testing.T) {
		listed, err := eng.ListContexts(t.Context())
		if err != nil {
			t.Fatalf("ListContexts: %v", err)
		}
		direct := list(listed)

		raw, isError := parityCall(t, session, "list_contexts", map[string]any{})
		if isError {
			t.Fatalf("list_contexts failed: %s", raw)
		}
		var wire listContextsOutput
		if err := json.Unmarshal([]byte(raw), &wire); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}

		if !reflect.DeepEqual(direct, wire.Contexts) {
			t.Errorf("the engine lists %+v and list_contexts lists %+v", direct, wire.Contexts)
		}
		if len(direct) == 0 {
			t.Fatal("the fixture configured no contexts, so nothing below compares anything")
		}
		t.Logf("both sides list %+v (wire: %s)", direct, raw)
	})

	for _, version := range parityVersions {
		t.Run(version.id, func(t *testing.T) {
			t.Run("search_code", func(t *testing.T) { paritySearchCode(t, cfg, eng, session, version.id, version.own, version.other) })
			t.Run("trace_calls", func(t *testing.T) { parityTraceCalls(t, cfg, eng, session, version.id, version.own, version.other) })
			t.Run("get_code", func(t *testing.T) { parityGetCode(t, cfg, eng, session, version.id, version.own, version.other) })
		})
	}
}

// paritySearchCode compares the two paths through search_code: an answer, the
// empty answer that is version isolation, and a refusal.
func paritySearchCode(t *testing.T, cfg *config.Config, eng *engine.Engine, session *mcp.ClientSession, contextID, own, other string) {
	t.Helper()

	search := func(query string) parityWire {
		t.Helper()
		result, err := eng.SearchCode(t.Context(), engine.SearchCodeRequest{Context: contextID, Query: query})
		if err != nil {
			t.Fatalf("engine.SearchCode(%s, %s): %v", contextID, query, err)
		}
		assertScoped(t, result.Context(), contextID)

		matches := make([]searchMatch, 0, len(result.Matches()))
		for _, match := range result.Matches() {
			matches = append(matches, searchMatch{Path: match.Path, Line: match.Line, Snippet: match.Snippet})
		}
		direct := parityWire{
			Context:  onTheWire(t, result.Context()),
			Evidence: evidenceOnTheWire(t, result.Context(), result.Evidence()),
			Matches:  matches,
		}
		wire, raw := parityResult(t, cfg, session, "search_code", map[string]any{"context": contextID, "query": query})
		return assertParity(t, "search_code("+contextID+", "+query+")", direct, wire, raw)
	}

	// The symbol this version has. Both sides find it, and find the same ones.
	if found := search(own); len(found.Matches) == 0 {
		t.Errorf("search_code(%s, %s) found nothing on either side, so agreeing on it proves nothing", contextID, own)
	}

	// decision-8's case: the symbol the *other* version has. The answer is empty
	// rather than an error — that branch simply does not contain it — and it has
	// to be equally empty through both paths. A tool that widened its scope, or
	// an engine that did, would answer this one.
	if absent := search(other); len(absent.Matches) != 0 {
		t.Errorf("search_code(%s, %s) = %+v, want no matches: that symbol belongs to the other version", contextID, other, absent.Matches)
	}

	// A context that does not exist, refused identically. An empty result here
	// would read as "this version has no such code", which is a claim about code
	// the server has no version to make.
	_, err := eng.SearchCode(t.Context(), engine.SearchCodeRequest{Context: "demo-v3", Query: own})
	assertCodeParity(t, "search_code(demo-v3)", directCode(t, err),
		parityErrorCode(t, session, "search_code", map[string]any{"context": "demo-v3", "query": own}),
		vacerr.ContextNotFound)
}

// parityTraceCalls compares the two paths through trace_calls: a walk, and the
// refusal that is version isolation — the other version's symbol is not in this
// version's graph, so tracing it is SYMBOL_NOT_FOUND on both sides rather than
// a reach across to the graph that has it.
func parityTraceCalls(t *testing.T, cfg *config.Config, eng *engine.Engine, session *mcp.ClientSession, contextID, own, other string) {
	t.Helper()

	args := map[string]any{"context": contextID, "symbol": "Process", "direction": "callees", "depth": 3}
	result, err := eng.TraceCalls(t.Context(), engine.TraceCallsRequest{
		Context: contextID, Symbol: "Process", Direction: provider.Callees, Depth: 3,
	})
	if err != nil {
		t.Fatalf("engine.TraceCalls(%s, Process): %v", contextID, err)
	}
	assertScoped(t, result.Context(), contextID)

	graph := result.Graph()
	calls := make([]call, 0, len(graph.Edges))
	for _, edge := range graph.Edges {
		calls = append(calls, call{Caller: edge.Caller, Callee: edge.Callee, Path: edge.Path, Line: edge.Line})
	}
	direct := parityWire{
		Context:  onTheWire(t, result.Context()),
		Evidence: evidenceOnTheWire(t, result.Context(), result.Evidence()),
		Symbol:   graph.Symbol,
		Calls:    calls,
	}
	wire, raw := parityResult(t, cfg, session, "trace_calls", args)
	traced := assertParity(t, "trace_calls("+contextID+", Process)", direct, wire, raw)

	// Agreement is only worth something if both sides walked a real graph, and
	// the graph they walked has to be this version's.
	called := calleesOf(traced.Calls, "Process")
	if !contains(called, own) {
		t.Errorf("Process calls %v in %s, want it to include %s", called, contextID, own)
	}
	if contains(called, other) {
		t.Errorf("Process calls %v in %s, which is the other version's handler: the trace crossed graphs", called, contextID)
	}

	// The other version's symbol, refused by both. Not an empty graph: a walk
	// that found nothing and a symbol that is not there are different answers.
	_, err = eng.TraceCalls(t.Context(), engine.TraceCallsRequest{
		Context: contextID, Symbol: other, Direction: provider.Callers, Depth: 2,
	})
	assertCodeParity(t, "trace_calls("+contextID+", "+other+")", directCode(t, err),
		parityErrorCode(t, session, "trace_calls", map[string]any{
			"context": contextID, "symbol": other, "direction": "callers", "depth": 2,
		}),
		vacerr.SymbolNotFound)
}

// parityGetCode compares the two paths through get_code: the same file read at
// each version's revision, and a path no revision has.
func parityGetCode(t *testing.T, cfg *config.Config, eng *engine.Engine, session *mcp.ClientSession, contextID, own, other string) {
	t.Helper()

	// processor.go lines 4 to 6 are the body of Process() on every branch, which
	// is where the versions differ.
	const path = "processor.go"
	args := map[string]any{"context": contextID, "path": path, "start_line": 4, "end_line": 6}

	result, err := eng.GetCode(t.Context(), engine.GetCodeRequest{
		Context: contextID, Path: path, StartLine: 4, EndLine: 6,
	})
	if err != nil {
		t.Fatalf("engine.GetCode(%s, %s): %v", contextID, path, err)
	}
	assertScoped(t, result.Context(), contextID)

	src := result.Source()
	direct := parityWire{
		Context:   onTheWire(t, result.Context()),
		Evidence:  evidenceOnTheWire(t, result.Context(), result.Evidence()),
		Path:      src.Path,
		StartLine: src.StartLine,
		EndLine:   src.EndLine,
		Content:   src.Content,
	}
	wire, raw := parityResult(t, cfg, session, "get_code", args)
	read := assertParity(t, "get_code("+contextID+", "+path+")", direct, wire, raw)

	// The bytes both sides agreed on are this version's bytes. Without this the
	// two could agree on the wrong version's content and still pass.
	if !strings.Contains(read.Content, own) || strings.Contains(read.Content, other) {
		t.Errorf("get_code(%s, %s) = %q, want the %s body", contextID, path, read.Content, own)
	}

	// A path no revision has, refused identically. Empty content would be
	// indistinguishable from an empty file.
	_, err = eng.GetCode(t.Context(), engine.GetCodeRequest{
		Context: contextID, Path: "does/not/exist.go", StartLine: 1, EndLine: 1,
	})
	assertCodeParity(t, "get_code("+contextID+", does/not/exist.go)", directCode(t, err),
		parityErrorCode(t, session, "get_code", map[string]any{
			"context": contextID, "path": "does/not/exist.go", "start_line": 1, "end_line": 1,
		}),
		vacerr.InvalidArgument)
}

// TestEngineAndMCPAnswerIdenticallyOverAMultiMemberWorkspace is AC #1, #2 and
// #9's harness: decision-8's release gate held over demo-multi, the fixture's
// workspace of two repositories, rather than only over the single-repository
// contexts [TestEngineAndMCPAnswerIdentically] compares.
//
// list_contexts already proves this for every configured context — demo-multi
// included — because that test's own list_contexts subtest compares the whole
// list [engine.Engine.ListContexts] and the tool return, and demo-multi is one
// of the entries in it. What that subtest cannot reach is a query that runs
// *inside* demo-multi, so this is the rest of decision-11 §5's boundary: a
// search left unnarrowed answers with the members shape [parityEvidence] and
// [searchMatch] carry their extra fields for, and every other tool answers
// narrowed to the one member repository picks out — the same flat shape
// single-repository parity already covers, sourced from a context that
// happens to name several.
func TestEngineAndMCPAnswerIdenticallyOverAMultiMemberWorkspace(t *testing.T) {
	cfg := parityFixture(t)
	eng := parityEngine(t, cfg)
	session := paritySession(t, cfg)

	t.Run("search_code_whole_workspace", func(t *testing.T) { parityMultiSearch(t, cfg, eng, session) })

	// Invoke, Process and NewHandler are each one repository's own — the first
	// second-demo-repo's, the other two versioned-demo-repo's — so a search for
	// one narrowed to the other proves isolation the way [paritySearchCode]'s
	// own and other do, unlike LegacyHandler, which both members declare and so
	// cannot say which repository was actually searched.
	//
	// multiRepo2's absent term is NewHandler rather than Process:
	// gen-second-demo-repo.sh's own invoke.go names Process in a doc comment
	// ("must never be versioned-demo-repo's Process"), which Zoekt's full-text
	// search finds there regardless of Process never being declared — a
	// collision in this fixture's own prose, not the isolation this test
	// checks. NewHandler names nothing second-demo-repo's text mentions at all.
	t.Run("search_code_repository_"+multiRepo1, func(t *testing.T) {
		parityMultiSearchNarrowed(t, cfg, eng, session, multiRepo1, "Process", "Invoke")
	})
	t.Run("search_code_repository_"+multiRepo2, func(t *testing.T) {
		parityMultiSearchNarrowed(t, cfg, eng, session, multiRepo2, "Invoke", "NewHandler")
	})

	// handler.go's own line count differs per repository — see
	// multirepo_integration_test.go's TestGetCodeOverAMultiMemberWorkspace
	// ReadsEachRepositorysOwnContent, which this end line is copied from.
	t.Run("get_code_repository_"+multiRepo1, func(t *testing.T) {
		parityMultiGetCode(t, cfg, eng, session, multiRepo1, 6, "legacy: ", "second: ")
	})
	t.Run("get_code_repository_"+multiRepo2, func(t *testing.T) {
		parityMultiGetCode(t, cfg, eng, session, multiRepo2, 9, "second: ", "legacy: ")
	})

	t.Run("trace_calls_repository_"+multiRepo1, func(t *testing.T) {
		parityMultiTraceCalls(t, cfg, eng, session, multiRepo1, "Process", "Invoke")
	})
	t.Run("trace_calls_repository_"+multiRepo2, func(t *testing.T) {
		parityMultiTraceCalls(t, cfg, eng, session, multiRepo2, "Invoke", "Process")
	})

	// repository selects one member per side of a comparison (decision-11 §4),
	// so demo-multi's own versioned-demo-repo member — the same repository,
	// revision and graph_ref as demo-v1 — compared against demo-v2 is the same
	// comparison [parityCompareCode] and [parityCompareCalls] already answer,
	// reached through a multi-member context and a repository argument instead
	// of a second single-repository one.
	t.Run("compare_code_repository_"+multiRepo1, func(t *testing.T) { parityMultiCompareCode(t, cfg, eng, session) })
	t.Run("compare_calls_repository_"+multiRepo1, func(t *testing.T) { parityMultiCompareCalls(t, cfg, eng, session) })
}

// parityMultiSearch compares the two paths through search_code left unnarrowed
// over demo-multi: the shape [TestEngineAndMCPAnswerIdentically]'s own
// paritySearchCode never reaches, because every context it runs against names
// one repository.
func parityMultiSearch(t *testing.T, cfg *config.Config, eng *engine.Engine, session *mcp.ClientSession) {
	t.Helper()

	// LegacyHandler is what both members declare, on purpose: a match that
	// crossed members would still be a real LegacyHandler, so this is the query
	// that puts the attribution on every match and every citation to the proof,
	// not merely the one that finds something.
	const query = "LegacyHandler"
	result, err := eng.SearchCode(t.Context(), engine.SearchCodeRequest{Context: demorepo.MultiContext, Query: query})
	if err != nil {
		t.Fatalf("engine.SearchCode(%s, %s): %v", demorepo.MultiContext, query, err)
	}
	workspace := result.Context()
	if len(workspace.Members) != 2 {
		t.Fatalf("SearchCode(%s) answered with members %+v, want the two it names", demorepo.MultiContext, workspace.Members)
	}

	matches := make([]searchMatch, 0, len(result.Matches()))
	for _, match := range result.Matches() {
		matches = append(matches, searchMatch{
			Path: match.Path, Line: match.Line, Snippet: match.Snippet,
			Repository: match.Repository, Revision: match.Revision,
		})
	}
	direct := parityWire{
		// list is production's own per-workspace projection (list_contexts.go),
		// reused rather than duplicated here: it is what decides the shape a
		// workspace's member count gets, and a second copy of that decision
		// could drift from the one list_contexts actually runs.
		Context:  list([]vacctx.Workspace{workspace})[0],
		Evidence: evidenceOnTheWire(t, workspace, result.Evidence()),
		Matches:  matches,
	}
	wire, raw := parityResult(t, cfg, session, "search_code", map[string]any{"context": demorepo.MultiContext, "query": query})
	found := assertParity(t, "search_code("+demorepo.MultiContext+", "+query+")", direct, wire, raw)

	seenRepository := map[string]bool{}
	for _, match := range found.Matches {
		if match.Repository == "" || match.Revision == "" {
			t.Errorf("match %+v carries no repository or revision, want the member it was found in", match)
		}
		seenRepository[match.Repository] = true
	}
	for _, repository := range []string{multiRepo1, multiRepo2} {
		if !seenRepository[repository] {
			t.Errorf("search_code(%s, %s) matches = %+v, want at least one from %s", demorepo.MultiContext, query, found.Matches, repository)
		}
	}
}

// parityMultiSearchNarrowed compares the two paths through search_code
// narrowed to one member of demo-multi by repository: the flat, unattributed
// shape a single-repository context already gets, sourced from a context that
// names several. own is a symbol only repository declares and absent is a
// real symbol that belongs to the other member, mirroring the own/other pair
// [paritySearchCode] checks isolation with.
func parityMultiSearchNarrowed(t *testing.T, cfg *config.Config, eng *engine.Engine, session *mcp.ClientSession, repository, own, absent string) {
	t.Helper()

	search := func(query string) parityWire {
		t.Helper()
		result, err := eng.SearchCode(t.Context(), engine.SearchCodeRequest{
			Context: demorepo.MultiContext, Repository: repository, Query: query,
		})
		if err != nil {
			t.Fatalf("engine.SearchCode(%s, repository=%s, %s): %v", demorepo.MultiContext, repository, query, err)
		}
		workspace := result.Context()
		if len(workspace.Members) != 1 || workspace.Members[0].Repository != repository {
			t.Fatalf("SearchCode(%s, repository=%s) answered with members %+v, want the one it names", demorepo.MultiContext, repository, workspace.Members)
		}

		matches := make([]searchMatch, 0, len(result.Matches()))
		for _, match := range result.Matches() {
			matches = append(matches, searchMatch{Path: match.Path, Line: match.Line, Snippet: match.Snippet})
		}
		direct := parityWire{
			Context:  onTheWire(t, workspace),
			Evidence: evidenceOnTheWire(t, workspace, result.Evidence()),
			Matches:  matches,
		}
		wire, raw := parityResult(t, cfg, session, "search_code", map[string]any{
			"context": demorepo.MultiContext, "repository": repository, "query": query,
		})
		return assertParity(t, "search_code("+demorepo.MultiContext+", repository="+repository+", "+query+")", direct, wire, raw)
	}

	if found := search(own); len(found.Matches) == 0 {
		t.Errorf("search_code(%s, repository=%s, %s) found nothing on either side", demorepo.MultiContext, repository, own)
	}
	if found := search(absent); len(found.Matches) != 0 {
		t.Errorf("search_code(%s, repository=%s, %s) = %+v, want no matches: that belongs to the other repository",
			demorepo.MultiContext, repository, absent, found.Matches)
	}
}

// parityMultiGetCode compares the two paths through get_code narrowed to one
// member of demo-multi by repository. handler.go exists in both members and
// says something different in each, which is where a read answered from the
// wrong one would show it; endLine is that repository's own line count,
// because get_code refuses a range past the end of the file rather than
// clamping it.
func parityMultiGetCode(t *testing.T, cfg *config.Config, eng *engine.Engine, session *mcp.ClientSession, repository string, endLine int, want, absent string) {
	t.Helper()

	const path = "handler.go"
	args := map[string]any{
		"context": demorepo.MultiContext, "repository": repository, "path": path, "start_line": 1, "end_line": endLine,
	}
	result, err := eng.GetCode(t.Context(), engine.GetCodeRequest{
		Context: demorepo.MultiContext, Repository: repository, Path: path, StartLine: 1, EndLine: endLine,
	})
	if err != nil {
		t.Fatalf("engine.GetCode(%s, repository=%s, %s): %v", demorepo.MultiContext, repository, path, err)
	}
	workspace := result.Context()
	if len(workspace.Members) != 1 || workspace.Members[0].Repository != repository {
		t.Fatalf("GetCode(%s, repository=%s) answered with members %+v, want the one it names", demorepo.MultiContext, repository, workspace.Members)
	}

	src := result.Source()
	direct := parityWire{
		Context:   onTheWire(t, workspace),
		Evidence:  evidenceOnTheWire(t, workspace, result.Evidence()),
		Path:      src.Path,
		StartLine: src.StartLine,
		EndLine:   src.EndLine,
		Content:   src.Content,
	}
	wire, raw := parityResult(t, cfg, session, "get_code", args)
	read := assertParity(t, "get_code("+demorepo.MultiContext+", repository="+repository+", "+path+")", direct, wire, raw)

	if !strings.Contains(read.Content, want) || strings.Contains(read.Content, absent) {
		t.Errorf("get_code(%s, repository=%s, %s) = %q, want the %s body", demorepo.MultiContext, repository, path, read.Content, want)
	}
}

// parityMultiTraceCalls compares the two paths through trace_calls narrowed to
// one member of demo-multi by repository. LegacyHandler is declared in both
// members' graphs, so a walk answered from the wrong one reports the wrong
// caller among LegacyHandler's callers rather than merely a different count —
// the same collision multirepo_integration_test.go's own trace_calls test
// walks.
func parityMultiTraceCalls(t *testing.T, cfg *config.Config, eng *engine.Engine, session *mcp.ClientSession, repository, wantCaller, absentCaller string) {
	t.Helper()

	args := map[string]any{
		"context": demorepo.MultiContext, "repository": repository, "symbol": "LegacyHandler", "direction": "callers", "depth": 2,
	}
	result, err := eng.TraceCalls(t.Context(), engine.TraceCallsRequest{
		Context: demorepo.MultiContext, Repository: repository, Symbol: "LegacyHandler", Direction: provider.Callers, Depth: 2,
	})
	if err != nil {
		t.Fatalf("engine.TraceCalls(%s, repository=%s, LegacyHandler): %v", demorepo.MultiContext, repository, err)
	}
	workspace := result.Context()
	if len(workspace.Members) != 1 || workspace.Members[0].Repository != repository {
		t.Fatalf("TraceCalls(%s, repository=%s) answered with members %+v, want the one it names", demorepo.MultiContext, repository, workspace.Members)
	}

	graph := result.Graph()
	calls := make([]call, 0, len(graph.Edges))
	for _, edge := range graph.Edges {
		calls = append(calls, call{Caller: edge.Caller, Callee: edge.Callee, Path: edge.Path, Line: edge.Line})
	}
	direct := parityWire{
		Context:  onTheWire(t, workspace),
		Evidence: evidenceOnTheWire(t, workspace, result.Evidence()),
		Symbol:   graph.Symbol,
		Calls:    calls,
	}
	wire, raw := parityResult(t, cfg, session, "trace_calls", args)
	traced := assertParity(t, "trace_calls("+demorepo.MultiContext+", repository="+repository+", LegacyHandler)", direct, wire, raw)

	callers := callersOf(traced.Calls, "LegacyHandler")
	if !contains(callers, wantCaller) {
		t.Errorf("LegacyHandler's callers in %s = %v, want it to include %s", repository, callers, wantCaller)
	}
	if contains(callers, absentCaller) {
		t.Errorf("LegacyHandler's callers in %s = %v, which includes %s from the other repository: the trace crossed graphs", repository, callers, absentCaller)
	}
}

// parityMultiCompareCode compares the two paths through compare_code with the
// from side narrowed out of demo-multi by repository: the same processor.go
// MODIFIED comparison [parityCompareCode] runs between demo-v1 and demo-v2,
// reached with one side named through the multi-member context instead.
func parityMultiCompareCode(t *testing.T, cfg *config.Config, eng *engine.Engine, session *mcp.ClientSession) {
	t.Helper()

	const path = "processor.go"
	args := map[string]any{
		"from_context": demorepo.MultiContext, "repository": multiRepo1, "to_context": v2, "path": path,
	}
	result, err := eng.CompareCode(t.Context(), engine.CompareCodeRequest{
		FromContext: demorepo.MultiContext, Repository: multiRepo1, ToContext: v2, Path: path,
	})
	if err != nil {
		t.Fatalf("engine.CompareCode(%s repository=%s, %s, %s): %v", demorepo.MultiContext, multiRepo1, v2, path, err)
	}
	direct := comparedCodeOnTheWire(t, result)

	res, raw := compareRaw(t, session, "compare_code", args)
	if res.IsError {
		t.Fatalf("compare_code(%v) failed: %s", args, raw)
	}
	var wire compareCodeWire
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	assertNoGraphRefLeak(t, cfg, "compare_code", raw)

	if !reflect.DeepEqual(direct, wire) {
		t.Errorf("compare_code(%s repository=%s, %s, %s):\n  engine: %+v\n     MCP: %+v", demorepo.MultiContext, multiRepo1, v2, path, direct, wire)
	}
	if wire.Change != "MODIFIED" || wire.Path != path {
		t.Errorf("compare_code(%s repository=%s, %s, %s) = %q at %q, want MODIFIED at %s",
			demorepo.MultiContext, multiRepo1, v2, path, wire.Change, wire.Path, path)
	}
	assertParitySide(t, "from", wire.From, raw, true, memberOf(t, cfg, demorepo.MultiContext, multiRepo1))
	assertParitySide(t, "to", wire.To, raw, true, only(cfg, v2))
	t.Logf("compare_code(%s repository=%s, %s, %s): both sides answered %s", demorepo.MultiContext, multiRepo1, v2, path, raw)
}

// parityMultiCompareCalls is [parityMultiCompareCode]'s mirror for
// compare_calls: the same Process added-NewHandler/removed-LegacyHandler
// comparison [parityCompareCalls] runs between demo-v1 and demo-v2, with the
// from side narrowed out of demo-multi by repository instead.
func parityMultiCompareCalls(t *testing.T, cfg *config.Config, eng *engine.Engine, session *mcp.ClientSession) {
	t.Helper()

	args := map[string]any{
		"from_context": demorepo.MultiContext, "repository": multiRepo1, "to_context": v2,
		"symbol": "Process", "direction": "callees", "depth": 1,
	}
	result, err := eng.CompareCalls(t.Context(), engine.CompareCallsRequest{
		FromContext: demorepo.MultiContext, Repository: multiRepo1, ToContext: v2,
		Symbol: "Process", Direction: provider.Callees, Depth: 1,
	})
	if err != nil {
		t.Fatalf("engine.CompareCalls(%s repository=%s, %s, Process): %v", demorepo.MultiContext, multiRepo1, v2, err)
	}
	direct := comparedCallsOnTheWire(t, result)
	wire, raw := compareCallsOnFixture(t, cfg, session, args)

	if !reflect.DeepEqual(direct, wire) {
		t.Errorf("compare_calls(%s repository=%s, %s, Process):\n  engine: %+v\n     MCP: %+v", demorepo.MultiContext, multiRepo1, v2, direct, wire)
	}
	if wire.Presence != "BOTH" {
		t.Errorf("compare_calls(%s repository=%s, %s, Process) presence = %q, want BOTH", demorepo.MultiContext, multiRepo1, v2, wire.Presence)
	}
	if got := classified(wire.Added); !slices.Equal(got, []string{"Process -> NewHandler"}) {
		t.Errorf("compare_calls(%s repository=%s, %s, Process) added = %v, want [Process -> NewHandler]", demorepo.MultiContext, multiRepo1, v2, got)
	}
	if got := classified(wire.Removed); !slices.Equal(got, []string{"Process -> LegacyHandler"}) {
		t.Errorf("compare_calls(%s repository=%s, %s, Process) removed = %v, want [Process -> LegacyHandler]", demorepo.MultiContext, multiRepo1, v2, got)
	}
	assertParitySide(t, "from", wire.From, raw, true, memberOf(t, cfg, demorepo.MultiContext, multiRepo1))
	assertParitySide(t, "to", wire.To, raw, true, only(cfg, v2))
	t.Logf("compare_calls(%s repository=%s, %s, Process): both sides answered %s", demorepo.MultiContext, multiRepo1, v2, raw)
}

// TestEngineAndMCPRefuseAMultiMemberWorkspaceWithoutARepositoryIdentically is
// AC #2's other half over demo-multi: the four tools decision-11 §3 requires a
// repository from refuse identically, with the same INVALID_ARGUMENT and the
// same repositories named in the details, whether asked of the engine or
// through MCP. search_code is absent for the reason [SearchCodeRequest]'s own
// doc gives: repository is optional there, so a blank one is a wider search
// and not a refusal.
func TestEngineAndMCPRefuseAMultiMemberWorkspaceWithoutARepositoryIdentically(t *testing.T) {
	cfg := parityFixture(t)
	eng := parityEngine(t, cfg)
	session := paritySession(t, cfg)

	t.Run("get_code", func(t *testing.T) {
		_, err := eng.GetCode(t.Context(), engine.GetCodeRequest{
			Context: demorepo.MultiContext, Path: "handler.go", StartLine: 1, EndLine: 9,
		})
		assertCodeParity(t, "get_code("+demorepo.MultiContext+", no repository)", directCode(t, err),
			parityErrorCode(t, session, "get_code", map[string]any{
				"context": demorepo.MultiContext, "path": "handler.go", "start_line": 1, "end_line": 9,
			}), vacerr.InvalidArgument)
	})

	t.Run("trace_calls", func(t *testing.T) {
		_, err := eng.TraceCalls(t.Context(), engine.TraceCallsRequest{
			Context: demorepo.MultiContext, Symbol: "LegacyHandler", Direction: provider.Callers, Depth: 2,
		})
		assertCodeParity(t, "trace_calls("+demorepo.MultiContext+", no repository)", directCode(t, err),
			parityErrorCode(t, session, "trace_calls", map[string]any{
				"context": demorepo.MultiContext, "symbol": "LegacyHandler", "direction": "callers", "depth": 2,
			}), vacerr.InvalidArgument)
	})

	t.Run("compare_code", func(t *testing.T) {
		_, err := eng.CompareCode(t.Context(), engine.CompareCodeRequest{
			FromContext: demorepo.MultiContext, ToContext: v2, Path: "processor.go",
		})
		assertCodeParity(t, "compare_code("+demorepo.MultiContext+", no repository)", directCode(t, err),
			parityErrorCode(t, session, "compare_code", map[string]any{
				"from_context": demorepo.MultiContext, "to_context": v2, "path": "processor.go",
			}), vacerr.InvalidArgument)
	})

	t.Run("compare_calls", func(t *testing.T) {
		_, err := eng.CompareCalls(t.Context(), engine.CompareCallsRequest{
			FromContext: demorepo.MultiContext, ToContext: v2, Symbol: "Process", Direction: provider.Callees, Depth: 1,
		})
		assertCodeParity(t, "compare_calls("+demorepo.MultiContext+", no repository)", directCode(t, err),
			parityErrorCode(t, session, "compare_calls", map[string]any{
				"from_context": demorepo.MultiContext, "to_context": v2, "symbol": "Process", "direction": "callees", "depth": 1,
			}), vacerr.InvalidArgument)
	})
}

// TestEngineAndMCPCompareIdentically is the same gate for the two comparison
// tools, which the test above cannot cover: a comparison is answered in two
// versions at once, so its result has no single context and no single evidence
// list to fill a [parityWire] with. It is compared through the two-sided wire
// shapes compare_code_test.go and compare_calls_test.go already decode into,
// which is what pins the field names on both halves of the answer.
//
// The stakes are higher here than for a single-context tool. A comparison
// carries two contexts, two evidence lists and a classification derived from
// both, so an adapter that swapped the sides, merged the citations or dropped
// the absent one would still return a well-formed result — and only a
// side-by-side comparison with what the engine decided shows it.
func TestEngineAndMCPCompareIdentically(t *testing.T) {
	cfg := parityFixture(t)
	eng := parityEngine(t, cfg)
	session := paritySession(t, cfg)

	t.Run("compare_code", func(t *testing.T) { parityCompareCode(t, cfg, eng, session) })
	t.Run("compare_calls", func(t *testing.T) { parityCompareCalls(t, cfg, eng, session) })
}

// parityCompareCode compares the two paths through compare_code: one file per
// outcome the tool can report, and a refusal.
func parityCompareCode(t *testing.T, cfg *config.Config, eng *engine.Engine, session *mcp.ClientSession) {
	t.Helper()

	// No file serves two of these, so two paths that agreed by classifying
	// everything the same way fail here rather than passing on the one case that
	// happens to agree. ADDED and REMOVED are the ones where a side is absent,
	// which is the shape a merged or defaulted side would fill in.
	tests := map[string]struct {
		path           string
		change         string
		fromHas, toHas bool
	}{
		"modified: the handler Process delegates to": {"processor.go", "MODIFIED", true, true},
		"added: a file only release/v2 ever had":     {"newonly.go", "ADDED", false, true},
		"removed: a file only release/v1 ever had":   {"oldonly.go", "REMOVED", true, false},
		"unchanged: a file both releases inherited":  {"shared.go", "UNCHANGED", true, true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			result, err := eng.CompareCode(t.Context(), engine.CompareCodeRequest{
				FromContext: v1, ToContext: v2, Path: tc.path,
			})
			if err != nil {
				t.Fatalf("engine.CompareCode(%s): %v", tc.path, err)
			}
			direct := comparedCodeOnTheWire(t, result)
			wire, raw := compareCodeCall(t, session, v1, v2, tc.path)
			if !reflect.DeepEqual(direct, wire) {
				t.Errorf("compare_code(%s):\n  engine: %+v\n     MCP: %+v", tc.path, direct, wire)
			}

			// What the two sides agreed on, held to what the fixture really wrote:
			// agreement on the wrong classification is still agreement.
			if wire.Change != tc.change || wire.Path != tc.path {
				t.Errorf("compare_code(%s) = %q at %q, want %s at %s", tc.path, wire.Change, wire.Path, tc.change, tc.path)
			}
			assertParitySide(t, "from", wire.From, raw, tc.fromHas, only(cfg, v1))
			assertParitySide(t, "to", wire.To, raw, tc.toHas, only(cfg, v2))
			if tc.fromHas {
				assertScoped(t, result.From().Context(), v1)
			}
			if tc.toHas {
				assertScoped(t, result.To().Context(), v2)
			}
			t.Logf("compare_code(%s, %s, %s): both sides answered %s", v1, v2, tc.path, raw)
		})
	}

	// A context that does not exist, refused identically. An UNCHANGED result
	// here would read as "the file is the same in both versions", which is a
	// claim about a version this server was never given.
	_, err := eng.CompareCode(t.Context(), engine.CompareCodeRequest{
		FromContext: v1, ToContext: "demo-v3", Path: "processor.go",
	})
	assertCodeParity(t, "compare_code(demo-v3)", directCode(t, err),
		parityErrorCode(t, session, "compare_code", map[string]any{
			"from_context": v1, "to_context": "demo-v3", "path": "processor.go",
		}),
		vacerr.ContextNotFound)
}

// parityCompareCalls compares the two paths through compare_calls: the relation
// classification in both directions of presence, and two refusals.
func parityCompareCalls(t *testing.T, cfg *config.Config, eng *engine.Engine, session *mcp.ClientSession) {
	t.Helper()

	// Every case names the relations it expects in each list, so agreement that
	// classified everything as added — or as unchanged — fails here.
	tests := map[string]struct {
		symbol                    string
		direction                 provider.Direction
		presence                  string
		fromHas, toHas            bool
		added, removed, unchanged []string
	}{
		"an added edge and a removed one": {
			symbol: "Process", direction: provider.Callees, presence: "BOTH", fromHas: true, toHas: true,
			added: []string{"Process -> NewHandler"}, removed: []string{"Process -> LegacyHandler"},
		},
		"an unchanged edge": {
			symbol: "Keep", direction: provider.Callees, presence: "BOTH", fromHas: true, toHas: true,
			unchanged: []string{"Keep -> SharedHandler"},
		},
		"a symbol only release/v2 declares": {
			symbol: "NewHandler", direction: provider.Callers, presence: "TO_ONLY", toHas: true,
			added: []string{"Process -> NewHandler"},
		},
		"a symbol only release/v1 declares": {
			symbol: "LegacyHandler", direction: provider.Callers, presence: "FROM_ONLY", fromHas: true,
			removed: []string{"Process -> LegacyHandler"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			result, err := eng.CompareCalls(t.Context(), engine.CompareCallsRequest{
				FromContext: v1, ToContext: v2, Symbol: tc.symbol, Direction: tc.direction, Depth: 1,
			})
			if err != nil {
				t.Fatalf("engine.CompareCalls(%s): %v", tc.symbol, err)
			}
			direct := comparedCallsOnTheWire(t, result)
			wire, raw := compareCallsOnFixture(t, cfg, session, map[string]any{
				"from_context": v1, "to_context": v2,
				"symbol": tc.symbol, "direction": string(tc.direction), "depth": 1,
			})
			if !reflect.DeepEqual(direct, wire) {
				t.Errorf("compare_calls(%s):\n  engine: %+v\n     MCP: %+v", tc.symbol, direct, wire)
			}

			if wire.Presence != tc.presence || wire.RequestedSymbol != tc.symbol {
				t.Errorf("compare_calls(%s) = presence %q for requested %q, want %s for %s",
					tc.symbol, wire.Presence, wire.RequestedSymbol, tc.presence, tc.symbol)
			}
			for _, classification := range []struct {
				name string
				got  []callRelationWire
				want []string
			}{
				{"added", wire.Added, tc.added},
				{"removed", wire.Removed, tc.removed},
				{"unchanged", wire.Unchanged, tc.unchanged},
			} {
				if got := classified(classification.got); !slices.Equal(got, classification.want) {
					t.Errorf("compare_calls(%s) reports %v as %s, want %v", tc.symbol, got, classification.name, classification.want)
				}
			}
			assertParitySide(t, "from", wire.From, raw, tc.fromHas, only(cfg, v1))
			assertParitySide(t, "to", wire.To, raw, tc.toHas, only(cfg, v2))
			if tc.fromHas {
				assertScoped(t, result.From().Context(), v1)
			}
			if tc.toHas {
				assertScoped(t, result.To().Context(), v2)
			}
			t.Logf("compare_calls(%s, %s, %s): both sides answered %s", v1, v2, tc.symbol, raw)
		})
	}

	// A context that does not exist, and a symbol neither version has. The
	// second is the comparison-specific one: an empty result would read as "the
	// symbol is in both versions and nothing about it changed".
	_, err := eng.CompareCalls(t.Context(), engine.CompareCallsRequest{
		FromContext: "demo-v3", ToContext: v2, Symbol: "Process", Direction: provider.Callees, Depth: 1,
	})
	assertCodeParity(t, "compare_calls(demo-v3)", directCode(t, err),
		parityErrorCode(t, session, "compare_calls", map[string]any{
			"from_context": "demo-v3", "to_context": v2, "symbol": "Process", "direction": "callees", "depth": 1,
		}),
		vacerr.ContextNotFound)

	_, err = eng.CompareCalls(t.Context(), engine.CompareCallsRequest{
		FromContext: v1, ToContext: v2, Symbol: "NeverWritten", Direction: provider.Callees, Depth: 1,
	})
	assertCodeParity(t, "compare_calls(NeverWritten)", directCode(t, err),
		parityErrorCode(t, session, "compare_calls", map[string]any{
			"from_context": v1, "to_context": v2, "symbol": "NeverWritten", "direction": "callees", "depth": 1,
		}),
		vacerr.SymbolNotFound)
}

// assertParitySide holds one half of an agreed comparison to the version it was
// answered in, or to being absent.
//
// It is assertComparedSide's job with the configured revision checked as well,
// which is the field the two paths could agree on while both being stale: the
// comparison above proves they read the same revision, and this proves it is
// the one the context declares.
func assertParitySide(t *testing.T, which string, side *comparisonSideWire, raw string, present bool, want vacctx.CodeContext) {
	t.Helper()
	assertComparedSide(t, which, side, raw, present, want)
	if present && side != nil && side.Context.Revision != want.Revision {
		t.Errorf("the %s side was answered at revision %s, want %s", which, side.Context.Revision, want.Revision)
	}
}

// comparedCodeOnTheWire is an engine code comparison projected onto the shape a
// client receives.
//
// It is written out here rather than borrowed from compare_code.go's own
// encoder: a comparison that ran both sides through the encoder under test
// would agree with itself whatever that encoder did.
func comparedCodeOnTheWire(t *testing.T, result engine.CompareCodeResult) compareCodeWire {
	t.Helper()
	hunks := make([]hunkWire, 0, len(result.Hunks()))
	for _, h := range result.Hunks() {
		lines := make([]diffLineWire, 0, len(h.Lines))
		for _, line := range h.Lines {
			lines = append(lines, diffLineWire{Kind: string(line.Kind), Content: line.Content})
		}
		hunks = append(hunks, hunkWire{
			OldStart: h.OldStart, OldLines: h.OldLines,
			NewStart: h.NewStart, NewLines: h.NewLines,
			Lines: lines,
		})
	}
	return compareCodeWire{
		From:   comparedSideOnTheWire(t, result.From()),
		To:     comparedSideOnTheWire(t, result.To()),
		Change: string(result.Change()),
		Path:   result.Path(),
		Binary: result.Binary(),
		Hunks:  hunks,
	}
}

// comparedCallsOnTheWire is an engine call graph comparison projected onto the
// shape a client receives, the mirror of [comparedCodeOnTheWire].
func comparedCallsOnTheWire(t *testing.T, result engine.CompareCallsResult) compareCallsWire {
	t.Helper()
	return compareCallsWire{
		From:               comparedSideOnTheWire(t, result.From()),
		To:                 comparedSideOnTheWire(t, result.To()),
		Presence:           string(result.Presence()),
		RequestedSymbol:    result.RequestedSymbol(),
		FromResolvedSymbol: result.FromResolvedSymbol(),
		ToResolvedSymbol:   result.ToResolvedSymbol(),
		Added:              relationsOnTheWire(result.Added()),
		Removed:            relationsOnTheWire(result.Removed()),
		Unchanged:          relationsOnTheWire(result.Unchanged()),
	}
}

// comparedSideOnTheWire is one version's half as MCP publishes it: the four
// public context fields and that version's own citations, or null where the
// version had nothing. The graph reference is deliberately absent, exactly as
// [onTheWire] leaves it out of a single-context result.
func comparedSideOnTheWire(t *testing.T, s engine.ComparisonSide) *comparisonSideWire {
	t.Helper()
	if !s.Present() {
		return nil
	}
	codeCtx := answeredIn(t, s.Context())
	side := &comparisonSideWire{Evidence: citations(t, s.Evidence())}
	side.Context.ID = codeCtx.ID
	side.Context.Repository = codeCtx.Repository
	side.Context.Branch = codeCtx.Branch
	side.Context.Revision = codeCtx.Revision
	return side
}

// answeredIn is the one member a result was answered in.
//
// Every context in these fixtures names one repository, so a result carrying any
// other number of members is a version scope that went wrong rather than a shape
// to go on reading: this stops the test instead of picking one of them and
// comparing the rest of the answer against a version nobody asked about.
func answeredIn(t *testing.T, workspace vacctx.Workspace) vacctx.CodeContext {
	t.Helper()
	if len(workspace.Members) != 1 {
		t.Fatalf("the engine answered in %d repositories (%+v), want the one this context names",
			len(workspace.Members), workspace)
	}
	return workspace.Members[0]
}

// citations is the one list of citations such a result carries, for the reason
// [answeredIn] insists on the one member: the citations arrive one list per
// member, so a second list would be a second repository's evidence going out
// under a context that names one.
func citations(t *testing.T, cited [][]evidence.Evidence) []evidence.Evidence {
	t.Helper()
	if len(cited) != 1 {
		t.Fatalf("the engine cited %d repositories (%+v), want the one this context names", len(cited), cited)
	}
	return cited[0]
}

// relationsOnTheWire projects one classification list. A version that does not
// have the relation cites an empty list rather than a null one, because null and
// [] are different answers and only one of them survives the round trip as
// itself.
func relationsOnTheWire(found []engine.CallRelation) []callRelationWire {
	wire := make([]callRelationWire, 0, len(found))
	for _, rel := range found {
		wire = append(wire, callRelationWire{
			Caller:       rel.Caller,
			Callee:       rel.Callee,
			Path:         rel.Path,
			FromEvidence: citedOnTheWire(rel.FromEvidence),
			ToEvidence:   citedOnTheWire(rel.ToEvidence),
		})
	}
	return wire
}

func citedOnTheWire(cites []evidence.Evidence) []evidence.Evidence {
	if cites == nil {
		return []evidence.Evidence{}
	}
	return cites
}

// classified names one relation list as the calls it holds, which is the
// classification a comparison is judged on. Order is the engine's — from's
// relations then to's — so comparing with [slices.Equal] holds both sides to
// reporting the same relations in the same order.
func classified(list []callRelationWire) []string {
	named := make([]string, 0, len(list))
	for _, rel := range list {
		named = append(named, rel.Caller+" -> "+rel.Callee)
	}
	return named
}

// assertParity is the comparison itself, and returns the agreed result so the
// caller can go on to check it is not agreement on nothing.
func assertParity(t *testing.T, what string, direct parityWire, wire parityWire, raw string) parityWire {
	t.Helper()
	if !reflect.DeepEqual(direct, wire) {
		t.Errorf("%s:\n  engine: %+v\n     MCP: %+v", what, direct, wire)
	}
	t.Logf("%s: both sides answered %s", what, raw)
	return wire
}

// assertCodeParity requires both paths to have refused with the same code, and
// with the code the contract names. Comparing them to each other alone would be
// satisfied by both being wrong in the same way.
func assertCodeParity(t *testing.T, what string, direct, wire, want vacerr.Code) {
	t.Helper()
	if direct != want {
		t.Errorf("%s: the engine failed with %q, want %q", what, direct, want)
	}
	if wire != direct {
		t.Errorf("%s: the engine failed with %q and the MCP tool with %q", what, direct, wire)
	}
	t.Logf("%s: both sides refused with %s", what, direct)
}

// assertScoped checks the half of the engine's context that MCP does not carry.
//
// GraphRef is the CBM project backing a context: internal, and deliberately not
// on the wire, so the parity comparison cannot see it. That makes this the only
// place it can be checked at all — and it has to be, because an empty one is
// how a trace would end up in no graph in particular.
func assertScoped(t *testing.T, workspace vacctx.Workspace, contextID string) {
	t.Helper()
	codeCtx := answeredIn(t, workspace)
	if codeCtx.ID != contextID {
		t.Errorf("the engine answered in context %q, want %q", codeCtx.ID, contextID)
	}
	if strings.TrimSpace(codeCtx.GraphRef) == "" {
		t.Errorf("context %q reached a provider with no graph_ref", contextID)
	}
}

// onTheWire is the context as MCP publishes it: the four public fields, without
// the graph reference. It mirrors the evidence package's own projection, which
// is what makes it the right shape to compare a tool result against.
func onTheWire(t *testing.T, workspace vacctx.Workspace) listedContext {
	t.Helper()
	codeCtx := answeredIn(t, workspace)
	return listedContext{
		ID:         codeCtx.ID,
		Repository: codeCtx.Repository,
		Branch:     codeCtx.Branch,
		Revision:   codeCtx.Revision,
	}
}

// parityFixture is the prepared fixture's configuration, with a Zoekt started
// for this test. It also requires CBM to be runnable, because trace_calls is
// half of what is being compared and a missing graph engine would turn that
// half into an unnoticed pass.
func parityFixture(t *testing.T) *config.Config {
	t.Helper()
	cfg := fixtureConfig(t)
	if _, err := exec.LookPath(cfg.Providers.CBM.Command); err != nil {
		t.Skipf("codebase-memory-mcp is not runnable at %q: %v", cfg.Providers.CBM.Command, err)
	}
	return cfg
}

// parityEngine builds the engine with all three providers, which is the wiring
// cmd/vacmcp runs. Every other test in this package builds one with the
// providers its own tool reaches and nil for the rest; here both sides have to
// answer every tool.
func parityEngine(t *testing.T, cfg *config.Config) *engine.Engine {
	t.Helper()
	eng := engine.New(resolver.New(cfg), zoektadapter.New(cfg), cbmadapter.New(cfg), gitadapter.New(cfg))
	t.Cleanup(func() { _ = eng.Close() })
	return eng
}

// paritySession serves every tool over a real MCP server on its own engine, and
// connects a client to it. Its engine is a second one over the same
// configuration rather than the caller's: two stacks agreeing is the claim, and
// sharing one would leave only the encoding tested.
//
// It is the wiring cmd/vacmcp runs, so the comparison tools are registered here
// too — compare_code_integration_test.go and compare_calls_integration_test.go
// ask this session for them rather than standing up a second server that would
// have to be kept the same as this one.
func paritySession(t *testing.T, cfg *config.Config) *mcp.ClientSession {
	t.Helper()

	srv := server.New(testVersion)
	eng := parityEngine(t, cfg)
	AddListContexts(srv, eng)
	AddSearchCode(srv, eng)
	AddTraceCalls(srv, eng)
	AddGetCode(srv, eng)
	AddCompareCode(srv, eng)
	AddCompareCalls(srv, eng)

	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true},
	))
	t.Cleanup(httpServer.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "vacmcp-test", Version: testVersion}, nil)
	clientSession, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: httpServer.URL}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	return clientSession
}

// paritySessionOverStdio is [paritySession]'s own registration — every tool,
// on a second, independently built *mcp.Server and engine over cfg — reached
// instead over [mcp.IOTransport], the newline-delimited-JSON framing
// mcp.StdioTransport itself is: `Connect` on that type does nothing but pair
// os.Stdin and os.Stdout into one of these. Paired over an in-memory pipe
// rather than run as the built binary's subprocess, because what could differ
// between the two transports this server offers is that framing and nothing
// about a process boundary: the tool handlers, the engine and doc-1's Tool
// Contract are all reached identically either way, and the process boundary
// itself is doc-1 §15's release gate's to check, over the binary it builds.
//
// A third engine and a third *mcp.Server, not [paritySession]'s own, for the
// reason that function's doc gives: agreement is only interesting between two
// independently constructed things, and comparing a server's HTTP answer to
// its own STDIO answer would only prove it agrees with itself down two wires.
func paritySessionOverStdio(t *testing.T, cfg *config.Config) *mcp.ClientSession {
	t.Helper()

	srv := server.New(testVersion)
	eng := parityEngine(t, cfg)
	AddListContexts(srv, eng)
	AddSearchCode(srv, eng)
	AddTraceCalls(srv, eng)
	AddGetCode(srv, eng)
	AddCompareCode(srv, eng)
	AddCompareCalls(srv, eng)

	// Two pipes, one per direction: a client's Reader is the other end of the
	// server's Writer, and the other way round, which is what turns two
	// unidirectional pipes into the duplex stream STDIO gives a real subprocess.
	serverRead, clientWrite := io.Pipe()
	clientRead, serverWrite := io.Pipe()
	go func() { _ = srv.Run(t.Context(), &mcp.IOTransport{Reader: serverRead, Writer: serverWrite}) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "vacmcp-test", Version: testVersion}, nil)
	clientSession, err := client.Connect(t.Context(), &mcp.IOTransport{Reader: clientRead, Writer: clientWrite}, nil)
	if err != nil {
		t.Fatalf("connect over stdio: %v", err)
	}
	// Closing the client's writer is what lets srv.Run's goroutine see EOF and
	// return, the in-process mirror of a real STDIO subprocess's stdin closing
	// when its caller goes away.
	t.Cleanup(func() { _ = clientSession.Close() })
	return clientSession
}

// TestSTDIOAndStreamableHTTPAnswerIdenticallyOverAMultiMemberWorkspace is AC
// #7: the transport a client happens to use is no part of the version
// contract, so it must not be part of the answer either. Every other
// comparison in this file is the engine against one MCP transport, Streamable
// HTTP; this is MCP against MCP, comparing that transport's own bytes against
// [paritySessionOverStdio]'s, over demo-multi — where a members array and
// per-item attribution are what a transport-specific difference in framing or
// encoding would have the most room to disturb — and over every one of the
// shapes decision-11 §5 draws: list_contexts's members array, an unnarrowed
// search's attribution, and the flat shape each repository-narrowed tool falls
// back to.
func TestSTDIOAndStreamableHTTPAnswerIdenticallyOverAMultiMemberWorkspace(t *testing.T) {
	cfg := parityFixture(t)
	streamable := paritySession(t, cfg)
	stdio := paritySessionOverStdio(t, cfg)

	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"list_contexts", map[string]any{}},
		{"search_code", map[string]any{"context": demorepo.MultiContext, "query": "LegacyHandler"}},
		{"get_code", map[string]any{
			"context": demorepo.MultiContext, "repository": multiRepo1, "path": "handler.go", "start_line": 1, "end_line": 6,
		}},
		{"trace_calls", map[string]any{
			"context": demorepo.MultiContext, "repository": multiRepo2, "symbol": "LegacyHandler", "direction": "callers", "depth": 2,
		}},
		{"compare_code", map[string]any{
			"from_context": demorepo.MultiContext, "repository": multiRepo1, "to_context": v2, "path": "processor.go",
		}},
		{"compare_calls", map[string]any{
			"from_context": demorepo.MultiContext, "repository": multiRepo1, "to_context": v2,
			"symbol": "Process", "direction": "callees", "depth": 1,
		}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			httpRaw, httpIsError := parityCall(t, streamable, tc.tool, tc.args)
			stdioRaw, stdioIsError := parityCall(t, stdio, tc.tool, tc.args)
			if httpIsError != stdioIsError {
				t.Fatalf("%s(%v): Streamable HTTP is-error = %v, STDIO is-error = %v\n  HTTP:  %s\n  STDIO: %s",
					tc.tool, tc.args, httpIsError, stdioIsError, httpRaw, stdioRaw)
			}
			if httpRaw != stdioRaw {
				t.Errorf("%s(%v):\n  HTTP:  %s\n  STDIO: %s", tc.tool, tc.args, httpRaw, stdioRaw)
			}
			t.Logf("%s(%v): both transports answered %s", tc.tool, tc.args, httpRaw)
		})
	}
}

// parityCall makes one tools/call and returns the JSON text the client received
// together with whether it was an error result. The text rather than the
// decoded structured content, for the reason the other integration tests give:
// the difference between [] and null only survives in the bytes.
func parityCall(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("tools/call %s(%v): %v", name, args, err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("%s result carries %d content blocks, want 1", name, len(res.Content))
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("%s result content = %#v, want text", name, res.Content[0])
	}
	return text.Text, res.IsError
}

// parityResult calls a tool, requires it to have answered, and decodes what came
// back. The raw text travels with it so a failed comparison can be read.
func parityResult(t *testing.T, cfg *config.Config, session *mcp.ClientSession, name string, args map[string]any) (parityWire, string) {
	t.Helper()

	raw, isError := parityCall(t, session, name, args)
	if isError {
		t.Fatalf("%s(%v) failed: %s", name, args, raw)
	}
	var wire parityWire
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	assertNoGraphRefLeak(t, cfg, name, raw)
	return wire, raw
}

// configuredGraphRefs is the CBM project name behind every member of every
// context cfg configures — every graph a tool's output could leak, read off
// the fixture's own configuration rather than assumed from a naming
// convention. A hardcoded "vacmcp-demo-" prefix, which this used to be, misses
// second-demo-repo's own project: demorepo.Repo2Graph is "vacmcp-demo2", with
// no dash after "demo", so it would leak silently under that check and still
// pass.
func configuredGraphRefs(cfg *config.Config) []string {
	seen := map[string]bool{}
	var refs []string
	for _, workspace := range cfg.Contexts {
		for _, member := range workspace.Members {
			if member.GraphRef != "" && !seen[member.GraphRef] {
				seen[member.GraphRef] = true
				refs = append(refs, member.GraphRef)
			}
		}
	}
	return refs
}

// assertNoGraphRefLeak fails the test if raw names the graph_ref field or any
// of cfg's configured CBM project names. The graph reference is internal, and
// a tool's output is the only place it could leak from; the struct comparison
// above cannot catch it, because it is not a field parityWire has.
func assertNoGraphRefLeak(t *testing.T, cfg *config.Config, what, raw string) {
	t.Helper()
	if strings.Contains(raw, "graph_ref") {
		t.Errorf("%s leaked the graph reference: %s", what, raw)
		return
	}
	for _, ref := range configuredGraphRefs(cfg) {
		if strings.Contains(raw, ref) {
			t.Errorf("%s leaked the graph reference %s: %s", what, ref, raw)
		}
	}
}

// memberOf returns the one member of cfg's context id whose repository is
// repository, failing the test when id names no such repository. Read by
// name rather than by [config.Config.Contexts]'s member index, so it does not
// assume the fixture's own members are written in a particular order — that
// order is already checked once, in concurrency_integration_test.go's
// concurrentVersions, and this need not repeat it to rely on the same fact.
func memberOf(t *testing.T, cfg *config.Config, id, repository string) vacctx.CodeContext {
	t.Helper()
	for _, member := range cfg.Contexts[id].Members {
		if member.Repository == repository {
			return member
		}
	}
	t.Fatalf("%s names no repository %s", id, repository)
	return vacctx.CodeContext{}
}

// parityErrorCode calls a tool, requires it to have refused, and returns the
// code it refused with.
func parityErrorCode(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) vacerr.Code {
	t.Helper()

	raw, isError := parityCall(t, session, name, args)
	if !isError {
		t.Fatalf("%s(%v) answered with %s, want an error result", name, args, raw)
	}
	var body struct {
		Error struct {
			Code vacerr.Code `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return body.Error.Code
}

// directCode is the code an engine error carries, which is the same error model
// the wire shape above is built from.
func directCode(t *testing.T, err error) vacerr.Code {
	t.Helper()
	if err == nil {
		t.Fatal("the engine answered, want it to have refused")
	}
	var vErr *vacerr.Error
	if !errors.As(err, &vErr) {
		t.Fatalf("the engine failed with %v (%T), want a *vacerr.Error", err, err)
	}
	return vErr.Code
}
