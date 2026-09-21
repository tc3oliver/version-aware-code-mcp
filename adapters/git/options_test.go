package git

import (
	"strings"
	"testing"
	"time"

	"github.com/tc3oliver/version-aware-code-mcp/config"
)

// The options are the only way a budget moves, so what they refuse matters as
// much as what they accept. Zero is refused along with negatives: a zero budget
// reaches deadline.With as "no budget at all", so accepting it would turn
// WithReadBudget(0) into the way to run an unbounded git — the thing the
// budgets exist to prevent.

func TestGitBudgetOptionsOverrideTheDefaults(t *testing.T) {
	p := New(emptyConfig(), WithReadBudget(time.Second), WithDiffBudget(2*time.Second), WithHistoryBudget(3*time.Second))

	if p.readBudget != time.Second || p.diffBudget != 2*time.Second || p.historyBudget != 3*time.Second {
		t.Fatalf("budgets = %s/%s/%s, want 1s/2s/3s", p.readBudget, p.diffBudget, p.historyBudget)
	}
}

func TestGitWithoutOptionsKeepsTheDefaults(t *testing.T) {
	p := New(emptyConfig())

	if p.readBudget != defaultReadBudget || p.diffBudget != defaultDiffBudget || p.historyBudget != defaultHistoryBudget {
		t.Fatalf("budgets = %s/%s/%s, want the defaults %s/%s/%s",
			p.readBudget, p.diffBudget, p.historyBudget,
			defaultReadBudget, defaultDiffBudget, defaultHistoryBudget)
	}
}

func TestGitBudgetOptionsRejectNonPositiveDurations(t *testing.T) {
	options := map[string]func(time.Duration) Option{
		"git.WithReadBudget":    WithReadBudget,
		"git.WithDiffBudget":    WithDiffBudget,
		"git.WithHistoryBudget": WithHistoryBudget,
	}
	for name, option := range options {
		for _, d := range []time.Duration{0, -time.Second} {
			t.Run(name+"/"+d.String(), func(t *testing.T) {
				assertRejects(t, name, func() { New(emptyConfig(), option(d)) })
			})
		}
	}
}

func emptyConfig() *config.Config {
	return &config.Config{Repositories: map[string]config.Repository{}}
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
