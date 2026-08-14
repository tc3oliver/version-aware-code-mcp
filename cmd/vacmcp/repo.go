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

const repoUsage = `Usage:
  vacmcp repo add NAME --url URL [--data-dir DIR]   clone a repository into the data directory
  vacmcp repo list [--data-dir DIR]                 list the managed repositories
  vacmcp repo status NAME [--data-dir DIR]          report one repository
  vacmcp repo sync NAME|--all [--data-dir DIR]      fetch remote refs
  vacmcp repo remove NAME [--data-dir DIR]          forget a repository and delete its clone`

// repoCommand dispatches `vacmcp repo <subcommand>`.
//
// Every subcommand below is flags in and rows out: the clone, the fetch, the
// locks and the records are the managed package's, and decision-4 keeps all of
// them off the MCP tool surface so a coding agent cannot make the server clone
// or delete anything.
func repoCommand(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("repo: no subcommand given\n\n" + repoUsage)
	}
	switch subcommand := args[0]; subcommand {
	case "add":
		return repoAdd(args[1:], out)
	case "list":
		return repoList(args[1:], out)
	case "status":
		return repoStatus(args[1:], out)
	case "sync":
		return repoSync(args[1:], out)
	case "remove":
		return repoRemove(args[1:], out)
	default:
		return fmt.Errorf("unknown repo subcommand %q\n\n%s", subcommand, repoUsage)
	}
}

func repoAdd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("vacmcp repo add", flag.ExitOnError)
	url := fs.String("url", "", "git remote URL to clone")
	dataDir := fs.String("data-dir", "", "vacmcp data directory (default ~/.vacmcp)")
	name, err := parseRepoFlags(fs, args)
	if err != nil {
		return err
	}
	if name == "" {
		return errors.New("repo add: NAME is required")
	}
	if strings.TrimSpace(*url) == "" {
		return errors.New("repo add: --url is required")
	}

	repositories, err := managed.NewRepositoryManager(*dataDir)
	if err != nil {
		return err
	}
	added, err := repositories.Add(context.Background(), name, *url)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s\t%s\t%s\n", added.Name, added.State, added.Path)
	return err
}

// repoList prints one line per managed repository, ordered by name.
func repoList(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("vacmcp repo list", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "vacmcp data directory (default ~/.vacmcp)")
	_ = fs.Parse(args)

	repositories, err := managed.NewRepositoryManager(*dataDir)
	if err != nil {
		return err
	}
	records, err := repositories.List()
	if err != nil {
		return err
	}
	for _, r := range records {
		if _, err := fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", r.Name, r.State, r.URL, lastSync(r.LastSyncAt)); err != nil {
			return err
		}
	}
	return nil
}

// repoStatus reports one repository, including where its clone is and which
// contexts depend on it — the two things a record does not spell out and that
// decide what may be done to it next.
func repoStatus(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("vacmcp repo status", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "vacmcp data directory (default ~/.vacmcp)")
	name, err := parseRepoFlags(fs, args)
	if err != nil {
		return err
	}
	if name == "" {
		return errors.New("repo status: NAME is required")
	}

	repositories, err := managed.NewRepositoryManager(*dataDir)
	if err != nil {
		return err
	}
	status, err := repositories.Status(name)
	if err != nil {
		return err
	}

	depending := "none"
	if len(status.Contexts) > 0 {
		depending = strings.Join(status.Contexts, ", ")
	}
	for _, row := range [][2]string{
		{"name", status.Name},
		{"url", status.URL},
		{"state", status.State},
		{"path", status.Path},
		{"last sync", lastSync(status.LastSyncAt)},
		{"contexts", depending},
	} {
		if _, err := fmt.Fprintf(out, "%-12s%s\n", row[0], row[1]); err != nil {
			return err
		}
	}
	return nil
}

// repoSync fetches remote refs for one repository or, with --all, for every
// managed one. The repositories that were synced are reported whether or not
// another one failed, so a run that could not reach one remote still says what
// it did with the rest.
func repoSync(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("vacmcp repo sync", flag.ExitOnError)
	all := fs.Bool("all", false, "sync every managed repository")
	dataDir := fs.String("data-dir", "", "vacmcp data directory (default ~/.vacmcp)")
	name, err := parseRepoFlags(fs, args)
	if err != nil {
		return err
	}
	switch {
	case name == "" && !*all:
		return errors.New("repo sync: give either NAME or --all")
	case name != "" && *all:
		return errors.New("repo sync: --all takes no repository name")
	}

	repositories, err := managed.NewRepositoryManager(*dataDir)
	if err != nil {
		return err
	}
	var names []string
	if !*all {
		names = []string{name}
	}

	synced, syncErr := repositories.Sync(context.Background(), names)
	for _, r := range synced {
		if _, err := fmt.Fprintf(out, "%s\t%s\t%s\n", r.Name, r.State, lastSync(r.LastSyncAt)); err != nil {
			return err
		}
	}
	return syncErr
}

func repoRemove(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("vacmcp repo remove", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "vacmcp data directory (default ~/.vacmcp)")
	name, err := parseRepoFlags(fs, args)
	if err != nil {
		return err
	}
	if name == "" {
		return errors.New("repo remove: NAME is required")
	}

	repositories, err := managed.NewRepositoryManager(*dataDir)
	if err != nil {
		return err
	}
	if err := repositories.Remove(name); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s\tREMOVED\n", name)
	return err
}

// parseRepoFlags parses the flags of a repo subcommand that takes a repository
// name before them.
//
// The flag package stops at the first non-flag argument, so `repo status NAME
// --data-dir DIR` would leave the flags unparsed. The name is therefore taken
// off the front first. Anything left after the flags is an error rather than
// ignored: a second name is far more likely a typo than something to drop.
func parseRepoFlags(fs *flag.FlagSet, args []string) (string, error) {
	name := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if rest := fs.Args(); len(rest) > 0 {
		return "", fmt.Errorf("%s: unexpected argument %q", fs.Name(), rest[0])
	}
	return name, nil
}

// lastSync renders when a repository's refs were last fetched. A repository that
// has not been synced since it was added says so, rather than reporting the zero
// time as if it were a date.
func lastSync(at time.Time) string {
	if at.IsZero() {
		return "never synced"
	}
	return at.Format(time.RFC3339)
}
