// Package vacctx defines CodeContext, the version scope every vacmcp result is
// bound to, and Workspace, the set of them one context ID names.
//
// A CodeContext is the single knob an MCP client turns: it decides the branch
// searched, the graph queried and the revision read, so the client never has to
// know about Zoekt branches, CBM project names or checkout paths.
//
// The package is deliberately tiny and dependency free: config produces
// CodeContext values, providers and tools consume them, and none of those has to
// import the others. It is not called "context" because every provider method
// also takes a standard library [context.Context], and one of the two would have
// to be import-aliased at every call site.
package vacctx

// CodeContext is one named version scope. The yaml tags are the configuration
// file field names; ID is not one of them, it is the key the context is filed
// under in the config's contexts mapping.
//
// GraphRef is the CBM project name backing this context. It is internal: it
// travels with the context so the graph adapter can scope its queries, but it is
// never part of a tool's wire output, which carries only ID, Repository, Branch
// and Revision.
type CodeContext struct {
	ID         string `yaml:"-"`
	Repository string `yaml:"repository"`
	Branch     string `yaml:"branch"`
	Revision   string `yaml:"revision"`
	GraphRef   string `yaml:"graph_ref"`
}

// Workspace is what one context ID names: the repositories a question asked in
// that context is answered over, each pinned to its own revision.
//
// A context naming one repository is a workspace with exactly one member. That
// is not a special case dressed up as the general one, it is the whole reason
// this type exists as a list: one member and several members are the same value
// at different lengths, so nothing above here has a single-repository code path
// to keep correct beside a multi-repository one. Two such paths would drift, and
// the one nobody exercises is the one that answers from the wrong version.
//
// ID names the set and the members carry the scope. Every member is filled in
// with the workspace's own ID, because a member is not separately addressable:
// a request names a context and the results and citations are scoped by that
// name, whichever repository inside it they came from.
//
// The yaml tag is the multi-repository spelling of a context in the
// configuration file; the single-repository spelling writes the member's own
// fields directly under the context ID, and
// [github.com/tc3oliver/version-aware-code-mcp/config] folds both into this.
type Workspace struct {
	ID      string        `yaml:"-"`
	Members []CodeContext `yaml:"members"`
}
