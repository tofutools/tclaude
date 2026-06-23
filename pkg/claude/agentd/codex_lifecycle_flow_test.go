package agentd_test

import (
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Scenario (JOH-160 acceptance): the daemon owns a Codex agent end to
// end — it spawns one into a group, the agent receives an inbox message
// like any peer, and a soft stop takes it down GRACEFULLY via Codex's
// `/quit` slash command rather than a hard kill-session.
//
// This is the integration payoff of the harness seam: the same spawn /
// message / stop machinery that drives Claude Code now drives Codex with
// no special-casing in the daemon's flow — only the harness descriptor
// differs. The pieces it pins:
//
//   - Spawn threads `--harness codex` all the way to the SessionRow, so
//     every downstream read (harnessForConv, resume, identity) resolves
//     the Codex harness. The simSpawner builds a CodexSim (a real
//     date-indexed rollout .jsonl), exactly as the production hook
//     callback would have recorded a real Codex launch.
//   - The Codex worker shows up in the group. Codex has no in-pane /rename
//     command, so the post-spawn rename degrades to the title store
//     (ConvStore.SetTitle, real since JOH-161). The simSpawner's CodexSim
//     models Codex's session-start threads-row creation, so that out-of-band
//     UPDATE now lands on a real row instead of warning "no threads row" —
//     the harness-aware rename path the lead signed off on (Q2).
//   - Messaging is harness-agnostic: a grouped peer's message lands in the
//     Codex worker's inbox and nudges its live pane the same as any agent.
//   - A soft stop injects Codex's `/quit` (verified vs openai/codex
//     rust-v0.139.0 → request_quit_without_confirmation) and the action is
//     "soft_stopped", NOT the "killed_no_soft_exit" fallback — the
//     explicit acceptance bar. The pane then reads offline.
func TestCodexAgent_SpawnMessageGracefulStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)

		f.HaveGroup("codex-crew")

		// A grouped peer that will message the Codex worker. Same group, so
		// intra-group messaging is allowed without the message.direct slug.
		const sender = "send-aaaa-bbbb-cccc-111111111111"
		f.HaveConvWithTitle(sender, "lead")
		f.HaveMember("codex-crew", sender)

		// Spawn a Codex agent through the daemon mux — the production
		// handleGroupSpawn → executeSpawn → Spawner.SpawnNew(--harness codex)
		// path, observed against the CodexSim instead of a real subprocess.
		spawn := f.AsHuman().SpawnHarness("codex-crew", "codex-worker", "codex")

		// The spawn tagged the SessionRow with the codex harness — the key the
		// whole soft-stop / resume / identity path reads back.
		sessions, err := db.FindSessionsByConvID(spawn.ConvID)
		require.NoError(t, err, "FindSessionsByConvID")
		require.NotEmpty(t, sessions, "spawned codex session row should exist")
		assert.Equal(t, "codex", sessions[0].Harness, "spawned session is tagged codex")

		// Membership surface — what `tclaude agent groups members codex-crew`
		// renders. Codex has no in-pane /rename, so the post-spawn rename
		// degrades to the native title store (ConvStore.SetTitle → threads.title,
		// now landing on the modeled session-start row); the displayed name
		// resolves to the intended --name either way.
		f.AssertGroupMember("codex-crew", spawn.ConvID, "codex-worker", 5*time.Second)

		// Let the post-spawn background goroutine settle BEFORE anything else
		// types into this pane. This spawn carries no briefing, so its [system: …]
		// welcome rides in the launch SEED (Codex's first-turn prompt), not a
		// post-connect send-keys — runSpawnPostInit therefore injects no welcome
		// here. But it still fires asynchronously (goBackground) to persist the
		// out-of-band Codex title, and historically (when the welcome WAS injected
		// here) a still-in-flight Enter could interleave with the `/quit` below and
		// wedge the quit handler — the rare macOS-CI flake. Waiting for the
		// background goroutine keeps that ordering guarantee regardless: the message
		// nudge and `/quit` below — both synchronous — can't race the post-init work.
		agentd.WaitForBackgroundForTest()

		// Message it: a grouped peer sends; the message lands in the Codex
		// worker's inbox and nudges its live pane — proving the messaging
		// transport is harness-agnostic.
		rec := postMessage(t, f, sender, map[string]any{"to": spawn.ConvID, "body": "hello codex"})
		require.Equal(t, http.StatusOK, rec.Code, "message send body=%s", rec.Body.String())
		rows, err := db.ListAgentMessagesForConv(spawn.ConvID, 100)
		require.NoError(t, err, "ListAgentMessagesForConv")
		require.Len(t, rows, 1, "message landed in the codex worker's inbox")
		assert.Equal(t, "hello codex", rows[0].Body)
		f.AssertSentContains(spawn.TmuxTarget(), "new agent message", 2*time.Second)

		// Stop GRACEFULLY. The daemon resolves the conv's harness, sees a
		// soft-exit command, and injects Codex's `/quit`. The result must be
		// "soft_stopped" — the lead's explicit ask is that this exercises the
		// graceful path, not the killed_no_soft_exit hard-kill fallback.
		stop := f.AsHuman().Stop(spawn.ConvID, false)
		f.AssertSoftStopped(stop)
		f.AssertSentContains(spawn.TmuxTarget(), "/quit", 2*time.Second)
		reason, err := db.GetSessionExitReason(sessions[0].ID)
		require.NoError(t, err, "GetSessionExitReason")
		assert.Equal(t, "soft_exit", reason,
			"daemon soft-stop must record a clean reason before the reaper sees Codex disappear")

		// `/quit` took the pane down: the CodexSim's quit handler flipped it
		// dead, so has-session now reports offline. With the welcome drained
		// above, the `/quit` injection no longer races it on the input buffer,
		// so MarkDead runs synchronously during the stop; the short poll stays
		// only as a defensive guard against send-keys settle timing.
		require.Eventually(t, func() bool {
			return !f.World.Tmux.IsAlive(spawn.TmuxSession)
		}, 2*time.Second, 10*time.Millisecond,
			"after a graceful /quit the codex pane must be offline")

		// Codex has no SessionEnd hook, so the reaper is what persists
		// status=exited after /quit. It must preserve the daemon's clean
		// reason instead of stamping exit_reason=unexpected, which the
		// dashboard renders as "crashed".
		reaper := agentd.NewSessionReaperForTest(0, func(string, string) {})
		require.Equal(t, 1, reaper.Tick(), "the stopped Codex session should be reaped")
		reason, err = db.GetSessionExitReason(sessions[0].ID)
		require.NoError(t, err, "GetSessionExitReason after reaper")
		assert.Equal(t, "soft_exit", reason,
			"reaper must preserve the daemon-recorded clean Codex shutdown reason")

		t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
		snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
		var got *dashMember
		for _, g := range snap.Groups {
			for i := range g.Members {
				if g.Members[i].ConvID == spawn.ConvID {
					got = &g.Members[i]
				}
			}
		}
		require.NotNil(t, got, "stopped Codex worker should still be listed in its group")
		assert.False(t, got.Online, "stopped Codex worker should be offline")
		assert.Equal(t, "soft_exit", got.State.ExitReason,
			"dashboard must render daemon-stopped Codex as offline, not crashed")
	})
}
