package processcmd

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	processengine "github.com/tofutools/tclaude/pkg/claude/process/engine"
	processexec "github.com/tofutools/tclaude/pkg/claude/process/exec"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

func TestEpochV8CLIReadSurfacesWithoutScheduling(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	templatePath := writeTemplate(t, `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: schema7-cli-reads
start: approve
nodes:
  approve:
    type: task
    performer:
      kind: human
      profile: johan
      ask: Approve migrated release?
    next: { pass: end, fail: failed }
  end: { type: end, result: completed }
  failed: { type: end, result: failed }
`)
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	var ignored bytes.Buffer
	if err := runRun(cmd, &runParams{Template: templatePath, StoreRoot: root, RunID: "schema7-cli-reads"}, &ignored); err != nil {
		t.Fatal(err)
	}
	fs, err := store.NewFS(root)
	if err != nil {
		t.Fatal(err)
	}
	host := processengine.New(fs, "test:schema7-cli", map[model.PerformerKind]processexec.Adapter{
		model.PerformerHuman: showDeferredAdapter{},
	})
	if err := host.EnableExclusiveV7(); err != nil {
		t.Fatal(err)
	}
	results, err := host.Tick(t.Context())
	if err != nil || len(results) != 1 || results[0].Error != "" {
		t.Fatalf("automatic schema-7 migration tick = %#v, %v", results, err)
	}

	var out bytes.Buffer
	if err := runShow(cmd, &showParams{RunID: "schema7-cli-reads", StoreRoot: root}, &out); err != nil {
		t.Fatalf("show schema 7: %v\n%s", err, out.String())
	}
	for _, want := range []string{"Run: schema7-cli-reads", "State schema: 8", "Epochs: 1", "Authorities: 4"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("schema-7 show output missing %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	if err := runVerify(t.Context(), &verifyParams{RunID: "schema7-cli-reads", StoreRoot: root}, &out); err != nil {
		t.Fatalf("verify schema 7: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "Effective status: epoch_v8") {
		t.Fatalf("schema-7 verify output is not healthy:\n%s", out.String())
	}

	out.Reset()
	if err := runRunsLs(cmd, &runsLsParams{StoreRoot: root}, &out); err != nil {
		t.Fatalf("runs ls schema 7: %v", err)
	}
	if !strings.Contains(out.String(), "schema7-cli-reads") || !strings.Contains(out.String(), "epoch_v8") || strings.Contains(out.String(), "load_error") {
		t.Fatalf("schema-7 runs listing is not live:\n%s", out.String())
	}

	out.Reset()
	if err := runWorklist(cmd, &worklistParams{StoreRoot: root, Run: "schema7-cli-reads", Status: "pending"}, &out); err != nil {
		t.Fatalf("worklist schema 7: %v", err)
	}
	if !strings.Contains(out.String(), "skipped process run schema7-cli-reads: epoch_v8") {
		t.Fatalf("schema-8 worklist did not refuse unreleased scheduling:\n%s", out.String())
	}
}

func TestSchema7CLIAndWorklistReadPostSplitParallelCheckpoint(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	templatePath := writeTemplate(t, `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: schema7-parallel-reads
start: fork
nodes:
  fork:
    type: parallel
    next: {left: left, right: right}
  left:
    type: task
    performer: {kind: agent, profile: dev, prompt: left}
    next: merge
  right:
    type: task
    performer: {kind: agent, profile: dev, prompt: right}
    next: merge
  merge:
    type: end
    metadata: {join: all}
    result: completed
`)
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	var out bytes.Buffer
	if err := runRun(cmd, &runParams{Template: templatePath, StoreRoot: root, RunID: "schema7-parallel-reads"}, &out); err != nil {
		t.Fatal(err)
	}
	fs, err := store.NewFS(root)
	if err != nil {
		t.Fatal(err)
	}
	host := processengine.New(fs, "test:schema7-parallel-reads", map[model.PerformerKind]processexec.Adapter{model.PerformerAgent: showDeferredAdapter{}})
	if err := host.EnableExclusiveV7(); err != nil {
		t.Fatal(err)
	}
	results, err := host.Tick(t.Context())
	if err != nil || len(results) != 1 || results[0].Error != "" {
		t.Fatalf("parallel schema-7 tick = %#v, %v", results, err)
	}

	out.Reset()
	if err := runShow(cmd, &showParams{RunID: "schema7-parallel-reads", StoreRoot: root}, &out); err != nil {
		t.Fatalf("show post-split parallel checkpoint: %v", err)
	}
	out.Reset()
	if err := runVerify(t.Context(), &verifyParams{RunID: "schema7-parallel-reads", StoreRoot: root}, &out); err != nil || !strings.Contains(out.String(), "Effective status: epoch_v8") {
		t.Fatalf("verify post-split parallel checkpoint: %v\n%s", err, out.String())
	}
	out.Reset()
	if err := runRunsLs(cmd, &runsLsParams{StoreRoot: root}, &out); err != nil || strings.Contains(out.String(), "load_error") {
		t.Fatalf("list post-split parallel checkpoint: %v\n%s", err, out.String())
	}
	out.Reset()
	if err := runWorklist(cmd, &worklistParams{StoreRoot: root, Run: "schema7-parallel-reads"}, &out); err != nil {
		t.Fatalf("worklist post-split parallel checkpoint: %v", err)
	}
}
