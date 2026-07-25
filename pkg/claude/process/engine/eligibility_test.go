package engine

import (
	"errors"
	"fmt"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

func TestSequentialProgramTemplateIsEligible(t *testing.T) {
	if diagnostics := CheckEligibility(sequentialTemplate("one", "two")); len(diagnostics) != 0 {
		t.Fatalf("eligibility diagnostics = %#v", diagnostics)
	}
}

func TestExclusiveDecisionTemplatesAreEligible(t *testing.T) {
	for _, tmpl := range []*model.Template{
		decisionDiamondTemplate(),
		decisionDirectMergeTemplate(),
		decisionMultiEndTemplate(),
	} {
		assertAuthoringValid(t, tmpl)
		if diagnostics := CheckEligibility(tmpl); len(diagnostics) != 0 {
			t.Fatalf("eligibility diagnostics for %q = %#v", tmpl.ID, diagnostics)
		}
	}
}

// TestStructuredParallelTemplatesAreEligible pins the admission rule: a fan-out
// shape is executable exactly where the static authoring parallel-scope
// validator already says it is structured and valid.
func TestStructuredParallelTemplatesAreEligible(t *testing.T) {
	for _, tmpl := range []*model.Template{
		parallelTemplate(),
		fanOutTemplate("left", "right"),
		fanOutTemplate("one", "two", "three", "four"),
		nestedFanOutTemplate(),
		wideFanOutTemplate(12),
	} {
		assertAuthoringValid(t, tmpl)
		if diagnostics := CheckEligibility(tmpl); len(diagnostics) != 0 {
			t.Fatalf("eligibility diagnostics for %q = %#v", tmpl.ID, diagnostics)
		}
		if _, err := Prepare(tmpl, nil); err != nil {
			t.Fatalf("prepare %q: %v", tmpl.ID, err)
		}
	}
}

// TestUnstructuredParallelStaysIneligible proves admission is delegated to the
// static scope validator rather than re-derived: a fork whose branches never
// reduce at one complete structural join is refused, and the refusal keeps the
// authoring validator's own diagnostic code.
func TestUnstructuredParallelStaysIneligible(t *testing.T) {
	tmpl := parallelTemplate()
	// Let the left branch run straight to its own end, so the fork's branches
	// never converge on a single complete reducer.
	tmpl.Nodes["left"] = programTask("escaped", "left")
	tmpl.Nodes["escaped"] = model.Node{Type: model.NodeTypeEnd}
	right := tmpl.Nodes["right"]
	right.Next = model.Next{model.DefaultOutcome: "join"}
	tmpl.Nodes["right"] = right
	tmpl.Nodes["join"] = model.Node{Type: model.NodeTypeEnd}

	if !hasCode(CheckEligibility(tmpl), "cross_scope_join_v1") {
		t.Fatalf("unstructured fan-out was admitted: %#v", CheckEligibility(tmpl))
	}
	if _, err := Prepare(tmpl, nil); err == nil {
		t.Fatal("Prepare admitted an unstructured fan-out")
	}
}

// TestBoundedProgramRetryIsEligible pins the one retry shape this engine
// executes: a plain program task with a positive budget, no backoff wait, and
// fresh-attempt semantics — including the default the authoring model resolves
// an unset onFail to.
func TestBoundedProgramRetryIsEligible(t *testing.T) {
	for _, retry := range []*model.RetryPolicy{
		{MaxAttempts: 1},
		{MaxAttempts: 3},
		{MaxAttempts: 3, OnFail: model.RetryModeFreshAttempt},
	} {
		tmpl := retryTemplate(retry)
		assertAuthoringValid(t, tmpl)
		if diagnostics := CheckEligibility(tmpl); len(diagnostics) != 0 {
			t.Fatalf("eligibility diagnostics for %#v = %#v", retry, diagnostics)
		}
		definition, err := Prepare(tmpl, nil)
		if err != nil {
			t.Fatalf("prepare %#v: %v", retry, err)
		}
		if got := definition.nodes[definition.index["task"]].maxAttempts; got != retry.MaxAttempts {
			t.Fatalf("prepared budget = %d, want the authored %d", got, retry.MaxAttempts)
		}
	}
	// A task with no authored retry keeps the fail-fast budget of one attempt.
	definition := mustPrepare(t, sequentialTemplate("task"), nil)
	if got := definition.nodes[definition.index["task"]].maxAttempts; got != 1 {
		t.Fatalf("default budget = %d, want 1", got)
	}
}

// TestExecutableRetryBudgetIsCapped pins the ceiling exactly at its boundary.
// Every executable retry here is immediate — backoff is ineligible and the next
// attempt commits with the failed observation — so an unbounded budget is an
// unthrottled spawn loop with no run-cancel verb to stop it.
//
// The gate is eligibility, not authoring: a template that reaches Prepare by
// some other route still fails closed, which is what stops an over-budget
// definition from ever backing a run.
func TestExecutableRetryBudgetIsCapped(t *testing.T) {
	atCap := retryTemplate(&model.RetryPolicy{MaxAttempts: maxExecutableRetryAttempts})
	assertAuthoringValid(t, atCap)
	if diagnostics := CheckEligibility(atCap); len(diagnostics) != 0 {
		t.Fatalf("the budget at the cap was refused: %#v", diagnostics)
	}
	definition, err := Prepare(atCap, nil)
	if err != nil {
		t.Fatalf("prepare at the cap: %v", err)
	}
	if got := definition.nodes[definition.index["task"]].maxAttempts; got != maxExecutableRetryAttempts {
		t.Fatalf("prepared budget = %d, want the cap %d", got, maxExecutableRetryAttempts)
	}

	for _, budget := range []int{maxExecutableRetryAttempts + 1, 1_000_000} {
		overCap := retryTemplate(&model.RetryPolicy{MaxAttempts: budget})
		// Authoring still accepts it: this is an execution capability limit, not a
		// redefinition of what templates are valid to edit.
		assertAuthoringValid(t, overCap)
		diagnostics := CheckEligibility(overCap)
		if !hasCodeAtPath(diagnostics, "unsupported_retry", "nodes.task.retry.maxAttempts") {
			t.Fatalf("budget %d was admitted: %#v", budget, diagnostics)
		}
		if _, err := Prepare(overCap, nil); !errors.Is(err, ErrTemplateIneligible) {
			t.Fatalf("Prepare admitted budget %d: %v", budget, err)
		}
	}

	// With no operator retry the authored cap is also what the load boundary
	// admits: validateAttempts compares against the node's CURRENT ceiling,
	// which is the authored budget until an operator raises it, so eligibility
	// and the boundary cannot disagree about an unretried run's attempt number.
	checkpoint, err := Initialize("run-capped", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint.Nodes["task"] = NodeRunning
	checkpoint.Attempts = map[string]int{"task": maxExecutableRetryAttempts}
	checkpoint.Commands = []Command{programCommand(checkpoint.RunID,
		definition.nodes[definition.index["task"]], maxExecutableRetryAttempts)}
	if err := ValidateCheckpoint(checkpoint, definition); err != nil {
		t.Fatalf("an attempt at the cap must load: %v", err)
	}
	checkpoint.Attempts["task"] = maxExecutableRetryAttempts + 1
	if err := ValidateCheckpoint(checkpoint, definition); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("an attempt past the cap loaded: %v", err)
	}
}

func TestEligibilityRejectsUnsupportedAuthoringValidFeatures(t *testing.T) {
	tests := []struct {
		name string
		code string
		tmpl func() *model.Template
	}{
		{
			name: "agent decider",
			code: "unsupported_performer",
			tmpl: func() *model.Template {
				tmpl := decisionDiamondTemplate()
				node := tmpl.Nodes["choose"]
				node.Performer = &model.Performer{Kind: model.PerformerAgent, Prompt: "Decide"}
				tmpl.Nodes["choose"] = node
				return tmpl
			},
		},
		{
			name: "decider contact schedule",
			code: "unsupported_contact",
			tmpl: func() *model.Template {
				tmpl := decisionDiamondTemplate()
				node := tmpl.Nodes["choose"]
				node.Performer = &model.Performer{Kind: model.PerformerHuman, Ask: "Continue?",
					Contact: &model.ContactSchedule{Cadence: "1h", Budget: 3, EscalationTarget: "operator"}}
				tmpl.Nodes["choose"] = node
				return tmpl
			},
		},
		{
			name: "wait",
			code: "unsupported_wait",
			tmpl: func() *model.Template {
				tmpl := sequentialTemplate("task")
				tmpl.Nodes["start"] = model.Node{Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "wait"}}
				tmpl.Nodes["wait"] = model.Node{Type: model.NodeTypeWait, Wait: &model.WaitConfig{Duration: "1s"}, Next: model.Next{model.DefaultOutcome: "task"}}
				return tmpl
			},
		},
		{
			name: "retry backoff wait",
			code: "unsupported_retry",
			tmpl: func() *model.Template {
				return retryTemplate(&model.RetryPolicy{MaxAttempts: 2, Backoff: "30s"})
			},
		},
		{
			name: "same-session retry",
			code: "unsupported_retry",
			tmpl: func() *model.Template {
				return retryTemplate(&model.RetryPolicy{MaxAttempts: 2, OnFail: model.RetryModeFeedbackSameSession})
			},
		},
		{
			name: "retry on a decision node",
			code: "unsupported_retry",
			tmpl: func() *model.Template {
				tmpl := decisionDiamondTemplate()
				node := tmpl.Nodes["choose"]
				node.Retry = &model.RetryPolicy{MaxAttempts: 2}
				tmpl.Nodes["choose"] = node
				return tmpl
			},
		},
		{
			// Only a task node derives stages, so stages anywhere else are still a
			// capability this engine has no meaning for at all.
			name: "compound stages on a non-task node",
			code: "unsupported_compound_stages",
			tmpl: func() *model.Template {
				tmpl := decisionDiamondTemplate()
				node := tmpl.Nodes["choose"]
				node.Plan = &model.Step{ID: "plan", Performer: model.Performer{Kind: model.PerformerProgram, Run: "plan"}}
				tmpl.Nodes["choose"] = node
				return tmpl
			},
		},
		{
			name: "human plan approval gate",
			code: "unsupported_approval",
			tmpl: func() *model.Template {
				return compoundStageTemplate(func(node *model.Node) {
					node.Plan.Approval = model.PlanApprovalHuman
				})
			},
		},
		{
			name: "human plan stage performer",
			code: "unsupported_performer",
			tmpl: func() *model.Template {
				return compoundStageTemplate(func(node *model.Node) {
					node.Plan.Performer = model.Performer{Kind: model.PerformerHuman, Ask: "Plan it?"}
				})
			},
		},
		{
			name: "agent check stage performer",
			code: "unsupported_performer",
			tmpl: func() *model.Template {
				return compoundStageTemplate(func(node *model.Node) {
					node.Checks[0].Performer = model.Performer{Kind: model.PerformerAgent, Prompt: "Check it"}
				})
			},
		},
		{
			name: "human review stage performer",
			code: "unsupported_performer",
			tmpl: func() *model.Template {
				return compoundStageTemplate(func(node *model.Node) {
					node.Review.Performer = model.Performer{Kind: model.PerformerHuman, Ask: "Ship it?"}
				})
			},
		},
		{
			name: "stage performer contact schedule",
			code: "unsupported_contact",
			tmpl: func() *model.Template {
				return compoundStageTemplate(func(node *model.Node) {
					node.Review.Performer.Contact = &model.ContactSchedule{
						Cadence: "1h", Budget: 3, EscalationTarget: "operator"}
				})
			},
		},
		{
			name: "plan stage retry",
			code: "unsupported_retry",
			tmpl: func() *model.Template {
				return compoundStageTemplate(func(node *model.Node) {
					node.Plan.Retry = &model.RetryPolicy{MaxAttempts: 2}
				})
			},
		},
		{
			name: "check stage retry",
			code: "unsupported_retry",
			tmpl: func() *model.Template {
				return compoundStageTemplate(func(node *model.Node) {
					node.Checks[0].Retry = &model.RetryPolicy{MaxAttempts: 2}
				})
			},
		},
		{
			name: "review stage retry",
			code: "unsupported_retry",
			tmpl: func() *model.Template {
				return compoundStageTemplate(func(node *model.Node) {
					node.Review.Retry = &model.RetryPolicy{MaxAttempts: 2}
				})
			},
		},
		{
			name: "plan approval retry",
			code: "unsupported_retry",
			tmpl: func() *model.Template {
				return compoundStageTemplate(func(node *model.Node) {
					node.Plan.Approval = model.PlanApprovalHuman
					node.Plan.ApprovalRetry = &model.RetryPolicy{MaxAttempts: 2}
				})
			},
		},
		{
			name: "compound routing to more than one outcome",
			code: "unsupported_routing",
			tmpl: func() *model.Template {
				tmpl := compoundStageTemplate(nil)
				node := tmpl.Nodes["build"]
				node.Next = model.Next{"pass": "end", "fail": "end"}
				tmpl.Nodes["build"] = node
				return tmpl
			},
		},
		{
			name: "expansion above the executable node ceiling",
			code: "expanded_node_limit",
			tmpl: func() *model.Template {
				tmpl := compoundStageTemplate(func(node *model.Node) {
					// start, end, the parent, and its plan/do/review/done stages
					// already account for seven; the checks carry it over the top.
					checks := make([]model.Step, 0, model.MaxNormalizedNodes)
					for index := range model.MaxNormalizedNodes {
						checks = append(checks, model.Step{
							ID:        fmt.Sprintf("check%04d", index),
							Performer: model.Performer{Kind: model.PerformerProgram, Run: "check"},
						})
					}
					node.Checks = checks
				})
				return tmpl
			},
		},
		{
			name: "agent performer",
			code: "unsupported_performer",
			tmpl: func() *model.Template {
				tmpl := sequentialTemplate("task")
				node := tmpl.Nodes["task"]
				node.Performer = &model.Performer{Kind: model.PerformerAgent, Prompt: "Do it"}
				tmpl.Nodes["task"] = node
				return tmpl
			},
		},
		{
			name: "human performer",
			code: "unsupported_performer",
			tmpl: func() *model.Template {
				tmpl := sequentialTemplate("task")
				node := tmpl.Nodes["task"]
				node.Performer = &model.Performer{Kind: model.PerformerHuman, Ask: "Do it?"}
				tmpl.Nodes["task"] = node
				return tmpl
			},
		},
		{
			name: "multiple routing",
			code: "unsupported_routing",
			tmpl: multipleRoutingTemplate,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tmpl := test.tmpl()
			assertAuthoringValid(t, tmpl)
			diagnostics := CheckEligibility(tmpl)
			if !hasCode(diagnostics, test.code) {
				t.Fatalf("missing %q in %#v", test.code, diagnostics)
			}
		})
	}
}

