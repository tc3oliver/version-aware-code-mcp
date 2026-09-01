package tools

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/evidence"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// stackedNext is [stackedSearch] one revision on: the same two repositories at
// new revisions, so a comparison between the two contexts is a comparison of two
// versions rather than of two repositories, which is the only comparison this
// server offers.
var stackedNext = vacctx.Workspace{ID: "stack-next", Members: []vacctx.CodeContext{
	{
		ID: "stack-next", Repository: "alpha", Branch: "main",
		Revision: "4444444444444444444444444444444444444444", GraphRef: "alpha-main-next",
	},
	{
		ID: "stack-next", Repository: "beta", Branch: "release/2.x",
		Revision: "5555555555555555555555555555555555555555", GraphRef: "beta-v2-next",
	},
}}

// traceCallsWire is a trace as a client receives it. Decoding into it is what
// pins the field names: a wrong json tag leaves its field empty.
type traceCallsWire struct {
	Context struct {
		ID         string `json:"id"`
		Repository string `json:"repository"`
		Branch     string `json:"branch"`
		Revision   string `json:"revision"`
	} `json:"context"`
	Evidence []evidence.Evidence `json:"evidence"`
	Symbol   string              `json:"symbol"`
	Calls    []call              `json:"calls"`
}

// TestListContextsShapeFollowsTheMemberCount is decision-11 §5 on the wire: the
// context naming one repository carries repository, branch and revision, and the
// context naming two carries a members array instead.
//
// The whole document is pinned rather than a field at a time, because both halves
// of the rule are about what is *not* there: no members array on the
// one-repository context, which is what a v0.4.0 client would not know how to
// read, and no repository, branch or revision on the two-repository one, which
// would be one member's version standing for the whole set.
//
// This is also the only place an agent can learn that a context has several
// repositories, so it has to say so before any other tool requires it.
func TestListContextsShapeFollowsTheMemberCount(t *testing.T) {
	raw := callListContexts(t, map[string]vacctx.Workspace{
		soloSearch.ID:    soloSearch,
		stackedSearch.ID: stackedSearch,
	})

	const want = `{"contexts":[` +
		`{"branch":"main","id":"solo","repository":"alpha","revision":"1111111111111111111111111111111111111111"},` +
		`{"id":"stack","members":[` +
		`{"branch":"main","repository":"alpha","revision":"1111111111111111111111111111111111111111"},` +
		`{"branch":"release/2.x","repository":"beta","revision":"2222222222222222222222222222222222222222"}]}]}`
	if raw != want {
		t.Errorf("list_contexts returned\n%s\nwant\n%s", raw, want)
	}

	// A member is not separately addressable, so it carries no id of its own: one
	// would only be the workspace's repeated, and an agent could not pass it
	// anywhere.
	if strings.Count(raw, `"id"`) != 2 {
		t.Errorf("an id is listed per member rather than per context: %s", raw)
	}
	if strings.Contains(raw, "graph") {
		t.Errorf("list_contexts leaked a graph reference: %s", raw)
	}
}

// TestToolsRefuseAMultiMemberContextWithoutARepository is the other half of
// list_contexts saying which repositories a context has: the four tools that
// answer in exactly one of them refuse to pick.
//
// The refusal is the point. Answering in the first member — or in the only one
// that happens to have the path today — would put a whole repository's worth of
// code outside an answer that names the context the caller asked for, and the
// caller would have no way to see it happened. So the check is on what reaches
// the client: the error envelope with its code intact, and nothing beside it a
// client could read as half an answer.
func TestToolsRefuseAMultiMemberContextWithoutARepository(t *testing.T) {
	session := workspaceSession(t)

	for _, tc := range []struct {
		tool string
		args map[string]any
		// The keys of a successful answer from this tool, none of which may
		// travel with a failure. They are written with their colon because the
		// refusal names the side it is about, and "side":"from" is not a from
		// side leaking into an error.
		answered []string
	}{
		{
			"get_code",
			map[string]any{"context": stackedSearch.ID, "path": comparedPath, "start_line": 4, "end_line": 6},
			[]string{`"content":`, `"evidence":`},
		},
		{
			"trace_calls",
			map[string]any{"context": stackedSearch.ID, "symbol": comparedSymbol, "direction": "callees", "depth": 2},
			[]string{`"calls":`, `"evidence":`},
		},
		{
			"compare_code",
			map[string]any{"from_context": stackedSearch.ID, "to_context": stackedNext.ID, "path": comparedPath},
			[]string{`"from":`, `"to":`, `"change":`, `"hunks":`},
		},
		{
			"compare_calls",
			map[string]any{
				"from_context": stackedSearch.ID, "to_context": stackedNext.ID,
				"symbol": comparedSymbol, "direction": "callees", "depth": 2,
			},
			[]string{`"from":`, `"to":`, `"presence":`, `"added":`},
		},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			vErr, raw := compareError(t, session, tc.tool, tc.args)
			t.Logf("%s without a repository -> %s", tc.tool, raw)

			if vErr.Code != vacerr.InvalidArgument {
				t.Errorf("code = %q, want INVALID_ARGUMENT", vErr.Code)
			}
			for _, leak := range tc.answered {
				if strings.Contains(raw, leak) {
					t.Errorf("error result carries %s: %s", leak, raw)
				}
			}
			// The caller cannot see the configuration, so a refusal that did not
			// name the repositories would leave it holding the argument that fixes
			// this with nothing to write in it.
			for _, repository := range []string{"alpha", "beta"} {
				if !strings.Contains(raw, repository) {
					t.Errorf("the refusal does not name the selectable repository %q: %s", repository, raw)
				}
			}
		})
	}
}

