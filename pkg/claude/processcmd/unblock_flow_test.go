package processcmd

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	processengine "github.com/tofutools/tclaude/pkg/claude/process/engine"
	processexec "github.com/tofutools/tclaude/pkg/claude/process/exec"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/plan"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

func TestEpochV8CLISettlementRetryRunsFreshAuthority(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	templatePath := writeTemplate(t, `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: epoch-cli-unblock
start: work
nodes:
  work:
    type: task
    performer: {kind: agent, prompt: retry me}
    next: {pass: done}
  done: {type: end, result: completed}
`)
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	var out bytes.Buffer
	if err := runRun(cmd, &runParams{Template: templatePath, StoreRoot: root, RunID: "epoch-cli-unblock"}, &out); err != nil {
		t.Fatal(err)
	}
	fs, err := store.NewFS(root)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &epochRetryAdapter{}
	host := processengine.New(fs, "test:epoch-cli-unblock", map[model.PerformerKind]processexec.Adapter{model.PerformerAgent: adapter})
	results, err := host.Tick(t.Context())
	if err != nil || len(results) != 1 || results[0].Error != "" {
		t.Fatalf("first epoch tick = %#v, %v", results, err)
	}
	out.Reset()
	if err := runUnblock(cmd, &unblockParams{
		RunID: "epoch-cli-unblock", NodeID: "work", StoreRoot: root, Decision: "retry",
		Actor: "human:johan", Reason: "reviewed failure", EvidenceRef: "ticket:TCL-604",
	}, &out); err != nil {
		t.Fatalf("epoch unblock: %v\n%s", err, out.String())
	}
	assertOutputContains(t, out.String(), "Resolved blocked node work")
	results, err = host.Tick(t.Context())
	if err != nil || len(results) != 1 || results[0].Error != "" || results[0].Status != state.RunStatusCompleted {
		t.Fatalf("rescued epoch tick = %#v, %v", results, err)
	}
	if adapter.calls != 2 {
		t.Fatalf("adapter calls = %d, want 2", adapter.calls)
	}
}

type epochRetryAdapter struct{ calls int }

func (a *epochRetryAdapter) Validate(processexec.Request) error { return nil }
func (a *epochRetryAdapter) Perform(_ context.Context, _ processexec.Request) (processexec.Observation, error) {
	a.calls++
	verdict := "pass"
	if a.calls == 1 {
		verdict = "fail"
	}
	return processexec.Observation{Actor: "human:johan", Verdict: verdict}, nil
}

func TestPoisonUnblockRetryFlowCompletes(t *testing.T) {
	cmd, root := poisonedCompoundFlow(t, "unblock_retry")
	unblockFlowOK(t, cmd, root, "unblock_retry", "implement.test.tests", state.BlockDecisionRetry)

	fs, err := openStore(root, true)
	if err != nil {
		t.Fatal(err)
	}
	before, err := fs.LoadRun(cmd.Context(), "unblock_retry")
	if err != nil {
		t.Fatal(err)
	}
	firstAttempt := before.State.Nodes["implement.test.tests"].Attempt
	advanceOK(t, cmd, root, "unblock_retry", "implement.test.tests", "pass", "artifacts/test-log-retry.txt")
	advanceOK(t, cmd, root, "unblock_retry", "implement.review", "pass", "artifacts/review.md")
	assertEffectiveStatus(t, cmd, root, "unblock_retry", "completed")
	after, err := fs.LoadRun(cmd.Context(), "unblock_retry")
	if err != nil {
		t.Fatal(err)
	}
	if after.State.Nodes["implement.test.tests"].Attempt != firstAttempt+1 {
		t.Fatalf("retry attempt = %d, want %d", after.State.Nodes["implement.test.tests"].Attempt, firstAttempt+1)
	}
}

func TestPoisonUnblockSkipFlowCompletes(t *testing.T) {
	cmd, root := poisonedCompoundFlow(t, "unblock_skip")
	unblockFlowOK(t, cmd, root, "unblock_skip", "implement", state.BlockDecisionSkip)
	executeNextInternalCommand(t, cmd, root, "unblock_skip", plan.CommandKindActivateNode)
	verifyFlowCheckpoint(t, cmd, root, "unblock_skip")
	advanceOK(t, cmd, root, "unblock_skip", "implement.review", "pass", "artifacts/review.md")
	assertEffectiveStatus(t, cmd, root, "unblock_skip", "completed")
}

