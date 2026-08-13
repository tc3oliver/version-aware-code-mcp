package demorepo

import (
	"os"
	"path/filepath"
	"testing"
)

// Fixture is what testdata/prepare-fixture.sh leaves behind: the demo
// repository, a Zoekt index holding all of its branches, and the configuration
// naming the contexts whose graph_refs are the CBM projects it built.
//
// The CBM graphs are not paths — they live in CBM's own store and are reached
// through the graph_ref of a context in Config.
type Fixture struct {
	Repo       string
	Config     string
	ZoektIndex string
}

// AmbiguousGraph is the CBM project the script builds beside the two versions:
// one function name declared in two packages. It is no version of the demo
// repository — the repository duplicates no name — so it is named here rather
// than configured as a context, and a test asking about an ambiguous symbol
// points at it instead of indexing a project of its own.
const AmbiguousGraph = "vacmcp-demo-ambiguous"

// Prepared returns the fixture, skipping the test when it has not been built on
// this machine. Building it needs Zoekt and CBM, which a plain `make test` does
// not require, so an integration test asks for the fixture and gets skipped
// rather than failing where the tools are absent.
func Prepared(t testing.TB) Fixture {
	t.Helper()
	root := moduleRoot(t)
	fixture := Fixture{
		Repo:       filepath.Join(root, "testdata", "versioned-demo-repo"),
		Config:     filepath.Join(root, "testdata", "fixture", "config.yaml"),
		ZoektIndex: filepath.Join(root, "testdata", "fixture", "zoekt-index"),
	}
	// The script writes the configuration last, so it is there only after a run
	// that indexed and built every graph.
	if _, err := os.Stat(fixture.Config); err != nil {
		t.Skipf("demorepo: fixture not prepared, run testdata/prepare-fixture.sh: %v", err)
	}
	return fixture
}
