package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tc3oliver/version-aware-code-mcp/adapters/zoekt"
	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/managed"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/resolver"
	"github.com/tc3oliver/version-aware-code-mcp/server"
	"github.com/tc3oliver/version-aware-code-mcp/store"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
)

// The rows doctor reports, in the order doc-1 §9 lists them.
const (
	sdkCheck    = "MCP SDK"
	configCheck = "Config"
	zoektCheck  = "Zoekt"
	cbmCheck    = "CBM"
	gitCheck    = "Git repo"
)

// A check either passed, failed, or could not be carried out. The third is not
// a variant of the first: a dependency whose endpoint is unknown because the
// configuration did not load has not been shown to work.
const (
	statusOK   = "OK"
	statusFail = "FAIL"
	statusSkip = "SKIP"
)

const (
	// minCBM is the codebase-memory-mcp version decision-3 pins. An older one
	// is reported as a version mismatch rather than left to fail later inside
	// trace_calls.
	minCBM = "0.10.1"

	// sdkModule is the MCP Go SDK, looked up in this binary's build
	// information to report which version it was built against.
	sdkModule = "github.com/modelcontextprotocol/go-sdk"

	// probeName is what doctor asks Zoekt about: a repository, a branch and a
	// term nothing is called, so the answer that means "working" is no matches
	// and no error.
	probeName = "vacmcp-doctor-probe"

	// probeTimeout bounds one dependency check. doctor is a diagnosis, so a
	// dependency that never answers has to be reported as one instead of
	// hanging the command that was run to find it.
	probeTimeout = 30 * time.Second
)

// check is one reported row: what was checked, how it went, and the detail that
// makes the result actionable — an endpoint, a version, or the reason it failed.
type check struct {
	name   string
	status string
	detail string
}

func (c check) String() string {
	if c.detail == "" {
		return fmt.Sprintf("%-14s%s", c.name, c.status)
	}
	return fmt.Sprintf("%-14s%-6s%s", c.name, c.status, c.detail)
}

