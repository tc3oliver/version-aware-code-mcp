package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// memberToCompare resolves one side of a comparison to the single member it is
// compared in: the one repository selects, or the only one the context names.
//
// Both comparisons still answer in one repository per side, so this narrows to
// one member and nothing downstream of it knows a workspace can hold more. What
// repository does here it does on both sides identically, exactly as the path
// and the symbol are asked of both sides identically: a comparison of one
// repository on the from side against another on the to side is not a
// difference between two versions.
//
// It is [Engine.resolveMember]'s work with one thing said differently, and the
// difference is what it says when the context names several repositories and the
// request named none. resolveMember reports a scope this server cannot answer a
// question in, which was the whole truth while no comparison could name a
// repository; now one can, so the caller is told that repository is required and
// which of its two contexts needs it. Told only "this context names several", a
// caller would have no way to know it is holding the argument that fixes it.
func (e *Engine) memberToCompare(ctx context.Context, tool, side, id, repository string) (vacctx.CodeContext, error) {
	workspace, err := e.resolve(ctx, id)
	if err != nil {
		return vacctx.CodeContext{}, err
	}
	members, err := selectMembers(id, repository, workspace)
	if err != nil {
		return vacctx.CodeContext{}, err
	}
	if len(members) != 1 {
		// A blank repository over a workspace of several is the only way to reach
		// here: a named repository selects exactly one member or is refused, and a
		// workspace with no member at all is refused before either.
		return vacctx.CodeContext{}, repositoryRequired(tool, side, id, members)
	}
	return members[0], nil
}

// repositoryRequired refuses a side naming several repositories with nothing to
// choose between them by.
//
// The repositories travel with it because the caller cannot see the
// configuration: told only that its context names several, it cannot tell which
// name to write. The side travels with it because a comparison names two
// contexts and only one of them is the one to fix — a caller that guessed wrong
// would go on to narrow the side that was never the problem.
//
// It is [vacerr.InvalidArgument] for the reason [severalRepositories] is: the
// code set is the public tool API, and a missing required argument is what
// INVALID_ARGUMENT has said since v0.1.0. Refusing is the point — picking a
// member for the caller would compare a repository it named nothing of, under
// the name of the context it did name.
func repositoryRequired(tool, side, id string, members []vacctx.CodeContext) error {
	repositories := make([]string, 0, len(members))
	for _, member := range members {
		repositories = append(repositories, member.Repository)
	}
	return vacerr.New(
		vacerr.InvalidArgument,
		fmt.Sprintf("%s: the %s context %q names %d repositories (%s), so repository is required to say which one to compare",
			tool, side, id, len(repositories), strings.Join(repositories, ", ")),
		map[string]any{"context": id, "side": side, "repositories": repositories},
	)
}

// ComparisonSide is one version's half of a comparison: what the from context
// had, or what the to context had, or the fact that this version had nothing.
//
// A comparison is answered by two of these, and they are never merged. Evidence
// only means anything at the revision it was read at: handler.go:42 in the from
// context and handler.go:42 in the to context are two different lines that
// happen to share a spelling, so a single flattened citation list cannot say
// which version any entry came from — which is the one thing this server exists
// to say. Whatever assembles a comparison result therefore keeps from's context
// and citations on the from side and to's on the to side, and offers no
// combined list for a caller to mistake for either. This is a rule about the
// answer, not about tidiness: a merged list is a cross-version answer wearing
// the clothes of a version-scoped one.
//
// It embeds [answer], so a present side carries the version it was answered in
// and the citations backing it exactly as [SearchCodeResult] and the others do,
// and for the same reason it has no exported fields: no code outside this
// package can name them, so the only sides carrying a version are the ones a
// method here built. The one value a caller can write for itself is the zero
// one, which is the absent side.
type ComparisonSide struct {
	answer
}

// Present reports whether this version had the thing being compared. It is what
// tells "the symbol is not in v2" apart from "the symbol is in v2 and nothing
// about it is worth citing".
//
// It is derived rather than stored: a side is present exactly when it carries a
// version to be present in, which is a member in its workspace, so an absent
// side is the zero [ComparisonSide] and there is no second field that can
// disagree with the first. Calling Context and Evidence on an absent side is
// safe and reports the zero workspace and no evidence — a side reporting Present
// false has nothing to cite by definition, and nothing here dereferences
// anything to say so.
func (s ComparisonSide) Present() bool { return len(s.workspace.Members) != 0 }

// CodeChange is what happened to the compared code between the two contexts.
// It is the one-word answer a caller reads first, and the four values below are
// all of them: every pair of sides is exactly one of found-in-both-and-equal,
// found-in-both-and-different, only-in-to, only-in-from.
type CodeChange string

const (
	// CodeUnchanged: both versions have it and they are the same. Not "no diff
	// was computed" — the comparison ran and found nothing to report.
	CodeUnchanged CodeChange = "UNCHANGED"
	// CodeModified: both versions have it and they differ.
	CodeModified CodeChange = "MODIFIED"
	// CodeAdded: only the to context has it. The from side is absent, so it
	// cites nothing, and that absence is the answer rather than a gap in it.
	CodeAdded CodeChange = "ADDED"
	// CodeRemoved: only the from context has it, the mirror of [CodeAdded].
	CodeRemoved CodeChange = "REMOVED"
)

// SymbolPresence is which of the two contexts the compared symbol was found in.
// It is the question asked before any content is compared, because the answer
// decides whether there is anything to compare at all: two sides can be diffed,
// one cannot.
//
// It is deliberately not folded into [CodeChange]. Presence is a fact about the
// two sides, and comes out of resolving the symbol in each version; the change
// is a fact about their content, and only [PresenceBoth] leaves the reading of
// it open. Keeping them apart is what stops "not found in v2" being reported as
// a modification of nothing.
type SymbolPresence string

const (
	// PresenceBoth: found in both contexts, so the sides can be compared and
	// the outcome is [CodeUnchanged] or [CodeModified].
	PresenceBoth SymbolPresence = "BOTH"
	// PresenceToOnly: found only in the to context, which is [CodeAdded].
	PresenceToOnly SymbolPresence = "TO_ONLY"
	// PresenceFromOnly: found only in the from context, which is
	// [CodeRemoved].
	PresenceFromOnly SymbolPresence = "FROM_ONLY"
)
