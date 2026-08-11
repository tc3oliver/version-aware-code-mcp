// Package cbm answers structural questions about code by driving
// codebase-memory-mcp (>=0.10.1) as an external graph engine.
//
// CBM is not forked and not linked: it is run as a one-shot subprocess,
// `codebase-memory-mcp cli <tool> --format json`, which does the work of one
// tool call and exits. That mode needs no MCP server, no JSON-RPC wire and no
// daemon of our own, so os/exec and encoding/json are the whole dependency
// list. On success CBM writes the tool's JSON to standard output; its progress
// logging, its daemon hint and its error payloads all go to standard error,
// with a non-zero exit status on failure.
//
// The project every query runs against is the context's GraphRef and nothing
// else. A CBM project is one indexed graph, so two versions of a repository are
// two projects; an adapter that let a query reach another project would answer
// from the wrong version, which is the failure this server exists to prevent.
//
// Resolving the symbol is this adapter's job, not the caller's. doc-1 §13 fixes
// the flow: search_graph first, and only a single match is traced. Several
// matches are reported as SYMBOL_AMBIGUOUS with the candidates, because picking
// one of them would return the call graph of a symbol nobody asked about.
package cbm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// maxNodes bounds how many rows a single CBM query may return, so one trace of
// a hot symbol cannot pull an entire graph into memory.
//
// ponytail: no pagination. CBM offers a cursor; a graph wider than this comes
// back truncated rather than in pages. Follow the cursor if a real codebase
// hits the ceiling.
const maxNodes = 1000

// Provider is the CBM implementation of [provider.GraphProvider].
type Provider struct {
	command string
}

// New returns a Provider running the codebase-memory-mcp binary named in cfg.
func New(cfg *config.Config) *Provider {
	return &Provider{command: cfg.Providers.CBM.Command}
}

// TraceCalls returns the call graph around req.Symbol inside the CBM project
// named by codeCtx.GraphRef.
//
// Every failure is a *[vacerr.Error]. A symbol no node matches is
// [vacerr.SymbolNotFound], a symbol several nodes match is
// [vacerr.SymbolAmbiguous] carrying the candidates, and anything wrong with CBM
// itself — binary absent, process failed, graph not indexed, output not the
// JSON it promises — is [vacerr.GraphProviderUnavailable]. Only the graph
// degrades: search_code does not come through here.
func (p *Provider) TraceCalls(ctx context.Context, codeCtx vacctx.CodeContext, req provider.TraceRequest) (*provider.CallGraph, error) {
	direction, err := validate(codeCtx, req)
	if err != nil {
		return nil, err
	}

	root, err := p.resolve(ctx, codeCtx, req.Symbol)
	if err != nil {
		return nil, err
	}

	hops, err := p.trace(ctx, codeCtx, root, direction, req.Depth)
	if err != nil {
		return nil, err
	}
	if len(hops) == 0 {
		return &provider.CallGraph{Symbol: root.Name}, nil
	}

	// The root is where the traversal started, so it sits at hop 0 and the
	// layering below reads outwards from it.
	//
	// ponytail: a symbol that reaches itself is flattened onto hop 0, so its
	// recursive edge is not reported. Special-case it if tracing recursion
	// turns out to matter.
	hops[root.Name] = 0

	nodes, err := p.locate(ctx, codeCtx, hops)
	if err != nil {
		return nil, err
	}
	return &provider.CallGraph{Symbol: root.Name, Edges: edges(hops, nodes, req.Direction)}, nil
}

// validate checks everything that does not need CBM, and returns the direction
// name CBM uses. The symbol and the request fields cross a trust boundary, so a
// bad one is answered here rather than passed to a subprocess.
func validate(codeCtx vacctx.CodeContext, req provider.TraceRequest) (string, error) {
	if strings.TrimSpace(codeCtx.GraphRef) == "" {
		return "", invalid(
			fmt.Sprintf("trace_calls: context %q has no graph_ref, so there is no graph to trace in", codeCtx.ID),
			map[string]any{"context": codeCtx.ID},
		)
	}
	if strings.TrimSpace(req.Symbol) == "" {
		return "", invalid("trace_calls: symbol is required", map[string]any{"context": codeCtx.ID})
	}
	if req.Depth < 1 {
		return "", invalid(
			fmt.Sprintf("trace_calls: depth %d is not a number of levels to walk", req.Depth),
			map[string]any{"context": codeCtx.ID, "symbol": req.Symbol, "depth": req.Depth},
		)
	}
	switch req.Direction {
	case provider.Callers:
		return "inbound", nil
	case provider.Callees:
		return "outbound", nil
	default:
		return "", invalid(
			fmt.Sprintf("trace_calls: direction %q is neither %q nor %q", req.Direction, provider.Callers, provider.Callees),
			map[string]any{"context": codeCtx.ID, "symbol": req.Symbol, "direction": string(req.Direction)},
		)
	}
}

