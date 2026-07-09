package processcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// writeGateBudgetTemplate is the section 12 strawman with explicit gate
// budgets: the tests check may fail testsRetry times and the review gate
// twice before poisoning; the do stage gets three attempts to answer
// feedback.
func writeGateBudgetTemplate(t *testing.T, testsRetry string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "code-change-budgets.yaml")
	body := `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: code-change-with-budgets
params:
  issue:
    type: string
    required: true
start: implement
nodes:
  implement:
    type: task
    performer:
      kind: agent
      profile: dev
      prompt: Implement {{ params.issue }}
    plan:
      id: plan
      approval: human
      performer:
        kind: agent
        prompt: Plan the implementation of {{ params.issue }}
    checks:
      - id: tests
        performer:
          kind: program
          run: go test ./...
        retry:
          maxAttempts: ` + testsRetry + `
    review:
      id: review
      performer:
        kind: agent
        profile: reviewer
        prompt: Cold-review the diff
      retry:
        maxAttempts: 2
    retry:
      maxAttempts: 3
      onFail: feedback-same-session
    next:
      pass: done
  done:
    type: end
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func gateLoopFlowSetup(t *testing.T, runID, testsRetry string) (*cobra.Command, string) {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	root := filepath.Join(t.TempDir(), "store")
	var out bytes.Buffer
	if err := runRun(cmd, &runParams{
		Template:  writeGateBudgetTemplate(t, testsRetry),
		StoreRoot: root,
		RunID:     runID,
		Param:     []string{"issue=TCL-276"},
	}, &out); err != nil {
		t.Fatal(err)
	}
	return cmd, root
}

// advanceLoopOK is advanceOK with the gate-loop inputs: an evidence hash on
// work settles and a feedback payload on gate failures.
func advanceLoopOK(t *testing.T, cmd *cobra.Command, root, runID, nodeID, verdict, evidenceRef, evidenceHash, feedback string) {
	t.Helper()
	var out bytes.Buffer
	if err := runAdvance(cmd, &advanceParams{
		RunID: runID, NodeID: nodeID, StoreRoot: root,
		Verdict: verdict, EvidenceRef: evidenceRef, EvidenceHash: evidenceHash, Feedback: feedback,
		Actor: "human:johan",
	}, &out); err != nil {
		t.Fatalf("advance %s %s: %v\n%s", nodeID, verdict, err, out.String())
	}
	out.Reset()
	if err := runVerify(cmd.Context(), &verifyParams{RunID: runID, StoreRoot: root}, &out); err != nil {
		t.Fatalf("verify after %s %s: %v\n%s", nodeID, verdict, err, out.String())
	}
}

func TestCompoundGateFeedbackLoopWithinBudgetsCompletes(t *testing.T) {
	cmd, root := gateLoopFlowSetup(t, "loop_pass", "3")

	advanceOK(t, cmd, root, "loop_pass", "implement", "pass", "")
	advanceOK(t, cmd, root, "loop_pass", "implement.plan", "pass", "artifacts/plan.md")
	advanceOK(t, cmd, root, "loop_pass", "implement.plan.approval", "pass", "approval:johan")
	advanceLoopOK(t, cmd, root, "loop_pass", "implement.do", "pass", "commit:abc123", "hash-1", "")

	// The tests gate fails within budget: instead of poisoning, the loop
	// records feedback on the do stage and re-readies it.
	advanceLoopOK(t, cmd, root, "loop_pass", "implement.test.tests", "fail", "artifacts/test-log-1.txt", "", "TestFoo fails on nil input")

	var out bytes.Buffer
	if err := runShow(cmd, &showParams{RunID: "loop_pass", StoreRoot: root}, &out); err != nil {
		t.Fatal(err)
	}
	// The loop state is legible from the run dir alone: fail counter against
	// budget, and the pending feedback marker naming the gate.
	assertOutputContains(t, out.String(), "fails 1/3")
	assertOutputContains(t, out.String(), "feedback pending from implement.test.tests")

	// The do stage answers the feedback with new work; the gate re-runs and
	// passes; the rest of the chain completes the node and the run.
	advanceLoopOK(t, cmd, root, "loop_pass", "implement.do", "pass", "commit:def456", "hash-2", "")
	advanceLoopOK(t, cmd, root, "loop_pass", "implement.test.tests", "pass", "artifacts/test-log-2.txt", "", "")
	advanceOK(t, cmd, root, "loop_pass", "implement.review", "pass", "artifacts/review.md")

	out.Reset()
	if err := runVerify(cmd.Context(), &verifyParams{RunID: "loop_pass", StoreRoot: root}, &out); err != nil {
		t.Fatalf("final verify: %v\n%s", err, out.String())
	}
	assertOutputContains(t, out.String(), "Effective status: completed")

	out.Reset()
	if err := runShow(cmd, &showParams{RunID: "loop_pass", StoreRoot: root}, &out); err != nil {
		t.Fatal(err)
	}
	// Two recorded verdicts on the tests gate, no pending feedback left.
	assertOutputContains(t, out.String(), "verdicts 2")
	if strings.Contains(out.String(), "feedback pending") {
		t.Fatalf("consumed feedback still rendered:\n%s", out.String())
	}
}

func TestCompoundGateBudgetExhaustionBlocks(t *testing.T) {
	cmd, root := gateLoopFlowSetup(t, "loop_poison", "2")

	advanceOK(t, cmd, root, "loop_poison", "implement", "pass", "")
	advanceOK(t, cmd, root, "loop_poison", "implement.plan", "pass", "artifacts/plan.md")
	advanceOK(t, cmd, root, "loop_poison", "implement.plan.approval", "pass", "approval:johan")
	advanceLoopOK(t, cmd, root, "loop_poison", "implement.do", "pass", "commit:abc123", "hash-1", "")

	// First failure loops; the second failed verdict spends the budget.
	advanceLoopOK(t, cmd, root, "loop_poison", "implement.test.tests", "fail", "artifacts/test-log-1.txt", "", "TestFoo fails")
	advanceLoopOK(t, cmd, root, "loop_poison", "implement.do", "pass", "commit:def456", "hash-2", "")
	advanceLoopOK(t, cmd, root, "loop_poison", "implement.test.tests", "fail", "artifacts/test-log-2.txt", "", "TestFoo still fails")

	var out bytes.Buffer
	if err := runVerify(cmd.Context(), &verifyParams{RunID: "loop_poison", StoreRoot: root}, &out); err != nil {
		t.Fatalf("verify after poison: %v\n%s", err, out.String())
	}
	assertOutputContains(t, out.String(), "Effective status: running")

	out.Reset()
	if err := runShow(cmd, &showParams{RunID: "loop_poison", StoreRoot: root}, &out); err != nil {
		t.Fatal(err)
	}
	assertOutputContains(t, out.String(), "blocked")
	assertOutputContains(t, out.String(), `gate "implement.test.tests" exhausted its budget of 2 failed verdicts`)
	assertOutputContains(t, out.String(), "fails 2/2")
	assertOutputContains(t, out.String(), "owner human:operator")
}

func TestCompoundWorkBudgetBoundsLoop(t *testing.T) {
	// The tests gate has verdicts left after two failures, but the third
	// failure cannot re-enter the do stage (its three attempts are spent):
	// the node poisons instead of looping forever.
	cmd, root := gateLoopFlowSetup(t, "loop_work_budget", "9")

	advanceOK(t, cmd, root, "loop_work_budget", "implement", "pass", "")
	advanceOK(t, cmd, root, "loop_work_budget", "implement.plan", "pass", "artifacts/plan.md")
	advanceOK(t, cmd, root, "loop_work_budget", "implement.plan.approval", "pass", "approval:johan")
	advanceLoopOK(t, cmd, root, "loop_work_budget", "implement.do", "pass", "commit:a", "hash-1", "")
	advanceLoopOK(t, cmd, root, "loop_work_budget", "implement.test.tests", "fail", "artifacts/log-1", "", "broken")
	advanceLoopOK(t, cmd, root, "loop_work_budget", "implement.do", "pass", "commit:b", "hash-2", "")
	advanceLoopOK(t, cmd, root, "loop_work_budget", "implement.test.tests", "fail", "artifacts/log-2", "", "still broken")
	advanceLoopOK(t, cmd, root, "loop_work_budget", "implement.do", "pass", "commit:c", "hash-3", "")
	advanceLoopOK(t, cmd, root, "loop_work_budget", "implement.test.tests", "fail", "artifacts/log-3", "", "broken forever")

	var out bytes.Buffer
	if err := runShow(cmd, &showParams{RunID: "loop_work_budget", StoreRoot: root}, &out); err != nil {
		t.Fatal(err)
	}
	assertOutputContains(t, out.String(), `stage "implement.do" has exhausted its budget of 3 attempts`)
}

func TestAdvanceRejectsReservedEngineActor(t *testing.T) {
	// The engine: namespace asserts the ENGINE synthesized a decision; a
	// manual advance claiming it would forge short-circuit provenance
	// without the reducer's hash check.
	cmd, root := gateLoopFlowSetup(t, "loop_engine_actor", "2")
	advanceOK(t, cmd, root, "loop_engine_actor", "implement", "pass", "")

	var out bytes.Buffer
	err := runAdvance(cmd, &advanceParams{
		RunID: "loop_engine_actor", NodeID: "implement.plan", StoreRoot: root,
		Verdict: "pass", EvidenceRef: "artifacts/plan.md", Actor: "engine:evidence-unchanged",
	}, &out)
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("expected reserved-actor refusal, got %v", err)
	}
}

func TestTamperedGateFailCountFailsVerify(t *testing.T) {
	cmd, root := gateLoopFlowSetup(t, "loop_tamper_count", "2")
	advanceOK(t, cmd, root, "loop_tamper_count", "implement", "pass", "")

	// Adversarial tamper: hand-edit the gate's fail counter past the budget
	// the pinned template grants it.
	statePath := filepath.Join(root, "runs", "loop_tamper_count", "state.json")
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	nodes := doc["nodes"].(map[string]any)
	gate := nodes["implement.test.tests"].(map[string]any)
	gate["failCount"] = 7
	tampered, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, tampered, 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err = runVerify(cmd.Context(), &verifyParams{RunID: "loop_tamper_count", StoreRoot: root}, &out)
	if err == nil {
		t.Fatalf("tampered fail count must fail verify:\n%s", out.String())
	}
	assertOutputContains(t, out.String(), "gate_fail_count_over_budget")
}

func TestForgedShortCircuitDecisionFailsVerify(t *testing.T) {
	cmd, root := gateLoopFlowSetup(t, "loop_forge_engine", "2")
	advanceOK(t, cmd, root, "loop_forge_engine", "implement", "pass", "")

	// Adversarial forgery: an engine short-circuit decision with no prior
	// verdict to stand.
	statePath := filepath.Join(root, "runs", "loop_forge_engine", "state.json")
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	nodes := doc["nodes"].(map[string]any)
	gate := nodes["implement.test.tests"].(map[string]any)
	gate["decisions"] = []any{map[string]any{
		"actor":       "engine:evidence-unchanged",
		"verdict":     "pass",
		"evidenceRef": "forged",
	}}
	tampered, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, tampered, 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err = runVerify(cmd.Context(), &verifyParams{RunID: "loop_forge_engine", StoreRoot: root}, &out)
	if err == nil {
		t.Fatalf("forged engine decision must fail verify:\n%s", out.String())
	}
	assertOutputContains(t, out.String(), "engine_decision_without_prior_verdict")
}
