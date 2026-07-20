package processcmd

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	processexec "github.com/tofutools/tclaude/pkg/claude/process/exec"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/plan"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

func TestRenderMermaidUsesStableEscapedIDs(t *testing.T) {
	snapshot := store.Snapshot{
		State: &state.State{
			Nodes: map[string]state.NodeState{
				"a-b": {Status: state.NodeStatusReady},
				"a_b": {Status: state.NodeStatusPending},
			},
		},
	}
	tmpl := &model.Template{
		Nodes: map[string]model.Node{
			"a-b": {Next: model.Next{"ok| x --> pwned[evil": "a_b"}},
			"a_b": {},
		},
	}

	var out bytes.Buffer
	renderMermaid(&out, snapshot, tmpl)
	text := out.String()
	if strings.Contains(text, "x --> pwned[evil") {
		t.Fatalf("raw injectable mermaid label leaked:\n%s", text)
	}
	if !strings.Contains(text, "n_612d62") || !strings.Contains(text, "n_615f62") {
		t.Fatalf("expected collision-free generated ids:\n%s", text)
	}
}

func TestShowRendersObligationsAndVisibleNudgeState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	templatePath := writeTemplate(t, `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: show-obligation
start: approve
nodes:
  approve:
    type: task
    performer:
      kind: human
      profile: johan
      ask: Approve merge?
      choices: [approve, reject]
      choiceOutcomes: {approve: pass, reject: fail}
      contact:
        cadence: 30m
        budget: 5
        escalationTarget: human:operator
    next: { pass: end }
  end: { type: end }
`)
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	var ignored bytes.Buffer
	if err := runRun(cmd, &runParams{Template: templatePath, StoreRoot: root, RunID: "show-obligation"}, &ignored); err != nil {
		t.Fatal(err)
	}
	fs, err := store.NewFS(root)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := fs.LoadRun(t.Context(), "show-obligation")
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := fs.GetTemplate(t.Context(), snapshot.Run.TemplateRef)
	if err != nil {
		t.Fatal(err)
	}
	commands, err := plan.Plan(snapshot.State, tmpl)
	if err != nil || len(commands) != 1 {
		t.Fatalf("commands=%#v err=%v", commands, err)
	}
	executor := processexec.New(fs, map[model.PerformerKind]processexec.Adapter{model.PerformerHuman: showDeferredAdapter{}})
	if _, err := executor.Execute(t.Context(), commands[0]); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runShow(cmd, &showParams{RunID: "show-obligation", StoreRoot: root}, &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"Obligations:", "Approve merge?", "human:johan", "approve,reject", "Nudges:", "0/5", "human:operator"} {
		if !strings.Contains(text, want) {
			t.Fatalf("show output missing %q:\n%s", want, text)
		}
	}
}

type showDeferredAdapter struct{}

func (showDeferredAdapter) Validate(processexec.Request) error { return nil }
func (showDeferredAdapter) Perform(context.Context, processexec.Request) (processexec.Observation, error) {
	return processexec.Observation{}, nil
}
func (showDeferredAdapter) Dispatch(context.Context, processexec.Request) (processexec.DispatchResult, error) {
	return processexec.DispatchResult{
		ExternalRef: "obligation:test", Assignee: "human:johan", Summary: "Approve merge?",
		AvailableActions: []string{"approve", "reject"}, CreateObligation: true,
	}, nil
}
func (showDeferredAdapter) ReconcileDeferred(context.Context, processexec.Request) (processexec.Observation, processexec.DeferredStatus, error) {
	return processexec.Observation{}, processexec.DeferredInFlight, nil
}
