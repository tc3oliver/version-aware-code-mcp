package cbm

import (
	"strings"
	"testing"
	"time"

	"github.com/tc3oliver/version-aware-code-mcp/config"
)

func TestCBMBudgetOptionsOverrideTheDefaults(t *testing.T) {
	p := New(emptyConfig(), WithTraceBudget(time.Second), WithConnectTimeout(2*time.Second))

	if p.traceBudget != time.Second || p.connectTimeout != 2*time.Second {
		t.Fatalf("budgets = %s/%s, want 1s/2s", p.traceBudget, p.connectTimeout)
	}
}

func TestCBMWithoutOptionsKeepsTheDefaults(t *testing.T) {
	p := New(emptyConfig())

	if p.traceBudget != defaultTraceBudget || p.connectTimeout != defaultConnectTimeout {
		t.Fatalf("budgets = %s/%s, want the defaults %s/%s",
			p.traceBudget, p.connectTimeout, defaultTraceBudget, defaultConnectTimeout)
	}
}

// Zero is refused along with negatives. A zero trace budget reaches
// deadline.With as "no budget at all", and a zero connect timeout would be a
// deadline that has already passed — neither is what a caller writing 0 means,
// and neither may be the way to ask for unlimited.
func TestCBMBudgetOptionsRejectNonPositiveDurations(t *testing.T) {
	options := map[string]func(time.Duration) Option{
		"cbm.WithTraceBudget":    WithTraceBudget,
		"cbm.WithConnectTimeout": WithConnectTimeout,
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
	return &config.Config{Providers: config.Providers{CBM: config.CBM{Command: "codebase-memory-mcp"}}}
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
