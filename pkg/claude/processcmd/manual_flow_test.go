package processcmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestManualRunFlowAndAdvanceRefusesInconsistentRun(t *testing.T) {
	ctx := context.Background()
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	root := filepath.Join(t.TempDir(), "store")
	templatePath := writeManualFlowTemplate(t)

	var out bytes.Buffer
	if err := runRun(cmd, &runParams{
		Template:  templatePath,
		StoreRoot: root,
		RunID:     "manual_flow",
		Param:     []string{"ticket=TCL-271"},
	}, &out); err != nil {
		t.Fatal(err)
	}
	assertOutputContains(t, out.String(), "Created run manual_flow")

	out.Reset()
	if err := runVerify(ctx, &verifyParams{RunID: "manual_flow", StoreRoot: root}, &out); err != nil {
		t.Fatalf("verify after create: %v\n%s", err, out.String())
	}
	assertOutputContains(t, out.String(), "Effective status: running")

	out.Reset()
	if err := runTemplatesLs(cmd, &templatesLsParams{StoreRoot: root}, &out); err != nil {
		t.Fatal(err)
	}
	assertOutputContains(t, out.String(), "manual-demo@sha256:")

	out.Reset()
	if err := runRunsLs(cmd, &runsLsParams{StoreRoot: root}, &out); err != nil {
		t.Fatal(err)
	}
	assertOutputContains(t, out.String(), "manual_flow")

	out.Reset()
	if err := runAdvance(cmd, &advanceParams{RunID: "manual_flow", NodeID: "implement", StoreRoot: root, Verdict: "fail", Actor: "human:johan"}, &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runVerify(ctx, &verifyParams{RunID: "manual_flow", StoreRoot: root}, &out); err != nil {
		t.Fatalf("verify after retryable fail: %v\n%s", err, out.String())
	}

	out.Reset()
	if err := runAdvance(cmd, &advanceParams{RunID: "manual_flow", NodeID: "implement", StoreRoot: root, Verdict: "pass", Actor: "human:johan"}, &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runShow(cmd, &showParams{RunID: "manual_flow", StoreRoot: root}, &out); err != nil {
		t.Fatal(err)
	}
	assertOutputContains(t, out.String(), "decide")
	assertOutputContains(t, out.String(), "ready")
	out.Reset()
	if err := runVerify(ctx, &verifyParams{RunID: "manual_flow", StoreRoot: root}, &out); err != nil {
		t.Fatalf("verify after pass: %v\n%s", err, out.String())
	}

	out.Reset()
	if err := runAdvance(cmd, &advanceParams{RunID: "manual_flow", NodeID: "decide", StoreRoot: root, Verdict: "approve", Actor: "human:johan"}, &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runVerify(ctx, &verifyParams{RunID: "manual_flow", StoreRoot: root}, &out); err != nil {
		t.Fatalf("verify after decision: %v\n%s", err, out.String())
	}
	assertOutputContains(t, out.String(), "Effective status: completed")

	out.Reset()
	if err := runShow(cmd, &showParams{RunID: "manual_flow", StoreRoot: root, Mermaid: true}, &out); err != nil {
		t.Fatal(err)
	}
	assertOutputContains(t, out.String(), "graph TD")
	assertOutputContains(t, out.String(), "|approve|")

	tornPath := filepath.Join(root, "runs", "manual_flow", "nodes", "implement", "log.jsonl")
	f, err := os.OpenFile(tornPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"schemaVersion":1,"seq":99`); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	if err := runVerify(ctx, &verifyParams{RunID: "manual_flow", StoreRoot: root}, &out); err == nil {
		t.Fatalf("expected verify failure after torn write:\n%s", out.String())
	}
	assertOutputContains(t, out.String(), "read_torn_tail")
	assertOutputContains(t, out.String(), "nodes/implement/log.jsonl")

	out.Reset()
	err = runAdvance(cmd, &advanceParams{RunID: "manual_flow", NodeID: "end", StoreRoot: root, Verdict: "pass", Actor: "human:johan"}, &out)
	if err == nil || !strings.Contains(err.Error(), "refusing to advance") {
		t.Fatalf("expected advance refusal, got %v\n%s", err, out.String())
	}
	assertOutputContains(t, out.String(), "read_torn_tail")
}

func TestManualFailurePathTerminatesRun(t *testing.T) {
	ctx := context.Background()
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	root := filepath.Join(t.TempDir(), "store")

	var out bytes.Buffer
	if err := runRun(cmd, &runParams{
		Template:  writeManualFlowTemplate(t),
		StoreRoot: root,
		RunID:     "manual_failure",
		Param:     []string{"ticket=TCL-271"},
	}, &out); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	if err := runAdvance(cmd, &advanceParams{RunID: "manual_failure", NodeID: "implement", StoreRoot: root, Verdict: "fail", Actor: "human:johan"}, &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runVerify(ctx, &verifyParams{RunID: "manual_failure", StoreRoot: root}, &out); err != nil {
		t.Fatalf("verify after retryable fail: %v\n%s", err, out.String())
	}
	assertOutputContains(t, out.String(), "Effective status: running")

	out.Reset()
	if err := runAdvance(cmd, &advanceParams{RunID: "manual_failure", NodeID: "implement", StoreRoot: root, Verdict: "fail", Actor: "human:johan"}, &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runVerify(ctx, &verifyParams{RunID: "manual_failure", StoreRoot: root}, &out); err != nil {
		t.Fatalf("verify after terminal fail: %v\n%s", err, out.String())
	}
	assertOutputContains(t, out.String(), "Effective status: failed")

	out.Reset()
	if err := runShow(cmd, &showParams{RunID: "manual_failure", StoreRoot: root}, &out); err != nil {
		t.Fatal(err)
	}
	assertOutputContains(t, out.String(), "failed")
	assertOutputContains(t, out.String(), "completed")
}

func writeManualFlowTemplate(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manual-demo.yaml")
	body := `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: manual-demo
name: Manual demo {{ params.ticket }}
params:
  ticket:
    type: string
    required: true
start: implement
nodes:
  implement:
    type: task
    performer:
      kind: human
      ask: Implement the change
    retry:
      maxAttempts: 2
    next:
      pass: decide
      fail: failed
  decide:
    type: decision
    performer:
      kind: human
      ask: Ship it?
    next:
      approve: end
      reject: failed
  failed:
    type: end
    result: failed
  end:
    type: end
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertOutputContains(t *testing.T, text, want string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Fatalf("output missing %q:\n%s", want, text)
	}
}
