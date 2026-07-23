package engine

import (
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

func TestSequentialProgramTemplateIsEligible(t *testing.T) {
	if diagnostics := CheckEligibility(sequentialTemplate("one", "two")); len(diagnostics) != 0 {
		t.Fatalf("eligibility diagnostics = %#v", diagnostics)
	}
}

func TestEligibilityRejectsUnsupportedAuthoringValidFeatures(t *testing.T) {
	tests := []struct {
		name string
		code string
		tmpl func() *model.Template
	}{
		{
			name: "decision",
			code: "unsupported_decision",
			tmpl: func() *model.Template {
				tmpl := sequentialTemplate("task")
				tmpl.Nodes["start"] = model.Node{Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "choose"}}
				tmpl.Nodes["choose"] = model.Node{
					Type:      model.NodeTypeDecision,
					Performer: &model.Performer{Kind: model.PerformerHuman, Ask: "Continue?"},
					Next:      model.Next{"yes": "task"},
				}
				return tmpl
			},
		},
		{
			name: "parallel and join",
			code: "unsupported_parallel",
			tmpl: parallelTemplate,
		},
		{
			name: "join",
			code: "unsupported_join",
			tmpl: parallelTemplate,
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
			name: "retry",
			code: "unsupported_retry",
			tmpl: func() *model.Template {
				tmpl := sequentialTemplate("task")
				node := tmpl.Nodes["task"]
				node.Retry = &model.RetryPolicy{MaxAttempts: 2}
				tmpl.Nodes["task"] = node
				return tmpl
			},
		},
		{
			name: "compound stages",
			code: "unsupported_compound_stages",
			tmpl: func() *model.Template {
				tmpl := sequentialTemplate("task")
				node := tmpl.Nodes["task"]
				node.Plan = &model.Step{ID: "plan", Performer: model.Performer{Kind: model.PerformerProgram, Run: "plan"}}
				tmpl.Nodes["task"] = node
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
