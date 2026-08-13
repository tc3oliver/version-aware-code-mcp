package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/tc3oliver/version-aware-code-mcp/managed"
)

const contextUsage = `Usage:
  vacmcp context create NAME --repo REPO --ref REF [--data-dir DIR]
                                                    pin a repository ref to a new context
  vacmcp context list [--data-dir DIR]              list the managed contexts
  vacmcp context status NAME [--data-dir DIR]       report one context
  vacmcp context verify NAME [--data-dir DIR]       re-check a context without changing it
  vacmcp context retry NAME [--data-dir DIR]        rebuild a context that did not reach READY
  vacmcp context remove NAME [--data-dir DIR]       forget a context, its worktree and its graph`

// contextCommand dispatches `vacmcp context <subcommand>`.
//
// Like the repo commands these are the management plane, reachable from the CLI
// alone: decision-4 keeps them off the MCP tool surface so a coding agent cannot
// make the server create or delete the contexts it is being served.
//
// There is no update subcommand, and no flag anywhere below writes a revision
// into an existing record. A context is immutable: another revision of the same
// repository is another context ID, because every answer already given under
// this one was evidence about the revision it was created with.
func contextCommand(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("context: no subcommand given\n\n" + contextUsage)
	}
	switch subcommand := args[0]; subcommand {
	case "create":
		return contextCreate(args[1:], out)
	case "list":
		return contextList(args[1:], out)
	case "status":
		return contextStatus(args[1:], out)
	case "verify":
		return contextVerify(args[1:], out)
	case "retry":
		return contextRetry(args[1:], out)
	case "remove":
		return contextRemove(args[1:], out)
	default:
		return fmt.Errorf("unknown context subcommand %q\n\n%s", subcommand, contextUsage)
	}
}

func contextCreate(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("vacmcp context create", flag.ExitOnError)
	repository := fs.String("repo", "", "managed repository to pin a revision of")
	ref := fs.String("ref", "", "branch, tag or commit to resolve once and pin")
	dataDir := fs.String("data-dir", "", "vacmcp data directory (default ~/.vacmcp)")
	id, err := parseRepoFlags(fs, args)
	if err != nil {
		return err
	}
	switch {
	case id == "":
		return errors.New("context create: NAME is required")
	case strings.TrimSpace(*repository) == "":
		return errors.New("context create: --repo is required")
	case strings.TrimSpace(*ref) == "":
		return errors.New("context create: --ref is required")
	}

	contexts, err := managed.NewContextManager(*dataDir)
	if err != nil {
		return err
	}
	created, err := contexts.Create(context.Background(), id, *repository, *ref)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s\t%s\t%s\n", created.ID, created.State, created.Revision)
	return err
}

// contextList prints one line per managed context, ordered by ID.
func contextList(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("vacmcp context list", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "vacmcp data directory (default ~/.vacmcp)")
	_ = fs.Parse(args)

	contexts, err := managed.NewContextManager(*dataDir)
	if err != nil {
		return err
	}
	records, err := contexts.List()
	if err != nil {
		return err
	}
	for _, c := range records {
		if _, err := fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", c.ID, c.Repository, c.Revision, c.State); err != nil {
			return err
		}
	}
	return nil
}

// contextStatus reports one context, including the names it generated and where
// its worktree goes. Those are internal — nobody has to type them, and the query
// plane does not show them — but this is the management plane, where an operator
// diagnosing a context needs to know which ref and which graph are its.
func contextStatus(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("vacmcp context status", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "vacmcp data directory (default ~/.vacmcp)")
	id, err := parseRepoFlags(fs, args)
	if err != nil {
		return err
	}
	if id == "" {
		return errors.New("context status: NAME is required")
	}

	contexts, err := managed.NewContextManager(*dataDir)
	if err != nil {
		return err
	}
	status, err := contexts.Status(id)
	if err != nil {
		return err
	}

	for _, row := range [][2]string{
		{"name", status.ID},
		{"repository", status.Repository},
		{"revision", status.Revision},
		{"state", status.State},
		{"search ref", status.SearchRef},
		{"graph ref", status.GraphRef},
		{"worktree", status.Worktree},
		{"retry", retryHint(status.Context)},
		{"updated", status.UpdatedAt.Format(time.RFC3339)},
	} {
		if _, err := fmt.Fprintf(out, "%-12s%s\n", row[0], row[1]); err != nil {
			return err
		}
	}
	return nil
}

// retryHint says what to do about the state a context is in.
//
// A context is READY, being removed, or one that needs rebuilding, and the
// building states do not divide any finer than that: a record left in
// INDEXING_GRAPH by a process that was killed and one being written by a create
// that is running right now look the same on disk, and nothing an operator can
// read tells them apart. This says the one thing that is true of both, and
// `context retry` is what resolves the difference — it waits for the running
// create, if there is one, and then finds the READY it produced.
func retryHint(c managed.Context) string {
	switch c.State {
	case managed.ContextReady:
		return "not needed"
	case managed.ContextRemoving:
		// A retry refuses this one, so pointing at it would be a dead end. What
		// an interrupted removal needs is the removal again.
		return "not needed: vacmcp context remove " + c.ID
	default:
		return "needed: vacmcp context retry " + c.ID
	}
}

func contextVerify(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("vacmcp context verify", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "vacmcp data directory (default ~/.vacmcp)")
	id, err := parseRepoFlags(fs, args)
	if err != nil {
		return err
	}
	if id == "" {
		return errors.New("context verify: NAME is required")
	}

	contexts, err := managed.NewContextManager(*dataDir)
	if err != nil {
		return err
	}
	verified, err := contexts.Verify(context.Background(), id)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s\tOK\t%s\n", verified.ID, verified.Revision)
	return err
}

func contextRetry(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("vacmcp context retry", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "vacmcp data directory (default ~/.vacmcp)")
	id, err := parseRepoFlags(fs, args)
	if err != nil {
		return err
	}
	if id == "" {
		return errors.New("context retry: NAME is required")
	}

	contexts, err := managed.NewContextManager(*dataDir)
	if err != nil {
		return err
	}
	rebuilt, err := contexts.Retry(context.Background(), id)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s\t%s\t%s\n", rebuilt.ID, rebuilt.State, rebuilt.Revision)
	return err
}

func contextRemove(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("vacmcp context remove", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "vacmcp data directory (default ~/.vacmcp)")
	id, err := parseRepoFlags(fs, args)
	if err != nil {
		return err
	}
	if id == "" {
		return errors.New("context remove: NAME is required")
	}

	contexts, err := managed.NewContextManager(*dataDir)
	if err != nil {
		return err
	}
	if err := contexts.Remove(context.Background(), id); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s\tREMOVED\n", id)
	return err
}
