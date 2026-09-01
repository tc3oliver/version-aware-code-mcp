package engine_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/engine"
	"github.com/tc3oliver/version-aware-code-mcp/evidence"
	"github.com/tc3oliver/version-aware-code-mcp/provider"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// recordingSource answers per repository and records every call it was made,
// with the whole context it was handed.
//
// It records the calls rather than the last one for [recordingSearch]'s reason:
// what a member-selection test is about is the set of them — that the named
// member was read, that it was read with its own version, and that no other
// member was read at all. A fake keeping only the latest call cannot tell a read
// that asked one member from one that asked both and kept the second answer.
type recordingSource struct {
	content map[string]provider.SourceContent
	fail    map[string]error
	calls   []vacctx.CodeContext
}

func (r *recordingSource) Read(_ context.Context, codeCtx vacctx.CodeContext, path string, start, end int) (*provider.SourceContent, error) {
	r.calls = append(r.calls, codeCtx)
	if err := r.fail[codeCtx.Repository]; err != nil {
		return nil, err
	}
	content := r.content[codeCtx.Repository]
	return &content, nil
}

// A context naming several repositories needs the repository argument, and
// leaving it out is refused with an error the caller can act on: it names the
// repositories it could have asked for and says the argument is what is missing.
//
// Not a fallback to a member, which would read a version the caller named
// nothing of under the name of the context it did name, and not an empty result,
// which would read as "this version has no such file". The source provider is
// not reached at all: an error returned after a read would still have read a file
// in a version nobody chose.
func TestGetCodeRequiresARepositoryWhenTheContextNamesSeveral(t *testing.T) {
	source := &recordingSource{}
	eng := engine.New(fakeContexts{workspace: stack}, &fakeSearch{}, &fakeGraph{}, source)

	out, err := eng.GetCode(context.Background(), engine.GetCodeRequest{
		Context: stack.ID, Path: "process.go", StartLine: 1, EndLine: 1,
	})
	if err == nil {
		t.Fatal("GetCode read a file in a context naming two repositories without being told which")
	}
	assertCode(t, err, vacerr.InvalidArgument)
	assertNotAnAnswer(t, out)
	if out.Source() != (provider.SourceContent{}) {
		t.Errorf("a refused read returned content %+v", out.Source())
	}
	if len(source.calls) != 0 {
		t.Fatalf("a read with no repository named reached the source provider as %+v", source.calls)
	}

	// The message is what an agent reads before it retries. It has to say which
	// context it cannot use as asked, and that the missing repository argument is
	// what would make the same call answerable.
	if !strings.Contains(err.Error(), stack.ID) || !strings.Contains(err.Error(), "2 repositories") {
		t.Errorf("GetCode failed with %q, want it to name the context and its two repositories", err)
	}
	if !strings.Contains(err.Error(), "repository") {
		t.Errorf("GetCode failed with %q, want it to say the repository argument is what is required", err)
	}

	var vErr *vacerr.Error
	if !errors.As(err, &vErr) {
		t.Fatalf("GetCode failed with %v, want a *vacerr.Error", err)
	}
	// The caller cannot see the configuration: told only "several", it has
	// nothing to put in the argument it was just asked for.
	if got, ok := vErr.Details["repositories"].([]string); !ok || !slices.Equal(got, []string{"alpha", "beta"}) {
		t.Errorf("the refusal offers %v, want the repositories this context does name", vErr.Details["repositories"])
	}
}

