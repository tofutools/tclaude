package processcmd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

// Scenario: a run parked at the schema-7 (pathv1) checkpoint classifies as
// reset-required. The daemon routes are covered elsewhere
// (TestSchema7RoutesAndHostReturnStableResetRequired); this test pins the
// direct CLI branches — show, verify, resolve, and the runs listing — so each
// surfaces the stable reset-required signal without deleting or rewriting the
// checkpointed run state on disk.
func TestSchema7ResetRequiredCLISurfacesWithoutMutatingState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	const runID = "schema7-reset-required-cli"
	fs, err := store.NewFS(root)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &model.Template{
		APIVersion: model.APIVersion,
		Kind:       model.Kind,
		ID:         "schema7-reset-required-cli",
		Start:      "work",
		Nodes: map[string]model.Node{
			"work":   {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "work"}, Next: model.Next{"pass": "end", "fail": "failed"}},
			"end":    {Type: model.NodeTypeEnd},
			"failed": {Type: model.NodeTypeEnd, Result: "failed"},
		},
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	if err != nil {
		t.Fatal(err)
	}
	nodes := make([]state.NodeInit, 0, len(tmpl.Nodes))
	for id, node := range tmpl.Nodes {
		status := state.NodeStatusPending
		if id == tmpl.Start {
			status = state.NodeStatusReady
		}
		nodes = append(nodes, state.NodeInit{ID: id, Type: node.Type, Status: status})
	}
	initial := state.New(runID, record.Ref, record.Ref, nodes)
	initial.Status = state.RunStatusRunning
	if _, err := fs.CreateRun(t.Context(), store.RunRecord{ID: runID, TemplateRef: record.Ref}, initial); err != nil {
		t.Fatal(err)
	}
	proof, err := fs.UpgradeNeeded(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.InitializePathV1(t.Context(), runID, proof); err != nil {
		t.Fatal(err)
	}
	kind, err := fs.RunStateSchemaKind(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if kind != store.RunSchemaResetRequired {
		t.Fatalf("seeded run schema = %q, want %q", kind, store.RunSchemaResetRequired)
	}

	statePath := filepath.Join(root, "runs", runID, "state.json")
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	requireStateUnchanged := func(surface string) {
		t.Helper()
		after, readErr := os.ReadFile(statePath)
		if readErr != nil {
			t.Fatalf("%s: reread checkpointed state: %v", surface, readErr)
		}
		if !bytes.Equal(before, after) {
			t.Fatalf("%s mutated the reset-required run state on disk", surface)
		}
	}

	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	var out bytes.Buffer

	if err := runShow(cmd, &showParams{RunID: runID, StoreRoot: root}, &out); !errors.Is(err, store.ErrRunResetRequired) {
		t.Fatalf("show error = %v, want %v", err, store.ErrRunResetRequired)
	}
	requireStateUnchanged("show")

	out.Reset()
	if err := runVerify(t.Context(), &verifyParams{RunID: runID, StoreRoot: root}, &out); !errors.Is(err, store.ErrRunResetRequired) {
		t.Fatalf("verify error = %v, want %v", err, store.ErrRunResetRequired)
	}
	requireStateUnchanged("verify")

	out.Reset()
	err = runResolve(cmd, &resolveParams{
		RunID: runID, NodeID: "work", StoreRoot: root,
		Verdict: "pass", Actor: "human:tester",
	}, &out)
	if !errors.Is(err, store.ErrRunResetRequired) {
		t.Fatalf("resolve error = %v, want %v", err, store.ErrRunResetRequired)
	}
	requireStateUnchanged("resolve")

	out.Reset()
	if err := runRunsLs(cmd, &runsLsParams{StoreRoot: root}, &out); err != nil {
		t.Fatalf("runs ls: %v", err)
	}
	if !strings.Contains(out.String(), runID) || !strings.Contains(out.String(), string(store.RunSchemaResetRequired)) {
		t.Fatalf("runs listing does not render the reset-required run:\n%s", out.String())
	}
	if strings.Contains(out.String(), "load_error") {
		t.Fatalf("runs listing degraded a classifiable reset-required run to load_error:\n%s", out.String())
	}
	requireStateUnchanged("runs ls")
}
