package agentd_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// havePiSeancePredecessor stands up a retired pi generation with a real
// session file, recorded under the given sandbox implementation and approval
// posture, and a live successor of the same actor. It returns the two conv
// ids and the predecessor's session file.
func havePiSeancePredecessor(t *testing.T, f *testharness.Flow, implementation, approval string) (string, string, string) {
	t.Helper()
	f.HaveGroup("alpha")
	resp, sim := spawnPi(t, f, "alpha", map[string]any{
		"name":            "pi-elder",
		"initial_message": "remember the token",
	})
	require.NoError(t, sim.Reply("the token is 42"))
	rows, err := db.FindSessionsByConvID(resp.ConvID)
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	snapshot := sandboxpolicy.EmptySnapshot()
	for _, row := range rows {
		row.HarnessBuiltinMode = harness.PiSandboxOff
		row.SandboxImplementation = implementation
		row.ApprovalPolicy = approval
		row.EffectiveSandbox = &snapshot
		require.NoError(t, db.SaveSession(row))
	}
	source := sim.Path()
	require.FileExists(t, source)

	const newConv = "9e3e0000-1111-2222-3333-555555555555"
	haveSeanceSession(f, newConv, "pi-heir", "tmux-pi-heir", f.TestCwd("pi-heir"))
	_, err = db.RotateAgentConv(resp.ConvID, newConv, "reincarnate")
	require.NoError(t, err)
	return resp.ConvID, newConv, source
}

// fakePiSeance plays `pi --print --session <copy>`: it checks the copy holds
// the predecessor's conversation, appends a turn to it as pi would, and
// answers.
func fakePiSeance(t *testing.T, source string, captured *agentd.SeanceExecPlan) {
	t.Helper()
	previous := agentd.RunSeanceHarness
	agentd.RunSeanceHarness = func(_ context.Context, plan agentd.SeanceExecPlan) agentd.SeanceExecResult {
		*captured = plan
		copyPath := ""
		if at := slices.Index(plan.Argv, "--session"); at >= 0 && at+1 < len(plan.Argv) {
			copyPath = plan.Argv[at+1]
		} else if _, rest, ok := strings.Cut(strings.Join(plan.Argv, " "), "--session "); ok {
			copyPath = strings.Trim(strings.Fields(rest)[0], "'")
		}
		original, _ := os.ReadFile(source)
		if got, err := os.ReadFile(copyPath); err != nil || string(got) != string(original) {
			return agentd.SeanceExecResult{Stdout: "no copy\n", Started: true}
		}
		f, err := os.OpenFile(copyPath, os.O_APPEND|os.O_WRONLY, 0)
		if err == nil {
			_, _ = f.WriteString(`{"type":"message","id":"zz","parentId":null,"message":{"role":"user","content":"What was the token?"}}` + "\n")
			_ = f.Close()
		}
		return agentd.SeanceExecResult{Stdout: "42\n", Started: true}
	}
	t.Cleanup(func() { agentd.RunSeanceHarness = previous })
}

// TestSeanceRun_PiContinuesACopyUnderItsOwnPosture: a pi séance replays the
// predecessor's recorded approval posture on a copy of its conversation, and
// leaves nothing behind: not the copy, not a turn in the predecessor's file.
func TestSeanceRun_PiContinuesACopyUnderItsOwnPosture(t *testing.T) {
	f := newFlow(t)
	oldConv, newConv, source := havePiSeancePredecessor(t, f,
		string(sandboxpolicy.ImplementationHarnessBuiltin), harness.PiApprovalReadOnly)
	before, err := os.ReadFile(source)
	require.NoError(t, err)

	var captured agentd.SeanceExecPlan
	fakePiSeance(t, source, &captured)
	got, status, body := requestSeanceRun(t, f, newConv, map[string]any{
		"target": oldConv, "question": "What was the token?",
	})
	require.Equal(t, http.StatusOK, status, "body=%s", body)
	assert.Equal(t, "42\n", got.Answer)
	assert.Equal(t, harness.PiName, got.Harness)

	require.NotEmpty(t, captured.Argv)
	assert.Equal(t, "pi", captured.Argv[0])
	command := strings.Join(captured.Argv, " ")
	assert.Contains(t, command, "--print")
	assert.Contains(t, command, "--tools read,grep,find,ls", "the recorded read-only posture is replayed")
	assert.Contains(t, command, "-- What was the token?")
	copyPath := captured.Argv[slices.Index(captured.Argv, "--session")+1]
	assert.NotEqual(t, source, copyPath, "pi is handed a copy, never the original")
	assert.Equal(t, "tclaude-seance", filepath.Base(filepath.Dir(filepath.Dir(copyPath))))

	assert.NoFileExists(t, copyPath, "the copy is removed")
	after, err := os.ReadFile(source)
	require.NoError(t, err)
	assert.Equal(t, before, after, "the predecessor's conversation is untouched")
}

// TestSeanceRun_PiTclaudeLayerGenerationStaysInsideTheLayer: a generation
// recorded under tclaude's sandbox is consulted inside it again, against a
// walled agent directory of its own.
func TestSeanceRun_PiTclaudeLayerGenerationStaysInsideTheLayer(t *testing.T) {
	if err := session.TclaudeLayerServerHostAvailability(); err != nil {
		t.Skipf("tclaude layer unavailable: %v", err)
	}
	f := newFlow(t)
	oldConv, newConv, source := havePiSeancePredecessor(t, f,
		string(sandboxpolicy.ImplementationTclaudeLayer), harness.PiApprovalAuto)

	var captured agentd.SeanceExecPlan
	fakePiSeance(t, source, &captured)
	_, status, body := requestSeanceRun(t, f, newConv, map[string]any{
		"target": oldConv, "question": "What was the token?",
	})
	require.Equal(t, http.StatusOK, status, "body=%s", body)
	require.Len(t, captured.Argv, 3)
	assert.Equal(t, []string{"/bin/sh", "-c"}, captured.Argv[:2], "the turn runs inside tclaude's sandbox")
	assert.Contains(t, captured.Argv[2], "--session")
	assert.Contains(t, captured.Argv[2], harness.PiAgentDirEnvVar+"=")
	assert.Contains(t, captured.Argv[2], filepath.Join(".tclaude", "pi-agent"), "pi runs against a walled agent directory")
}
