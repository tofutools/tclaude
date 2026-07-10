package processcmd

import (
	"bytes"
	"testing"

	"github.com/spf13/cobra"
	processexec "github.com/tofutools/tclaude/pkg/claude/process/exec"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/plan"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

func TestPoisonUnblockRetryFlowCompletes(t *testing.T) {
	cmd, root := poisonedCompoundFlow(t, "unblock_retry")
	unblockFlowOK(t, cmd, root, "unblock_retry", state.BlockDecisionRetry)

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
	unblockFlowOK(t, cmd, root, "unblock_skip", state.BlockDecisionSkip)
	executeNextInternalCommand(t, cmd, root, "unblock_skip", plan.CommandKindActivateNode)
	verifyFlowCheckpoint(t, cmd, root, "unblock_skip")
	advanceOK(t, cmd, root, "unblock_skip", "implement.review", "pass", "artifacts/review.md")
	assertEffectiveStatus(t, cmd, root, "unblock_skip", "completed")
}

func TestPoisonUnblockCancelFlowSettlesCanceled(t *testing.T) {
	cmd, root := poisonedCompoundFlow(t, "unblock_cancel")
	unblockFlowOK(t, cmd, root, "unblock_cancel", state.BlockDecisionCancel)
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

func unblockFlowOK(t *testing.T, cmd *cobra.Command, root, runID string, decision state.BlockDecision) {
	t.Helper()
	var out bytes.Buffer
	params := &unblockParams{
		RunID: runID, NodeID: "implement.test.tests", StoreRoot: root,
		Decision: string(decision), Actor: "human:johan", Reason: "operator reviewed exhausted gate", EvidenceRef: "decision:" + string(decision),
	}
	if err := runUnblock(cmd, params, &out); err != nil {
		t.Fatalf("unblock %s: %v\n%s", decision, err, out.String())
	}
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
