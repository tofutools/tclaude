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
	"github.com/tofutools/tclaude/pkg/claude/process/state"
)

// writeCompoundTemplate is the design-doc section 12 strawman shape: an agent
// work slot (mocked through manual verdicts), a program check, an agent
// reviewer, and a human plan approval.
func writeCompoundTemplate(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "code-change.yaml")
	body := `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: code-change-with-review
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
    review:
      id: review
      performer:
        kind: agent
        profile: reviewer
        prompt: Cold-review the diff
    retry:
      maxAttempts: 2
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

func compoundFlowSetup(t *testing.T, runID string) (*cobra.Command, string) {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	root := filepath.Join(t.TempDir(), "store")
	var out bytes.Buffer
	if err := runRun(cmd, &runParams{
		Template:  writeCompoundTemplate(t),
		StoreRoot: root,
		RunID:     runID,
		Param:     []string{"issue=TCL-276"},
	}, &out); err != nil {
		t.Fatal(err)
	}
	return cmd, root
}

func advanceOK(t *testing.T, cmd *cobra.Command, root, runID, nodeID, verdict, evidenceRef string) {
	t.Helper()
	var out bytes.Buffer
	if err := runAdvance(cmd, &advanceParams{
		RunID: runID, NodeID: nodeID, StoreRoot: root,
		Verdict: verdict, EvidenceRef: evidenceRef, Actor: "human:johan",
	}, &out); err != nil {
		t.Fatalf("advance %s %s: %v\n%s", nodeID, verdict, err, out.String())
	}
	out.Reset()
	if err := runVerify(cmd.Context(), &verifyParams{RunID: runID, StoreRoot: root}, &out); err != nil {
		t.Fatalf("verify after %s %s: %v\n%s", nodeID, verdict, err, out.String())
	}
}

func TestCompoundNodeExpandsAndCompletesThroughGates(t *testing.T) {
	cmd, root := compoundFlowSetup(t, "compound_pass")

	// Expansion: advancing the ready compound parent records the child stages.
	advanceOK(t, cmd, root, "compound_pass", "implement", "pass", "")

	var out bytes.Buffer
	if err := runShow(cmd, &showParams{RunID: "compound_pass", StoreRoot: root}, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"implement.plan", "implement.plan.approval", "implement.do", "implement.test.tests", "implement.review", "implement.done", "stage:do"} {
		assertOutputContains(t, out.String(), want)
	}

	// Claimed done is not done: a stage pass without evidence is rejected.
	out.Reset()
	err := runAdvance(cmd, &advanceParams{RunID: "compound_pass", NodeID: "implement.plan", StoreRoot: root, Verdict: "pass", Actor: "human:johan"}, &out)
	if err == nil || !strings.Contains(err.Error(), "--evidence") {
		t.Fatalf("expected evidence requirement, got %v", err)
	}

	advanceOK(t, cmd, root, "compound_pass", "implement.plan", "pass", "artifacts/plan.md")
	advanceOK(t, cmd, root, "compound_pass", "implement.plan.approval", "pass", "approval:johan")

	// Work fails once and retries within its budget.
	advanceOK(t, cmd, root, "compound_pass", "implement.do", "fail", "")
	advanceOK(t, cmd, root, "compound_pass", "implement.do", "pass", "commit:abc123")

	advanceOK(t, cmd, root, "compound_pass", "implement.test.tests", "pass", "artifacts/test-log.txt")
	advanceOK(t, cmd, root, "compound_pass", "implement.review", "pass", "artifacts/review.md")

	out.Reset()
	if err := runVerify(cmd.Context(), &verifyParams{RunID: "compound_pass", StoreRoot: root}, &out); err != nil {
		t.Fatalf("final verify: %v\n%s", err, out.String())
	}
	assertOutputContains(t, out.String(), "Effective status: completed")

	out.Reset()
	if err := runShow(cmd, &showParams{RunID: "compound_pass", StoreRoot: root}, &out); err != nil {
		t.Fatal(err)
	}
	assertOutputContains(t, out.String(), "evidence: commit:abc123")
}

func TestCompoundGateFailurePoisonsToBlocked(t *testing.T) {
	cmd, root := compoundFlowSetup(t, "compound_poison")

	advanceOK(t, cmd, root, "compound_poison", "implement", "pass", "")
	advanceOK(t, cmd, root, "compound_poison", "implement.plan", "pass", "artifacts/plan.md")
	advanceOK(t, cmd, root, "compound_poison", "implement.plan.approval", "pass", "approval:johan")
	advanceOK(t, cmd, root, "compound_poison", "implement.do", "pass", "commit:abc123")

	// A failed gate poisons the node: child and parent block, the run does not
	// auto-fail, and reason plus owner stay legible from the run dir alone.
	advanceOK(t, cmd, root, "compound_poison", "implement.test.tests", "fail", "artifacts/test-log.txt")

	var out bytes.Buffer
	if err := runVerify(cmd.Context(), &verifyParams{RunID: "compound_poison", StoreRoot: root}, &out); err != nil {
		t.Fatalf("verify after poison: %v\n%s", err, out.String())
	}
	assertOutputContains(t, out.String(), "Effective status: running")

	out.Reset()
	if err := runShow(cmd, &showParams{RunID: "compound_poison", StoreRoot: root}, &out); err != nil {
		t.Fatal(err)
	}
	assertOutputContains(t, out.String(), "blocked")
	// The strawman template declares no gate retry budget, so the default of
	// one failed verdict is spent immediately.
	assertOutputContains(t, out.String(), `gate "implement.test.tests" exhausted its budget of 1 failed verdicts`)
	assertOutputContains(t, out.String(), "owner human:operator")
	assertOutputContains(t, out.String(), " since ")
	fs, err := openStore(root, true)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := fs.LoadRun(t.Context(), "compound_poison")
	if err != nil {
		t.Fatal(err)
	}
	child := snapshot.State.Nodes["implement.test.tests"]
	if child.BlockedAt.IsZero() {
		t.Fatal("manual poison did not persist blockedAt")
	}
	contactFound := false
	for commandID, command := range snapshot.State.OutstandingCommands {
		if command.Kind == state.CommandKindBlockNode && command.NodeID == "implement.test.tests" {
			contact, ok := snapshot.State.Contacts[commandID]
			contactFound = ok && contact.Kind == state.WaitKindHuman && contact.Assignee == "human:operator"
		}
	}
	if !contactFound {
		t.Fatalf("manual poison missing blocked owner contact: %#v", snapshot.State.Contacts)
	}

	// Blocked nodes are not advanceable.
	out.Reset()
	err = runAdvance(cmd, &advanceParams{RunID: "compound_poison", NodeID: "implement.test.tests", StoreRoot: root, Verdict: "pass", EvidenceRef: "x", Actor: "human:johan"}, &out)
	if err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("expected blocked advance refusal, got %v", err)
	}
	out.Reset()
	err = runAdvance(cmd, &advanceParams{RunID: "compound_poison", NodeID: "implement", StoreRoot: root, Verdict: "pass", Actor: "human:johan"}, &out)
	if err == nil || !strings.Contains(err.Error(), "stage children") {
		t.Fatalf("expected expanded-parent advance refusal, got %v", err)
	}
}

func TestCompoundTamperedExpansionFailsVerify(t *testing.T) {
	cmd, root := compoundFlowSetup(t, "compound_tamper")
	advanceOK(t, cmd, root, "compound_tamper", "implement", "pass", "")

	// Adversarial tamper: hand-edit the recorded expansion in state.json so it
	// no longer matches what the pinned template derives.
	statePath := filepath.Join(root, "runs", "compound_tamper", "state.json")
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	nodes := doc["nodes"].(map[string]any)
	implement := nodes["implement"].(map[string]any)
	children := implement["children"].([]any)
	children[1] = "implement.backdoor"
	tampered, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, tampered, 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err = runVerify(cmd.Context(), &verifyParams{RunID: "compound_tamper", StoreRoot: root}, &out)
	if err == nil {
		t.Fatalf("tampered expansion must fail verify:\n%s", out.String())
	}
	assertOutputContains(t, out.String(), "expansion_template_mismatch")

	out.Reset()
	err = runAdvance(cmd, &advanceParams{RunID: "compound_tamper", NodeID: "implement.plan", StoreRoot: root, Verdict: "pass", EvidenceRef: "x", Actor: "human:johan"}, &out)
	if err == nil || !strings.Contains(err.Error(), "refusing to advance") {
		t.Fatalf("expected advance refusal on tampered run, got %v", err)
	}
}
