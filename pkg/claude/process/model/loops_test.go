package model

import (
	"strings"
	"testing"
)

func loopTestTemplate(mutate func(nodes map[string]Node)) *Template {
	decide := Node{
		Type:      NodeTypeDecision,
		Performer: &Performer{Kind: PerformerHuman, Ask: "Ship it?"},
		Next:      Next{"approved": "merge", "changes": "implement", "escalate": "escalated"},
		Loops:     map[string]LoopBound{"changes": {MaxIterations: 3, OnExhausted: "escalate"}},
	}
	task := func(next string) Node {
		return Node{Type: NodeTypeTask, Performer: &Performer{Kind: PerformerProgram, Run: "true"}, Next: Next{DefaultOutcome: next}}
	}
	nodes := map[string]Node{
		"start":     {Type: NodeTypeStart, Next: Next{DefaultOutcome: "implement"}},
		"implement": task("review"),
		"review":    task("decide"),
		"decide":    decide,
		"merge":     task("end"),
		"end":       {Type: NodeTypeEnd},
		"escalated": {Type: NodeTypeEnd, Result: "canceled"},
	}
	if mutate != nil {
		mutate(nodes)
	}
	return &Template{APIVersion: APIVersion, Kind: Kind, ID: "loop", Start: "start", Nodes: nodes}
}

func loopErrorCodes(tmpl *Template) map[string]string {
	codes := map[string]string{}
	for _, diagnostic := range Validate(tmpl, NormalizeEdges(tmpl)) {
		if diagnostic.Severity == SeverityError {
			codes[diagnostic.Code] = diagnostic.Path
		}
	}
	return codes
}

func TestBoundedLoopValidates(t *testing.T) {
	tmpl := loopTestTemplate(nil)
	if codes := loopErrorCodes(tmpl); len(codes) != 0 {
		t.Fatalf("errors = %v", codes)
	}
	loops := Loops(tmpl, NormalizeEdges(tmpl))
	if len(loops) != 1 {
		t.Fatalf("loops = %#v", loops)
	}
	loop := loops[0]
	if loop.To != "implement" || len(loop.Body) != 3 || !loop.Contains("review") || len(loop.Exits) != 2 {
		t.Fatalf("loop = %#v", loop)
	}
}

func TestBoundedLoopRoundTripsThroughYAML(t *testing.T) {
	data, err := CanonicalYAML(loopTestTemplate(nil))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Diagnostics.HasErrors() {
		t.Fatalf("diagnostics = %#v", parsed.Diagnostics)
	}
	if got := parsed.Template.Nodes["decide"].Loops["changes"]; got.MaxIterations != 3 || got.OnExhausted != "escalate" {
		t.Fatalf("loops = %#v\n%s", got, data)
	}
}

func TestBoundedLoopRejections(t *testing.T) {
	cases := map[string]struct {
		mutate func(nodes map[string]Node)
		code   string
	}{
		"unbounded cycle": {func(nodes map[string]Node) {
			decide := nodes["decide"]
			decide.Loops = nil
			nodes["decide"] = decide
		}, "graph_cycle"},
		"too few rounds": {func(nodes map[string]Node) {
			decide := nodes["decide"]
			decide.Loops = map[string]LoopBound{"changes": {MaxIterations: 1}}
			nodes["decide"] = decide
		}, "loop_iterations"},
		"unknown onExhausted": {func(nodes map[string]Node) {
			decide := nodes["decide"]
			decide.Loops = map[string]LoopBound{"changes": {MaxIterations: 3, OnExhausted: "nope"}}
			nodes["decide"] = decide
		}, "loop_exhaustion"},
		"forward edge": {func(nodes map[string]Node) {
			decide := nodes["decide"]
			decide.Loops = map[string]LoopBound{"changes": {MaxIterations: 3}, "approved": {MaxIterations: 3}}
			nodes["decide"] = decide
		}, "loop_not_back"},
		"self loop": {func(nodes map[string]Node) {
			decide := nodes["decide"]
			decide.Next["wait"] = "decide"
			decide.Loops = map[string]LoopBound{"changes": {MaxIterations: 3}, "wait": {MaxIterations: 3}}
			nodes["decide"] = decide
		}, "loop_not_back"},
		"second entry": {func(nodes map[string]Node) {
			nodes["start"] = Node{Type: NodeTypeDecision, Performer: &Performer{Kind: PerformerHuman, Ask: "Skip?"},
				Next: Next{"build": "implement", "skip": "review"}}
		}, "loop_entry"},
		"task exit": {func(nodes map[string]Node) {
			review := nodes["review"]
			review.Next = Next{DefaultOutcome: "decide", "fail": "escalated"}
			nodes["review"] = review
		}, "loop_exit"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			codes := loopErrorCodes(loopTestTemplate(tc.mutate))
			if _, ok := codes[tc.code]; !ok {
				t.Fatalf("errors = %v, want %s", codes, tc.code)
			}
		})
	}
}

