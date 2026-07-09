package model

import "testing"

// TestDurationValidation covers authoring-time parsing of duration-ish fields:
// wait.duration, retry.backoff, and performer.timeout.
func TestDurationValidation(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{
			name: "invalid wait duration",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: bad-wait-duration
start: a
nodes:
  a:
    type: wait
    wait: { duration: banana }
    next: done
  done: { type: end }
`,
		},
		{
			name: "invalid retry backoff",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: bad-backoff
start: a
nodes:
  a:
    type: task
    performer: { kind: agent, prompt: "Do it" }
    retry: { maxAttempts: 2, backoff: nope }
    next: done
  done: { type: end }
`,
		},
		{
			name: "invalid performer timeout",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: bad-timeout
start: a
nodes:
  a:
    type: task
    performer: { kind: agent, prompt: "Do it", timeout: soon }
    next: done
  done: { type: end }
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := Parse([]byte(tt.yaml))
			if err != nil {
				t.Fatal(err)
			}
			if !hasDiagnostic(parsed.Diagnostics, SeverityError, "invalid_duration") {
				t.Fatalf("expected invalid_duration error, got %#v", parsed.Diagnostics)
			}
		})
	}
}

func TestValidDurationsClean(t *testing.T) {
	data := `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: good-durations
start: a
nodes:
  a:
    type: task
    performer: { kind: agent, prompt: "Do it", timeout: 30s }
    retry: { maxAttempts: 2, backoff: 1h30m }
    next: done
  wait-node:
    type: wait
    wait: { duration: 500ms }
    next: done
  done: { type: end }
`
	parsed, err := Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if hasDiagnostic(parsed.Diagnostics, SeverityError, "invalid_duration") {
		t.Fatalf("unexpected invalid_duration for valid durations: %#v", parsed.Diagnostics.Errors())
	}
}

// TestInertParamRefWarnings documents the templating surface: refs outside
// prompt/ask/run/args are inert and warn rather than silently doing nothing.
func TestInertParamRefWarnings(t *testing.T) {
	data := `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: inert-refs
params:
  slow:
    type: string
start: a
nodes:
  a:
    type: task
    performer:
      kind: agent
      prompt: "Use {{ params.slow }}"
      profile: "{{ params.slow }}"
      timeout: "{{ params.slow }}"
    retry:
      maxAttempts: 2
      backoff: "{{ params.slow }}"
    next: waiter
  waiter:
    type: wait
    wait:
      duration: "{{ params.slow }}"
      until: "{{ params.slow }}"
      signal: "{{ params.slow }}"
    next: done
  done: { type: end }
`
	parsed, err := Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	// The declared param is referenced only in inert fields plus a valid prompt,
	// so there must be no errors at all (no undeclared refs, no invalid_duration).
	if parsed.Diagnostics.HasErrors() {
		t.Fatalf("unexpected errors: %#v", parsed.Diagnostics.Errors())
	}
	wantPaths := []string{
		"nodes.a.performer.profile",
		"nodes.a.performer.timeout",
		"nodes.a.retry.backoff",
		"nodes.waiter.wait.duration",
		"nodes.waiter.wait.until",
		"nodes.waiter.wait.signal",
	}
	for _, path := range wantPaths {
		if !hasDiagnosticPath(parsed.Diagnostics, "inert_param_ref", path) {
			t.Fatalf("expected inert_param_ref warning at %q, got %#v", path, parsed.Diagnostics)
		}
	}
}

// TestMergeKeyInNextIsDiagnostic verifies a merge key inside next produces a
// diagnostic instead of hard-failing Parse.
func TestMergeKeyInNextIsDiagnostic(t *testing.T) {
	data := `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: merge-next
start: a
nodes:
  a:
    type: task
    performer: { kind: agent, prompt: "Do it" }
    metadata:
      defaults: &s
        pass: done
    next:
      <<: *s
      fail: done
  done: { type: end }
`
	parsed, err := Parse([]byte(data))
	if err != nil {
		t.Fatalf("merge key in next should not hard-fail Parse: %v", err)
	}
	if !hasDiagnostic(parsed.Diagnostics, SeverityError, "merge_key_unsupported") {
		t.Fatalf("expected merge_key_unsupported diagnostic, got %#v", parsed.Diagnostics)
	}
	// The merge key is skipped, so only the explicit fail edge survives.
	if _, ok := parsed.Template.Nodes["a"].Next["<<"]; ok {
		t.Fatal("merge key should not appear as an outcome")
	}
	if parsed.Template.Nodes["a"].Next["fail"] != "done" {
		t.Fatalf("explicit fail edge missing: %#v", parsed.Template.Nodes["a"].Next)
	}
}