// node is one node of a CBM graph, flattened out of the grouped rows CBM
// returns. QualifiedName is what trace_path has to be given: a plain name can
// be ambiguous to CBM even when it is not ambiguous to us.
type node struct {
	Name          string
	QualifiedName string
	Label         string
	Path          string
	Line          int
	Connected     []string
}

// resolve turns the symbol the caller asked about into the one graph node it
// names. This is doc-1 §13 step 1: CBM's own symbol matching is left to CBM,
// and the count of what it finds decides the answer.
func (p *Provider) resolve(ctx context.Context, codeCtx vacctx.CodeContext, symbol string) (node, error) {
	// The symbol comes from a tool caller and CBM reads name_pattern as a
	// regular expression, so it is quoted before being anchored: a caller
	// cannot turn its own symbol into a pattern matching half the graph. The
	// match stays case-insensitive, which is CBM's behaviour and is exactly the
	// imprecision doc-1 wants absorbed here.
	candidates, err := p.search(ctx, codeCtx, "^"+regexp.QuoteMeta(symbol)+"$", false)
	if err != nil {
		return node{}, err
	}

	switch len(candidates) {
	case 0:
		return node{}, vacerr.New(
			vacerr.SymbolNotFound,
			fmt.Sprintf("trace_calls: context %q has no symbol %q in graph %q", codeCtx.ID, symbol, codeCtx.GraphRef),
			map[string]any{"context": codeCtx.ID, "symbol": symbol, "graph_ref": codeCtx.GraphRef},
		)
	case 1:
		return candidates[0], nil
	default:
		return node{}, ambiguous(codeCtx, symbol, candidates)
	}
}

// ambiguous reports several matches as a structured result. The candidates
// travel with the error so the caller can name one of them exactly; choosing
// for them would answer about a symbol they did not ask about, and doing it
// silently is the same class of failure as answering from the wrong version.
func ambiguous(codeCtx vacctx.CodeContext, symbol string, candidates []node) error {
	listed := make([]map[string]any, 0, len(candidates))
	for _, c := range candidates {
		listed = append(listed, map[string]any{
			"qualified_name": c.QualifiedName,
			"name":           c.Name,
			"label":          c.Label,
			"path":           c.Path,
			"line":           c.Line,
		})
	}
	return vacerr.New(
		vacerr.SymbolAmbiguous,
		fmt.Sprintf("trace_calls: context %q has %d symbols matching %q; name one of the candidates", codeCtx.ID, len(candidates), symbol),
		map[string]any{"context": codeCtx.ID, "symbol": symbol, "graph_ref": codeCtx.GraphRef, "candidates": listed},
	)
}

// trace walks the graph from root and returns every node it reaches, mapped to
// the number of hops it took. This is doc-1 §13 step 2, reached only after the
// symbol resolved to exactly one node.
func (p *Provider) trace(ctx context.Context, codeCtx vacctx.CodeContext, root node, direction string, depth int) (map[string]int, error) {
	out, err := p.run(ctx, codeCtx, "trace_path",
		"--function-name", root.QualifiedName,
		"--direction", direction,
		"--depth", strconv.Itoa(depth),
		"--limit", strconv.Itoa(maxNodes),
	)
	if err != nil {
		return nil, err
	}

	var body struct {
		Status  string `json:"status"`
		Message string `json:"message"`
		Callees rows   `json:"callees"`
		Callers rows   `json:"callers"`
	}
	if err := decode(out, &body, codeCtx, "trace_path"); err != nil {
		return nil, err
	}
	// Defensive: the qualified name resolve() passed in should be unique, so
	// CBM should never have to choose. If it says it does, the request is
	// ambiguous and the adapter reports that rather than an empty graph.
	if body.Status == "ambiguous" {
		return nil, vacerr.New(
			vacerr.SymbolAmbiguous,
			fmt.Sprintf("trace_calls: context %q: CBM cannot resolve %q to one function: %s", codeCtx.ID, root.QualifiedName, body.Message),
			map[string]any{"context": codeCtx.ID, "symbol": root.Name, "graph_ref": codeCtx.GraphRef},
		)
	}

	reached := body.Callees
	if direction == "inbound" {
		reached = body.Callers
	}
	hops := map[string]int{}
	for _, group := range reached.Groups {
		for _, row := range group.Rows {
			name, ok := text(reached.Cols, row, "name")
			if !ok {
				continue
			}
			hop, _ := number(reached.Cols, row, "hop")
			// A node found again on a longer walk is still first reached at
			// the shorter distance, which is the hop the layering needs.
			if seen, dup := hops[name]; !dup || hop < seen {
				hops[name] = hop
			}
		}
	}
	return hops, nil
}

