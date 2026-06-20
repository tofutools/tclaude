package agentd_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Scenario: the default (efficient) Claude Code spawn flow.
//
// Instead of launching a bare `claude`, polling for its conv-id, and then
// injecting `/rename <name>` + the welcome over tmux with delays, the daemon
// presets the conv-id (`claude --session-id`), enrolls the agent, and bakes the
// rename (`--name`) + welcome (the positional prompt) into the launch command.
//
// Expected:
//   - the session row carries the PRESET conv-id (the one the spawn returned),
//     proving the daemon knew it before launch rather than waiting on the hook;
//   - the name was applied as a launch arg and resolves as the conversation
//     title (claude --name writes a custom-title turn just like /rename);
//   - the welcome rode in as the launch prompt and still points the agent at
//     its inbox briefing by id — identical content, delivered more efficiently;
//   - NOTHING was injected over tmux.
func TestSpawn_LaunchEnrollment_PresetsConvIDNoInjection(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	const brief = "Investigate the flaky deploy job and report back"
	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name":            "worker",
		"initial_message": brief,
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	// The conv-id was known before launch: the session row carries the same id
	// the spawn returned (preset via claude --session-id), not one a later hook
	// back-filled.
	s, err := db.LoadSession(spawn.Label)
	require.NoError(t, err)
	require.NotNil(t, s, "spawned session row missing")
	assert.Equal(t, spawn.ConvID, s.ConvID,
		"session row conv-id should be the daemon-preset id")

	// The name + welcome rode in as launch args, not tmux injection.
	f.AssertSpawnName(spawn.ConvID, "worker", 2*time.Second)
	msg := soleInboxMessage(t, spawn.ConvID)
	f.AssertSpawnInitialPrompt(spawn.ConvID,
		fmt.Sprintf("inbox read %d", msg.ID), 2*time.Second)

	// The title resolves from the .jsonl exactly as a /rename would.
	f.AssertGroupMember("alpha", spawn.ConvID, "worker", 2*time.Second)

	// Nothing was injected over tmux — the whole point of the new path.
	if sent := f.World.Tmux.Sent(); len(sent) != 0 {
		t.Fatalf("launch-enrollment spawn must not send-keys; got %+v", sent)
	}
}

// Scenario: a slow host — the forked `tclaude session new`'s session-row write
// lags past the daemon's conv-id poll, so the poll times out even though the
// pane is coming up fine (named + greeted via the launch args).
//
// Expected: because the launch-enrollment path preset the conv-id and enrolled
// the agent BEFORE forking, the daemon returns SUCCESS against the preset id and
// KEEPS the enrollment — it must NOT roll back, which would strand a live,
// named, greeted, group-less pane whose welcome points at a deleted briefing.
func TestSpawn_LaunchEnrollment_SlowRowWriteKeepsEnrollment(t *testing.T) {
	f := newFlow(t)
	// Shrink the async grace so the poll times out quickly, and make the sim
	// withhold the SessionRow so the poll never resolves it.
	t.Cleanup(agentd.SetAsyncSpawnInlineGraceForTest(50 * time.Millisecond))
	f.World.SkipSpawnRow = true

	f.HaveGroup("alpha")
	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name":            "worker",
		"initial_message": "do the thing",
	})

	// A slow row write must still succeed against the preset id — never a 504
	// or a rolled-back enrollment.
	require.Equalf(t, http.StatusOK, spawn.Code,
		"slow pane must still succeed, not fail/roll back; body=%s", spawn.Raw)
	require.NotEmpty(t, spawn.ConvID, "must return the preset conv-id")

	// The pre-fork enrollment survived: the agent is still a group member and
	// its briefing is still in its inbox.
	members := f.ListGroupMembers("alpha")
	var found bool
	for _, m := range members {
		if m.ConvID == spawn.ConvID {
			found = true
		}
	}
	assert.Truef(t, found, "slow-pane agent %s must stay a group member, not be rolled back", spawn.ConvID)

	msg := soleInboxMessage(t, spawn.ConvID)
	assert.Equal(t, "Startup context", msg.Subject, "the briefing must survive the slow poll")
}

// Scenario: the config escape hatch — agent.spawn_legacy_injection=true.
//
// The operator can revert to the legacy flow if the launch-arg path ever
// misbehaves. With the flag set the daemon launches a bare `claude`, polls for
// its conv-id, and injects `/rename` + the welcome over tmux — exactly as
// before — and does NOT use any launch args.
func TestSpawn_LegacyInjection_ConfigRevertsToSendKeys(t *testing.T) {
	f := newFlow(t)

	// Flip the revert switch before spawning. config.Load reads it fresh on
	// the spawn path (no caching), and newFlow points HOME at this test's
	// temp dir, so this config governs this spawn only.
	legacy := true
	require.NoError(t, config.Save(&config.Config{
		Agent: &config.AgentConfig{SpawnLegacyInjection: &legacy},
	}))

	f.HaveGroup("alpha")
	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name":            "worker",
		"initial_message": "Audit the auth module",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	target := spawn.TmuxTarget()
	msg := soleInboxMessage(t, spawn.ConvID)

	// Legacy path: /rename + welcome are injected over tmux, as before.
	f.AssertSentContains(target, "/rename worker", 5*time.Second)
	f.AssertSentContains(target, fmt.Sprintf("inbox read %d", msg.ID), 5*time.Second)
	f.AssertGroupMember("alpha", spawn.ConvID, "worker", 5*time.Second)

	// And it did NOT take the launch-arg path: no launch --name / prompt.
	if name, _ := f.World.SpawnName(spawn.ConvID); name != "" {
		t.Fatalf("legacy path must not set a launch --name; got %q", name)
	}
	if prompt, _ := f.World.SpawnInitialPrompt(spawn.ConvID); prompt != "" {
		t.Fatalf("legacy path must not set a launch prompt; got %q", prompt)
	}
}
