//go:build integration

package tools

import (
	"strings"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
)

// compare_code over a real MCP session against the real git repository
// testdata/gen-versioned-demo-repo.sh generates. There is no fake source
// backend: what a fake would prove is that this package agrees with our own idea
// of git, where the four outcomes below are facts about two real branches — and
// they are written into that repository on purpose, so every change kind the
// tool can report has one file that really is that change.
//
// The wire shapes are the ones compare_code_test.go already decodes into, and
// the sides are held to the contexts the prepared fixture configures.

// TestCompareCodeReportsEveryChangeKindOnTheRealFixture is all four outcomes
// across the two release branches, in the direction demo-v1 -> demo-v2.
//
// No file serves two of them, so a tool that classified everything the same way
// — or read one revision twice — fails here rather than passing on the one case
// that happens to agree.
func TestCompareCodeReportsEveryChangeKindOnTheRealFixture(t *testing.T) {
	cfg := traceFixture(t)
	session := paritySession(t, cfg)

	tests := map[string]struct {
		path      string
		change    string
		from, to  bool
		wantLines []diffLineWire
	}{
		"modified: the handler Process delegates to": {
			path: "processor.go", change: "MODIFIED", from: true, to: true,
			wantLines: []diffLineWire{
				{Kind: "REMOVED", Content: "LegacyHandler"},
				{Kind: "ADDED", Content: "NewHandler"},
			},
		},
		"added: a file only release/v2 ever had": {
			path: "newonly.go", change: "ADDED", to: true,
			wantLines: []diffLineWire{{Kind: "ADDED", Content: "func NewOnly"}},
		},
		"removed: a file only release/v1 ever had": {
			path: "oldonly.go", change: "REMOVED", from: true,
			wantLines: []diffLineWire{{Kind: "REMOVED", Content: "func OldOnly"}},
		},
		"unchanged: a file both releases inherited from main": {
			path: "shared.go", change: "UNCHANGED", from: true, to: true,
		},
		"unchanged: a file neither release touched": {
			path: "go.mod", change: "UNCHANGED", from: true, to: true,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, raw := compareCodeCall(t, session, v1, v2, tc.path)

			if got.Change != tc.change || got.Path != tc.path {
				t.Errorf("compare_code(%s) = %q at %q, want %s at %s", tc.path, got.Change, got.Path, tc.change, tc.path)
			}
			if got.Binary {
				t.Errorf("%s came back marked binary, which is not what it is", tc.path)
			}

			// Each side is the version it was read at, or absent — and an absent
			// side is most of the answer for ADDED and REMOVED.
			assertComparedSide(t, "from", got.From, raw, tc.from, cfg.Contexts[v1])
			assertComparedSide(t, "to", got.To, raw, tc.to, cfg.Contexts[v2])

			// An unchanged file has nothing to show line by line, and says so with
			// an empty list rather than a null one: to an agent those differ.
			if tc.change == "UNCHANGED" {
				if len(got.Hunks) != 0 || !strings.Contains(raw, `"hunks":[]`) {
					t.Errorf("%s is UNCHANGED and carries %d hunks: %s", tc.path, len(got.Hunks), raw)
				}
			} else if len(got.Hunks) == 0 {
				t.Fatalf("%s is %s and carries no hunks: %s", tc.path, got.Change, raw)
			}

			for _, h := range got.Hunks {
				// A hunk that does not locate itself in the version it belongs to
				// cannot be checked against the file it claims to describe.
				if (tc.from && h.OldStart < 1) || (tc.to && h.NewStart < 1) {
					t.Errorf("hunk %+v of %s does not locate itself", h, tc.path)
				}
			}
			for _, want := range tc.wantLines {
				if !hunksShow(got.Hunks, want.Kind, want.Content) {
					t.Errorf("no %s line mentioning %q among the hunks of %s: %s", want.Kind, want.Content, tc.path, raw)
				}
			}
			t.Logf("compare_code(%s, %s, %s) = %s", v1, v2, tc.path, raw)
		})
	}
}

// assertComparedSide holds one half of a comparison to the version it was
// answered in, or to being absent. Both comparison tools report their sides in
// the same shape, so compare_calls_integration_test.go uses it too.
//
// The absent case is checked in the bytes as well as in the decoded value: a
// null and an omitted key both decode to a nil pointer, and only one of them is
// the shape the contract promises.
func assertComparedSide(t *testing.T, which string, side *comparisonSideWire, raw string, present bool, want vacctx.CodeContext) {
	t.Helper()
	if !present {
		if side != nil {
			t.Errorf("the %s side answered %+v, want it absent: that version does not have this", which, side)
		}
		if !strings.Contains(raw, `"`+which+`":null`) {
			t.Errorf("the %s side is absent but is not null on the wire: %s", which, raw)
		}
		return
	}
	if side == nil {
		t.Fatalf("the %s side is null, want it answered in %s: %s", which, want.ID, raw)
	}
	assertSideContext(t, which, side, want)
}

// hunksShow reports whether any hunk line of that kind carries the text.
func hunksShow(hunks []hunkWire, kind, text string) bool {
	for _, h := range hunks {
		for _, line := range h.Lines {
			if line.Kind == kind && strings.Contains(line.Content, text) {
				return true
			}
		}
	}
	return false
}
