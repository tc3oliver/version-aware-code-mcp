// Package vacctx defines CodeContext, the version scope every vacmcp result is
// bound to.
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
