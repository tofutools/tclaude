package workflows

import (
	"bytes"
	"strings"
	"testing"
)

// These cover only the client-side pre-flight gates that return BEFORE
// the daemon is contacted, so they're deterministic whether or not an
// agentd happens to be running on the test host. The end-to-end inject
// mechanism is covered by the agentd TmuxSim flow tests.

func TestRunRun_RequiresTarget(t *testing.T) {
	var out, errb bytes.Buffer
	code := RunRun(&RunParams{Name: "demo-flow"}, &out, &errb)
	if code != rcInvalidArg {
		t.Fatalf("code = %d, want %d (stderr=%s)", code, rcInvalidArg, errb.String())
	}
	if !strings.Contains(errb.String(), "--target") {
		t.Errorf("stderr should mention --target; got %q", errb.String())
	}
}

func TestRunRun_RejectsUnsafeName(t *testing.T) {
	cases := []string{
		"",                       // empty
		"demo flow",              // space
		"a/b",                    // slash
		"demo-flow\n/rename x",   // newline breakout
		strings.Repeat("x", 129), // too long
	}
	for _, name := range cases {
		var out, errb bytes.Buffer
		code := RunRun(&RunParams{Name: name, Target: "worker"}, &out, &errb)
		if code != rcInvalidArg {
			t.Errorf("name %q: code = %d, want %d (stderr=%s)", name, code, rcInvalidArg, errb.String())
		}
	}
}

func TestIsValidWorkflowName(t *testing.T) {
	ok := []string{"demo-flow", "review_changes", "deep-research", "a", "x.y", "A1_b-2.c"}
	for _, n := range ok {
		if !isValidWorkflowName(n) {
			t.Errorf("isValidWorkflowName(%q) = false, want true", n)
		}
	}
	bad := []string{"", "has space", "slash/name", "new\nline", "tab\there", "uni€ode", strings.Repeat("a", 129)}
	for _, n := range bad {
		if isValidWorkflowName(n) {
			t.Errorf("isValidWorkflowName(%q) = true, want false", n)
		}
	}
}