// doctor reports whether each thing vacmcp depends on is usable, and whether
// each configured context resolves.
//
// Every check runs, whatever the ones before it did. The command exists so a
// user can tell which of MCP, the configuration, Zoekt, CBM and git is at
// fault; one that stopped at the first failure would leave exactly that
// question unanswered — an unreachable Zoekt would hide a CBM that is broken
// too. A failure is reported as a row and again as a non-zero exit, never as a
// missing row.
//
// --config is required, as it is for validate and contexts: which configuration
// is being diagnosed is not something to guess at. --managed diagnoses a data
// directory instead, on the configuration `serve --managed` would generate from
// it, plus the two sections only a managed installation has: the repositories
// and the contexts as their records leave them.
func doctor(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("vacmcp doctor", flag.ExitOnError)
	path := fs.String("config", "", "path to the vacmcp configuration file")
	managedMode := fs.Bool("managed", false, "diagnose a managed data directory instead of a configuration file")
	dataDir := fs.String("data-dir", "", "vacmcp data directory in managed mode (default ~/.vacmcp)")
	zoektURL := fs.String("zoekt-url", defaultZoektURL, "Zoekt web server to check in managed mode")
	cbmCmd := fs.String("cbm-command", managed.CBMCommand, "codebase-memory-mcp binary to check in managed mode")
	_ = fs.Parse(args)
	switch {
	case *managedMode && *path != "":
		return errors.New("doctor: give either --config or --managed, not both")
	case !*managedMode && *path == "":
		return errors.New("doctor: --config is required")
	}

	ctx := context.Background()

	// In managed mode the configuration is the generated one, so the row below
	// names the file the server would serve and every check after it runs on
	// exactly what the server would run on.
	source := *path
	var s *store.Store
	var cfg *config.Config
	var cfgErr error
	if *managedMode {
		if s, cfgErr = store.Open(*dataDir); cfgErr == nil {
			source = filepath.Join(s.RuntimeDir(), generatedConfig)
			cfg, cfgErr = managedConfig(s, *zoektURL, *cbmCmd)
		}
	} else {
		cfg, cfgErr = config.Load(*path)
	}
	if cfgErr != nil {
		cfg = &config.Config{}
	}

	// The MCP check needs no configuration and the configuration check needs
	// nothing else, so both are answered even when everything below them is
	// unknown.
	checks := []check{checkSDK(ctx, cfg), checkConfig(source, cfg, cfgErr)}
	var repositories, contexts []check

	if cfgErr != nil {
		// Zoekt's URL, CBM's command and the repositories are all read out of
		// the configuration, so without one there is nothing to check them
		// against. Reporting them as failures would send the user to look at
		// engines that may be perfectly fine.
		const because = "not checked: the configuration did not load"
		for _, name := range []string{zoektCheck, cbmCheck, gitCheck} {
			checks = append(checks, check{name, statusSkip, because})
		}
		if *managedMode {
			repositories = []check{{"-", statusSkip, because}}
		}
		contexts = append(contexts, check{"-", statusSkip, because})
	} else {
		checks = append(checks, checkZoekt(ctx, cfg), checkCBM(ctx, cfg), checkGit(ctx, cfg))
		if *managedMode {
			repositories, contexts = checkRepositories(s), checkManagedContexts(ctx, s, cfg)
		} else {
			contexts = checkContexts(ctx, cfg)
		}
	}

	lines := make([]string, 0, len(checks)+len(repositories)+len(contexts)+4)
	for _, c := range checks {
		lines = append(lines, c.String())
	}
	if *managedMode {
		lines = append(lines, "", "Repositories")
		for _, c := range repositories {
			lines = append(lines, c.String())
		}
	}
	lines = append(lines, "", "Contexts")
	for _, c := range contexts {
		lines = append(lines, c.String())
	}
	if _, err := fmt.Fprintln(out, strings.Join(lines, "\n")); err != nil {
		return err
	}

	failed := 0
	for _, c := range slices.Concat(checks, repositories, contexts) {
		if c.status != statusOK {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("doctor: %d of %d checks did not pass", failed, len(checks)+len(repositories)+len(contexts))
	}
	return nil
}

// checkRepositories reports the repositories a data directory manages: the
// state the last command left each of them in, and when its refs were last
// fetched. It reads the records and runs no git, because a fetch is the
// management plane's to run and doctor is a diagnosis.
func checkRepositories(s *store.Store) []check {
	records, err := s.Repositories()
	if err != nil {
		return []check{{"-", statusFail, err.Error()}}
	}
	rows := make([]check, 0, len(records))
	for _, r := range records {
		status := statusOK
		if r.State != managed.RepositoryReady {
			status = statusFail
		}
		rows = append(rows, check{r.Name, status, fmt.Sprintf("%s, %s", r.State, lastSync(r.LastSyncAt))})
	}
	return rows
}

// checkManagedContexts reports every context a data directory manages, whether
// or not the query plane serves it.
//
// The READY ones are resolved exactly as a configured context is, which is the
// check that the version they name is really on this machine. The rest are
// reported with the state that is keeping them out of the served set: a context
// still being built cannot be checked yet, and one that FAILED is a failure to
// say so about. Neither is hidden — a context that is not served is the first
// thing an operator comes to this command to find.
func checkManagedContexts(ctx context.Context, s *store.Store, cfg *config.Config) []check {
	records, err := s.Contexts()
	if err != nil {
		return []check{{"-", statusFail, err.Error()}}
	}

	contexts := resolver.New(cfg)
	rows := make([]check, 0, len(records))
	for _, record := range records {
		if record.State != managed.ContextReady {
			status := statusSkip
			if record.State == managed.ContextFailed {
				status = statusFail
			}
			rows = append(rows, check{record.ID, status, fmt.Sprintf("%s %s", record.State, record.Revision)})
			continue
		}
		workspace, err := contexts.Resolve(ctx, record.ID)
		if err != nil {
			rows = append(rows, check{record.ID, statusFail, err.Error()})
			continue
		}
		// One entry per repository the context names. A managed context names
		// one, so this row reads exactly as it did before a context could name
		// several.
		scopes := make([]string, 0, len(workspace.Members))
		for _, member := range workspace.Members {
			scopes = append(scopes, fmt.Sprintf("%s %s", member.Repository, member.Revision))
		}
		rows = append(rows, check{record.ID, statusOK, fmt.Sprintf("%s %s", record.State, strings.Join(scopes, ", "))})
	}
	return rows
}

// checkSDK exercises the MCP layer itself: the server is built, the tools are
// registered on it, and a client can list them back.
//
// It runs in process over an in-memory transport, so it needs no port and no
// configuration. That is what lets this row stay answerable when everything
// else is broken, which is the whole point of asking it separately: "MCP is
// fine, the problem is elsewhere" has to be a verdict this command can reach.
func checkSDK(ctx context.Context, cfg *config.Config) check {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	srv := server.New(version)
	eng := addTools(srv, cfg)
	// The ownership serve takes, taken here too: whoever asks addTools for an
	// engine closes it. Nothing this check does reaches a provider, so today it
	// closes a CBM session that was never started. The error is dropped rather
	// than reported, unlike serve's: this row is the MCP layer's verdict, and a
	// shutdown is not part of it.
	defer func() { _ = eng.Close() }()

	clientSide, serverSide := mcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverSide, nil)
	if err != nil {
		return check{sdkCheck, statusFail, fmt.Sprintf("the MCP server does not start: %v", err)}
	}
	defer func() { _ = serverSession.Close() }()

	client := mcp.NewClient(&mcp.Implementation{Name: "vacmcp-doctor", Version: version}, nil)
	session, err := client.Connect(ctx, clientSide, nil)
	if err != nil {
		return check{sdkCheck, statusFail, fmt.Sprintf("a client cannot connect to the MCP server: %v", err)}
	}
	defer func() { _ = session.Close() }()

	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		return check{sdkCheck, statusFail, fmt.Sprintf("tools/list failed: %v", err)}
	}
	return check{sdkCheck, statusOK, fmt.Sprintf("go-sdk %s, %d tools", sdkVersion(), len(listed.Tools))}
}

