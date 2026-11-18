package model

import (
	"strings"
	"testing"
)

// A fallback's lifetime is one of step, run or keep, and only on a spawn.
func TestFallbackLifetimeValidation(t *testing.T) {
	const tmpl = `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: lifetimes
start: a
nodes:
  a:
    type: task
    performer: { kind: agent, prompt: "Do it", fallback: { action: %s, lifetime: %s } }
    next: done
  done: { type: end }
`
	parse := func(action, lifetime string) Diagnostics {
		t.Helper()
		parsed, err := Parse([]byte(strings.Replace(strings.Replace(tmpl, "%s", action, 1), "%s", lifetime, 1)))
		if err != nil {
			t.Fatal(err)
		}
		return parsed.Diagnostics.Errors()
	}
	for _, lifetime := range []string{LifetimeStep, LifetimeRun, LifetimeKeep} {
		if errs := parse(FallbackSpawn, lifetime); len(errs) != 0 {
			t.Errorf("spawn with lifetime %s: %#v", lifetime, errs)
		}
	}
	if errs := parse(FallbackSpawn, "forever"); len(errs) != 1 || !strings.Contains(errs[0].Message, "lifetime must be") {
		t.Errorf("unknown lifetime: %#v", errs)
	}
	if errs := parse(FallbackWait, LifetimeStep); len(errs) != 1 || !strings.Contains(errs[0].Message, "only valid with action spawn") {
		t.Errorf("lifetime on a wait: %#v", errs)
	}
}