// locate fetches the file, line and neighbours of every node the trace reached,
// in one query. trace_path reports which nodes are reachable and how far away
// they are, but neither where they live nor which of them calls which, and a
// call graph whose edges cannot be cited is not a result this server returns.
func (p *Provider) locate(ctx context.Context, codeCtx vacctx.CodeContext, hops map[string]int) (map[string]node, error) {
	quoted := make([]string, 0, len(hops))
	for name := range hops {
		quoted = append(quoted, regexp.QuoteMeta(name))
	}
	slices.Sort(quoted)

	found, err := p.search(ctx, codeCtx, "^("+strings.Join(quoted, "|")+")$", true)
	if err != nil {
		return nil, err
	}
	nodes := map[string]node{}
	for _, n := range found {
		// Only nodes the trace reached matter, and a name CBM matched twice is
		// kept once: the first row wins, deterministically, because CBM groups
		// its rows by qualified name.
		if _, wanted := hops[n.Name]; !wanted {
			continue
		}
		if _, dup := nodes[n.Name]; !dup {
			nodes[n.Name] = n
		}
	}
	return nodes, nil
}

// edges rebuilds the call relations from the traversal.
//
// CBM's connected list is undirected, but the hop distances are not: on an
// outbound walk a node at hop h+1 was reached through a node at hop h, so a
// connection that crosses one hop level is a call in the direction of the walk.
// Only those pairs become edges, so nothing is emitted that CBM did not assert.
//
// ponytail: neighbours are matched by plain name. Two reached nodes sharing a
// name in different packages would be conflated; match on qualified names if
// CBM ever reports them in the connected list.
func edges(hops map[string]int, nodes map[string]node, direction provider.Direction) []provider.CallEdge {
	var built []provider.CallEdge
	for name, hop := range hops {
		from, ok := nodes[name]
		if !ok {
			continue
		}
		for _, neighbour := range from.Connected {
			to, located := nodes[neighbour]
			// A neighbour the trace did not reach is outside the requested
			// depth, and one that could not be located has nowhere to cite:
			// neither may become an edge with a hole in it.
			if !located || hops[neighbour] != hop+1 {
				continue
			}
			// The call is written inside whichever of the two is the caller,
			// so that is the node the edge is cited at.
			caller, callee := from, to
			if direction == provider.Callers {
				caller, callee = callee, caller
			}
			built = append(built, provider.CallEdge{
				Caller: caller.Name,
				Callee: callee.Name,
				Path:   caller.Path,
				Line:   caller.Line,
			})
		}
	}
	slices.SortFunc(built, func(a, b provider.CallEdge) int {
		if c := strings.Compare(a.Caller, b.Caller); c != 0 {
			return c
		}
		return strings.Compare(a.Callee, b.Callee)
	})
	return built
}

// rows is CBM's tabular payload: rows grouped by qualified-name prefix, each
// row a slice of values positioned by Cols. Reading a value by its column name
// rather than by index keeps the adapter working when CBM adds a column.
type rows struct {
	Cols   []string `json:"cols"`
	Groups []struct {
		QNPrefix string              `json:"qn_prefix"`
		File     string              `json:"file"`
		Rows     [][]json.RawMessage `json:"rows"`
	} `json:"groups"`
}

// search runs one search_graph query and flattens its rows into nodes.
func (p *Provider) search(ctx context.Context, codeCtx vacctx.CodeContext, pattern string, connected bool) ([]node, error) {
	args := []string{"--name-pattern", pattern, "--limit", strconv.Itoa(maxNodes)}
	if connected {
		args = append(args, "--include-connected", "true")
	}
	out, err := p.run(ctx, codeCtx, "search_graph", args...)
	if err != nil {
		return nil, err
	}

	var body rows
	if err := decode(out, &body, codeCtx, "search_graph"); err != nil {
		return nil, err
	}

	var found []node
	for _, group := range body.Groups {
		for _, row := range group.Rows {
			name, ok := text(body.Cols, row, "name")
			if !ok {
				continue
			}
			label, _ := text(body.Cols, row, "label")
			start, _ := lineRange(body.Cols, row)
			n := node{
				Name:          name,
				QualifiedName: strings.TrimPrefix(group.QNPrefix+"."+name, "."),
				Label:         label,
				Path:          group.File,
				Line:          start,
			}
			if raw, at := column(body.Cols, row, "connected"); at {
				_ = json.Unmarshal(raw, &n.Connected)
			}
			found = append(found, n)
		}
	}
	return found, nil
}