// sdkVersion returns the MCP Go SDK version this binary was built against, read
// out of its build information. doc-1 §2 pins that version to a protocol
// version, so it is the first thing worth knowing about an MCP that misbehaves.
func sdkVersion() string {
	info, available := debug.ReadBuildInfo()
	if !available {
		return "(version unknown)"
	}
	for _, dep := range info.Deps {
		if dep.Path == sdkModule {
			return dep.Version
		}
	}
	return "(version unknown)"
}

// checkConfig reports the configuration as config.Load left it. The error is
// passed through as produced, so the user sees the vacerr code and the field it
// names rather than a rephrasing of them.
func checkConfig(path string, cfg *config.Config, err error) check {
	if err != nil {
		return check{configCheck, statusFail, err.Error()}
	}
	return check{
		configCheck,
		statusOK,
		fmt.Sprintf("%s: %d repositories, %d contexts", path, len(cfg.Repositories), len(cfg.Contexts)),
	}
}

// checkZoekt asks the configured Zoekt web server for something that cannot
// match, through the same adapter search_code uses.
//
// Answering a query proves more than a reachable port does: the JSON API the
// adapter needs is one zoekt-webserver only serves with -rpc, and a server
// started without it is up, healthy, and useless to this one. The query is
// doctor's own rather than a configured context's, so this row reports on the
// engine and not on what happens to be indexed in it.
func checkZoekt(ctx context.Context, cfg *config.Config) check {
	if strings.TrimSpace(cfg.Providers.Zoekt.URL) == "" {
		return check{zoektCheck, statusFail, "providers.zoekt.url is not configured"}
	}

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	probe := vacctx.CodeContext{ID: "doctor", Repository: probeName, Branch: probeName, Revision: "HEAD", GraphRef: probeName}
	if _, err := zoekt.New(cfg).Search(ctx, probe, provider.SearchQuery{Query: probeName}); err != nil {
		return check{zoektCheck, statusFail, err.Error()}
	}
	return check{zoektCheck, statusOK, cfg.Providers.Zoekt.URL}
}

// checkCBM reports whether the configured codebase-memory-mcp binary is there
// and new enough, and whether it has a resident daemon.
func checkCBM(ctx context.Context, cfg *config.Config) check {
	command := strings.TrimSpace(cfg.Providers.CBM.Command)
	if command == "" {
		return check{cbmCheck, statusFail, "providers.cbm.command is not configured"}
	}

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, command, "--version").Output()
	if err != nil {
		return check{cbmCheck, statusFail, fmt.Sprintf("cannot run %s --version: %v", command, err)}
	}

	// codebase-memory-mcp --version prints "codebase-memory-mcp <version>".
	reported := firstLine(string(out))
	fields := strings.Fields(reported)
	got := ""
	if len(fields) > 0 {
		got = fields[len(fields)-1]
	}
	if len(release(got)) == 0 {
		return check{cbmCheck, statusFail, fmt.Sprintf("cannot read a version from %q", reported)}
	}
	if !atLeast(got, minCBM) {
		return check{cbmCheck, statusFail, fmt.Sprintf("%s is older than the required %s", got, minCBM)}
	}

	detail := got
	if !residentCBM(ctx, command) {
		detail += fmt.Sprintf("; no resident daemon, so every call cold-starts one and the first can take over 10s (run `%s daemon start`)", command)
	}
	return check{cbmCheck, statusOK, detail}
}

