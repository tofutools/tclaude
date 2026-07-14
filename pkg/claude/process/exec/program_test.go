package processexec

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/plan"
)

func TestProgramAdapterCapturesFailureAndBoundedOutputTails(t *testing.T) {
	adapter := ProgramAdapter{DefaultTimeout: 5 * time.Second, OutputTailBytes: 5}
	request := Request{
		Command: plan.Command{ID: "cmd_test", IdempotencyKey: "run/start", RunID: "run"},
		Performer: model.Performer{
			Kind: model.PerformerProgram,
			Run:  "/bin/sh",
			Args: []string{"-c", "printf 123456789; printf abcdefghi >&2; exit 7"},
		},
	}
	observation, err := adapter.Perform(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Actor != "program:/bin/sh@exit7" || observation.Verdict != "fail" || observation.Evidence == nil {
		t.Fatalf("observation = %#v", observation)
	}
	var evidence ProgramEvidence
	if err := json.Unmarshal(observation.Evidence.Data, &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence.ExitCode != 7 || evidence.StdoutTail != "56789" || evidence.StderrTail != "efghi" {
		t.Fatalf("evidence = %#v", evidence)
	}
	if !strings.Contains(evidence.Error, "exit status 7") {
		t.Fatalf("evidence error = %q", evidence.Error)
	}
}

func TestProgramAdapterRejectsInvalidTimeoutBeforeExecution(t *testing.T) {
	adapter := ProgramAdapter{}
	err := adapter.Validate(Request{Performer: model.Performer{
		Kind:    model.PerformerProgram,
		Run:     "/bin/sh",
		Timeout: "not-a-duration",
	}})
	if err == nil || !strings.Contains(err.Error(), "invalid program timeout") {
		t.Fatalf("expected timeout error, got %v", err)
	}
}

func TestProgramAdapterDoesNotInheritParentSecrets(t *testing.T) {
	t.Setenv("TCLAUDE_SECRET_TOKEN", "must-not-leak")
	adapter := ProgramAdapter{DefaultTimeout: 5 * time.Second}
	observation, err := adapter.Perform(t.Context(), Request{
		Command: plan.Command{ID: "cmd_env", IdempotencyKey: "run/env", RunID: "run"},
		Performer: model.Performer{
			Kind: model.PerformerProgram,
			Run:  "/bin/sh",
			Args: []string{"-c", `printf %s "${TCLAUDE_SECRET_TOKEN-unset}"`},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if observation.Evidence == nil {
		t.Fatalf("observation = %#v", observation)
	}
	var evidence ProgramEvidence
	if err := json.Unmarshal(observation.Evidence.Data, &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence.StdoutTail != "unset" || strings.Contains(string(observation.Evidence.Data), "must-not-leak") {
		t.Fatalf("parent secret leaked into program evidence: %#v", evidence)
	}
}
