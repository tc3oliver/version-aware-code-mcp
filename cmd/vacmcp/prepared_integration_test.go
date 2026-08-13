//go:build integration

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/store"
)

// One managed installation, built once for the whole package and shared by the
// tests that only read what a `context create` produced.
//
// It is immutable and query-only, and that is not a convention: a test using it
// may run in any order, any number of times and beside any other, so anything
// it wrote would reach a test that never asked for it. Only tests that ask this
// installation questions may share it. Every test of a command that writes —
// create, retry, remove, the crash cases, and the refusals that hold those
// commands to writing nothing — builds its own from t.TempDir() as before, and
// a refused write is a write for this purpose: what the test is there to catch
// is the day it stops being refused. preparedIsUnchanged holds the line at the
// end of the run.
//
// A create drives the real engines: it checks the revision out, rebuilds the
// repository's Zoekt shard and indexes a CBM graph, and the graph alone is some
// ten seconds of subprocess. decision-7 draws the line at what a test is about
// — a test asserting how a context is built pays for building one, and a test
// asserting what a built context reads like takes this installation instead of
// building the same shape again.
//
// The two branches carry different source on purpose, and neither is an
// ancestor of the other. main defines LegacyHandler and alphaToken, release/v2
// defines NewHandler and betaToken, and neither has the other's — so a context
// answering out of the wrong revision shows up here as the wrong symbol rather
// than as a plausible answer.
const (
	preparedLegacy = "app-v1" // pins main
	preparedModern = "app-v2" // pins release/v2
	preparedTwin   = "app-v3" // pins release/v2 as well, at the very same revision
)

// preparedInstallation is the shared data directory and the two revisions its
// contexts pin.
type preparedInstallation struct {
	data   string
	legacy string // what preparedLegacy pins
	modern string // what preparedModern and preparedTwin pin
}

var preparedOnce = sync.OnceValues(buildPreparedInstallation)

// preparedManaged returns the shared installation, building it on first use.
func preparedManaged(t *testing.T) preparedInstallation {
	t.Helper()
	requireIndexer(t)
	cbmOrSkip(t)

	prepared, err := preparedOnce()
	if err != nil {
		t.Fatalf("building the shared installation: %v", err)
	}
	return prepared
}

// buildPreparedInstallation creates the source repository, adds it and creates
// the three contexts.
//
// It takes no *testing.T because what it builds outlives the test that asked
// for it first: it can use neither that test's temporary directory nor its
// failure reporting. The graphs it leaves in CBM's own store are taken out by
// discardPrepared, which TestMain runs after the last test.
func buildPreparedInstallation() (preparedInstallation, error) {
	root, err := os.MkdirTemp("", "vacmcp-prepared-")
	if err != nil {
		return preparedInstallation{}, fmt.Errorf("temporary directory: %w", err)
	}

	source := filepath.Join(root, "source")
	if err := gitRun("init", "-q", "-b", "main", source); err != nil {
		return preparedInstallation{}, err
	}
	base, err := commitFiles(source, "one", map[string]string{"one.txt": "one\n"})
	if err != nil {
		return preparedInstallation{}, err
	}
	legacy, err := commitFiles(source, "the legacy handler", map[string]string{
		"go.mod":       "module demo\n\ngo 1.26\n",
		"processor.go": processorSource(legacyHandler),
		"alpha.go":     "package demo\n\nfunc " + alphaToken + "() {}\n",
	})
	if err != nil {
		return preparedInstallation{}, err
	}
	// From the commit before that one, so the branch that follows carries none
	// of it: a query answered out of the other revision has no way to look like
	// this one's.
	if err := gitRun("-C", source, "checkout", "-q", "-b", "release/v2", base); err != nil {
		return preparedInstallation{}, err
	}
	modern, err := commitFiles(source, "the new handler", map[string]string{
		"go.mod":       "module demo\n\ngo 1.26\n",
		"processor.go": processorSource(newHandler),
		"beta.go":      "package demo\n\nfunc " + betaToken + "() {}\n",
	})
	if err != nil {
		return preparedInstallation{}, err
	}
	if err := gitRun("-C", source, "checkout", "-q", "main"); err != nil {
		return preparedInstallation{}, err
	}

	data := filepath.Join(root, "data")
	if err := vacmcpRun("repo", "add", "demo", "--url", source, "--data-dir", data); err != nil {
		return preparedInstallation{}, err
	}
	for _, created := range []struct{ id, ref string }{
		{preparedLegacy, "main"},
		{preparedModern, "release/v2"},
		{preparedTwin, "release/v2"},
	} {
		if err := vacmcpRun("context", "create", created.id, "--repo", "demo", "--ref", created.ref, "--data-dir", data); err != nil {
			return preparedInstallation{}, err
		}
	}

	built, err := preparedFingerprint(data)
	if err != nil {
		return preparedInstallation{}, err
	}
	discardPrepared = func() error {
		// Before it is taken down, because after that there is nothing left to
		// compare: a test that shares this installation may read it and may do
		// nothing else, and the only way to hold that is to look.
		unchanged := preparedIsUnchanged(data, built)
		discardGraphsIn(data, func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "cleanup: "+format+"\n", args...)
		})
		if err := os.RemoveAll(root); err != nil {
			fmt.Fprintf(os.Stderr, "cleanup: %v\n", err)
		}
		return unchanged
	}
	return preparedInstallation{data: data, legacy: legacy, modern: modern}, nil
}