// Repository names one member and the file is read there and nowhere else, and
// the content that comes back is that repository's.
//
// The two members answer with the same path on purpose, and with different
// content and different revisions: the same request differing only in its
// repository is what a multi-repo workspace makes possible, and the only thing
// that can decide which content it gets is which member was handed to the
// provider. The other member is not read at all — not read and discarded, which
// would look the same in the result and would still be a read in a version the
// caller did not name.
func TestGetCodeReadsTheNamedMemberAndNoOther(t *testing.T) {
	alpha, beta := stack.Members[0], stack.Members[1]
	source := &recordingSource{content: map[string]provider.SourceContent{
		"alpha": {Path: "process.go", StartLine: 1, EndLine: 1, Content: "package alpha\n", Revision: alpha.Revision},
		"beta":  {Path: "process.go", StartLine: 1, EndLine: 1, Content: "package beta\n", Revision: beta.Revision},
	}}

	for _, want := range []vacctx.CodeContext{alpha, beta} {
		t.Run(want.Repository, func(t *testing.T) {
			source.calls = nil
			eng := engine.New(fakeContexts{workspace: stack}, &fakeSearch{}, &fakeGraph{}, source)

			out, err := eng.GetCode(context.Background(), engine.GetCodeRequest{
				Context: stack.ID, Repository: want.Repository, Path: "process.go", StartLine: 1, EndLine: 1,
			})
			if err != nil {
				t.Fatalf("GetCode: %v", err)
			}
			if !slices.Equal(source.calls, []vacctx.CodeContext{want}) {
				t.Fatalf("the provider was called with %+v, want one call with %s's own context %+v",
					source.calls, want.Repository, want)
			}
			if got := out.Source().Content; got != source.content[want.Repository].Content {
				t.Fatalf("read %q, want %s's own content %q", got, want.Repository, source.content[want.Repository].Content)
			}
			// The scope of the answer is the member it was read in, not the
			// context it was cut out of: reporting the other member beside it
			// would claim a repository was read that never was.
			if got := answeredIn(t, out); got != want {
				t.Fatalf("result context is %+v, want a workspace of only %+v", out.Context(), want)
			}
			if cited := citedIn(t, out); !slices.Equal(cited, []evidence.Evidence{evidence.At("process.go", 1, 1, "")}) {
				t.Fatalf("evidence is %+v, want the range that was read", out.Evidence())
			}
		})
	}
}

// A repository the context does not name is refused, and the refusal says which
// ones it could have asked for. No member is read: a fallback would answer in a
// version the caller never asked about under the name of one it did, and an
// empty result would read as "that repository has no such file" about a
// repository this context never covered.
func TestGetCodeRefusesARepositoryTheContextDoesNotName(t *testing.T) {
	source := &recordingSource{}
	eng := engine.New(fakeContexts{workspace: stack}, &fakeSearch{}, &fakeGraph{}, source)

	out, err := eng.GetCode(context.Background(), engine.GetCodeRequest{
		Context: stack.ID, Repository: "gamma", Path: "process.go", StartLine: 1, EndLine: 1,
	})
	if err == nil {
		t.Fatal("GetCode answered for a repository outside the workspace")
	}
	assertCode(t, err, vacerr.InvalidArgument)
	assertNotAnAnswer(t, out)
	if out.Source() != (provider.SourceContent{}) {
		t.Errorf("a refused read returned content %+v", out.Source())
	}
	if len(source.calls) != 0 {
		t.Fatalf("a repository outside the workspace reached the source provider as %+v", source.calls)
	}

	var vErr *vacerr.Error
	if !errors.As(err, &vErr) {
		t.Fatalf("GetCode failed with %v, want a *vacerr.Error", err)
	}
	if vErr.Details["repository"] != "gamma" {
		t.Errorf("the refusal says repository %v, want the one that was asked for", vErr.Details["repository"])
	}
	if got, ok := vErr.Details["repositories"].([]string); !ok || !slices.Equal(got, []string{"alpha", "beta"}) {
		t.Errorf("the refusal offers %v, want the repositories this context does name", vErr.Details["repositories"])
	}
}