func TestEligibilityPreservesPreciseAuthoringFailures(t *testing.T) {
	tests := []struct {
		name string
		code string
		tmpl func() *model.Template
	}{
		{
			name: "missing route",
			code: "missing_next",
			tmpl: func() *model.Template {
				tmpl := sequentialTemplate("task")
				node := tmpl.Nodes["task"]
				node.Next = nil
				tmpl.Nodes["task"] = node
				return tmpl
			},
		},
		{
			name: "cycle",
			code: "graph_cycle",
			tmpl: func() *model.Template {
				tmpl := sequentialTemplate("task")
				node := tmpl.Nodes["task"]
				node.Next = model.Next{model.DefaultOutcome: "start"}
				tmpl.Nodes["task"] = node
				delete(tmpl.Nodes, "end")
				return tmpl
			},
		},
		{
			name: "unknown target",
			code: "unknown_target",
			tmpl: func() *model.Template {
				tmpl := sequentialTemplate("task")
				node := tmpl.Nodes["task"]
				node.Next = model.Next{model.DefaultOutcome: "missing"}
				tmpl.Nodes["task"] = node
				return tmpl
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			diagnostics := CheckEligibility(test.tmpl())
			if !hasCode(diagnostics, test.code) {
				t.Fatalf("missing %q in %#v", test.code, diagnostics)
			}
		})
	}
}

