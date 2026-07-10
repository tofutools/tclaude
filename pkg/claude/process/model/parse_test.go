package model

import (
	"bytes"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// FuzzCanonicalYAMLRoundTrip pins the editor's lossless Go round-trip
// contract across every parseable template shape the fuzzer discovers.
// Comments are intentionally canonicalized away; modeled content, including
// layout and all name/description/doc fields, must survive exactly while the
// semantic identity remains stable.
func FuzzCanonicalYAMLRoundTrip(f *testing.F) {
	f.Add([]byte(validTemplateYAML))
	f.Add([]byte(`
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: documented
name: Documented template
description: Template description
doc: |
  Template documentation.
params:
  issue:
    type: string
    name: Issue
    description: Issue description
    doc: Issue documentation
start: begin
nodes:
  begin:
    type: start
    name: Begin
    description: Start description
    doc: Start documentation
    next: { pass: done }
  done:
    type: end
    name: Done
    description: End description
    doc: End documentation
    result: success
layout:
  nodes:
    begin: { x: 10.5, y: -4 }
    done: { x: 220, y: 30 }
`))
	f.Fuzz(func(t *testing.T, source []byte) {
		parsed, err := Parse(source)
		if err != nil {
			return
		}
		canonical, err := CanonicalYAML(parsed.Template)
		if err != nil {
			t.Fatalf("canonicalize: %v", err)
		}
		roundTrip, err := Parse(canonical)
		if err != nil {
			t.Fatalf("parse canonical output: %v\n%s", err, canonical)
		}
		if parsed.SemanticHash != roundTrip.SemanticHash {
			t.Fatalf("semantic hash changed: %s != %s", parsed.SemanticHash, roundTrip.SemanticHash)
		}
		if !reflect.DeepEqual(parsed.Template, roundTrip.Template) {
			t.Fatalf("modeled template changed\nbefore: %#v\nafter:  %#v", parsed.Template, roundTrip.Template)
		}
	})
}

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
      contact:
        cadence: 5m
        budget: 3
        escalationTarget: human:operator
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
      retry: implement
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
	contact := parsed.Template.Nodes["implement"].Performer.Contact
	if contact == nil || contact.Cadence != "5m" || contact.Budget != 3 || contact.EscalationTarget != "human:operator" {
		t.Fatalf("contact = %#v", contact)
	}
}

func TestParseAllowsPoisonEscalationRetryCycle(t *testing.T) {
	parsed, err := Parse([]byte(validTemplateYAML))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Diagnostics.HasErrors() {
		t.Fatalf("poison escalation retry diagnostics: %#v", parsed.Diagnostics.Errors())
	}
}