// A context over one repository answers the same whether or not its repository
// is named: naming it selects the member that was going to be read anyway, and
// omitting it is the call every caller before workspaces wrote.
//
// The two results are compared rather than checked separately, so the two ways
// of asking cannot drift into two answers.
func TestGetCodeNamingTheOnlyRepositoryReadsWhatOmittingItReads(t *testing.T) {
	only := stack.Members[0]
	content := provider.SourceContent{
		Path: "process.go", StartLine: 1, EndLine: 1, Content: "package alpha\n", Revision: only.Revision,
	}
	source := &recordingSource{content: map[string]provider.SourceContent{only.Repository: content}}
	eng := engine.New(fakeContexts{workspace: single(only)}, &fakeSearch{}, &fakeGraph{}, source)

	omitted, err := eng.GetCode(context.Background(), engine.GetCodeRequest{
		Context: only.ID, Path: "process.go", StartLine: 1, EndLine: 1,
	})
	if err != nil {
		t.Fatalf("GetCode with no repository named: %v", err)
	}
	named, err := eng.GetCode(context.Background(), engine.GetCodeRequest{
		Context: only.ID, Repository: only.Repository, Path: "process.go", StartLine: 1, EndLine: 1,
	})
	if err != nil {
		t.Fatalf("GetCode naming the context's only repository: %v", err)
	}

	if !slices.Equal(source.calls, []vacctx.CodeContext{only, only}) {
		t.Fatalf("the provider was called with %+v, want the one member both times %+v", source.calls, only)
	}
	if omitted.Source() != content || named.Source() != content {
		t.Fatalf("read %+v with no repository named and %+v with it, want the provider's %+v",
			omitted.Source(), named.Source(), content)
	}
	if answeredIn(t, omitted) != only || answeredIn(t, named) != only {
		t.Fatalf("the two calls answered in %+v and %+v, want both in %+v",
			omitted.Context(), named.Context(), only)
	}
	if !slices.Equal(citedIn(t, omitted), citedIn(t, named)) {
		t.Fatalf("the two calls cited %+v and %+v", omitted.Evidence(), named.Evidence())
	}
}

// Selecting a member changes which repository is read and nothing about what
// happens when that repository cannot serve the declared revision. The provider
// fails closed with SOURCE_MISMATCH, and the failure arrives with no content
// beside it: content from another revision is the one answer this server exists
// to refuse, and it would be no better for having been asked for by name.
func TestGetCodeInAWorkspaceStillFailsClosedOnSourceMismatch(t *testing.T) {
	alpha, beta := stack.Members[0], stack.Members[1]
	mismatch := vacerr.NewSourceMismatch(alpha.Revision, "9999999999999999999999999999999999999999", map[string]any{
		"context": stack.ID, "repository": alpha.Repository,
	})
	source := &recordingSource{
		// beta can answer, and is here to make the point that the failure is not
		// routed around: a read of alpha does not fall back to the member that
		// would have succeeded.
		content: map[string]provider.SourceContent{
			"beta": {Path: "process.go", StartLine: 1, EndLine: 1, Content: "package beta\n", Revision: beta.Revision},
		},
		fail: map[string]error{alpha.Repository: mismatch},
	}
	eng := engine.New(fakeContexts{workspace: stack}, &fakeSearch{}, &fakeGraph{}, source)

	out, err := eng.GetCode(context.Background(), engine.GetCodeRequest{
		Context: stack.ID, Repository: alpha.Repository, Path: "process.go", StartLine: 1, EndLine: 1,
	})
	if !errors.Is(err, mismatch) {
		t.Fatalf("GetCode failed with %v, want the provider's own fail-closed error", err)
	}
	assertCode(t, err, vacerr.SourceMismatch)
	assertNotAnAnswer(t, out)
	if out.Source() != (provider.SourceContent{}) {
		t.Fatalf("a mismatched read returned content %+v, want none at all", out.Source())
	}
	if !slices.Equal(source.calls, []vacctx.CodeContext{alpha}) {
		t.Fatalf("the provider was called with %+v, want only the member that was named %+v", source.calls, alpha)
	}
}
