package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Scenario (JOH-200 acceptance): a daemon-spawned Codex agent runs detached
// in tmux with no human at its TUI, so it must come up with a NON-ESCALATING
// approval posture — otherwise any boundary-crossing tool call surfaces an
// approval prompt no one can answer and the agent deadlocks forever. The fix
// is to default the spawn's `--ask-for-approval` to `never` (safe because the
// agent is sandbox-confined by default, JOH-192).
//
// These pin the spawned Codex argv's approval posture, observed via the
// simSpawner's recorded approval policy — the same surface the sandbox/effort/
// model flow tests assert against.

// TestCodexSpawn_DefaultsToNonEscalatingApproval: a plain Codex spawn resolves
// to the non-escalating default (never) and threads it through the spawn path,
// so the unattended pane can't deadlock on an approval prompt.
func TestCodexSpawn_DefaultsToNonEscalatingApproval(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("codex-crew")

	spawn := f.AsHuman().SpawnHarness("codex-crew", "codex-worker", "codex")

	got, ok := f.World.SpawnApproval(spawn.ConvID)
	require.True(t, ok, "the codex spawn should have been observed by the sim spawner")
	assert.Equal(t, "never", got,
		"a daemon-spawned (unattended) Codex agent must default to the non-escalating `never` approval policy")
}

// TestCodexSpawn_ExplicitApprovalThreadsThrough: an explicit approval policy
// on the spawn request is validated and threaded through verbatim — the knob
// is a real per-spawn override, not just a fixed default. (A human who plans
// to attach to the pane can ask for an escalating policy.)
func TestCodexSpawn_ExplicitApprovalThreadsThrough(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("codex-crew")

	resp := f.AsHuman().SpawnWith("codex-crew", map[string]any{
		"name":     "supervised",
		"harness":  "codex",
		"approval": "on-request",
	})
	require.Equal(t, 200, resp.Code,
		"an explicit valid approval policy must be accepted; body=%s", resp.Raw)

	got, ok := f.World.SpawnApproval(resp.ConvID)
	require.True(t, ok, "the codex spawn should have been observed by the sim spawner")
	assert.Equal(t, "on-request", got,
		"an explicit approval policy must thread through unchanged")
}

// TestCodexSpawn_InvalidApprovalRejected: a bad approval policy is a 400 at
// the spawn boundary, not a silent fork that exits non-zero later.
func TestCodexSpawn_InvalidApprovalRejected(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("codex-crew")

	resp := f.AsHuman().SpawnWith("codex-crew", map[string]any{
		"name":     "bad-approval",
		"harness":  "codex",
		"approval": "yolo",
	})
	require.Equal(t, 400, resp.Code,
		"an invalid approval policy must be refused with a 400; body=%s", resp.Raw)
	assert.Contains(t, string(resp.Raw), "invalid_approval",
		"the refusal should name the approval validation; body=%s", resp.Raw)
}

// TestClaudeSpawn_HasNoApprovalNever: a plain claude spawn resolves its approval
// posture to the Claude harness default, `auto` — a real --permission-mode value,
// NOT a Codex `never` (which Claude can't parse). The detached pane comes up on
// the supervisor-classifier mode instead of an unknown settings.json posture.
func TestClaudeSpawn_HasNoApprovalNever(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("cc-crew")

	spawn := f.AsHuman().Spawn("cc-crew", "cc-worker")

	got, ok := f.World.SpawnApproval(spawn.ConvID)
	require.True(t, ok, "the claude spawn should have been observed by the sim spawner")
	assert.Equal(t, "auto", got,
		"a Claude Code spawn resolves approval to the auto default, never a Codex policy")
}