// run executes one CBM tool against the context's graph and returns its
// standard output. The project is codeCtx.GraphRef on every call, which is the
// only place it is set, so no query can reach another version's graph.
func (p *Provider) run(ctx context.Context, codeCtx vacctx.CodeContext, tool string, args ...string) ([]byte, error) {
	full := append([]string{"cli", tool, "--project", codeCtx.GraphRef, "--format", "json"}, args...)
	out, err := exec.CommandContext(ctx, p.command, full...).Output()
	if err == nil {
		return out, nil
	}

	// CBM reports a failure by exiting non-zero and writing a JSON payload to
	// standard error, mixed in with its progress logging.
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		reason := failure(exit.Stderr)
		// A name that resolved to a node CBM will not trace as a function —
		// a variable, a section of a document — is a symbol problem, not an
		// unreachable engine, and saying so keeps the operator away from
		// debugging a CBM that is working.
		if strings.Contains(reason, "function not found") {
			return nil, vacerr.New(
				vacerr.SymbolNotFound,
				fmt.Sprintf("trace_calls: context %q: graph %q has no function to trace for this symbol: %s", codeCtx.ID, codeCtx.GraphRef, reason),
				map[string]any{"context": codeCtx.ID, "graph_ref": codeCtx.GraphRef},
			)
		}
		return nil, unavailable(codeCtx, tool, fmt.Sprintf("%v: %s", err, reason))
	}
	return nil, unavailable(codeCtx, tool, err.Error())
}

// failure pulls the reason out of what CBM wrote to standard error: the JSON
// payload if it is there, otherwise the last thing it said. CBM's logging
// shares the stream, so the payload is looked for line by line rather than
// assumed to be the whole of it.
func failure(stderr []byte) string {
	lines := strings.Split(strings.TrimSpace(string(stderr)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		var body struct {
			Error string `json:"error"`
		}
		if json.Unmarshal([]byte(line), &body) == nil && body.Error != "" {
			return body.Error
		}
	}
	if len(lines) > 0 {
		return strings.TrimSpace(lines[len(lines)-1])
	}
	return "no output"
}

// decode parses a successful CBM response. Output that is not the JSON CBM
// promises means the engine is not answering as its contract says, which is a
// provider that cannot be used rather than an answer to guess at.
func decode(out []byte, into any, codeCtx vacctx.CodeContext, tool string) error {
	if err := json.Unmarshal(out, into); err != nil {
		return unavailable(codeCtx, tool, fmt.Sprintf("response is not JSON: %v", err))
	}
	return nil
}

// column returns the raw value of the named column of row.
func column(cols []string, row []json.RawMessage, name string) (json.RawMessage, bool) {
	at := slices.Index(cols, name)
	if at < 0 || at >= len(row) {
		return nil, false
	}
	return row[at], true
}

func text(cols []string, row []json.RawMessage, name string) (string, bool) {
	raw, ok := column(cols, row, name)
	if !ok {
		return "", false
	}
	var value string
	if json.Unmarshal(raw, &value) != nil || value == "" {
		return "", false
	}
	return value, true
}

func number(cols []string, row []json.RawMessage, name string) (int, bool) {
	raw, ok := column(cols, row, name)
	if !ok {
		return 0, false
	}
	var value int
	if json.Unmarshal(raw, &value) != nil {
		return 0, false
	}
	return value, true
}

// lineRange reads the "lines" column, which CBM writes as "start-end" and
// leaves empty for nodes that are not a span of source.
func lineRange(cols []string, row []json.RawMessage) (int, bool) {
	value, ok := text(cols, row, "lines")
	if !ok {
		return 0, false
	}
	start, err := strconv.Atoi(strings.TrimSpace(strings.Split(value, "-")[0]))
	if err != nil {
		return 0, false
	}
	return start, true
}

func unavailable(codeCtx vacctx.CodeContext, tool, reason string) *vacerr.Error {
	return vacerr.New(
		vacerr.GraphProviderUnavailable,
		fmt.Sprintf("trace_calls: context %q: CBM %s failed on graph %q: %s", codeCtx.ID, tool, codeCtx.GraphRef, reason),
		map[string]any{"context": codeCtx.ID, "graph_ref": codeCtx.GraphRef, "tool": tool},
	)
}

func invalid(message string, details map[string]any) *vacerr.Error {
	return vacerr.New(vacerr.InvalidArgument, message, details)
}
