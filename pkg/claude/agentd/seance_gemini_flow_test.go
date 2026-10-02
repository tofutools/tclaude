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

// haveGeminiSeancePredecessor stands up a retired Gemini generation with a
// real session file, recorded under the given sandbox posture, and a live
// successor of the same actor. It returns the two conv ids and the
// predecessor's session file.
func haveGeminiSeancePredecessor(t *testing.T, f *testharness.Flow, mode, implementation string) (string, string, string) {
	t.Helper()
	f.HaveGroup("alpha")
	resp, sim := spawnGemini(t, f, "alpha", map[string]any{
		"name":            "gemini-elder",
		"initial_message": "remember the token",
	})
	sim.WriteGeminiReply("the token is 42", "gemini-3.5-flash")
	rows, err := db.FindSessionsByConvID(resp.ConvID)
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	snapshot := sandboxpolicy.EmptySnapshot()
	for _, row := range rows {
		row.HarnessBuiltinMode = mode
		row.SandboxImplementation = implementation
		row.ApprovalPolicy = harness.GeminiApprovalYolo
		row.EffectiveSandbox = &snapshot
		require.NoError(t, db.SaveSession(row))
	}
	source, found, err := harness.LocateGeminiSessionFile(resp.ConvID)
	require.NoError(t, err)
	require.True(t, found, "the predecessor's session file must exist")

	const newConv = "9e3e0000-1111-2222-3333-444444444444"
	haveSeanceSession(f, newConv, "gemini-heir", "tmux-gemini-heir", f.TestCwd("gemini-heir"))
	_, err = db.RotateAgentConv(resp.ConvID, newConv, "reincarnate")
	require.NoError(t, err)
	return resp.ConvID, newConv, source
}

// fakeGeminiSeance plays `gemini --session-file <copy>`: it imports the copy
// into a new session in the predecessor's chats directory, as the CLI does,
// and answers.
func fakeGeminiSeance(t *testing.T, chats string, captured *agentd.SeanceExecPlan) {
	t.Helper()
	previous := agentd.RunSeanceHarness
	agentd.RunSeanceHarness = func(_ context.Context, plan agentd.SeanceExecPlan) agentd.SeanceExecResult {
		*captured = plan
		command := strings.Join(plan.Argv, " ")
		at := slices.Index(plan.Argv, "--session-file")
		copyPath := ""
		if at >= 0 && at+1 < len(plan.Argv) {
			copyPath = plan.Argv[at+1]
		} else if _, rest, ok := strings.Cut(command, "--session-file "); ok {
			copyPath = strings.Trim(strings.Fields(rest)[0], "'")
		}
		if copyPath != "" {
			fork := filepath.Join(chats, "session-1790925491807-feedf00d.jsonl")
			_ = os.WriteFile(fork, []byte(`{"sessionId":"feedf00d-0000-0000-0000-000000000000","kind":"main"}`+"\n"+
				`{"id":"import-1","type":"info","content":"Imported session from `+copyPath+`"}`+"\n"), 0o644)
		}
		return agentd.SeanceExecResult{Stdout: "42\n", Started: true}
	}
	t.Cleanup(func() { agentd.RunSeanceHarness = previous })
}

// TestSeanceRun_GeminiForksAndReplaysItsOwnPosture: a Gemini séance replays
// the predecessor's recorded sandbox mode and approval on a FORK of a copy of
// its conversation, and leaves nothing behind: not the fork, not the copy,
// not a turn in the predecessor's own file.
func TestSeanceRun_GeminiForksAndReplaysItsOwnPosture(t *testing.T) {
	f := newFlow(t)
	oldConv, newConv, source := haveGeminiSeancePredecessor(t, f,
		harness.GeminiSandboxOff, string(sandboxpolicy.ImplementationHarnessBuiltin))
	before, err := os.ReadFile(source)
	require.NoError(t, err)

	var captured agentd.SeanceExecPlan
	fakeGeminiSeance(t, filepath.Dir(source), &captured)
	got, status, body := requestSeanceRun(t, f, newConv, map[string]any{
		"target": oldConv, "question": "What was the token?",
	})
	require.Equal(t, http.StatusOK, status, "body=%s", body)
	assert.Equal(t, "42\n", got.Answer)
	assert.Equal(t, harness.GeminiName, got.Harness)

	require.GreaterOrEqual(t, len(captured.Argv), 4)
	assert.Equal(t, []string{"env", "GEMINI_SANDBOX=false", "SANDBOX="}, captured.Argv[:3],
		"the recorded sandbox mode is replayed")
	command := strings.Join(captured.Argv, " ")
	assert.Contains(t, command, "--approval-mode=yolo")
	assert.Contains(t, command, "--prompt=What was the token?")
	assert.NotContains(t, command, "--resume", "a resume would append to the predecessor")
	copyPath := captured.Argv[slices.Index(captured.Argv, "--session-file")+1]
	assert.NotEqual(t, source, copyPath, "Gemini is handed a copy, never the original")

	assert.NoFileExists(t, copyPath)
	assert.NoFileExists(t, filepath.Join(filepath.Dir(source), "session-1790925491807-feedf00d.jsonl"),
		"the fork Gemini wrote is removed")
	after, err := os.ReadFile(source)
	require.NoError(t, err)
	assert.Equal(t, before, after, "the predecessor's conversation is untouched")
}

// TestSeanceRun_GeminiTclaudeLayerGenerationStaysInsideTheLayer: a generation
// recorded under tclaude's sandbox is consulted inside it again; its recorded
// native mode alone (`off`) would be no containment at all.
func TestSeanceRun_GeminiTclaudeLayerGenerationStaysInsideTheLayer(t *testing.T) {
	if err := session.TclaudeLayerServerHostAvailability(); err != nil {
		t.Skipf("tclaude layer unavailable: %v", err)
	}
	f := newFlow(t)
	oldConv, newConv, source := haveGeminiSeancePredecessor(t, f,
		harness.GeminiSandboxOff, string(sandboxpolicy.ImplementationTclaudeLayer))

	var captured agentd.SeanceExecPlan
	fakeGeminiSeance(t, filepath.Dir(source), &captured)
	_, status, body := requestSeanceRun(t, f, newConv, map[string]any{
		"target": oldConv, "question": "What was the token?",
	})
	require.Equal(t, http.StatusOK, status, "body=%s", body)
	require.Len(t, captured.Argv, 3)
	assert.Equal(t, []string{"/bin/sh", "-c"}, captured.Argv[:2], "the turn runs inside tclaude's sandbox")
	assert.Contains(t, captured.Argv[2], "--session-file")
	assert.Contains(t, captured.Argv[2], "GEMINI_SANDBOX=false")
}
