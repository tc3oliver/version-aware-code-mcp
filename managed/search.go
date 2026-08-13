package managed

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/tc3oliver/version-aware-code-mcp/store"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// indexer is the Zoekt indexing binary. It is invoked rather than imported:
// this module is an adapter to the engines a deployment runs, and the shard
// format they write is theirs, not a file format vacmcp may learn to parse.
const indexer = "zoekt-git-index"

// putSearchRef points a context's search ref at the commit it pins.
//
// The clone has no working tree, and none is needed: a ref is an entry in the
// object database, and what indexes it reads the tree out of the commit. So
// nothing here checks anything out.
//
// The ref is created under refs/heads/ because that is where Zoekt looks for
// the branches it is asked to index, which is what makes it the branch the
// query plane later filters on. Writing it again with the same revision is what
// git does anyway, so this is safe to repeat.
func putSearchRef(ctx context.Context, repoDir string, c store.Context) error {
	return runGit(ctx, "-C", repoDir, "update-ref", "--end-of-options", "refs/heads/"+c.Branch, c.Revision)
}

// dropSearchRef removes a context's search ref.
//
// Deleting a ref that is not there succeeds, and a clone that is not there is
// treated the same way, because both are the same situation: a context still
// has to be removable when the artifacts it owns are already gone.
func dropSearchRef(ctx context.Context, repoDir string, c store.Context) error {
	if _, err := os.Stat(repoDir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return runGit(ctx, "-C", repoDir, "update-ref", "-d", "--end-of-options", "refs/heads/"+c.Branch)
}

// indexRepository makes one repository's search refs and its index agree with
// what its context records say they should be.
//
// One repository is one shard, carrying every context's search ref at once —
// Zoekt indexes several branches of a repository together and picks between
// them at query time, so a shard per context would be the same source stored
// once per version for nothing.
//
// The records are the only input: whatever contexts the repository has is what
// the clone ends up having refs for and what the index ends up holding. That is
// what makes this the same call after a create and after a remove, and it is
// also what makes it repair rather than trip over a record whose ref is
// missing — a context interrupted halfway, or one written by a version that
// computed the name without creating the ref. Re-asserting a ref cannot move a
// context: the revision written into it comes from the record, which is
// immutable, and git refuses a commit the clone does not have.
//
// Reading the records and indexing what they say is not atomic on its own: two
// of these running on one repository could have the one that read the older
// records finish last and leave a shard missing a context the registry calls
// READY. What keeps them apart is that every command reaching this holds the
// repository's lock (see withRepositoryLock), so this is never entered twice for
// one repository at a time. It does not take that lock itself: its callers hold
// it already, and taking it again is the one way that lock deadlocks.
func indexRepository(ctx context.Context, s *store.Store, repository string) error {
	contexts, err := s.Contexts()
	if err != nil {
		return err
	}
	repoDir, err := s.RepositoryDir(repository)
	if err != nil {
		return err
	}
	// A repository whose clone is gone has no source to index, and therefore
	// nothing it may keep an index of: the shards go, which also leaves the
	// contexts behind it removable rather than wedged behind a rebuild that can
	// never run again.
	if _, err := os.Stat(repoDir); errors.Is(err, fs.ErrNotExist) {
		return dropShards(s.ZoektDir(), repository)
	}

	var refs []string
	for _, c := range contexts {
		// A record that has not been given a search ref yet names nothing to
		// index, and naming a ref that does not exist fails the whole run.
		//
		// A record being removed is skipped for the opposite reason: its ref is
		// one this rebuild would re-create out of the record, which is the whole
		// point everywhere else and here would put back the source a removal
		// has just taken out — the removal's own rebuild, and any create of the
		// same repository that lands between an interrupted removal and the run
		// that finishes it.
		if c.Repository != repository || c.Branch == "" || c.State == ContextRemoving {
			continue
		}
		if err := putSearchRef(ctx, repoDir, c); err != nil {
			return err
		}
		refs = append(refs, c.Branch)
	}
	if len(refs) == 0 {
		return dropShards(s.ZoektDir(), repository)
	}

	// Every argument is its own element of argv and none of it is ever a shell
	// string, for the reason runGit spells out: these paths and refs come from
	// names a user chose.
	cmd := exec.CommandContext(ctx, indexer, "-index", s.ZoektDir(), "-branches", strings.Join(refs, ","), repoDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		if message := strings.TrimSpace(string(out)); message != "" {
			return fmt.Errorf("%s: cannot index repository %q: %w: %s", indexer, repository, err, message)
		}
		return fmt.Errorf("%s: cannot index repository %q: %w", indexer, repository, err)
	}
	return nil
}

// repositoryShards returns the paths of the search index files of one
// repository.
//
// Zoekt names both the shard and the repository inside it after the directory
// it indexed, which is the clone, which is named after the repository — the
// same equality the query plane's repo: filter depends on, so this is not an
// assumption made only here. The match is anchored around the version and shard
// numbers rather than being a prefix glob: "app_v*" would also select the
// shards of a repository called app_v1.
func repositoryShards(indexDir, repository string) ([]string, error) {
	shard := regexp.MustCompile(`^` + regexp.QuoteMeta(repository) + `_v[0-9]+\.[0-9]+\.zoekt$`)
	entries, err := os.ReadDir(indexDir)
	if err != nil {
		return nil, fmt.Errorf("cannot list the search index at %s: %w", indexDir, err)
	}
	var paths []string
	for _, entry := range entries {
		if entry.IsDir() || !shard.MatchString(entry.Name()) {
			continue
		}
		paths = append(paths, filepath.Join(indexDir, entry.Name()))
	}
	return paths, nil
}

// dropShards deletes the shards of one repository, for the case a rebuild
// cannot cover: the last context is gone, and an index of no branches is not
// something Zoekt can be asked to build. Leaving the shard would leave the
// removed context's source in the served index.
func dropShards(indexDir, repository string) error {
	shards, err := repositoryShards(indexDir, repository)
	if err != nil {
		return err
	}
	for _, path := range shards {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("cannot delete the search index of repository %q: %w", repository, err)
		}
	}
	return nil
}

