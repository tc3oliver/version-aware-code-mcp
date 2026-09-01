package managed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/tc3oliver/version-aware-code-mcp/store"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// The artifacts a context owns: a checkout of the revision it pins, and the
// graph built from that checkout.
//
// Both are created here and nowhere else, so `context create` is the whole of
// what puts them on disk and `context remove` is the whole of what takes them
// off it. A user never runs `git worktree add` or `codebase-memory-mcp cli
// index_repository` themselves.
//
// codebase-memory-mcp is reached twice over, through two different code paths
// that must not be confused. adapters/cbm holds one long-lived MCP session and
// asks the query tools through it, because a trace pays for a process start
// otherwise. Building and deleting a graph is not a query: it happens once per
// context, at management-plane speed, and is a one-shot `cli` subprocess here.
//
// CBM 0.10.1 indexes different projects concurrently without interfering with
// them — measured, not assumed: two, four and six `cli index_repository` runs
// launched together, with and without a warm daemon, all exited 0 and left
// graphs that query independently. So there is deliberately no semaphore around
// the call below. If a later CBM ever needs one, this file is the only place it
// goes, and it wraps runIndex alone: the per-repository concurrency of git,
// Zoekt and the context lifecycle is a separate question that must not be
// answered with a global lock.

// CBMCommand is the graph engine, run as a subprocess. It is the binary name
// config/example.yaml names, resolved on PATH like every other tool the
// management plane shells out to.
const CBMCommand = "codebase-memory-mcp"

// prepareSource checks out the revision a context pins.
//
// The record is written before this runs, so a failure anywhere below leaves a
// context that is still managed and still removable rather than a checkout
// nothing knows about.
func prepareSource(ctx context.Context, repoDir, worktree, id string, m store.ContextMember) error {
	// A worktree of the repository's clone, not a clone of its own: every
	// context of one repository reads the same object database, so a second
	// version of it costs a checkout instead of a second copy of the history.
	// --detach because the revision is a commit and nothing here may create or
	// move a branch in the clone.
	if err := runGit(ctx, "-C", repoDir, "worktree", "add", "--detach", worktree, m.Revision); err != nil {
		return fmt.Errorf("context create: cannot check revision %s of repository %q out at %s: %w", m.Revision, m.Repository, worktree, err)
	}
	// Before anything is indexed, never after: the search index and the graph
	// are both built from whatever is in the worktree, so a checkout that is
	// not the pinned revision would answer about another version for the rest of
	// the context's life. The lifecycle asks again once the indexing is done,
	// which is what makes the pair of checks say the source did not move
	// underneath it.
	return verifySource(ctx, worktree, id, m)
}

// discardSource deletes the graph and the checkout of one context.
//
// The graph goes first. It lives in codebase-memory-mcp's own store, outside
// this data directory, and it is the one artifact that would otherwise survive
// the record that names it; a worktree left behind is a directory in the data
// directory, which the next removal attempt can still take.
func discardSource(ctx context.Context, repoDir, worktree, id string, m store.ContextMember) error {
	if err := deleteGraph(ctx, id, m); err != nil {
		return err
	}
	if err := os.RemoveAll(worktree); err != nil {
		return fmt.Errorf("context remove: cannot delete the worktree of %q at %s: %w", id, worktree, err)
	}
	// Removing the directory leaves the clone still administering a worktree
	// that is not there, and git refuses to add another one at that path until
	// it is pruned — which is what a user re-creating a context under the same
	// name would hit. A clone that is itself gone has nothing left to prune, and
	// must not be what makes a context impossible to remove.
	if _, err := os.Stat(repoDir); err != nil {
		return nil
	}
	return runGit(ctx, "-C", repoDir, "worktree", "prune")
}