func TestLoopBoundOnATaskIsRefused(t *testing.T) {
	tmpl := loopTestTemplate(func(nodes map[string]Node) {
		review := nodes["review"]
		review.Loops = map[string]LoopBound{DefaultOutcome: {MaxIterations: 2}}
		nodes["review"] = review
	})
	if _, ok := loopErrorCodes(tmpl)["loop_source"]; !ok {
		t.Fatalf("errors = %v", loopErrorCodes(tmpl))
	}
}

func TestUnboundedCycleHintNamesTheDecisionOrAsksForOne(t *testing.T) {
	message := func(tmpl *Template) string {
		for _, diagnostic := range Validate(tmpl, NormalizeEdges(tmpl)) {
			if diagnostic.Code == "graph_cycle" {
				return diagnostic.Message
			}
		}
		t.Fatal("no graph_cycle")
		return ""
	}
	decided := loopTestTemplate(func(nodes map[string]Node) {
		decide := nodes["decide"]
		decide.Loops = nil
		nodes["decide"] = decide
	})
	got := message(decided)
	if !strings.Contains(got, `bound decision "decide"'s outcome "changes"`) || strings.Contains(got, "start") {
		t.Fatalf("message = %q", got)
	}
	taskOnly := loopTestTemplate(func(nodes map[string]Node) {
		review := nodes["review"]
		review.Next = Next{DefaultOutcome: "decide", "again": "implement"}
		nodes["review"] = review
		decide := nodes["decide"]
		decide.Loops = nil
		decide.Next = Next{"approved": "merge", "escalate": "escalated"}
		nodes["decide"] = decide
	})
	if got := message(taskOnly); !strings.Contains(got, "only a decision can send work back") {
		t.Fatalf("message = %q", got)
	}
}

// With two decisions on one cycle, only the one that closes it from the start
// goes back. The hint must name it: bounding the other decision's forward
// outcome would leave a loop entered past its top.
func TestUnboundedCycleHintNamesTheClosingDecision(t *testing.T) {
	twoDecisions := func(bounded bool) *Template {
		return loopTestTemplate(func(nodes map[string]Node) {
			nodes["start"] = Node{Type: NodeTypeStart, Next: Next{DefaultOutcome: "zgate"}}
			nodes["zgate"] = Node{
				Type:      NodeTypeDecision,
				Performer: &Performer{Kind: PerformerHuman, Ask: "Start?"},
				Next:      Next{"go": "implement", "no": "escalated"},
			}
			decide := nodes["decide"]
			decide.Next = Next{"approved": "merge", "changes": "zgate", "escalate": "escalated"}
			if !bounded {
				decide.Loops = nil
			}
			nodes["decide"] = decide
		})
	}
	var got string
	for _, diagnostic := range Validate(twoDecisions(false), NormalizeEdges(twoDecisions(false))) {
		if diagnostic.Code == "graph_cycle" {
			got = diagnostic.Message
		}
	}
	if !strings.Contains(got, `bound decision "decide"'s outcome "changes"`) {
		t.Fatalf("message = %q", got)
	}
	if codes := loopErrorCodes(twoDecisions(true)); len(codes) != 0 {
		t.Fatalf("following the hint leaves errors = %v", codes)
	}
}