// verifySearchRef checks that the source a context is searched through is
// there: the ref its record names, on the commit it pins, and a shard of its
// repository in the index.
//
// Both halves are needed and neither is the other. The ref is what the index is
// built from and what the query plane filters on, so a ref that is missing or
// on another commit is a context that would be searched against the wrong
// source; the shard is whether any of it was ever indexed, or has been deleted
// since. A ref on the wrong commit is SOURCE_MISMATCH like every other
// disagreement about a revision, and everything else is the same
// SEARCH_PROVIDER_UNAVAILABLE a search that cannot be served produces.
//
// It reads git and the index directory rather than asking a running
// zoekt-webserver: a context is created and verified on the management plane,
// where no server need be running yet, and requiring one would make a context
// created before the server unable to ever become READY.
func verifySearchRef(ctx context.Context, repoDir, indexDir string, c store.Context) error {
	found, err := gitOutput(ctx, "-C", repoDir, "rev-parse", "--verify", "--end-of-options", "refs/heads/"+c.Branch)
	if err != nil {
		return searchUnavailable(c, fmt.Sprintf("its search ref %q is not in the clone of repository %q: %v", c.Branch, c.Repository, err))
	}
	if found != c.Revision {
		return vacerr.NewSourceMismatch(c.Revision, found, map[string]any{"context": c.ID, "repository": c.Repository, "branch": c.Branch})
	}

	shards, err := repositoryShards(indexDir, c.Repository)
	if err != nil {
		return err
	}
	if len(shards) == 0 {
		return searchUnavailable(c, fmt.Sprintf("repository %q has no search index in %s", c.Repository, indexDir))
	}
	return nil
}

func searchUnavailable(c store.Context, reason string) error {
	return vacerr.New(
		vacerr.SearchProviderUnavailable,
		fmt.Sprintf("context %q: %s", c.ID, reason),
		map[string]any{"context": c.ID, "repository": c.Repository},
	)
}
