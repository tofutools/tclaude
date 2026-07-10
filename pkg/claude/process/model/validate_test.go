package model

import (
	"strings"
	"testing"
)

// mustParse parses a fixture and returns its diagnostics; a hard Parse error
// (unparseable YAML) fails the test because every fixture here is meant to
// reach semantic validation.
func mustParse(t *testing.T, source string) Diagnostics {
	t.Helper()
	parsed, err := Parse([]byte(source))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return parsed.Diagnostics
}

func findDiagnostics(diagnostics Diagnostics, code string) Diagnostics {
	var out Diagnostics
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			out = append(out, diagnostic)
		}
	}
	return out
}

func assertDiagnostic(t *testing.T, diagnostics Diagnostics, severity Severity, code, path string) {
	t.Helper()
	for _, diagnostic := range findDiagnostics(diagnostics, code) {
		if diagnostic.Severity == severity && diagnostic.Path == path {
			return
		}
	}
	t.Errorf("missing %s %s at %q; got %#v", severity, code, path, diagnostics)
}

func assertNoDiagnostic(t *testing.T, diagnostics Diagnostics, code string) {
	t.Helper()
	if found := findDiagnostics(diagnostics, code); len(found) > 0 {
		t.Errorf("unexpected %s diagnostics: %#v", code, found)
	}
}

// routingFixture builds a template around one task/wait/start node under test
// with the given YAML `next` map, plus enough end nodes for every target.
func routingFixture(nodeYAML string) string {
	return `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: routing-fixture
start: begin
nodes:
  begin:
    type: start
    next: work
` + nodeYAML + `
  done:
    type: end
    result: success
  failed:
    type: end
    result: failed
`
}

func TestValidateAmbiguousPassEdge(t *testing.T) {
	diagnostics := mustParse(t, routingFixture(`
  work:
    type: task
    performer: { kind: agent, prompt: Do it }
    next: { pass: done, done: done, success: failed, fail: failed }
`))
	assertDiagnostic(t, diagnostics, SeverityWarning, "ambiguous_pass_edge", "nodes.work.next.done")
	assertDiagnostic(t, diagnostics, SeverityWarning, "ambiguous_pass_edge", "nodes.work.next.success")
	assertNoDiagnostic(t, diagnostics, "missing_pass_edge")
	// Shadowed ≠ dead: pass routing checks the exact attempt verdict before
	// the alias fallback, and the message must say so (cold-review directive).
	for _, diagnostic := range findDiagnostics(diagnostics, "ambiguous_pass_edge") {
		if !strings.Contains(diagnostic.Message, "still routes here") {
			t.Errorf("ambiguous_pass_edge message must state exact-verdict reachability; got %q", diagnostic.Message)
		}
	}
}

func TestValidateAmbiguousFailEdge(t *testing.T) {
	diagnostics := mustParse(t, routingFixture(`
  work:
    type: task
    performer: { kind: agent, prompt: Do it }
    next: { pass: done, fail: failed, error: failed }
`))
	assertDiagnostic(t, diagnostics, SeverityWarning, "ambiguous_fail_edge", "nodes.work.next.error")
	assertNoDiagnostic(t, diagnostics, "ambiguous_pass_edge")
}

func TestValidateMissingPassEdgeFailOnlyTaskIsError(t *testing.T) {
	diagnostics := mustParse(t, routingFixture(`
  work:
    type: task
    performer: { kind: agent, prompt: Do it }
    next: { fail: failed, error: failed }
`))
	assertDiagnostic(t, diagnostics, SeverityError, "missing_pass_edge", "nodes.work.next")
}

func TestValidateMissingPassEdgeCustomVerdictsIsWarning(t *testing.T) {
	diagnostics := mustParse(t, routingFixture(`
  work:
    type: task
    performer: { kind: agent, prompt: Do it }
    next: { deployed: done, fail: failed }
`))
	assertDiagnostic(t, diagnostics, SeverityWarning, "missing_pass_edge", "nodes.work.next")
}

func TestValidateCancelEdgesOnTasksAreDead(t *testing.T) {
	// The runtime classifies cancel verdicts as failures (state.IsFailOutcome)
	// and fail routing only consults the fail vocabulary, so a cancel-labeled
	// task edge can never be taken.
	diagnostics := mustParse(t, routingFixture(`
  work:
    type: task
    performer: { kind: agent, prompt: Do it }
    next: { pass: done, cancel: failed }
`))
	assertDiagnostic(t, diagnostics, SeverityWarning, "dead_edge", "nodes.work.next.cancel")
	assertNoDiagnostic(t, diagnostics, "missing_pass_edge")

	// Fail + cancel edges only: no verdict can ever route a pass — the
	// guaranteed-stall error, not the softer custom-verdict warning.
	diagnostics = mustParse(t, routingFixture(`
  work:
    type: task
    performer: { kind: agent, prompt: Do it }
    next: { fail: failed, canceled: failed }
`))
	assertDiagnostic(t, diagnostics, SeverityError, "missing_pass_edge", "nodes.work.next")
	assertDiagnostic(t, diagnostics, SeverityWarning, "dead_edge", "nodes.work.next.canceled")
}

