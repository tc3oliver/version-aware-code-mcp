package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/evidence"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
)

// compareCallsInput is what a client sends.
//
// Every field is optional to the schema and none of them are constrained there
// — no enum on direction, no bounds on depth — for the reason trace_calls does
// the same: a value the schema rejects comes back as the SDK's own validation
// failure, while a value the tool rejects comes back as this project's error
// model, and keeping every value check on one side of that line means one wrong
// argument has one shape.
//
// The two contexts are the whole scope of the comparison, and there is no
// repository, branch or revision field: each side is walked in the graph its own
// context names. Symbol, direction and depth are asked of both sides
// identically — there is deliberately no way to walk the from side one way and
// the to side another, because a difference produced by asking two different
// questions is not a difference between two versions.
type compareCallsInput struct {
	FromContext string `json:"from_context,omitempty" jsonschema:"the id of the version context to compare from, as returned by list_contexts; the trace runs in that version's graph"`
	ToContext   string `json:"to_context,omitempty" jsonschema:"the id of the version context to compare to, as returned by list_contexts; it must name the same repository as from_context"`
	Symbol      string `json:"symbol,omitempty" jsonschema:"the function to compare, named as it is written in the source; it is resolved in each version separately"`
	Direction   string `json:"direction,omitempty" jsonschema:"callers to walk towards the functions that call the symbol, callees to walk towards the ones it calls"`
	Depth       int    `json:"depth,omitempty" jsonschema:"how many levels to walk, from 1 to 5; the same depth is walked on both sides"`
}

// callRelation is one caller-callee pair on one path, with the call sites it is
// written at in each version.
//
// The two evidence lists are kept apart for the reason the two sides are: a call
// site only means anything at the revision it was read at, so handler.go:42 in
// the from context and handler.go:42 in the to context are two different lines
// that happen to share a spelling. A relation only the to version has cites
// nothing on the from side, and the other way round; which list the relation is
// in — added, removed or unchanged — is what says why.
type callRelation struct {
	Caller       string              `json:"caller"`
	Callee       string              `json:"callee"`
	Path         string              `json:"path"`
	FromEvidence []evidence.Evidence `json:"from_evidence"`
	ToEvidence   []evidence.Evidence `json:"to_evidence"`
}

// compareCallsResult is a call graph comparison as a client receives it: the two
// sides kept apart, which versions had the symbol at all, and what changed
// around it.
//
// It is the whole wire object rather than a payload merged into an
// [evidence.Output], because there is no single context to merge into: the
// context and evidence of each version live on that version's side, and the rest
// are facts about the pair.
//
// The requested symbol and each version's resolved one are all reported,
// because one symbol can be renamed between versions and still be the symbol the
// caller asked about; a version that does not have it resolves to the empty
// string and is the absent side.
type compareCallsResult struct {
	From               *evidence.Output `json:"from"`
	To                 *evidence.Output `json:"to"`
	Presence           string           `json:"presence"`
	RequestedSymbol    string           `json:"requested_symbol"`
	FromResolvedSymbol string           `json:"from_resolved_symbol"`
	ToResolvedSymbol   string           `json:"to_resolved_symbol"`
	Added              []callRelation   `json:"added"`
	Removed            []callRelation   `json:"removed"`
	Unchanged          []callRelation   `json:"unchanged"`
}

// relations encodes one of the three relation lists. Both the list and every
// evidence list inside it are built empty rather than nil: null and [] are
// different answers to an agent, and "nothing was added" is an answer.
func relations(found []engine.CallRelation) []callRelation {
	encoded := make([]callRelation, 0, len(found))
	for _, rel := range found {
		encoded = append(encoded, callRelation{
			Caller:       rel.Caller,
			Callee:       rel.Callee,
			Path:         rel.Path,
			FromEvidence: cites(rel.FromEvidence),
			ToEvidence:   cites(rel.ToEvidence),
		})
	}
	return encoded
}

// cites is one relation's call sites in one version, as [] rather than null when
// that version does not have the relation at all.
func cites(ev []evidence.Evidence) []evidence.Evidence {
	if ev == nil {
		return []evidence.Evidence{}
	}
	return ev
}

// AddCompareCalls registers the compare_calls tool on srv, answered by eng.
//
// The tool decodes the call, hands it to [engine.Engine.CompareCalls] and
// encodes what comes back. Everything the answer is made of is decided before it
// returns: resolving both context IDs, refusing two repositories whose call
// graphs are not two versions of one, bounding the depth, walking each version's
// own graph, resolving the symbol separately in each, and classifying every
// relation as added, removed or unchanged. Nothing here compares anything.
//
// Every failure is reported as the error model's wire shape,
// {"error": {"code": ..., "message": ..., "details": {}}}, on a result marked as
// an error and with no content beside it.
func AddCompareCalls(srv *mcp.Server, eng *engine.Engine) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:  "compare_calls",
		Title: "Compare a symbol's call graph between two version contexts",
		Description: "Compare the call graph around one symbol between two version contexts. " +
			"Pass two context ids from list_contexts: the same symbol, direction and depth are walked in each version's own graph, and no repository, branch or revision can be given here. " +
			"Both contexts must name the same repository — two repositories' call graphs are not two versions of one, which is INVALID_ARGUMENT. " +
			"direction is \"callers\" (the functions that call the symbol) or \"callees\" (the ones it calls); depth is how many levels to walk, from 1 to 5. " +
			"presence says which versions had the symbol at all: BOTH, FROM_ONLY or TO_ONLY. The relations are reported as added, removed and unchanged, each carrying its call sites in each version separately. " +
			"The from and to sides are reported separately too, each with the version context it was traced in and the evidence backing it, and the side of a version that does not have the symbol is null. " +
			"A symbol neither version has is SYMBOL_NOT_FOUND, and one matching several functions is SYMBOL_AMBIGUOUS with the candidates and the side it was ambiguous on.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in compareCallsInput) (*mcp.CallToolResult, any, error) {
		result, err := eng.CompareCalls(ctx, engine.CompareCallsRequest{
			FromContext: in.FromContext,
			ToContext:   in.ToContext,
			Symbol:      in.Symbol,
			Direction:   provider.Direction(in.Direction),
			Depth:       in.Depth,
		})
		if err != nil {
			return failed(err)
		}

		from, err := side(result.From())
		if err != nil {
			// A side is present but its context is missing a field the contract
			// requires, so there is no way to answer without dropping part of it.
			return nil, nil, err
		}
		to, err := side(result.To())
		if err != nil {
			return nil, nil, err
		}

		return nil, compareCallsResult{
			From:               from,
			To:                 to,
			Presence:           string(result.Presence()),
			RequestedSymbol:    result.RequestedSymbol(),
			FromResolvedSymbol: result.FromResolvedSymbol(),
			ToResolvedSymbol:   result.ToResolvedSymbol(),
			Added:              relations(result.Added()),
			Removed:            relations(result.Removed()),
			Unchanged:          relations(result.Unchanged()),
		}, nil
	})
}
