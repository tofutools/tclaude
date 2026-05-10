package agentd_test

import (
	"testing"
	"time"
)

// Scenario: a human spawns a new agent into a group.
//
// Setup: a group "alpha" exists. Nothing else.
//
// Action: the human asks the daemon to spawn a worker into "alpha".
//
// Expected:
//   - The daemon launches a fresh CC session and registers it in
//     the group.
//   - Shortly after the response returns, a background goroutine
//     types `/rename worker` into the new pane so the agent shows
//     up under that name.
//   - When the worker later goes offline (CC crashed, human closed
//     the pane), `tclaude agent resume` brings it back via a fresh
//     subprocess — it does NOT silently report success against the
//     dead tmux session.
func TestSpawn_RenamesAndResumes(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("alpha")

	spawn := f.AsHuman().Spawn("alpha", "worker")

	// /rename lands ~2.5s after spawn returns (waitForConvAlive
	// poll + readyDelay + two paste-mode pauses); 5s gives slack.
	f.AssertSentContains(spawn.TmuxTarget(), "/rename worker", 5*time.Second)

	// What the human would see at the contact surface after the
	// rename settles: `tclaude agent groups members alpha` lists the
	// new conv with title "worker". Pinning at this surface catches
	// the bug class where the daemon's send-keys returns success but
	// the rename never actually lands as a renderable title (CC
	// dropped it, paste-mode swallowed Enter, etc.).
	f.AssertGroupMember("alpha", spawn.ConvID, "worker", "worker", 5*time.Second)

	f.MarkOffline(spawn.TmuxSession)
	resume := f.AsHuman().Resume(spawn.ConvID)
	f.AssertResumeSpawned(resume)
}