// preparedIsUnchanged reports whether the installation is still the one that
// was built, naming what moved when it is not.
//
// A test sharing this installation is one that only asks it questions, and an
// answer is not something that changes what was asked. This is what keeps that
// true as tests are added: the one that wrote is not always the one that fails,
// so a run that leaves the installation different fails the package rather than
// the next test to read it.
func preparedIsUnchanged(data, built string) error {
	after, err := preparedFingerprint(data)
	if err != nil {
		return fmt.Errorf("the shared installation: %w", err)
	}
	if after != built {
		return fmt.Errorf("a test changed the shared installation, which only tests that read it may use"+
			"\n built:\n%s\n after:\n%s", built, after)
	}
	return nil
}

// preparedFingerprint is everything about the installation a test could change
// and nothing that moves on its own: the records, the refs of the clone, the
// commit each worktree is checked out at, the search index byte for byte, and
// the graphs CBM still holds.
func preparedFingerprint(data string) (string, error) {
	s, err := store.Open(data)
	if err != nil {
		return "", fmt.Errorf("store.Open(%s): %w", data, err)
	}
	contexts, err := s.Contexts()
	if err != nil {
		return "", fmt.Errorf("Contexts(): %w", err)
	}

	var state []string
	graphs := map[string]bool{}
	for _, c := range contexts {
		state = append(state, fmt.Sprintf("context %s %s %s %s %s %s", c.ID, c.Repository, c.Revision, c.State, c.Branch, c.GraphRef))
		graphs[c.GraphRef] = false

		worktree, err := s.WorktreeDir(c.Repository, c.ID)
		if err != nil {
			return "", fmt.Errorf("WorktreeDir(%s): %w", c.ID, err)
		}
		head, err := exec.Command("git", "-C", worktree, "rev-parse", "HEAD").Output()
		if err != nil {
			return "", fmt.Errorf("git rev-parse HEAD in %s: %w", worktree, err)
		}
		state = append(state, fmt.Sprintf("worktree %s %s", c.ID, strings.TrimSpace(string(head))))

		clone, err := s.RepositoryDir(c.Repository)
		if err != nil {
			return "", fmt.Errorf("RepositoryDir(%s): %w", c.Repository, err)
		}
		refs, err := exec.Command("git", "-C", clone, "for-each-ref", "--format=%(objectname) %(refname)").Output()
		if err != nil {
			return "", fmt.Errorf("git for-each-ref in %s: %w", clone, err)
		}
		state = append(state, "refs "+c.Repository+" "+strings.Join(strings.Fields(string(refs)), " "))
	}

	// The shards themselves, so an index rebuilt behind a test that only read
	// is a difference here even when it names the same refs.
	shards, err := os.ReadDir(filepath.Join(data, "zoekt"))
	if err != nil {
		return "", fmt.Errorf("reading the index directory: %w", err)
	}
	for _, shard := range shards {
		body, err := os.ReadFile(filepath.Join(data, "zoekt", shard.Name()))
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", shard.Name(), err)
		}
		state = append(state, fmt.Sprintf("shard %s %x", shard.Name(), sha256.Sum256(body)))
	}

	// And the graphs, which are the one artifact that does not live in the
	// directory above: a deleted graph is invisible to everything else here.
	out, err := exec.Command(cbmCommand, "cli", "list_projects").Output()
	if err != nil {
		return "", fmt.Errorf("codebase-memory-mcp cli list_projects: %w", err)
	}
	var listed struct {
		Projects []struct {
			Name string `json:"name"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(out, &listed); err != nil {
		return "", fmt.Errorf("list_projects did not answer with the JSON it promises: %w", err)
	}
	for _, project := range listed.Projects {
		if _, ours := graphs[project.Name]; ours {
			graphs[project.Name] = true
		}
	}
	for ref, held := range graphs {
		state = append(state, fmt.Sprintf("graph %s %t", ref, held))
	}

	// Sorted, because neither the records nor CBM promise an order and a
	// fingerprint that changes with one would fail every run.
	slices.Sort(state)
	return strings.Join(state, "\n"), nil
}

// vacmcpRun runs one command of the CLI under test, through the same top-level
// dispatch the tests use.
func vacmcpRun(args ...string) error {
	var out bytes.Buffer
	if err := run(args, &out); err != nil {
		return fmt.Errorf("vacmcp %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func gitRun(args ...string) error {
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return nil
}

// commitFiles writes the files into repo and commits them, returning the commit
// it made.
func commitFiles(repo, message string, files map[string]string) (string, error) {
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o600); err != nil {
			return "", fmt.Errorf("writing %s: %w", name, err)
		}
	}
	if err := gitRun("-C", repo, "add", "-A"); err != nil {
		return "", err
	}
	// The caller's global git configuration decides neither the identity nor
	// whether this commit is signed or hooked.
	if err := gitRun("-C", repo,
		"-c", "user.name=vacmcp test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false",
		"commit", "--no-verify", "-q", "-m", message); err != nil {
		return "", err
	}

	out, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD in %s: %w", repo, err)
	}
	return strings.TrimSpace(string(out)), nil
}
