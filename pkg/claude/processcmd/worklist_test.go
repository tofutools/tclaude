package processcmd

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	processexec "github.com/tofutools/tclaude/pkg/claude/process/exec"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/plan"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

func TestWorklistCLIUsesSharedDerivation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	templatePath := writeTemplate(t, `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: worklist-cli
start: approve
nodes:
  approve:
    type: task
    performer:
      kind: human
      profile: johan
      ask: Approve merge?
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
	if err := runRun(cmd, &runParams{Template: templatePath, StoreRoot: root, RunID: "worklist-cli"}, &ignored); err != nil {
		t.Fatal(err)
	}
	fs, err := store.NewFS(root)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := fs.LoadRun(t.Context(), "worklist-cli")
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
	if err := runWorklist(cmd, &worklistParams{StoreRoot: root, Assignee: "human:johan", Kind: "human-wait", Status: "pending"}, &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"worklist-cli", "approve", "human-wait", "human:johan", "pending", "0/5", "Approve merge?", "approve,reject"} {
		if !strings.Contains(text, want) {
			t.Fatalf("worklist output missing %q:\n%s", want, text)
		}
	}

	out.Reset()
	if err := runWorklist(cmd, &worklistParams{StoreRoot: root, Assignee: "human:nobody"}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "worklist-cli") {
		t.Fatalf("assignee filter leaked item:\n%s", out.String())
	}
}
