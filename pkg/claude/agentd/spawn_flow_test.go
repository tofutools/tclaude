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
	f.AssertGroupMember("alpha", spawn.ConvID, "worker", 5*time.Second)

	f.MarkOffline(spawn.TmuxSession)
	resume := f.AsHuman().Resume(spawn.ConvID)
	f.AssertResumeSpawned(resume)
}

// Scenario: `tclaude agent spawn alpha --name reviewer`.
//
// The agent has exactly one name — its conversation title. The spawn
// `--name` becomes that title via the post-spawn /rename injection;
// there is no separate per-group alias. This pins the two guarantees
// the human cares about:
//   - the new agent's title (its single name) IS the spawned name, as
//     seen at `tclaude agent groups members alpha`;
//   - that name is a usable selector — `tclaude agent message reviewer
//     ...` resolves to the spawned conv-id, because resolution now
//     goes through the conv title, not a member-row alias.
func TestSpawn_NameBecomesTitleResolvableBySelector(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("alpha")

	spawn := f.AsHuman().Spawn("alpha", "reviewer")

	// The post-spawn injection renames the pane to the given name.
	f.AssertSentContains(spawn.TmuxTarget(), "/rename reviewer", 5*time.Second)

	// The name surfaces as the agent's title on the members view.
	f.AssertGroupMember("alpha", spawn.ConvID, "reviewer", 5*time.Second)

	// And the name resolves as a selector — what `tclaude agent
	// lookup reviewer` / `agent message reviewer ...` rely on.
	f.AssertResolvesByTitle("reviewer", spawn.ConvID, 5*time.Second)
}
