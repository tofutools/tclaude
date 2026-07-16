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
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

func TestSchema7CLIReadSurfacesAfterAutomaticMigration(t *testing.T) {
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
	for _, want := range []string{"Run: schema7-cli-reads", "Status: running", "State schema: 7", "approve", "activated"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("schema-7 show output missing %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	if err := runShow(cmd, &showParams{RunID: "schema7-cli-reads", StoreRoot: root, Mermaid: true}, &out); err != nil {
		t.Fatalf("show schema-7 mermaid: %v", err)
	}
	if !strings.Contains(out.String(), "graph TD") {
		t.Fatalf("schema-7 mermaid output missing graph:\n%s", out.String())
	}

	out.Reset()
	if err := runVerify(t.Context(), &verifyParams{RunID: "schema7-cli-reads", StoreRoot: root}, &out); err != nil {
		t.Fatalf("verify schema 7: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "Effective status: running") || !strings.Contains(out.String(), "Diagnostics: none") {
		t.Fatalf("schema-7 verify output is not healthy:\n%s", out.String())
	}

	out.Reset()
	if err := runRunsLs(cmd, &runsLsParams{StoreRoot: root}, &out); err != nil {
		t.Fatalf("runs ls schema 7: %v", err)
	}
	if !strings.Contains(out.String(), "schema7-cli-reads") || !strings.Contains(out.String(), "running") || strings.Contains(out.String(), "load_error") {
		t.Fatalf("schema-7 runs listing is not live:\n%s", out.String())
	}

	out.Reset()
	if err := runWorklist(cmd, &worklistParams{StoreRoot: root, Run: "schema7-cli-reads", Status: "pending"}, &out); err != nil {
		t.Fatalf("worklist schema 7: %v", err)
	}
	for _, want := range []string{"schema7-cli-reads", "approve", "human-wait", "human:johan", "pending", "Approve migrated release?"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("schema-7 worklist output missing %q:\n%s", want, out.String())
		}
	}
}

func TestPathV1RoutingStatesKeepsNewestNodeGeneration(t *testing.T) {
	aggregate := pathv1.AggregateCheckpoint{Routing: pathv1.RoutingState{Reservations: map[string]pathv1.ActivationReservation{
		"older": {ID: "older", NodeID: "approve", Generation: 1, State: pathv1.ReservationOpen},
		"newer": {ID: "newer", NodeID: "approve", Generation: 2, State: pathv1.ReservationActivated},
	}}}
	for range 100 {
		if got := pathV1RoutingStates(aggregate)["approve"]; got != string(pathv1.ReservationActivated) {
			t.Fatalf("newest schema-7 routing state = %q, want %q", got, pathv1.ReservationActivated)
		}
	}
}
