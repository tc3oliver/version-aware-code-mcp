// Package config loads the vacmcp YAML configuration: which repositories exist
// on this machine and which version contexts they can be searched, traced and
// read in.
//
// A context names one repository at one revision, or several — each pinned to
// its own revision — and both are loaded as the same [vacctx.Workspace], one
// with a single member and one with more. There is no third thing to keep
// correct: what the file spells two ways, everything downstream reads one way.
//
// The configuration file is a trust boundary. Parsing is strict — unknown
// fields, duplicate keys and multi-document streams are rejected rather than
// silently ignored — and every problem is reported as a [vacerr.Error] instead
// of a panic, so `vacmcp validate` and `vacmcp doctor` can tell the user exactly
// which context is wrong.
//
// The file has no LLM related settings of any kind: this server is agnostic to
// the model, the agent and the git provider.
package config

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	yaml "go.yaml.in/yaml/v4"

	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// Config is a whole configuration file, as validated: every context is a
// [vacctx.Workspace], whichever of the two ways the file wrote it.
type Config struct {
	Server       Server                      `yaml:"server"`
	Providers    Providers                   `yaml:"providers"`
	Repositories map[string]Repository       `yaml:"repositories"`
	Contexts     map[string]vacctx.Workspace `yaml:"contexts"`
}

// Server holds the MCP server settings.
type Server struct {
	Address string `yaml:"address"`
}

// Providers holds the settings of the external engines the adapters talk to.
type Providers struct {
	Zoekt Zoekt `yaml:"zoekt"`
	CBM   CBM   `yaml:"cbm"`
}

// Zoekt is the search engine's HTTP endpoint.
type Zoekt struct {
	URL string `yaml:"url"`
}

// CBM is the codebase-memory-mcp binary run as the graph engine.
type CBM struct {
	Command string `yaml:"command"`
}

// Repository is a git repository present on this machine.
type Repository struct {
	Path string `yaml:"path"`
}

// declaredContext is one entry of the contexts mapping as a file writes it.
//
// There are two spellings and one meaning. A context over a single repository
// writes that repository's fields directly under the context ID — the only
// spelling there has ever been, and still what almost every file says — and a
// context over several writes them as members instead. Both become a
// [vacctx.Workspace] in [declaredContext.workspace], so nothing downstream can
// tell which spelling was used and no second set of semantics exists for it to
// tell apart.
//
// The two forms are one struct, and the inline one is an embedded
// [vacctx.CodeContext], rather than a custom unmarshaller: that is what keeps
// yaml.WithKnownFields applying all the way down, so a misspelled field inside a
// context, or inside one of its members, is still refused rather than silently
// dropped. It is also what keeps id from being writable as a field, since the ID
// is the key the context is filed under and nothing else.
type declaredContext struct {
	vacctx.CodeContext `yaml:",inline"`

	Members []vacctx.CodeContext `yaml:"members"`
}

// workspace folds a declaration into the workspace it means, filing every member
// under id.
//
// A declaration that uses both spellings at once is refused rather than merged:
// the two would have to be put in some order, and a caller that wrote both did
// not say which repository it meant first — or, more likely, meant to delete one
// of them and did not.
func (d declaredContext) workspace(id string) (vacctx.Workspace, error) {
	inline := d.CodeContext
	inline.ID = id
	declaredInline := inline != vacctx.CodeContext{ID: id}

	switch {
	case declaredInline && d.Members != nil:
		return vacctx.Workspace{}, invalid(
			fmt.Sprintf("config: context %q declares a repository inline and a members list; a context is written one way or the other", id),
			map[string]any{"context": id},
		)
	case declaredInline:
		return vacctx.Workspace{ID: id, Members: []vacctx.CodeContext{inline}}, nil
	}

	members := make([]vacctx.CodeContext, 0, len(d.Members))
	for _, member := range d.Members {
		member.ID = id
		members = append(members, member)
	}
	return vacctx.Workspace{ID: id, Members: members}, nil
}