func TestPoisonUnblockCancelFlowSettlesCanceled(t *testing.T) {
	cmd, root := poisonedCompoundFlow(t, "unblock_cancel")
	unblockFlowOK(t, cmd, root, "unblock_cancel", "implement.test.tests", state.BlockDecisionCancel)
	assertEffectiveStatus(t, cmd, root, "unblock_cancel", "canceled")

	fs, err := openStore(root, true)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := fs.LoadRun(cmd.Context(), "unblock_cancel")
	if err != nil {
		t.Fatal(err)
	}
	commands, err := plan.Plan(snapshot.State, mustFlowTemplate(t, fs, snapshot.Run.TemplateRef))
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 0 {
		t.Fatalf("canceled run planned commands: %#v", commands)
	}
}

func poisonedCompoundFlow(t *testing.T, runID string) (*cobra.Command, string) {
	t.Helper()
	cmd, root := compoundFlowSetup(t, runID)
	advanceOK(t, cmd, root, runID, "implement", "pass", "")
	advanceOK(t, cmd, root, runID, "implement.plan", "pass", "artifacts/plan.md")
	advanceOK(t, cmd, root, runID, "implement.plan.approval", "pass", "approval:johan")
	advanceOK(t, cmd, root, runID, "implement.do", "pass", "commit:abc123")
	advanceOK(t, cmd, root, runID, "implement.test.tests", "fail", "artifacts/test-log.txt")
	return cmd, root
}

func unblockFlowOK(t *testing.T, cmd *cobra.Command, root, runID, targetNodeID string, decision state.BlockDecision) {
	t.Helper()
	var out bytes.Buffer
	params := &unblockParams{
		RunID: runID, NodeID: targetNodeID, StoreRoot: root,
		Decision: string(decision), Actor: "human:johan", Reason: "operator reviewed exhausted gate", EvidenceRef: "decision:" + string(decision),
	}
	if err := runUnblock(cmd, params, &out); err != nil {
		t.Fatalf("unblock %s: %v\n%s", decision, err, out.String())
	}
	assertOutputContains(t, out.String(), "Resolved blocked node implement.test.tests")
	verifyFlowCheckpoint(t, cmd, root, runID)
	fs, err := openStore(root, true)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := fs.LoadRun(cmd.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	child, parent := snapshot.State.Nodes["implement.test.tests"], snapshot.State.Nodes["implement"]
	if child.Status == state.NodeStatusBlocked || parent.Status == state.NodeStatusBlocked || child.BlockResolution == nil || parent.BlockResolution == nil {
		t.Fatalf("unblock %s did not clear mirrored pair: child=%#v parent=%#v", decision, child, parent)
	}
}

func executeNextInternalCommand(t *testing.T, cmd *cobra.Command, root, runID string, kind plan.CommandKind) {
	t.Helper()
	fs, err := openStore(root, true)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := fs.LoadRun(cmd.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	commands, err := plan.Plan(snapshot.State, mustFlowTemplate(t, fs, snapshot.Run.TemplateRef))
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range commands {
		if command.Kind != kind {
			continue
		}
		if _, err := processexec.New(fs, nil).Execute(cmd.Context(), command); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatalf("no planned %s command: %#v", kind, commands)
}

func verifyFlowCheckpoint(t *testing.T, cmd *cobra.Command, root, runID string) {
	t.Helper()
	var out bytes.Buffer
	if err := runVerify(cmd.Context(), &verifyParams{RunID: runID, StoreRoot: root}, &out); err != nil {
		t.Fatalf("verify %s: %v\n%s", runID, err, out.String())
	}
}

func assertEffectiveStatus(t *testing.T, cmd *cobra.Command, root, runID, want string) {
	t.Helper()
	var out bytes.Buffer
	if err := runVerify(cmd.Context(), &verifyParams{RunID: runID, StoreRoot: root}, &out); err != nil {
		t.Fatalf("verify %s: %v\n%s", runID, err, out.String())
	}
	assertOutputContains(t, out.String(), "Effective status: "+want)
}

func mustFlowTemplate(t *testing.T, fs store.Store, ref string) *model.Template {
	t.Helper()
	tmpl, err := fs.GetTemplate(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	return tmpl
}
