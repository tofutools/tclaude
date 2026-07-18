package agentd_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Scenario (JOH-261): re-arm Claude Code's built-in Remote Access across the
// three relaunch paths — resume / reincarnate / clone. Each recreates the CC
// pane (a fresh session row; for reincarnate/clone a fresh conv-id) which boots
// with Remote Access OFF, so an agent the operator armed for phone access would
// silently drop on every handoff. The fix carries the SOURCE conv's persisted
// best-known state (JOH-256) onto the relaunch as the --remote-control launch
// flag (the JOH-258 primitive, NOT a post-boot /remote-control toggle) and tags
// the NEW session row's best-known state out-of-band.
//
// These pin both halves at the same surfaces the JOH-258 spawn tests use: the
// threaded flag (World.SpawnRemoteControl — the simSpawner's recorded value) and
// the new row's tag (db.RemoteControlForConv — what the dashboard + CLI read).
// An UNARMED source carries nothing; a Codex relaunch never carries it even when
// a stale flag is force-set, proving the harness-capability gate.

// armSource records the source row's best-known remote-control state ON, the
// same out-of-band write the toggle/spawn path performs (db.SetSessionRemoteControl).
func armSource(t *testing.T, label string) {
	t.Helper()
	require.NoError(t, db.SetSessionRemoteControl(label, true), "arm source remote-control")
}

// TestReincarnate_ArmedCarriesRemoteControl: reincarnating an armed agent
// re-arms the successor — the relaunch threads --remote-control and the new
// row's best-known state is tagged on.
func TestReincarnate_ArmedCarriesRemoteControl(t *testing.T) {
	f := newFlow(t)

	const oldConv = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa261"
	const oldLabel = "spwn-rc-rinc"
	f.HaveAliveSession(oldConv, oldLabel, "tclaude-"+oldLabel, f.TestCwd("work"))
	armSource(t, oldLabel)

	r := f.AsHuman().Reincarnate(oldConv, "fresh start")

	got, ok := f.World.SpawnRemoteControl(r.NewConv)
	require.True(t, ok, "no spawn recorded for successor conv %s", r.NewConv)
	assert.True(t, got, "an armed source must thread --remote-control onto the reincarnated pane")

	rc, err := db.RemoteControlForConv(r.NewConv)
	require.NoError(t, err)
	assert.True(t, rc, "the successor row's best-known remote_control must be tagged on")
}

// TestReincarnate_UnarmedCarriesNothing: an unarmed source's reincarnation must
// not thread --remote-control nor tag the successor row.
func TestReincarnate_UnarmedCarriesNothing(t *testing.T) {
	f := newFlow(t)

	const oldConv = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbb261"
	const oldLabel = "spwn-rc-rinc-un"
	f.HaveAliveSession(oldConv, oldLabel, "tclaude-"+oldLabel, f.TestCwd("work"))

	r := f.AsHuman().Reincarnate(oldConv, "fresh start")

	got, ok := f.World.SpawnRemoteControl(r.NewConv)
	require.True(t, ok, "no spawn recorded for successor conv %s", r.NewConv)
	assert.False(t, got, "an unarmed source must not thread --remote-control")

	rc, err := db.RemoteControlForConv(r.NewConv)
	require.NoError(t, err)
	assert.False(t, rc, "the successor row must stay unarmed")
}

// TestCloneFresh_ArmedCarriesRemoteControl: a no-copy clone (fresh CC) of an
// armed agent becomes a second phone-reachable sibling — the operator-decided
// semantics (drive either from the phone).
func TestCloneFresh_ArmedCarriesRemoteControl(t *testing.T) {
	f := newFlow(t)

	const oldConv = "cccccccc-cccc-4ccc-8ccc-ccccccccc261"
	const oldLabel = "spwn-rc-clnf"
	f.HaveAliveSession(oldConv, oldLabel, "tclaude-"+oldLabel, f.TestCwd("work"))
	armSource(t, oldLabel)

	c := f.AsHuman().CloneFresh(oldConv)

	got, ok := f.World.SpawnRemoteControl(c.NewConv)
	require.True(t, ok, "no spawn recorded for clone conv %s", c.NewConv)
	assert.True(t, got, "an armed source must thread --remote-control onto the cloned pane")

	rc, err := db.RemoteControlForConv(c.NewConv)
	require.NoError(t, err)
	assert.True(t, rc, "the clone row's best-known remote_control must be tagged on")
}