// TestToolsAnswerInTheRepositoryTheRequestSelects is the same four tools with
// the argument supplied: each answers in that member, at that member's own
// revision, and says so.
//
// beta is deliberately the second member of both contexts. A tool that ignored
// the argument and took the workspace's first member would still answer, and
// would still carry a context block that looked complete — the repository and
// the revision are the only two fields that say it answered somewhere else.
func TestToolsAnswerInTheRepositoryTheRequestSelects(t *testing.T) {
	session := workspaceSession(t)
	from, to := stackedSearch.Members[1], stackedNext.Members[1]

	t.Run("get_code", func(t *testing.T) {
		raw := succeeded(t, session, "get_code", map[string]any{
			"context": stackedSearch.ID, "repository": from.Repository,
			"path": comparedPath, "start_line": 4, "end_line": 6,
		})
		var got getCodeWire
		decode(t, raw, &got)
		assertVersion(t, "context", got.Context.Repository, got.Context.Revision, from)
		if got.Content == "" {
			t.Errorf("get_code answered with no content: %s", raw)
		}
	})

	t.Run("trace_calls", func(t *testing.T) {
		raw := succeeded(t, session, "trace_calls", map[string]any{
			"context": stackedSearch.ID, "repository": from.Repository,
			"symbol": comparedSymbol, "direction": "callees", "depth": 2,
		})
		var got traceCallsWire
		decode(t, raw, &got)
		assertVersion(t, "context", got.Context.Repository, got.Context.Revision, from)
		if len(got.Calls) == 0 {
			t.Errorf("trace_calls answered with no calls: %s", raw)
		}
	})

	t.Run("compare_code", func(t *testing.T) {
		raw := succeeded(t, session, "compare_code", map[string]any{
			"from_context": stackedSearch.ID, "to_context": stackedNext.ID,
			"repository": from.Repository, "path": comparedPath,
		})
		var got compareCodeWire
		decode(t, raw, &got)
		if got.From == nil || got.To == nil {
			t.Fatalf("a comparison of a file both versions have reported a side as absent: %s", raw)
		}
		assertVersion(t, "from", got.From.Context.Repository, got.From.Context.Revision, from)
		assertVersion(t, "to", got.To.Context.Repository, got.To.Context.Revision, to)
	})

	t.Run("compare_calls", func(t *testing.T) {
		raw := succeeded(t, session, "compare_calls", map[string]any{
			"from_context": stackedSearch.ID, "to_context": stackedNext.ID,
			"repository": from.Repository, "symbol": comparedSymbol,
			"direction": "callees", "depth": 2,
		})
		var got compareCallsWire
		decode(t, raw, &got)
		if got.From == nil || got.To == nil {
			t.Fatalf("a symbol both versions have reported a side as absent: %s", raw)
		}
		assertVersion(t, "from", got.From.Context.Repository, got.From.Context.Revision, from)
		assertVersion(t, "to", got.To.Context.Repository, got.To.Context.Revision, to)
	})
}

// TestSearchCodeNarrowedToOneRepositoryAnswersInTheFlatShape is the one place
// repository means something other than "which one, since I can only do one":
// search covers every member without it, so giving it narrows an answer that
// would otherwise have been about two versions to one.
//
// The narrowed answer is the one-member shape, matches unattributed and context
// block flat, because that is what it is: one repository at one revision, named
// once. A result that kept the members shape after being narrowed would report
// versions it did not look in.
func TestSearchCodeNarrowedToOneRepositoryAnswersInTheFlatShape(t *testing.T) {
	raw := succeeded(t, workspaceSession(t), "search_code", map[string]any{
		"context": stackedSearch.ID, "repository": "beta", "query": "Process",
	})

	const want = `{"context":{"id":"stack","repository":"beta","branch":"release/2.x",` +
		`"revision":"2222222222222222222222222222222222222222"},` +
		`"evidence":[{"location":{"path":"beta.go","start_line":3,"end_line":3},"snippet":"func Process()"}],` +
		`"matches":[{"path":"beta.go","line":3,"snippet":"func Process()"}]}`
	if raw != want {
		t.Errorf("search_code narrowed to beta returned\n%s\nwant\n%s", raw, want)
	}
}

