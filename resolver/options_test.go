package resolver

import (
	"strings"
	"testing"
	"time"

	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/vacctx"
)

func TestResolveBudgetOptionOverridesTheDefault(t *testing.T) {
	if r := New(emptyConfig(), WithResolveBudget(time.Second)); r.resolveBudget != time.Second {
		t.Fatalf("resolveBudget = %s, want 1s", r.resolveBudget)
	}
}

func TestResolverWithoutOptionsKeepsTheDefault(t *testing.T) {
	if r := New(emptyConfig()); r.resolveBudget != defaultResolveBudget {
		t.Fatalf("resolveBudget = %s, want the default %s", r.resolveBudget, defaultResolveBudget)
	}
}

// Zero is refused along with negatives: a zero budget reaches deadline.With as
// "no budget at all", so accepting it would make WithResolveBudget(0) the way
// to hang every tool call behind a git that never returns.
func TestResolveBudgetRejectsNonPositiveDurations(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		t.Run(d.String(), func(t *testing.T) {
			assertRejects(t, "resolver.WithResolveBudget", func() { New(emptyConfig(), WithResolveBudget(d)) })
		})
	}
}

func emptyConfig() *config.Config {
	return &config.Config{
		Repositories: map[string]config.Repository{},
		Contexts:     map[string]vacctx.Workspace{},
	}
}

// assertRejects requires the construction to fail at once, naming the option
// that was called wrongly. It has to be a panic: New returns no error, and
// deferring the complaint to the first call would report a construction-time
// mistake from a stack that has nothing to do with it.
func assertRejects(t *testing.T, option string, construct func()) {
	t.Helper()

	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatalf("%s accepted a non-positive budget", option)
		}
		message, ok := recovered.(string)
		if !ok || !strings.Contains(message, option) {
			t.Fatalf("panicked with %v, want a message naming %s", recovered, option)
		}
		if !strings.Contains(message, "zero does not mean unlimited") {
			t.Errorf("panicked with %q, want it to say that zero is not unlimited", message)
		}
	}()
	construct()
}