// TestCloneCopy_ArmedCarriesRemoteControl: the jsonl-copy clone path (`session
// new -r` into the forked conv) must carry the arm too — `claude --resume` does
// not restore Remote Access on its own.
func TestCloneCopy_ArmedCarriesRemoteControl(t *testing.T) {
	f := newFlow(t)

	const oldConv = "dddddddd-dddd-4ddd-8ddd-ddddddddd261"
	const oldLabel = "spwn-rc-clnc"
	f.HaveAliveSession(oldConv, oldLabel, "tclaude-"+oldLabel, f.TestCwd("work"))
	armSource(t, oldLabel)

	c := f.AsHuman().CloneWith(oldConv, map[string]any{})
	require.Equal(t, 200, c.Code, "clone (copy path): %s", c.Raw)
	require.NotEmpty(t, c.NewConv, "copy-path clone minted a conv")

	got, ok := f.World.SpawnRemoteControl(c.NewConv)
	require.True(t, ok, "no resume-spawn recorded for clone conv %s", c.NewConv)
	assert.True(t, got, "an armed source must thread --remote-control onto the copy-path clone's resume")

	rc, err := db.RemoteControlForConv(c.NewConv)
	require.NoError(t, err)
	assert.True(t, rc, "the copy-path clone row's best-known remote_control must be tagged on")
}

// TestResume_ArmedCarriesRemoteControl: resuming an offline armed agent re-arms
// the fresh pane. Resume mints a new session row (new label) for the SAME
// conv-id, so the tag is applied in the background once that row comes online —
// drained via WaitForBackgroundForTest before asserting.
func TestResume_ArmedCarriesRemoteControl(t *testing.T) {
	f := newFlow(t)

	const conv = "eeeeeeee-eeee-4eee-8eee-eeeeeeeee261"
	const label = "spwn-rc-rsme"
	const tmux = "tclaude-" + label
	f.HaveAliveSession(conv, label, tmux, f.World.HomeDir)
	armSource(t, label)
	f.MarkOffline(tmux)

	r := f.AsHuman().Resume(conv)
	require.Equal(t, "resumed", r.Action, "resume action: %s", r.Raw)

	got, ok := f.World.SpawnRemoteControl(conv)
	require.True(t, ok, "no resume-spawn recorded for conv %s", conv)
	assert.True(t, got, "an armed source must thread --remote-control onto the resumed pane")

	// The new-row tag runs in goBackground (resume is fire-and-forget); drain it.
	agentd.WaitForBackgroundForTest()

	rc, err := db.RemoteControlForConv(conv)
	require.NoError(t, err)
	assert.True(t, rc, "the resumed pane's fresh row must be tagged armed")
}

// TestResume_UnarmedCarriesNothing: resuming an unarmed agent carries no flag
// and leaves the fresh row unarmed.
func TestResume_UnarmedCarriesNothing(t *testing.T) {
	f := newFlow(t)

	const conv = "ffffffff-ffff-4fff-8fff-fffffffff261"
	const label = "spwn-rc-rsme-un"
	const tmux = "tclaude-" + label
	f.HaveAliveSession(conv, label, tmux, f.World.HomeDir)
	f.MarkOffline(tmux)

	r := f.AsHuman().Resume(conv)
	require.Equal(t, "resumed", r.Action, "resume action: %s", r.Raw)

	got, ok := f.World.SpawnRemoteControl(conv)
	require.True(t, ok, "no resume-spawn recorded for conv %s", conv)
	assert.False(t, got, "an unarmed source must not thread --remote-control")

	agentd.WaitForBackgroundForTest()

	rc, err := db.RemoteControlForConv(conv)
	require.NoError(t, err)
	assert.False(t, rc, "the resumed pane's row must stay unarmed")
}

// TestReincarnate_CodexNeverCarriesRemoteControl: Codex has no built-in Remote
// Access, so a relaunch never carries --remote-control — even when a stale
// remote_control flag is force-set on the source row. This proves the
// harness-capability gate (remoteControlForRelaunch → CanRemoteControl), the
// defence-in-depth backstop behind the by-construction guarantee that only a CC
// conv can ever record the flag.
func TestReincarnate_CodexNeverCarriesRemoteControl(t *testing.T) {
	f := newFlow(t)

	const oldConv = "019ec004-4250-79b1-9ade-ebaea4150261"
	const oldLabel = "spwn-rc-cdx"
	f.HaveAliveCodexSession(oldConv, oldLabel, "tclaude-"+oldLabel, f.TestCwd("work"))
	// A Codex conv lives in its own threads store, not the CC .jsonl scan, so it
	// needs a conv_index row tagged harness=codex for the reincarnate selector to
	// resolve it (mirrors TestReincarnate_CodexInheritsLiveModelAndEffort).
	now := time.Now().Format(time.RFC3339)
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:      oldConv,
		ProjectDir:  f.TestCwd("work"),
		ProjectPath: f.TestCwd("work"),
		FirstPrompt: "codex work",
		Created:     now,
		Modified:    now,
		Harness:     "codex",
	}))
	// Force-set the flag on the Codex row to defeat the by-construction guard,
	// so this exercises the explicit capability gate rather than a false absence.
	armSource(t, oldLabel)

	r := f.AsHuman().Reincarnate(oldConv, "fresh start")

	got, ok := f.World.SpawnRemoteControl(r.NewConv)
	require.True(t, ok, "no spawn recorded for Codex successor conv %s", r.NewConv)
	assert.False(t, got, "a Codex relaunch must never carry --remote-control")

	rc, err := db.RemoteControlForConv(r.NewConv)
	require.NoError(t, err)
	assert.False(t, rc, "the Codex successor row must not be tagged armed")
}
