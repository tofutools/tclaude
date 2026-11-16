package engine

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// amendTemplate: start → work (agent) → review (agent) → ship (agent) → end.
func amendTemplate() *model.Template {
	agent := func(next, prompt string) model.Node {
		return model.Node{Type: model.NodeTypeTask, Next: model.Next{model.DefaultOutcome: next},
			Performer: &model.Performer{Kind: model.PerformerAgent, Role: "dev", Prompt: prompt}}
	}
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "amend", Start: "start",
		Nodes: map[string]model.Node{
			"start":  {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "work"}},
			"work":   agent("review", "Build it."),
			"review": agent("ship", "Review it."),
			"ship":   agent("end", "Ship it."),
			"end":    {Type: model.NodeTypeEnd},
		},
	}
}

func cloneTemplate(t *testing.T, tmpl *model.Template) *model.Template {
	t.Helper()
	out := *tmpl
	out.Nodes = map[string]model.Node{}
	for id, node := range tmpl.Nodes {
		out.Nodes[id] = node
	}
	return &out
}

// While work is out, a security review step is inserted between review and
// ship, and ship's prompt changes. work carries on as it was; after review
// the run goes through the new step to ship.
func TestAmendChangesTheNotYetStartedPartAndTheRunGoesOn(t *testing.T) {
	current := amendTemplate()
	oldDef := mustPrepare(t, current, nil)
	checkpoint, err := Initialize("run-amend", oldDef)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, work := advanceAndPlan(t, checkpoint, oldDef)

	amended := cloneTemplate(t, current)
	review := amended.Nodes["review"]
	review.Next = model.Next{model.DefaultOutcome: "security"}
	amended.Nodes["review"] = review
	amended.Nodes["security"] = model.Node{Type: model.NodeTypeTask, Next: model.Next{model.DefaultOutcome: "ship"},
		Performer: &model.Performer{Kind: model.PerformerAgent, Role: "sec", Prompt: "Check it for secrets."}}
	ship := amended.Nodes["ship"]
	ship.Performer = &model.Performer{Kind: model.PerformerAgent, Role: "dev", Prompt: "Ship it behind a flag."}
	amended.Nodes["ship"] = ship
	newDef := mustPrepare(t, amended, nil)

	next, summary, err := Amend(checkpoint, oldDef, current, amended, newDef)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(summary.Added, []string{"security"}) || !slices.Equal(summary.Rerouted, []string{"review"}) || !slices.Equal(summary.Changed, []string{"ship"}) {
		t.Fatalf("summary = %#v", summary)
	}
	if err := ClassifyCheckpoint(next, newDef); err != nil {
		t.Fatalf("classify: %v", err)
	}
	next = roundTrip(t, next, newDef)
	var ran []string
	command := work
	for command != nil {
		ran = append(ran, command.NodeID)
		if next, err = Apply(next, newDef, observed(command, ProgramSucceeded, 0)); err != nil {
			t.Fatal(err)
		}
		next, command = advanceAndPlan(t, next, newDef)
	}
	if !slices.Equal(ran, []string{"work", "review", "security", "ship"}) || next.Status != RunCompleted {
		t.Fatalf("ran %v, status %q", ran, next.Status)
	}
}

func TestAmendRefusesStartedStepsProgramsAndNoOps(t *testing.T) {
	current := amendTemplate()
	oldDef := mustPrepare(t, current, nil)
	checkpoint, err := Initialize("run-amend2", oldDef)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, _ = advanceAndPlan(t, checkpoint, oldDef)
	try := func(edit func(*model.Template)) error {
		amended := cloneTemplate(t, current)
		edit(amended)
		newDef, err := Prepare(amended, nil)
		if err != nil {
			return err
		}
		_, _, err = Amend(checkpoint, oldDef, current, amended, newDef)
		return err
	}
	for name, c := range map[string]struct {
		edit func(*model.Template)
		want string
	}{
		"a started step": {func(tmpl *model.Template) {
			work := tmpl.Nodes["work"]
			work.Performer = &model.Performer{Kind: model.PerformerAgent, Role: "dev", Prompt: "Build something else."}
			tmpl.Nodes["work"] = work
		}, "step work has started"},
		"a program": {func(tmpl *model.Template) {
			tmpl.Nodes["ship"] = programTask("end", "deploy")
		}, "runs a program"},
		"nothing": {func(*model.Template) {}, "changes nothing"},
	} {
		err := try(c.edit)
		if !errors.Is(err, ErrAmendmentRefused) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A step being worked on may be rerouted (a step inserted after it); once it
// has gone on, where it led stays.
func TestAmendReroutesAStartedStepOnlyUntilItGoesOn(t *testing.T) {
	current := amendTemplate()
	oldDef := mustPrepare(t, current, nil)
	checkpoint, err := Initialize("run-amend3", oldDef)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, work := advanceAndPlan(t, checkpoint, oldDef)
	amended := cloneTemplate(t, current)
	node := amended.Nodes["work"]
	node.Next = model.Next{model.DefaultOutcome: "ship"}
	amended.Nodes["work"] = node
	delete(amended.Nodes, "review")
	newDef := mustPrepare(t, amended, nil)
	if _, summary, err := Amend(checkpoint, oldDef, current, amended, newDef); err != nil || !slices.Equal(summary.Removed, []string{"review"}) {
		t.Fatalf("reroute a step being worked on: %#v, %v", summary, err)
	}
	done, err := Apply(checkpoint, oldDef, observed(work, ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}
	done, _ = advanceAndPlan(t, done, oldDef)
	if _, _, err := Amend(done, oldDef, current, amended, newDef); !errors.Is(err, ErrAmendmentRefused) {
		t.Fatalf("a step that went on keeps where it led: %v", err)
	}
}

// A step reset by a send-back has run before: removing it would lose its
// attempt numbers (and adding it back would reuse ones handed out), so it is
// refused; changing it carries them over.
func TestAmendRefusesRemovingAStepThatRanBefore(t *testing.T) {
	current := amendTemplate()
	oldDef := mustPrepare(t, current, nil)
	checkpoint, err := Initialize("run-amend4", oldDef)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, work := advanceAndPlan(t, checkpoint, oldDef)
	if checkpoint, err = Apply(checkpoint, oldDef, observed(work, ProgramSucceeded, 0)); err != nil {
		t.Fatal(err)
	}
	checkpoint, review := advanceAndPlan(t, checkpoint, oldDef)
	if checkpoint, err = Apply(checkpoint, oldDef, observed(review, ProgramSucceeded, 0)); err != nil {
		t.Fatal(err)
	}
	checkpoint, _ = advanceAndPlan(t, checkpoint, oldDef)
	if checkpoint, err = Apply(checkpoint, oldDef, sentBack("work")); err != nil {
		t.Fatal(err)
	}
	checkpoint, _ = advanceAndPlan(t, checkpoint, oldDef) // work is out again; review is pending
	amended := cloneTemplate(t, current)
	work2 := amended.Nodes["work"]
	work2.Next = model.Next{model.DefaultOutcome: "ship"}
	amended.Nodes["work"] = work2
	delete(amended.Nodes, "review")
	newDef := mustPrepare(t, amended, nil)
	if _, _, err := Amend(checkpoint, oldDef, current, amended, newDef); !errors.Is(err, ErrAmendmentRefused) || !strings.Contains(err.Error(), "has run before") {
		t.Fatalf("removing a step that ran: %v", err)
	}
}