// TestDuplicateKeyLastWins verifies duplicate mapping keys resolve last-wins, so
// the decoded template (and its semantic hash) match standard YAML semantics.
func TestDuplicateKeyLastWins(t *testing.T) {
	data := `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: dup-last-wins
start: a
nodes:
  a:
    type: end
    result: success
    result: failed
`
	parsed, err := Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if !hasDiagnostic(parsed.Diagnostics, SeverityError, "duplicate_key") {
		t.Fatalf("expected duplicate_key diagnostic, got %#v", parsed.Diagnostics)
	}
	if got := parsed.Template.Nodes["a"].Result; got != "failed" {
		t.Fatalf("duplicate key should resolve last-wins; result = %q, want \"failed\"", got)
	}
}

// TestScalarKeyCollisionIsCleanDiagnostic verifies that scalars which render to
// the same string — 1 (!!int) and "1" (!!str) — collide as duplicate keys and
// surface a clean duplicate_key diagnostic rather than hard-failing Decode. The
// model is string-keyed, so the two keys cannot coexist; last-wins pruning keeps
// one and Parse still succeeds.
func TestScalarKeyCollisionIsCleanDiagnostic(t *testing.T) {
	data := `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: scalar-key-tags
start: a
nodes:
  a:
    type: wait
    wait: { duration: 1m }
    metadata:
      1: intkey
      "1": strkey
    next: done
  done: { type: end }
`
	parsed, err := Parse([]byte(data))
	if err != nil {
		t.Fatalf("scalar key collision should not hard-fail Parse: %v", err)
	}
	if !hasDiagnostic(parsed.Diagnostics, SeverityError, "duplicate_key") {
		t.Fatalf("expected duplicate_key diagnostic for 1 vs \"1\": %#v", parsed.Diagnostics)
	}
}

// TestAliasNodeSchemaWalk verifies fields inside alias-defined content are still
// schema-checked (item 5).
func TestAliasNodeSchemaWalk(t *testing.T) {
	data := `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: alias-walk
start: a
nodes:
  a: &shared
    type: task
    performer: { kind: agent, prompt: "Do it" }
    bogus: 1
    next: done
  b: *shared
  done: { type: end }
`
	parsed, err := Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if !hasDiagnosticPath(parsed.Diagnostics, "unknown_field", "nodes.a.bogus") {
		t.Fatalf("expected unknown_field at nodes.a.bogus, got %#v", parsed.Diagnostics)
	}
	if !hasDiagnosticPath(parsed.Diagnostics, "unknown_field", "nodes.b.bogus") {
		t.Fatalf("alias-defined node should be schema-checked; missing unknown_field at nodes.b.bogus: %#v", parsed.Diagnostics)
	}
}

// TestNextAliasResolves confirms an alias to an outcome map is accepted (item 4:
// yaml.v3 resolves aliases before the custom unmarshaler runs).
func TestNextAliasResolves(t *testing.T) {
	data := `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: next-alias
start: a
nodes:
  a:
    type: task
    performer: { kind: agent, prompt: "Do it" }
    metadata:
      edges: &e
        pass: done
        fail: done
    next: *e
  done: { type: end }
`
	parsed, err := Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Diagnostics.HasErrors() {
		t.Fatalf("unexpected errors: %#v", parsed.Diagnostics.Errors())
	}
	assertEdge(t, parsed.Edges, Edge{From: "a", Outcome: "pass", To: "done"})
	assertEdge(t, parsed.Edges, Edge{From: "a", Outcome: "fail", To: "done"})
}

// TestDiagnosticPositions verifies raw-node-walk diagnostics carry source
// positions (item 2).
func TestDiagnosticPositions(t *testing.T) {
	data := `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: positions
start: a
nodes:
  a:
    type: wait
    wait: { duration: 1m }
    bogus: nope
    next: done
  done: { type: end }
`
	parsed, err := Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, diag := range parsed.Diagnostics {
		if diag.Code == "unknown_field" && diag.Path == "nodes.a.bogus" {
			found = true
			if diag.Line <= 0 || diag.Col <= 0 {
				t.Fatalf("expected populated Line/Col, got Line=%d Col=%d", diag.Line, diag.Col)
			}
		}
	}
	if !found {
		t.Fatalf("expected unknown_field at nodes.a.bogus, got %#v", parsed.Diagnostics)
	}
}

func hasDiagnosticPath(diagnostics Diagnostics, code, path string) bool {
	for _, diag := range diagnostics {
		if diag.Code == code && diag.Path == path {
			return true
		}
	}
	return false
}