// TestEveryToolRegistersWithAnInferredOutputSchema is what a client sees before
// it calls anything: six tools, and an output schema on exactly the one whose
// payload is its own type.
//
// The five that declare none answer with an [evidence.Output], whose wire shape
// is the evidence package's to decide and has two forms. A schema here would not
// document them, it would enforce one: the SDK validates every result against a
// declared schema, so a copy that described the flat context block would stop the
// members one from being returned at all — a protocol fault in place of an
// answer, carrying none of this server's error model.
//
// list_contexts is the exception because its payload is [listContextsOutput],
// which is not an evidence.Output and cannot drift from one. Its schema is
// checked against the Go type field by field, in both directions: a property the
// type does not have would be a hand-written schema, and a field the schema does
// not have would be one that had gone stale.
func TestEveryToolRegistersWithAnInferredOutputSchema(t *testing.T) {
	listed, err := workspaceSession(t).ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	registered := map[string]*mcp.Tool{}
	for _, tool := range listed.Tools {
		registered[tool.Name] = tool
	}

	for _, name := range []string{"search_code", "get_code", "trace_calls", "compare_code", "compare_calls"} {
		tool, ok := registered[name]
		if !ok {
			t.Fatalf("tools/list returned %v, none named %s", slices.Sorted(maps.Keys(registered)), name)
		}
		if tool.OutputSchema != nil {
			t.Errorf("%s advertises an output schema, which the SDK enforces on every result: %v", name, tool.OutputSchema)
		}
	}

	tool, ok := registered["list_contexts"]
	if !ok {
		t.Fatalf("tools/list returned %v, none named list_contexts", slices.Sorted(maps.Keys(registered)))
	}
	raw, err := json.Marshal(tool.OutputSchema)
	if err != nil {
		t.Fatalf("marshal output schema: %v", err)
	}
	var schema struct {
		Properties struct {
			Contexts struct {
				Items json.RawMessage `json:"items"`
			} `json:"contexts"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("decode output schema %s: %v", raw, err)
	}

	assertSchemaMatchesType(t, "contexts[]", schema.Properties.Contexts.Items, reflect.TypeFor[listedContext]())
	if strings.Contains(string(raw), "null") {
		t.Errorf("output schema admits null: %s", raw)
	}
}

// assertSchemaMatchesType checks an object schema declares exactly the json
// names of typ's fields, and recurses into an array of objects. It is what says
// the schema was inferred from the Go type rather than written out beside it.
func assertSchemaMatchesType(t *testing.T, path string, raw json.RawMessage, typ reflect.Type) {
	t.Helper()

	var object struct {
		Properties map[string]struct {
			Items json.RawMessage `json:"items"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatalf("decode %s schema %s: %v", path, raw, err)
	}

	fields := map[string]reflect.StructField{}
	for _, field := range reflect.VisibleFields(typ) {
		fields[strings.Split(field.Tag.Get("json"), ",")[0]] = field
	}
	if got, want := slices.Sorted(maps.Keys(object.Properties)), slices.Sorted(maps.Keys(fields)); !slices.Equal(got, want) {
		t.Errorf("%s schema declares %v, want the fields of %s, %v", path, got, typ, want)
	}

	for name, property := range object.Properties {
		field, ok := fields[name]
		if !ok || field.Type.Kind() != reflect.Slice {
			continue
		}
		assertSchemaMatchesType(t, path+"."+name+"[]", property.Items, field.Type.Elem())
	}
}

// workspaceSession serves the six tools over two contexts naming two
// repositories each, plus the one-repository context, with every provider wired:
// a tool that answered from the wrong member would answer rather than fail, which
// is the mistake these tests are looking for.
func workspaceSession(t *testing.T) *mcp.ClientSession {
	t.Helper()

	return serveTools(t, engine.New(
		compareContexts{
			soloSearch.ID:    soloSearch,
			stackedSearch.ID: stackedSearch,
			stackedNext.ID:   stackedNext,
		},
		wiredSearch{},
		compareGraph{stackedSearch.ID: compareFromGraph, stackedNext.ID: compareToGraph, soloSearch.ID: compareFromGraph},
		v040Source{},
	))
}

// succeeded calls a tool and requires it to have answered, returning the JSON
// text the client received. It reads the text rather than the decoded structured
// content because these assertions are about what is and is not on the wire.
func succeeded(t *testing.T, session *mcp.ClientSession, tool string, args map[string]any) string {
	t.Helper()

	res, raw := compareRaw(t, session, tool, args)
	if res.IsError {
		t.Fatalf("%s(%v) failed: %s", tool, args, raw)
	}
	// The graph reference is internal in both shapes, and a context of two members
	// is two chances to leak one.
	if strings.Contains(raw, "graph") {
		t.Errorf("%s leaked a graph reference: %s", tool, raw)
	}
	return raw
}

func decode(t *testing.T, raw string, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(raw), into); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
}

// assertVersion checks a context block reports the member the request selected.
// Both fields, because a repository alone would pass a result that named the
// right repository at another member's revision.
func assertVersion(t *testing.T, name, repository, revision string, want vacctx.CodeContext) {
	t.Helper()

	if repository != want.Repository || revision != want.Revision {
		t.Errorf("%s answered in %s at %s, want the selected member %s at %s",
			name, repository, revision, want.Repository, want.Revision)
	}
}
