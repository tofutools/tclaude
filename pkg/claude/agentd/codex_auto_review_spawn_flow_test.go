package agentd_test

import (
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Scenario (JOH-200 part 2): auto-review (Codex's guardian subagent) is an
// experimental, opt-in posture, orthogonal to the approval policy. When a
// spawn requests it, the daemon must gate it on the harness having an approvals
// subsystem (Codex) and thread `--auto-review` through the spawn path; an
// ordinary spawn leaves it OFF (the human reviews), and a Claude Code spawn —
// which has no guardian — must reject the opt-in rather than silently drop it.
//
// These pin the spawned argv's auto-review opt-in via the simSpawner's recorded
// flag — the same surface the sandbox/approval flow tests assert against.

// TestCodexSpawn_AutoReviewDefaultsOff: a plain Codex spawn does not engage the
// experimental guardian, so the human stays the approval reviewer by default.
func TestCodexSpawn_AutoReviewDefaultsOff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)
		f.HaveGroup("codex-crew")

		spawn := f.AsHuman().SpawnHarness("codex-crew", "codex-worker", "codex")

		got, ok := f.World.SpawnAutoReview(spawn.ConvID)
		require.True(t, ok, "the codex spawn should have been observed by the sim spawner")
		assert.False(t, got,
			"a plain Codex spawn must default auto-review OFF (the human reviews approvals)")
	})
}

// TestCodexSpawn_AutoReviewOptInThreadsThrough: an explicit auto_review:true on
// the spawn request is gated and threaded through, so the opt-in is a real
// per-spawn knob — engaging Codex's guardian subagent for that pane.
func TestCodexSpawn_AutoReviewOptInThreadsThrough(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)
		f.HaveGroup("codex-crew")

		resp := f.AsHuman().SpawnWith("codex-crew", map[string]any{
			"name":        "guarded",
			"harness":     "codex",
			"auto_review": true,
		})
		require.Equal(t, 200, resp.Code,
			"auto_review opt-in on a Codex spawn must be accepted; body=%s", resp.Raw)

		got, ok := f.World.SpawnAutoReview(resp.ConvID)
		require.True(t, ok, "the codex spawn should have been observed by the sim spawner")
		assert.True(t, got,
			"an explicit auto_review opt-in must thread through to the spawned argv")
	})
}

// TestClaudeSpawn_RejectsAutoReview: Claude Code has no guardian subagent, so an
// auto_review opt-in on a claude spawn is a 400 at the boundary, not a flag
// silently dropped onto a harness that can't honour it.
func TestClaudeSpawn_RejectsAutoReview(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)
		f.HaveGroup("cc-crew")

		resp := f.AsHuman().SpawnWith("cc-crew", map[string]any{
			"name":        "no-guardian",
			"auto_review": true,
		})
		require.Equal(t, 400, resp.Code,
			"auto_review on a Claude Code spawn must be refused with a 400; body=%s", resp.Raw)
		assert.Contains(t, string(resp.Raw), "invalid_auto_review",
			"the refusal should name the auto-review gate; body=%s", resp.Raw)
	})
}
