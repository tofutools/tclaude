package agentd_test

import (
	"testing"
	"testing/synctest"
	"time"
)

// Scenario: a human spawns a new agent into a group.
//
// Setup: a group "alpha" exists. Nothing else.
//
// Action: the human asks the daemon to spawn a worker into "alpha".
//
// Expected:
//   - The daemon launches a fresh CC session (with a preset conv-id) and
//     registers it in the group.
//   - The agent is named via a launch arg (`claude --name worker`) — no
//     post-connect `/rename` is injected over tmux.
//   - When the worker later goes offline (CC crashed, human closed
//     the pane), `tclaude agent resume` brings it back via a fresh
//     subprocess — it does NOT silently report success against the
//     dead tmux session.
func TestSpawn_RenamesAndResumes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)

		f.HaveGroup("alpha")

		spawn := f.AsHuman().Spawn("alpha", "worker")

		// The name rides in as the launch display name, not a tmux injection.
		f.AssertSpawnName(spawn.ConvID, "worker", 5*time.Second)

		// What the human would see at the contact surface: `tclaude agent groups
		// members alpha` lists the new conv with title "worker". `claude --name`
		// writes a custom-title turn just like /rename, so the conv-index resolves
		// the title from the .jsonl exactly as before.
		f.AssertGroupMember("alpha", spawn.ConvID, "worker", 5*time.Second)

		// The launch-enrollment path injects NOTHING over tmux — the whole point.
		if sent := f.World.Tmux.Sent(); len(sent) != 0 {
			t.Fatalf("launch-enrollment spawn must not send-keys; got %+v", sent)
		}

		f.MarkOffline(spawn.TmuxSession)
		resume := f.AsHuman().Resume(spawn.ConvID)
		f.AssertResumeSpawned(resume)
	})
}

// Scenario: `tclaude agent spawn alpha --name reviewer`.
//
// The agent has exactly one name — its conversation title. The spawn
// `--name` becomes that title via the launch-arg rename (`claude --name`);
// there is no separate per-group alias. This pins the two guarantees
// the human cares about:
//   - the new agent's title (its single name) IS the spawned name, as
//     seen at `tclaude agent groups members alpha`;
//   - that name is a usable selector — `tclaude agent message reviewer
//     ...` resolves to the spawned conv-id, because resolution now
//     goes through the conv title, not a member-row alias.
func TestSpawn_NameBecomesTitleResolvableBySelector(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)

		f.HaveGroup("alpha")

		spawn := f.AsHuman().Spawn("alpha", "reviewer")

		// The name is applied at launch (`claude --name reviewer`), not injected.
		f.AssertSpawnName(spawn.ConvID, "reviewer", 5*time.Second)

		// The name surfaces as the agent's title on the members view.
		f.AssertGroupMember("alpha", spawn.ConvID, "reviewer", 5*time.Second)

		// And the name resolves as a selector — what `tclaude agent
		// lookup reviewer` / `agent message reviewer ...` rely on.
		f.AssertResolvesByTitle("reviewer", spawn.ConvID, 5*time.Second)
	})
}
