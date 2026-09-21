package zoekt

import (
	"strings"
	"testing"
	"time"

	"github.com/tc3oliver/version-aware-code-mcp/config"
)

func TestZoektRequestBudgetOptionOverridesTheDefault(t *testing.T) {
	p := New(emptyConfig(), WithRequestBudget(time.Second))

	if p.requestBudget != time.Second {
		t.Fatalf("requestBudget = %s, want 1s", p.requestBudget)
	}
}

func TestZoektWithoutOptionsKeepsTheDefault(t *testing.T) {
	if p := New(emptyConfig()); p.requestBudget != defaultRequestBudget {
		t.Fatalf("requestBudget = %s, want the default %s", p.requestBudget, defaultRequestBudget)
	}
}

// Zero is refused along with negatives: a zero budget reaches deadline.With as
// "no budget at all", so accepting it would make WithRequestBudget(0) the way
// to hang for ever on a Zoekt that never answers.
func TestZoektRequestBudgetRejectsNonPositiveDurations(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		t.Run(d.String(), func(t *testing.T) {
			assertRejects(t, "zoekt.WithRequestBudget", func() { New(emptyConfig(), WithRequestBudget(d)) })
		})
	}
}

func emptyConfig() *config.Config {
	return &config.Config{Providers: config.Providers{Zoekt: config.Zoekt{URL: "http://127.0.0.1:0"}}}
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