// Load reads, parses and validates the configuration file at path. The returned
// contexts, and every member of them, have their ID filled in from the key they
// are filed under.
//
// Every error is a *[vacerr.Error]: a context pointing at an undeclared
// repository is [vacerr.RepositoryNotFound], everything else is
// [vacerr.InvalidArgument].
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, invalid(fmt.Sprintf("config: cannot read %s: %v", path, err), map[string]any{"path": path})
	}

	// The file as written, which is not quite the configuration as used: a
	// context declares either one repository or several, and only
	// [declaredContext] knows both spellings. Everything else here is the
	// Config's own, and is carried over unchanged below.
	//
	// Strict on both axes: WithKnownFields rejects fields this version does not
	// know (including misspellings that would otherwise silently disable a
	// setting), WithUniqueKeys rejects a mapping key defined twice, which is how
	// a duplicate context ID reaches us.
	var file struct {
		Server       Server                     `yaml:"server"`
		Providers    Providers                  `yaml:"providers"`
		Repositories map[string]Repository      `yaml:"repositories"`
		Contexts     map[string]declaredContext `yaml:"contexts"`
	}
	if err := yaml.Load(data, &file, yaml.WithKnownFields(), yaml.WithUniqueKeys()); err != nil {
		return nil, invalid(fmt.Sprintf("config: %s: %v", path, err), map[string]any{"path": path})
	}

	cfg := Config{Server: file.Server, Providers: file.Providers, Repositories: file.Repositories}
	if file.Contexts != nil {
		cfg.Contexts = make(map[string]vacctx.Workspace, len(file.Contexts))
	}
	// Sorted, because a map is not: two runs over one broken file must name the
	// same context, or an operator fixing them one at a time is chasing a
	// different error each time.
	for _, id := range slices.Sorted(maps.Keys(file.Contexts)) {
		workspace, err := file.Contexts[id].workspace(id)
		if err != nil {
			return nil, err
		}
		cfg.Contexts[id] = workspace
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// validate enforces everything that can be checked without touching the outside
// world. Resolving repositories, revisions and graphs against git, Zoekt and CBM
// is the context resolver's job, not the parser's.
func (c *Config) validate() error {
	for _, name := range slices.Sorted(maps.Keys(c.Repositories)) {
		if strings.TrimSpace(name) == "" {
			return invalid("config: repositories contains an empty repository name", nil)
		}
		if strings.TrimSpace(c.Repositories[name].Path) == "" {
			return invalid(fmt.Sprintf("config: repository %q: path is required", name), map[string]any{"repository": name})
		}
	}

	if len(c.Contexts) == 0 {
		return invalid("config: contexts is required and must declare at least one context", nil)
	}

	// A graph_ref is a CBM project, and a CBM project is one indexed graph. Two
	// members sharing a graph_ref across different revisions would trace calls
	// against the other version's graph, which is exactly the cross-version
	// contamination this server exists to prevent. The index spans every member
	// of every context, because a graph does not care which context reached it:
	// two repositories inside one workspace can collide with each other exactly
	// as two separate contexts can.
	byGraphRef := map[string]vacctx.CodeContext{}

	for _, id := range slices.Sorted(maps.Keys(c.Contexts)) {
		workspace := c.Contexts[id]

		if strings.TrimSpace(id) == "" {
			return invalid("config: contexts contains an empty context ID", nil)
		}
		// A context with no repository at all is not a narrower scope, it is no
		// scope: every query it named would have nowhere to run.
		if len(workspace.Members) == 0 {
			return invalid(
				fmt.Sprintf("config: context %q declares no repository", id),
				map[string]any{"context": id},
			)
		}

		// One repository twice in one workspace is refused rather than
		// deduplicated. The two entries pin two revisions of it, and a workspace
		// that answered in both would report one repository's code twice with no
		// way to say which version each half came from; if they pin the same
		// revision, one of them is a copy-paste the operator meant to edit.
		declared := map[string]bool{}

		for _, member := range workspace.Members {
			for _, field := range []struct{ name, value string }{
				{"repository", member.Repository},
				{"branch", member.Branch},
				{"revision", member.Revision},
				{"graph_ref", member.GraphRef},
			} {
				if strings.TrimSpace(field.value) == "" {
					return invalid(
						fmt.Sprintf("config: context %q: %s is required", id, field.name),
						map[string]any{"context": id, "field": field.name},
					)
				}
			}

			if _, ok := c.Repositories[member.Repository]; !ok {
				return vacerr.New(
					vacerr.RepositoryNotFound,
					fmt.Sprintf("config: context %q references repository %q, which is not declared in repositories", id, member.Repository),
					map[string]any{"context": id, "repository": member.Repository},
				)
			}

			if declared[member.Repository] {
				return invalid(
					fmt.Sprintf("config: context %q declares repository %q more than once", id, member.Repository),
					map[string]any{"context": id, "repository": member.Repository},
				)
			}
			declared[member.Repository] = true

			if other, ok := byGraphRef[member.GraphRef]; ok && (other.Repository != member.Repository || other.Revision != member.Revision) {
				return invalid(
					fmt.Sprintf("config: contexts %q and %q share graph_ref %q but are scoped to different revisions", other.ID, id, member.GraphRef),
					map[string]any{"context": id, "conflicting_context": other.ID, "graph_ref": member.GraphRef},
				)
			}
			byGraphRef[member.GraphRef] = member
		}
	}

	return nil
}

func invalid(message string, details map[string]any) *vacerr.Error {
	return vacerr.New(vacerr.InvalidArgument, message, details)
}