// residentCBM reports whether a CBM daemon is already running.
//
// Without one, CBM starts a temporary daemon per `cli` call and a single
// trace_calls takes over ten seconds end to end — long enough that an operator
// reasonably concludes CBM has hung and goes looking for a fault that is not
// there.
//
// A resident daemon reports itself as "daemon: active"; anything else, up to
// and including a status that cannot be read at all, is taken as none. Erring
// that way costs nothing, because this is advisory and never a failure: a cold
// CBM answers, it is only slow.
func residentCBM(ctx context.Context, command string) bool {
	out, err := exec.CommandContext(ctx, command, "daemon", "status").CombinedOutput()
	return err == nil && strings.Contains(string(out), "daemon: active")
}

// checkGit reports whether every repository the configuration declares is a git
// repository this machine can read. It asks git rather than looking for a .git
// entry: a worktree, a submodule and a bare clone are all usable, and none of
// them looks the same on disk.
//
// One unreadable repository does not stop the others from being checked, for
// the same reason one failed check does not stop the rest.
func checkGit(ctx context.Context, cfg *config.Config) check {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	var readable, failures []string
	for _, name := range slices.Sorted(maps.Keys(cfg.Repositories)) {
		path := cfg.Repositories[name].Path
		out, err := exec.CommandContext(ctx, "git", "-C", path, "rev-parse", "--git-dir").CombinedOutput()
		if err != nil {
			reason := firstLine(string(out))
			if reason == "" {
				reason = err.Error()
			}
			failures = append(failures, fmt.Sprintf("%s at %s: %s", name, path, reason))
			continue
		}
		readable = append(readable, name)
	}

	if len(failures) > 0 {
		return check{gitCheck, statusFail, strings.Join(failures, "; ")}
	}
	return check{gitCheck, statusOK, strings.Join(readable, ", ")}
}

// checkContexts resolves every configured context, sorted by ID. That is the
// check that the version a context names exists on this machine: its repository
// is readable and its revision is a commit in it.
func checkContexts(ctx context.Context, cfg *config.Config) []check {
	contexts := resolver.New(cfg)
	checks := make([]check, 0, len(cfg.Contexts))
	for _, id := range slices.Sorted(maps.Keys(cfg.Contexts)) {
		workspace, err := contexts.Resolve(ctx, id)
		if err != nil {
			checks = append(checks, check{id, statusFail, err.Error()})
			continue
		}
		// One entry per repository the context names, so a context over one
		// repository reads exactly as it did before a context could name
		// several, and one over two says so rather than showing half of itself.
		scopes := make([]string, 0, len(workspace.Members))
		for _, member := range workspace.Members {
			scopes = append(scopes, fmt.Sprintf("%s %s %s", member.Repository, member.Branch, member.Revision))
		}
		checks = append(checks, check{id, statusOK, strings.Join(scopes, ", ")})
	}
	return checks
}

// atLeast reports whether the version got is at least want, comparing their
// dot-separated numbers. A part want has and got does not counts as zero, so
// 0.10 is below 0.10.1.
//
// ponytail: not semver. A pre-release of the required version, 0.10.1-rc1,
// compares equal to it and passes; use a semver comparison if CBM ever ships
// one of those.
func atLeast(got, want string) bool {
	g, w := release(got), release(want)
	for i, part := range w {
		number := 0
		if i < len(g) {
			number = g[i]
		}
		if number != part {
			return number > part
		}
	}
	return true
}

// release returns the numbers of a version string, stopping at the first part
// that is not one: "0.10.1" and "v0.10.1-rc1" are both [0 10 1], and a string
// carrying no version at all is empty.
func release(v string) []int {
	v, _, _ = strings.Cut(v, "-")
	v, _, _ = strings.Cut(v, "+")

	var numbers []int
	for _, field := range strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".") {
		number, err := strconv.Atoi(field)
		if err != nil {
			break
		}
		numbers = append(numbers, number)
	}
	return numbers
}

// firstLine returns the first line of s, trimmed. A tool that failed says why on
// its first line and then adds detail nobody reading a table wants.
func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(line)
}
