package model

import (
	"bytes"
	"strings"
	"testing"
)

const validTemplateYAML = `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: code-change-with-review
name: Code change with review
description: Implement an issue, check it, and review it.
params:
  issue:
    type: string
start: implement
nodes:
  implement:
    type: task
    performer:
      kind: agent
      profile: dev
      prompt: "Implement {{ params.issue }}"
    checks:
      - id: unit-tests
        performer:
          kind: program
          run: go test ./...
    review:
      id: review
      performer:
        kind: agent
        profile: reviewer
        prompt: Review the implementation.
    retry:
      maxAttempts: 3
      backoff: 10m
      onFail: feedback-same-session
    next:
      pass: done
      fail: escalate
  escalate:
    type: decision
    performer:
      kind: human
      ask: "Retries exhausted. Continue?"
    next:
      ship-anyway: done
      cancel: canceled
  done:
    type: end
    result: success
  canceled:
    type: end
    result: canceled
layout:
  nodes:
    implement: { x: 120, y: 80 }
    escalate: { x: 320, y: 200 }
`

func TestParseValidTemplate(t *testing.T) {
	parsed, err := Parse([]byte(validTemplateYAML))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Diagnostics.HasErrors() {
		t.Fatalf("unexpected errors: %#v", parsed.Diagnostics.Errors())
	}
	if len(parsed.Diagnostics.Warnings()) != 0 {
		t.Fatalf("unexpected warnings: %#v", parsed.Diagnostics.Warnings())
	}
	if parsed.SemanticHash == "" {
		t.Fatal("semantic hash is empty")
	}
	if parsed.SourceHash == "" {
		t.Fatal("source hash is empty")
	}
	wantRef := "code-change-with-review@sha256:" + parsed.SemanticHash
	if parsed.Ref != wantRef {
		t.Fatalf("ref = %q, want %q", parsed.Ref, wantRef)
	}

	assertEdge(t, parsed.Edges, Edge{From: "", Outcome: "start", To: "implement"})
	assertEdge(t, parsed.Edges, Edge{From: "implement", Outcome: "pass", To: "done"})
	assertEdge(t, parsed.Edges, Edge{From: "implement", Outcome: "fail", To: "escalate"})
}

func TestCanonicalYAMLRoundTripPreservesSemantics(t *testing.T) {
	parsed, err := Parse([]byte(validTemplateYAML))
	if err != nil {
		t.Fatal(err)
	}
	data, err := CanonicalYAML(parsed.Template)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if roundTrip.Diagnostics.HasErrors() {
		t.Fatalf("round-trip errors: %#v", roundTrip.Diagnostics.Errors())
	}
	if roundTrip.SemanticHash != parsed.SemanticHash {
		t.Fatalf("semantic hash changed after round trip: %s != %s", roundTrip.SemanticHash, parsed.SemanticHash)
	}
	before, err := CanonicalSemanticJSON(parsed.Template)
	if err != nil {
		t.Fatal(err)
	}
	after, err := CanonicalSemanticJSON(roundTrip.Template)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("semantic JSON changed\nbefore: %s\nafter:  %s", before, after)
	}
}

func TestSemanticHashIgnoresLayoutAndComments(t *testing.T) {
	left, err := Parse([]byte(validTemplateYAML))
	if err != nil {
		t.Fatal(err)
	}

	rightYAML := strings.ReplaceAll(validTemplateYAML, "implement: { x: 120, y: 80 }", "implement: { x: 999, y: 80 }")
	rightYAML = "# editor-only comment\n" + rightYAML
	right, err := Parse([]byte(rightYAML))
	if err != nil {
		t.Fatal(err)
	}

	if left.SemanticHash != right.SemanticHash {
		t.Fatalf("semantic hash should ignore layout/comments: %s != %s", left.SemanticHash, right.SemanticHash)
	}
	if left.SourceHash == right.SourceHash {
		t.Fatal("source hash should include raw bytes and differ")
	}
}

func TestLayoutWarningsDoNotMakeTemplateInvalid(t *testing.T) {
	data := strings.ReplaceAll(validTemplateYAML, "escalate: { x: 320, y: 200 }", "removed-node: { x: 320, y: 200 }")
	parsed, err := Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Diagnostics.HasErrors() {
		t.Fatalf("unexpected errors: %#v", parsed.Diagnostics.Errors())
	}
	if !hasDiagnostic(parsed.Diagnostics, SeverityWarning, "stale_layout_node") {
		t.Fatalf("expected stale_layout_node warning, got %#v", parsed.Diagnostics)
	}
}

func TestInvalidTemplates(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		code string
	}{
		{
			name: "unknown edge target",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: unknown-target
start: a
nodes:
  a:
    type: task
    performer: { kind: agent, prompt: "Do it" }
    next: missing
`,
			code: "unknown_target",
		},
		{
			name: "unreachable node",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: unreachable
start: a
nodes:
  a: { type: end }
  b: { type: end }
`,
			code: "unreachable_node",
		},
		{
			name: "graph cycle",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: cycle
start: a
nodes:
  a:
    type: wait
    wait: { duration: 1m }
    next: b
  b:
    type: wait
    wait: { duration: 1m }
    next: a
`,
			code: "graph_cycle",
		},
		{
			name: "undeclared param",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: undeclared-param
start: a
nodes:
  a:
    type: task
    performer: { kind: agent, prompt: "Use {{ params.issue }}" }
    next: done
  done: { type: end }
`,
			code: "undeclared_param_ref",
		},
		{
			name: "retry without budget",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: retry-without-budget
start: a
nodes:
  a:
    type: task
    performer: { kind: agent, prompt: "Do it" }
    retry: { onFail: feedback-same-session }
    next: done
  done: { type: end }
`,
			code: "invalid_retry_budget",
		},
		{
			name: "duplicate node id",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: duplicate
start: a
nodes:
  a: { type: end }
  a: { type: task, performer: { kind: agent, prompt: "Do it" }, next: done }
  done: { type: end }
`,
			code: "duplicate_key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := Parse([]byte(tt.yaml))
			if err != nil {
				t.Fatal(err)
			}
			if !hasDiagnostic(parsed.Diagnostics, SeverityError, tt.code) {
				t.Fatalf("expected error code %q, got %#v", tt.code, parsed.Diagnostics)
			}
		})
	}
}

func assertEdge(t *testing.T, edges []Edge, want Edge) {
	t.Helper()
	for _, edge := range edges {
		if edge == want {
			return
		}
	}
	t.Fatalf("missing edge %#v in %#v", want, edges)
}

func hasDiagnostic(diagnostics Diagnostics, severity Severity, code string) bool {
	for _, diag := range diagnostics {
		if diag.Severity == severity && diag.Code == code {
			return true
		}
	}
	return false
}
