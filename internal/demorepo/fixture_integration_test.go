//go:build integration

package demorepo_test

import (
	"path/filepath"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/internal/demorepo"
)

// TestPreparedFixtureMatchesTheRepository catches a stale fixture: a
// configuration naming a revision the repository no longer carries, or two
// contexts pointing at one CBM project, would have the integration tests search
// one version and trace another.
func TestPreparedFixtureMatchesTheRepository(t *testing.T) {
	fixture := demorepo.Prepared(t)

	cfg, err := config.Load(fixture.Config)
	if err != nil {
		t.Fatalf("load %s: %v", fixture.Config, err)
	}

	graphRefs := map[string]string{}
	for id, branch := range map[string]string{"demo-v1": demorepo.V1, "demo-v2": demorepo.V2} {
		workspace, ok := cfg.Contexts[id]
		if !ok {
			t.Fatalf("context %q is missing from %s", id, fixture.Config)
		}
		if len(workspace.Members) != 1 {
			t.Fatalf("context %q names %d repositories, want the one %s declares", id, len(workspace.Members), fixture.Config)
		}
		codeCtx := workspace.Members[0]
		if codeCtx.Branch != branch {
			t.Errorf("context %q branch = %q, want %q", id, codeCtx.Branch, branch)
		}
		if rev := demorepo.Revision(t, fixture.Repo, branch); codeCtx.Revision != rev {
			t.Errorf("context %q revision = %s, want %s; re-run testdata/prepare-fixture.sh", id, codeCtx.Revision, rev)
		}
		if other, dup := graphRefs[codeCtx.GraphRef]; dup {
			t.Errorf("contexts %q and %q share graph_ref %q", other, id, codeCtx.GraphRef)
		}
		graphRefs[codeCtx.GraphRef] = id
	}

	shards, err := filepath.Glob(filepath.Join(fixture.ZoektIndex, "*.zoekt"))
	if err != nil || len(shards) == 0 {
		t.Fatalf("no Zoekt shard in %s: %v", fixture.ZoektIndex, err)
	}

	// The multi-member context: its second-demo-repo member's revision is
	// resolved the same way, never hard-coded, and its versioned-demo-repo
	// member reuses demo-v1's own graph_ref — the config validates that only
	// when the repository and the revision genuinely match, so this is also a
	// check that the two contexts agree about release/v1's commit.
	multi, ok := cfg.Contexts[demorepo.MultiContext]
	if !ok {
		t.Fatalf("context %q is missing from %s", demorepo.MultiContext, fixture.Config)
	}
	if len(multi.Members) != 2 {
		t.Fatalf("context %q names %d repositories, want the two the fixture declares", demorepo.MultiContext, len(multi.Members))
	}
	byRepository := map[string]string{}
	for _, member := range multi.Members {
		byRepository[member.Repository] = member.Revision
	}
	if rev := demorepo.Revision(t, fixture.Repo, demorepo.V1); byRepository["versioned-demo-repo"] != rev {
		t.Errorf("%s member versioned-demo-repo revision = %s, want %s; re-run testdata/prepare-fixture.sh", demorepo.MultiContext, byRepository["versioned-demo-repo"], rev)
	}
	if rev := demorepo.Revision(t, fixture.Repo2, demorepo.Repo2Main); byRepository["second-demo-repo"] != rev {
		t.Errorf("%s member second-demo-repo revision = %s, want %s; re-run testdata/prepare-fixture.sh", demorepo.MultiContext, byRepository["second-demo-repo"], rev)
	}
}