func TestValidateWaitNodeRouting(t *testing.T) {
	// A wait node routes only through its pass edge: no pass alias is an
	// error (guaranteed stall), any extra edge is dead.
	diagnostics := mustParse(t, routingFixture(`
  work:
    type: wait
    wait: { duration: 5m }
    next: { finished: done, timeout: failed }
`))
	assertDiagnostic(t, diagnostics, SeverityError, "missing_pass_edge", "nodes.work.next")

	diagnostics = mustParse(t, routingFixture(`
  work:
    type: wait
    wait: { duration: 5m }
    next: { pass: done, timeout: failed }
`))
	assertDiagnostic(t, diagnostics, SeverityWarning, "dead_edge", "nodes.work.next.timeout")
	assertNoDiagnostic(t, diagnostics, "missing_pass_edge")
}

func TestValidateStartNodeDeadEdge(t *testing.T) {
	diagnostics := mustParse(t, `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: routing-fixture
start: begin
nodes:
  begin:
    type: start
    next: { pass: done, alt: failed }
  done:
    type: end
    result: success
  failed:
    type: end
    result: failed
`)
	assertDiagnostic(t, diagnostics, SeverityWarning, "dead_edge", "nodes.begin.next.alt")
}

func TestValidateSingleEdgeAlwaysResolves(t *testing.T) {
	// One edge resolves via the runtime's lone-edge fallback regardless of
	// its label; no routing diagnostics may fire.
	diagnostics := mustParse(t, routingFixture(`
  work:
    type: task
    performer: { kind: agent, prompt: Do it }
    next: { deployed: done }
`))
	for _, code := range []string{"missing_pass_edge", "ambiguous_pass_edge", "ambiguous_fail_edge", "dead_edge"} {
		assertNoDiagnostic(t, diagnostics, code)
	}
}

func TestValidateDecisionNodesExemptFromRoutingChecks(t *testing.T) {
	diagnostics := mustParse(t, `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: routing-fixture
start: begin
nodes:
  begin:
    type: start
    next: choose
  choose:
    type: decision
    performer: { kind: human, ask: "Which way?" }
    next: { ship: done, hold: failed }
  done:
    type: end
    result: success
  failed:
    type: end
    result: failed
`)
	for _, code := range []string{"missing_pass_edge", "ambiguous_pass_edge", "ambiguous_fail_edge", "dead_edge"} {
		assertNoDiagnostic(t, diagnostics, code)
	}
}

// loopFixture is the sanctioned poison-escalation loop; retryYAML is spliced
// into the compound node ("" leaves the loop budget-less).
func loopFixture(retryYAML string) string {
	return `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: loop-fixture
start: implement
nodes:
  implement:
    type: task
    performer: { kind: agent, prompt: Implement it }
    checks:
      - id: tests
        performer: { kind: program, run: go test ./... }
` + retryYAML + `
    next: { pass: done, fail: escalate }
  escalate:
    type: decision
    performer: { kind: human, ask: "Retries exhausted. Continue?" }
    next: { retry: implement, cancel: canceled }
  done:
    type: end
    result: success
  canceled:
    type: end
    result: canceled
`
}

func TestValidateRetryLoopWithoutBudget(t *testing.T) {
	diagnostics := mustParse(t, loopFixture(""))
	assertDiagnostic(t, diagnostics, SeverityWarning, "retry_loop_without_budget", "nodes.implement.retry")
	if diagnostics.HasErrors() {
		t.Fatalf("budget-less loop must stay advisory; got errors: %#v", diagnostics.Errors())
	}

	diagnostics = mustParse(t, loopFixture("    retry: { maxAttempts: 3 }"))
	assertNoDiagnostic(t, diagnostics, "retry_loop_without_budget")
}

// TestValidateSection8aDiagnosticClasses pins one fixture per §8a diagnostic
// class the live-validation editor surfaces: unreachable nodes, missing or
// ambiguous outcome edges, undeclared param references, and budget-less loops.
func TestValidateSection8aDiagnosticClasses(t *testing.T) {
	source := `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: broken-fixture
start: begin
nodes:
  begin:
    type: start
    next: work
  work:
    type: task
    performer: { kind: agent, prompt: "Fix {{ params.issue }}" }
    next: { pass: gone, done: done }
  island:
    type: task
    performer: { kind: agent, prompt: Do nothing }
    next: { pass: done }
  done:
    type: end
    result: success
`
	diagnostics := mustParse(t, source)
	assertDiagnostic(t, diagnostics, SeverityError, "unreachable_node", "nodes.island")
	assertDiagnostic(t, diagnostics, SeverityError, "unknown_target", "nodes.work.next.pass")
	assertDiagnostic(t, diagnostics, SeverityWarning, "ambiguous_pass_edge", "nodes.work.next.done")
	assertDiagnostic(t, diagnostics, SeverityError, "undeclared_param_ref", "nodes.work.performer.prompt")

	loop := mustParse(t, loopFixture(""))
	assertDiagnostic(t, loop, SeverityWarning, "retry_loop_without_budget", "nodes.implement.retry")
}

func TestValidateLoopBudgetSkipsNonSanctionedShapes(t *testing.T) {
	// An agent-performed decision is not the sanctioned loop: the cycle is a
	// graph_cycle error and the budget warning must stay silent.
	source := strings.Replace(loopFixture(""), `performer: { kind: human, ask: "Retries exhausted. Continue?" }`,
		`performer: { kind: agent, prompt: "Retry?" }`, 1)
	diagnostics := mustParse(t, source)
	assertNoDiagnostic(t, diagnostics, "retry_loop_without_budget")
	if len(findDiagnostics(diagnostics, "graph_cycle")) == 0 {
		t.Fatalf("expected graph_cycle for the non-sanctioned loop; got %#v", diagnostics)
	}
}