func TestParseRejectsUnsupportedPoisonEscalationChoices(t *testing.T) {
	for _, test := range []struct {
		name string
		data string
	}{
		{name: "extra choice", data: strings.Replace(validTemplateYAML, "      cancel: canceled", "      cancel: canceled\n      ship-anyway: done", 1)},
		{name: "retry targets other node", data: strings.Replace(validTemplateYAML, "      retry: implement", "      retry: done", 1)},
		{name: "cancel targets successful end", data: strings.Replace(validTemplateYAML, "      cancel: canceled", "      cancel: done", 1)},
		{name: "non-reserved incoming edge", data: strings.Replace(validTemplateYAML, "  done:\n", "  intruder:\n    type: task\n    performer: { kind: agent, prompt: intrude }\n    next: { pass: done, fail: escalate }\n  done:\n", 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := Parse([]byte(test.data))
			if err != nil {
				t.Fatal(err)
			}
			if !hasDiagnostic(parsed.Diagnostics, SeverityError, "invalid_poison_escalation") {
				t.Fatalf("diagnostics = %#v", parsed.Diagnostics)
			}
		})
	}
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

func TestCanonicalYAMLIsIdempotent(t *testing.T) {
	parsed, err := Parse([]byte(validTemplateYAML))
	if err != nil {
		t.Fatal(err)
	}
	first, err := CanonicalYAML(parsed.Template)
	if err != nil {
		t.Fatal(err)
	}
	secondParsed, err := Parse(first)
	if err != nil {
		t.Fatal(err)
	}
	second, err := CanonicalYAML(secondParsed.Template)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("canonical YAML is not byte-stable\nfirst:\n%s\nsecond:\n%s", first, second)
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

func TestSemanticHashChangesForSemanticChanges(t *testing.T) {
	left, err := Parse([]byte(validTemplateYAML))
	if err != nil {
		t.Fatal(err)
	}
	rightYAML := strings.ReplaceAll(validTemplateYAML, "profile: dev", "profile: senior-dev")
	right, err := Parse([]byte(rightYAML))
	if err != nil {
		t.Fatal(err)
	}
	if left.SemanticHash == right.SemanticHash {
		t.Fatal("semantic hash should change when performer profile changes")
	}
}

func TestScalarNextRoundTrip(t *testing.T) {
	data := `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: scalar-next
start: a
nodes:
  a:
    type: wait
    wait: { duration: 1m }
    next: done
  done: { type: end }
`
	parsed, err := Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Diagnostics.HasErrors() {
		t.Fatalf("unexpected errors: %#v", parsed.Diagnostics.Errors())
	}
	assertEdge(t, parsed.Edges, Edge{From: "a", Outcome: DefaultOutcome, To: "done"})
	canonical, err := CanonicalYAML(parsed.Template)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(canonical), "next: done") {
		t.Fatalf("canonical scalar next did not stay scalar:\n%s", canonical)
	}
}

func TestFreeformRoundTrip(t *testing.T) {
	data := `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: freeform
params:
  settings:
    type: object
    default:
      enabled: true
      threshold: 1.5
      tags: [a, b]
start: a
nodes:
  a:
    type: wait
    wait: { duration: 1m }
    metadata:
      nested:
        count: 2
        none: null
    next: done
  done: { type: end }
`
	parsed, err := Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Diagnostics.HasErrors() {
		t.Fatalf("unexpected errors: %#v", parsed.Diagnostics.Errors())
	}
	canonical, err := CanonicalYAML(parsed.Template)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := Parse(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if roundTrip.SemanticHash != parsed.SemanticHash {
		t.Fatalf("semantic hash changed after freeform round trip: %s != %s", roundTrip.SemanticHash, parsed.SemanticHash)
	}
}

func TestDiagnosticsOrderIsStable(t *testing.T) {
	data := `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: stable-diagnostics
start: a
nodes:
  a:
    type: task
    performer:
      kind: agent
      prompt: "{{ params.zed }} {{ params.alpha }}"
      args: ["{{ params.mid }}"]
    next: done
  done: { type: end }
`
	var want []string
	for i := 0; i < 50; i++ {
		parsed, err := Parse([]byte(data))
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, diag := range parsed.Diagnostics {
			if diag.Code == "undeclared_param_ref" {
				got = append(got, diag.Path+":"+diag.Message)
			}
		}
		if i == 0 {
			want = got
			continue
		}
		if !slices.Equal(want, got) {
			t.Fatalf("diagnostic order changed\nwant: %#v\ngot:  %#v", want, got)
		}
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
		{
			name: "unknown field",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: typo
start: a
nodes:
  a:
    type: wait
    wait: { duration: 1m }
    nxet: done
`,
			code: "unknown_field",
		},
		{
			name: "non-string freeform key",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: non-string-freeform-key
start: a
nodes:
  a:
    type: wait
    wait: { duration: 1m }
    metadata:
      nested:
        1: one
    next: done
  done: { type: end }
`,
			code: "non_string_freeform_key",
		},
		{
			name: "invalid template id",
			yaml: strings.Replace(validTemplateYAML, "id: code-change-with-review", "id: bad id", 1),
			code: "invalid_id",
		},
		{
			name: "invalid node id",
			yaml: strings.Replace(validTemplateYAML, "  implement:", "  \"\":", 1),
			code: "invalid_id",
		},
		{
			name: "invalid end result",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: invalid-end-result
start: done
nodes:
  done:
    type: end
    result: failled
`,
			code: "invalid_end_result",
		},
		{
			name: "result on non-end node",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: non-end-result
start: a
nodes:
  a:
    type: wait
    wait: { duration: 1m }
    result: failed
    next: done
  done: { type: end }
`,
			code: "result_on_non_end_node",
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

func TestHeaderErrors(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		code string
	}{
		{
			name: "missing id",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
start: a
nodes:
  a: { type: end }
`,
			code: "missing_id",
		},
		{
			name: "unknown start",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: unknown-start
start: missing
nodes:
  a: { type: end }
`,
			code: "unknown_start",
		},
		{
			name: "invalid api version",
			yaml: `
apiVersion: v0
kind: ProcessTemplate
id: invalid-api
start: a
nodes:
  a: { type: end }
`,
			code: "invalid_api_version",
		},
		{
			name: "invalid kind",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: SomethingElse
id: invalid-kind
start: a
nodes:
  a: { type: end }
`,
			code: "invalid_kind",
		},
		{
			name: "missing nodes",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: missing-nodes
start: a
`,
			code: "missing_nodes",
		},
		{
			name: "empty input",
			yaml: ``,
			code: "missing_id",
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

func TestNodeShapeErrors(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		code string
	}{
		{
			name: "missing performer",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: missing-performer
start: a
nodes:
  a: { type: task, next: done }
  done: { type: end }
`,
			code: "missing_performer",
		},
		{
			name: "missing next",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: missing-next
start: a
nodes:
  a:
    type: task
    performer: { kind: agent, prompt: "Do it" }
`,
			code: "missing_next",
		},
		{
			name: "missing wait",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: missing-wait
start: a
nodes:
  a: { type: wait, next: done }
  done: { type: end }
`,
			code: "missing_wait",
		},
		{
			name: "blank wait",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: blank-wait
start: a
nodes:
  a:
    type: wait
    wait: { duration: "   " }
    next: done
  done: { type: end }
`,
			code: "missing_wait",
		},
		{
			name: "end has next",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: end-has-next
start: a
nodes:
  a: { type: end, next: done }
  done: { type: end }
`,
			code: "end_has_next",
		},
		{
			name: "multiple start nodes",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: multiple-start
start: a
nodes:
  a: { type: start, next: done }
  b: { type: start, next: done }
  done: { type: end }
`,
			code: "multiple_start_nodes",
		},
		{
			name: "invalid performer kind",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: invalid-performer
start: a
nodes:
  a:
    type: task
    performer: { kind: robot, prompt: "Do it" }
    next: done
  done: { type: end }
`,
			code: "invalid_performer_kind",
		},
		{
			name: "blank agent prompt",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: blank-agent-prompt
start: a
nodes:
  a:
    type: task
    performer: { kind: agent, prompt: "   " }
    next: done
  done: { type: end }
`,
			code: "missing_prompt",
		},
		{
			name: "blank human ask",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: blank-human-ask
start: a
nodes:
  a:
    type: decision
    performer: { kind: human, ask: "   " }
    next: { approve: done }
  done: { type: end }
`,
			code: "missing_prompt",
		},
		{
			name: "blank program run",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: blank-program-run
start: a
nodes:
  a:
    type: task
    performer: { kind: program, run: "   " }
    next: done
  done: { type: end }
`,
			code: "missing_run",
		},
		{
			name: "missing step id",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: missing-step-id
start: a
nodes:
  a:
    type: task
    performer: { kind: agent, prompt: "Do it" }
    checks:
      - performer: { kind: program, run: go test ./... }
    next: done
  done: { type: end }
`,
			code: "missing_step_id",
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

func TestProseParamRefsAreWarnings(t *testing.T) {
	data := `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: prose-param-ref
description: "Use {{ params.example }} in prompts."
start: a
nodes:
  a:
    type: wait
    wait: { duration: 1m }
    next: done
  done: { type: end }
`
	parsed, err := Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Diagnostics.HasErrors() {
		t.Fatalf("unexpected errors: %#v", parsed.Diagnostics.Errors())
	}
	if !hasDiagnostic(parsed.Diagnostics, SeverityWarning, "undeclared_param_ref") {
		t.Fatalf("expected prose undeclared_param_ref warning, got %#v", parsed.Diagnostics)
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