// verifySource checks that the checkout is the revision the context pins.
//
// It fails closed: a worktree that is on another commit is reported as
// SOURCE_MISMATCH and nothing carries on with it, because a context whose
// source is not what its record says is exactly the wrong-version answer this
// server exists to prevent.
func verifySource(ctx context.Context, worktree, id string, m store.ContextMember) error {
	head, err := gitOutput(ctx, "-C", worktree, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("context %q: cannot read the HEAD of its worktree of repository %q at %s: %w", id, m.Repository, worktree, err)
	}
	if head != m.Revision {
		return vacerr.NewSourceMismatch(m.Revision, head, map[string]any{"context": id, "repository": m.Repository})
	}
	return nil
}

// verifyGraph checks that codebase-memory-mcp holds the graph the context
// names. It asks CBM what it has rather than trusting that an index that once
// succeeded is still there: a graph can be deleted out from under a context by
// anything else driving the same CBM store.
func verifyGraph(ctx context.Context, id string, m store.ContextMember) error {
	cmd := exec.CommandContext(ctx, CBMCommand, "cli", "list_projects")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		return graphUnavailable(id, m, fmt.Sprintf("cannot list the graphs it holds: %v: %s", err, lastLine(stderr.Bytes())))
	}

	var body struct {
		Projects []struct {
			Name string `json:"name"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(out, &body); err != nil {
		return graphUnavailable(id, m, fmt.Sprintf("list_projects did not answer with the JSON it promises: %v", err))
	}
	for _, project := range body.Projects {
		if project.Name == m.GraphRef {
			return nil
		}
	}
	return graphUnavailable(id, m, "it holds no graph of that name")
}

// indexGraph builds the context's graph out of its checkout.
//
// CBM reports what it did in a JSON payload on standard output, and the exit
// status alone is not the whole answer: a run that indexed nothing would exit 0
// too. The status field is therefore read, so a context only carries on with a
// graph CBM said it indexed.
func indexGraph(ctx context.Context, worktree, id string, m store.ContextMember) error {
	cmd := exec.CommandContext(ctx, CBMCommand, "cli", "index_repository", "--repo-path", worktree, "--name", m.GraphRef)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		return graphUnavailable(id, m, fmt.Sprintf("cannot index %s: %v: %s", worktree, err, lastLine(stderr.Bytes())))
	}

	var body struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(out, &body); err != nil {
		return graphUnavailable(id, m, fmt.Sprintf("index_repository did not answer with the JSON it promises: %v", err))
	}
	if body.Status != "indexed" {
		return graphUnavailable(id, m, fmt.Sprintf("index_repository reported %q rather than an indexed graph", body.Status))
	}
	return nil
}

// deleteGraph removes the context's graph from CBM's store.
//
// A graph CBM does not have is a graph that is gone, so `not_found` is success:
// a context whose indexing never finished, or whose graph was already deleted,
// still has to be removable. A context that never got as far as a generated
// name has no graph to delete at all.
func deleteGraph(ctx context.Context, id string, m store.ContextMember) error {
	if m.GraphRef == "" {
		return nil
	}

	cmd := exec.CommandContext(ctx, CBMCommand, "cli", "delete_project", "--project", m.GraphRef)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		reason := lastLine(stderr.Bytes())
		if strings.Contains(reason, "not_found") {
			return nil
		}
		return graphUnavailable(id, m, fmt.Sprintf("cannot delete the graph: %v: %s", err, reason))
	}
	return nil
}

// lastLine is the last thing CBM said. Its progress logging shares standard
// error with the JSON payload it reports a failure in, and the payload is
// written last.
func lastLine(out []byte) string {
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return "no output"
	}
	lines := strings.Split(trimmed, "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func graphUnavailable(id string, m store.ContextMember, reason string) error {
	return vacerr.New(
		vacerr.GraphProviderUnavailable,
		fmt.Sprintf("context %q: repository %q: codebase-memory-mcp: %s", id, m.Repository, reason),
		map[string]any{"context": id, "repository": m.Repository},
	)
}