func parallelTemplate() *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion,
		Kind:       model.Kind,
		ID:         "parallel",
		Start:      "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "fork"}},
			"fork":  {Type: model.NodeTypeParallel, Next: model.Next{"left": "left", "right": "right"}},
			"left":  programTask("join", "left"),
			"right": programTask("join", "right"),
			"join":  {Type: model.NodeTypeEnd, Join: model.JoinAll},
		},
	}
}

func multipleRoutingTemplate() *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion,
		Kind:       model.Kind,
		ID:         "multiple-routing",
		Start:      "start",
		Nodes: map[string]model.Node{
			"start":  {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "task"}},
			"task":   {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerProgram, Run: "task"}, Next: model.Next{"pass": "done", "fail": "failed"}},
			"done":   {Type: model.NodeTypeEnd},
			"failed": {Type: model.NodeTypeEnd, Result: "failed"},
		},
	}
}

func assertAuthoringValid(t *testing.T, tmpl *model.Template) {
	t.Helper()
	edges, diagnostics := model.NormalizeEdgesWithinBudget(tmpl)
	if diagnostics.HasErrors() {
		t.Fatalf("edge diagnostics = %#v", diagnostics)
	}
	diagnostics = model.Validate(tmpl, edges)
	if diagnostics.HasErrors() {
		t.Fatalf("template should be authoring-valid: %#v", diagnostics.Errors())
	}
}

func hasCode(diagnostics model.Diagnostics, code string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}
