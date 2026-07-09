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
    next: waiter
  waiter:
    type: wait
    wait: { duration: 500ms }
    next: done
  done: { type: end }
`
	parsed, err := Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Diagnostics.HasErrors() {
		t.Fatalf("unexpected errors for valid durations: %#v", parsed.Diagnostics.Errors())
	}
}

// TestNonPositiveDurations pins that zero and negative durations are rejected
// (F2) — they parse cleanly but are authoring nonsense.
func TestNonPositiveDurations(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{
			name: "negative timeout",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: neg-timeout
start: a
nodes:
  a:
    type: task
    performer: { kind: agent, prompt: "Do it", timeout: -5s }
    next: done
  done: { type: end }
`,
		},
		{
			name: "zero backoff",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: zero-backoff
start: a
nodes:
  a:
    type: task
    performer: { kind: agent, prompt: "Do it" }
    retry: { maxAttempts: 2, backoff: 0s }
    next: done
  done: { type: end }
`,
		},
		{
			name: "negative wait duration",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: neg-wait
start: a
nodes:
  a:
    type: wait
    wait: { duration: -1m }
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
				t.Fatalf("expected invalid_duration for non-positive duration, got %#v", parsed.Diagnostics)
			}
		})
	}
}

// TestParamRefInDurationIsError pins F1: a param reference in a non-templatable
// duration field is a hard error (it can never parse), not merely an inert
// warning.
func TestParamRefInDurationIsError(t *testing.T) {
	data := `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: param-ref-duration
params:
  d:
    type: string
start: a
nodes:
  a:
    type: wait
    wait: { duration: "{{ params.d }}" }
    next: done
  done: { type: end }
`
	parsed, err := Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if !hasDiagnosticPath(parsed.Diagnostics, "invalid_duration", "nodes.a.wait.duration") {
		t.Fatalf("expected invalid_duration for param ref in duration, got %#v", parsed.Diagnostics)
	}
	// The inert warning should also fire, explaining why interpolation won't run.
	if !hasDiagnosticPath(parsed.Diagnostics, "inert_param_ref", "nodes.a.wait.duration") {
		t.Fatalf("expected inert_param_ref alongside invalid_duration, got %#v", parsed.Diagnostics)
	}
}

// TestInertParamRefWarnings documents the templating surface: refs in inert
// (non-templatable, non-duration) fields warn rather than silently doing
// nothing, and do not by themselves make the template invalid.
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
    next: waiter
  waiter:
    type: wait
    wait:
      until: "{{ params.slow }}"
      signal: "{{ params.slow }}"
    next: done
  done: { type: end }
`
	parsed, err := Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	// profile/until/signal are inert but not duration fields, so the only
	// diagnostics are inert warnings — no errors.
	if parsed.Diagnostics.HasErrors() {
		t.Fatalf("unexpected errors: %#v", parsed.Diagnostics.Errors())
	}
	wantPaths := []string{
		"nodes.a.performer.profile",
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

// TestMergeKeyOutsideNextIsDiagnostic verifies a merge key in a schema-checked
// mapping (here a node) is reported as merge_key_unsupported, not the misleading
// "unknown field" it produced before (F3). Decode silently applies such a merge,
// so the correct message matters.
func TestMergeKeyOutsideNextIsDiagnostic(t *testing.T) {
	data := `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: merge-outside-next
start: a
nodes:
  a:
    type: task
    performer: &p
      kind: agent
      prompt: "Do it"
    next: done
  b:
    <<: *p
    type: end
  done: { type: end }
`
	parsed, err := Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if !hasDiagnosticPath(parsed.Diagnostics, "merge_key_unsupported", "nodes.b.<<") {
		t.Fatalf("expected merge_key_unsupported at nodes.b.<<, got %#v", parsed.Diagnostics)
	}
	if hasDiagnosticPath(parsed.Diagnostics, "unknown_field", "nodes.b.<<") {
		t.Fatalf("merge key should not be reported as unknown_field: %#v", parsed.Diagnostics)
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
// one and Parse still succeeds. Both orders are covered: when the surviving key
// is the int scalar, yaml.v3 stringifies it into the map[string]any target.
func TestScalarKeyCollisionIsCleanDiagnostic(t *testing.T) {
	tests := []struct {
		name    string
		keyPair string
	}{
		{name: "int first, str last", keyPair: "      1: intkey\n      \"1\": strkey"},
		{name: "str first, int last", keyPair: "      \"1\": strkey\n      1: intkey"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
` + tt.keyPair + `
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
		})
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
