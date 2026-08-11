// Package config loads the vacmcp YAML configuration: which repositories exist
// on this machine and which version contexts they can be searched, traced and
// read in.
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

// Config is a whole configuration file.
type Config struct {
	Server       Server                        `yaml:"server"`
	Providers    Providers                     `yaml:"providers"`
	Repositories map[string]Repository         `yaml:"repositories"`
	Contexts     map[string]vacctx.CodeContext `yaml:"contexts"`
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

// Load reads, parses and validates the configuration file at path. The returned
// contexts have their ID filled in from the key they are filed under.
//
// Every error is a *[vacerr.Error]: a context pointing at an undeclared
// repository is [vacerr.RepositoryNotFound], everything else is
// [vacerr.InvalidArgument].
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, invalid(fmt.Sprintf("config: cannot read %s: %v", path, err), map[string]any{"path": path})
	}

	// Strict on both axes: WithKnownFields rejects fields this version does not
	// know (including misspellings that would otherwise silently disable a
	// setting), WithUniqueKeys rejects a mapping key defined twice, which is how
	// a duplicate context ID reaches us.
	var cfg Config
	if err := yaml.Load(data, &cfg, yaml.WithKnownFields(), yaml.WithUniqueKeys()); err != nil {
		return nil, invalid(fmt.Sprintf("config: %s: %v", path, err), map[string]any{"path": path})
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
	// contexts sharing a graph_ref across different revisions would trace calls
	// against the other version's graph, which is exactly the cross-version
	// contamination this server exists to prevent.
	byGraphRef := map[string]vacctx.CodeContext{}

	for _, id := range slices.Sorted(maps.Keys(c.Contexts)) {
		codeCtx := c.Contexts[id]
		codeCtx.ID = id

		if strings.TrimSpace(id) == "" {
			return invalid("config: contexts contains an empty context ID", nil)
		}
		for _, field := range []struct{ name, value string }{
			{"repository", codeCtx.Repository},
			{"branch", codeCtx.Branch},
			{"revision", codeCtx.Revision},
			{"graph_ref", codeCtx.GraphRef},
		} {
			if strings.TrimSpace(field.value) == "" {
				return invalid(
					fmt.Sprintf("config: context %q: %s is required", id, field.name),
					map[string]any{"context": id, "field": field.name},
				)
			}
		}

		if _, ok := c.Repositories[codeCtx.Repository]; !ok {
			return vacerr.New(
				vacerr.RepositoryNotFound,
				fmt.Sprintf("config: context %q references repository %q, which is not declared in repositories", id, codeCtx.Repository),
				map[string]any{"context": id, "repository": codeCtx.Repository},
			)
		}

		if other, ok := byGraphRef[codeCtx.GraphRef]; ok && (other.Repository != codeCtx.Repository || other.Revision != codeCtx.Revision) {
			return invalid(
				fmt.Sprintf("config: contexts %q and %q share graph_ref %q but are scoped to different revisions", other.ID, id, codeCtx.GraphRef),
				map[string]any{"context": id, "conflicting_context": other.ID, "graph_ref": codeCtx.GraphRef},
			)
		}
		byGraphRef[codeCtx.GraphRef] = codeCtx

		c.Contexts[id] = codeCtx
	}

	return nil
}

func invalid(message string, details map[string]any) *vacerr.Error {
	return vacerr.New(vacerr.InvalidArgument, message, details)
}
