package model

import (
	"strings"
	"testing"
)

func compoundTestNode() Node {
	return Node{
		Type:      NodeTypeTask,
		Performer: &Performer{Kind: PerformerAgent, Prompt: "Implement the change"},
		Plan:      &Step{ID: "plan", Performer: Performer{Kind: PerformerAgent, Prompt: "Plan the change"}, Approval: PlanApprovalHuman},
		Checks: []Step{
			{ID: "tests", Performer: Performer{Kind: PerformerProgram, Run: "go test ./..."}},
			{ID: "lint", Performer: Performer{Kind: PerformerProgram, Run: "golangci-lint run"}},
		},
		Review: &Step{ID: "review", Performer: Performer{Kind: PerformerAgent, Profile: "reviewer", Prompt: "Cold-review the diff"}},
		Retry:  &RetryPolicy{MaxAttempts: 3},
		Next:   Next{"pass": "done", "fail": "escalate"},
	}
}

func TestExpandNodeDerivesCanonicalStageChain(t *testing.T) {
	specs := ExpandNode("implement", compoundTestNode())
	want := []struct {
		id     string
		stage  StageKind
		stepID string
	}{
		{"implement.plan", StagePlan, ""},
		{"implement.plan.approval", StagePlanApproval, ""},
		{"implement.do", StageDo, ""},
		{"implement.test.tests", StageTest, "tests"},
		{"implement.test.lint", StageTest, "lint"},
		{"implement.review", StageReview, ""},
		{"implement.done", StageDone, ""},
	}
	if len(specs) != len(want) {
		t.Fatalf("specs = %d, want %d: %#v", len(specs), len(want), specs)
	}
	for i, spec := range specs {
		if spec.ChildID != want[i].id || spec.Stage != want[i].stage || spec.StepID != want[i].stepID {
			t.Fatalf("specs[%d] = %#v, want %+v", i, spec, want[i])
		}
	}
	if specs[2].Performer == nil || specs[2].Performer.Prompt != "Implement the change" {
		t.Fatalf("do stage must carry the node performer: %#v", specs[2].Performer)
	}
	if specs[2].Retry == nil || specs[2].Retry.MaxAttempts != 3 {
		t.Fatalf("do stage must carry the node retry budget: %#v", specs[2].Retry)
	}
	if specs[1].Performer == nil || specs[1].Performer.Kind != PerformerHuman || !strings.Contains(specs[1].Performer.Ask, "implement") {
		t.Fatalf("plan approval must synthesize a human gate: %#v", specs[1].Performer)
	}
	if specs[6].Performer != nil {
		t.Fatalf("done stage must not have a performer: %#v", specs[6].Performer)
	}
}

func TestExpandNodeSkipsOptionalStages(t *testing.T) {
	node := compoundTestNode()
	node.Plan = nil
	node.Review = nil
	specs := ExpandNode("implement", node)
	got := make([]string, 0, len(specs))
	for _, spec := range specs {
		got = append(got, spec.ChildID)
	}
	want := "implement.do implement.test.tests implement.test.lint implement.done"
	if strings.Join(got, " ") != want {
		t.Fatalf("children = %q, want %q", strings.Join(got, " "), want)
	}
}

func TestExpandNodeAutoApprovalHasNoApprovalGate(t *testing.T) {
	node := compoundTestNode()
	node.Plan.Approval = PlanApprovalAuto
	for _, spec := range ExpandNode("implement", node) {
		if spec.Stage == StagePlanApproval {
			t.Fatalf("auto approval must not derive an approval gate")
		}
	}
}

func TestExpandNodePlainTaskIsNotCompound(t *testing.T) {
	node := Node{Type: NodeTypeTask, Performer: &Performer{Kind: PerformerHuman, Ask: "do it"}, Next: Next{"pass": "done"}}
	if node.IsCompound() {
		t.Fatal("plain task must not be compound")
	}
	if specs := ExpandNode("implement", node); specs != nil {
		t.Fatalf("plain task must not expand: %#v", specs)
	}
}

