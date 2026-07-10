package model

// Tests for the kind-scoped performer fields (choices/assignee on human,
// model/effort on agent) and node capture names added for the node edit
// dialogs (TCL-298). The dialog's discipline rule lives here: a field set on
// the wrong performer kind is an authoring ERROR, never silently carried.

import (
	"reflect"
	"strings"
	"testing"
)

const kindScopedTemplateYAML = `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: kind-scoped-fields
start: implement
nodes:
  implement:
    type: task
    performer:
      kind: agent
      profile: dev
      prompt: Implement the change
      model: opus
      effort: high
    captures:
      - diff
      - test-report
    review:
      id: sign-off
      performer:
        kind: human
        profile: operator
        ask: Ship it?
        choices:
          - ship
          - hold
        assignee: johan
    next:
      pass: done
  done:
    type: end
    result: success
`

func TestKindScopedFieldsParseAndRoundTrip(t *testing.T) {
	parsed, err := Parse([]byte(kindScopedTemplateYAML))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Diagnostics.HasErrors() {
		t.Fatalf("valid kind-scoped template reported errors: %#v", parsed.Diagnostics)
	}
	implement := parsed.Template.Nodes["implement"]
	if implement.Performer.Model != "opus" || implement.Performer.Effort != "high" {
		t.Fatalf("agent model/effort not decoded: %#v", implement.Performer)
	}
	if !reflect.DeepEqual(implement.Captures, []string{"diff", "test-report"}) {
		t.Fatalf("captures not decoded: %#v", implement.Captures)
	}
	review := implement.Review.Performer
	if !reflect.DeepEqual(review.Choices, []string{"ship", "hold"}) || review.Assignee != "johan" {
		t.Fatalf("human choices/assignee not decoded: %#v", review)
	}

	canonical, err := CanonicalYAML(parsed.Template)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"model: opus", "effort: high", "captures:", "- diff", "- test-report", "choices:", "- ship", "assignee: johan"} {
		if !strings.Contains(string(canonical), needle) {
			t.Errorf("canonical YAML missing %q:\n%s", needle, canonical)
		}
	}
	roundTrip, err := Parse(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.SemanticHash != roundTrip.SemanticHash {
		t.Fatalf("semantic hash changed across round trip: %s != %s", parsed.SemanticHash, roundTrip.SemanticHash)
	}
	if !reflect.DeepEqual(parsed.Template, roundTrip.Template) {
		t.Fatalf("modeled template changed\nbefore: %#v\nafter:  %#v", parsed.Template, roundTrip.Template)
	}
}

// TestKindScopedFieldsDoNotDisturbExistingHashes pins that a template which
// uses none of the new optional fields keeps its pre-existing semantic hash
// shape (omitempty fields must not appear in the canonical semantic JSON).
func TestKindScopedFieldsAbsentFromCanonicalJSONWhenUnset(t *testing.T) {
	parsed, err := Parse([]byte(validTemplateYAML))
	if err != nil {
		t.Fatal(err)
	}
	data, err := CanonicalSemanticJSON(parsed.Template)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"choices", "assignee", "effort", "captures", `"model"`} {
		if strings.Contains(string(data), field) {
			t.Errorf("canonical semantic JSON leaks unset field %s: %s", field, data)
		}
	}
}

func TestKindScopedFieldErrors(t *testing.T) {
	template := func(performer string) string {
		return `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: wrong-kind
start: a
nodes:
  a:
    type: task
    performer: { ` + performer + ` }
    next: done
  done: { type: end }
`
	}
	tests := []struct {
		name      string
		yaml      string
		code      string
		pathHint  string
		wantError bool
	}{
		{
			name:      "choices on agent",
			yaml:      template(`kind: agent, prompt: "Do it", choices: [a, b]`),
			code:      "kind_scoped_field",
			pathHint:  "choices",
			wantError: true,
		},
		{
			name:      "assignee on program",
			yaml:      template(`kind: program, run: "true", assignee: johan`),
			code:      "kind_scoped_field",
			pathHint:  "assignee",
			wantError: true,
		},
		{
			name:      "model on human",
			yaml:      template(`kind: human, ask: "Approve?", model: opus`),
			code:      "kind_scoped_field",
			pathHint:  "model",
			wantError: true,
		},
		{
			name:      "effort on program",
			yaml:      template(`kind: program, run: "true", effort: high`),
			code:      "kind_scoped_field",
			pathHint:  "effort",
			wantError: true,
		},
		{
			name:      "blank choice",
			yaml:      template(`kind: human, ask: "Approve?", choices: ["ok", "  "]`),
			code:      "invalid_choice",
			pathHint:  "choices[1]",
			wantError: true,
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
			found := false
			for _, diagnostic := range parsed.Diagnostics {
				if diagnostic.Code == tt.code && strings.Contains(diagnostic.Path, tt.pathHint) {
					found = true
				}
			}
			if !found {
				t.Fatalf("expected %q diagnostic path to contain %q, got %#v", tt.code, tt.pathHint, parsed.Diagnostics)
			}
		})
	}
}

func TestCaptureErrors(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		code string
	}{
		{
			name: "captures on non-task node",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: captures-on-decision
start: a
nodes:
  a:
    type: decision
    performer: { kind: human, ask: "Which way?" }
    captures: [notes]
    next:
      yes: done
  done: { type: end }
`,
			code: "captures_on_non_task_node",
		},
		{
			name: "invalid capture name",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: captures-bad-name
start: a
nodes:
  a:
    type: task
    performer: { kind: agent, prompt: "Do it" }
    captures: ["Not A Name"]
    next: done
  done: { type: end }
`,
			code: "invalid_id",
		},
		{
			name: "duplicate capture name",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: captures-duplicate
start: a
nodes:
  a:
    type: task
    performer: { kind: agent, prompt: "Do it" }
    captures: [diff, diff]
    next: done
  done: { type: end }
`,
			code: "duplicate_capture",
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

// The new fields are literal (never interpolated); a param reference in them
// must warn like profile/timeout do.
func TestInertParamRefWarningsOnKindScopedFields(t *testing.T) {
	yaml := `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: inert-refs
params:
  who: { type: string }
start: a
nodes:
  a:
    type: task
    performer:
      kind: agent
      prompt: "Do {{ params.who }}"
      model: "{{ params.who }}"
      effort: "{{ params.who }}"
    next: done
  done: { type: end }
`
	parsed, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	warned := map[string]bool{}
	for _, diagnostic := range parsed.Diagnostics {
		if diagnostic.Code == "inert_param_ref" {
			warned[diagnostic.Path] = true
		}
	}
	for _, path := range []string{"nodes.a.performer.model", "nodes.a.performer.effort"} {
		if !warned[path] {
			t.Errorf("expected inert_param_ref warning at %s, got %#v", path, parsed.Diagnostics)
		}
	}
}