func TestValidateCompoundTemplateAdditions(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		code string
	}{
		{
			name: "approval on check step",
			yaml: compoundYAML("auto", "tests", "lint", "approval: human", "fresh-attempt", ""),
			code: "approval_on_non_plan_step",
		},
		{
			name: "invalid plan approval value",
			yaml: compoundYAML("sometimes", "tests", "lint", "", "fresh-attempt", ""),
			code: "invalid_plan_approval",
		},
		{
			name: "invalid retry mode",
			yaml: compoundYAML("human", "tests", "lint", "", "sometimes", ""),
			code: "invalid_retry_mode",
		},
		{
			name: "duplicate check step ids",
			yaml: compoundYAML("human", "tests", "tests", "", "fresh-attempt", ""),
			code: "duplicate_step_id",
		},
		{
			name: "step id charset",
			yaml: compoundYAML("human", "Tests", "lint", "", "fresh-attempt", ""),
			code: "invalid_id",
		},
		{
			name: "authored node collides with derived child",
			yaml: compoundYAML("human", "tests", "lint", "", "fresh-attempt", "implement.do:\n    type: end"),
			code: "node_id_collides_with_expansion",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := Parse([]byte(tt.yaml))
			if err != nil {
				t.Fatal(err)
			}
			if !hasDiagnostic(parsed.Diagnostics, SeverityError, tt.code) {
				t.Fatalf("missing %q diagnostic:\n%#v", tt.code, parsed.Diagnostics)
			}
		})
	}

	parsed, err := Parse([]byte(compoundYAML("human", "tests", "lint", "", "feedback-same-session", "")))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Diagnostics.HasErrors() {
		t.Fatalf("valid compound template must not error:\n%#v", parsed.Diagnostics.Errors())
	}
}

func TestValidateFlagsDerivedChildCollisionAcrossCompoundNodes(t *testing.T) {
	// Node ids may contain dots: node "a" with check id "do" derives
	// "a.test.do", and a compound node authored as "a.test" derives the same
	// id for its do stage. Undetected, the second expansion would be rejected
	// at runtime and wedge the run.
	body := `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: collide-demo
start: a
nodes:
  a:
    type: task
    performer: { kind: agent, prompt: work }
    checks:
      - id: do
        performer: { kind: program, run: check }
    next: { pass: a.test }
  a.test:
    type: task
    performer: { kind: agent, prompt: work }
    checks:
      - id: other
        performer: { kind: program, run: check }
    next: { pass: done }
  done:
    type: end
`
	parsed, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if !hasDiagnostic(parsed.Diagnostics, SeverityError, "node_id_collides_with_expansion") {
		t.Fatalf("derived-vs-derived collision must be flagged:\n%#v", parsed.Diagnostics)
	}
}

func TestValidateWarnsOnUnhonoredGateRetry(t *testing.T) {
	node := compoundTestNode()
	node.Checks[0].Retry = &RetryPolicy{MaxAttempts: 3}
	node.Review.Retry = &RetryPolicy{MaxAttempts: 2}
	tmpl := &Template{
		APIVersion: APIVersion,
		Kind:       Kind,
		ID:         "gate-retry-demo",
		Start:      "implement",
		Nodes: map[string]Node{
			"implement": func() Node { node.Next = Next{"pass": "done", "fail": "escalate"}; return node }(),
			"done":      {Type: NodeTypeEnd},
			"escalate":  {Type: NodeTypeEnd, Result: "failed"},
		},
	}
	diagnostics := Validate(tmpl, NormalizeEdges(tmpl))
	if diagnostics.HasErrors() {
		t.Fatalf("gate retry must stay valid: %#v", diagnostics.Errors())
	}
	count := 0
	for _, diag := range diagnostics.Warnings() {
		if diag.Code == "gate_retry_not_yet_honored" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("want 2 gate retry warnings, got %d: %#v", count, diagnostics)
	}
}

func compoundYAML(approval, check1, check2, check2Extra, onFail, extraNode string) string {
	body := `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: compound-demo
start: implement
nodes:
  implement:
    type: task
    performer:
      kind: agent
      prompt: Implement it
    plan:
      id: plan
      approval: ` + approval + `
      performer:
        kind: agent
        prompt: Plan it
    checks:
      - id: ` + check1 + `
        performer:
          kind: program
          run: go test ./...
      - id: ` + check2 + "\n"
	if check2Extra != "" {
		body += "        " + check2Extra + "\n"
	}
	body += `        performer:
          kind: program
          run: golangci-lint run
    retry:
      maxAttempts: 2
      onFail: ` + onFail + `
    next:
      pass: done
`
	if extraNode != "" {
		body += "  " + extraNode + "\n"
	}
	return body + `  done:
    type: end
`
}
