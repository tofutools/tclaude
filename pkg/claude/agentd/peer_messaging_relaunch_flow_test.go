package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Scenario: an operator who explicitly opted an agent INTO Claude Code's own
// cross-session messaging must keep it across a handoff. Each relaunch path
// (resume / reincarnate / clone) recreates the pane with a fresh session row,
// and the launch resolves peer messaging OFF unless something says otherwise —
// so without carrying the SOURCE conv's recorded posture, the first handoff
// silently recloses the mesh AND overwrites the record, making the loss
// permanent.
//
// These pin the carry at the same surface the auto-memory relaunch tests use:
// World.SpawnPeerMessaging (the simSpawner's recorded value, i.e. whether
// `--peer-messaging` was threaded onto the forked `tclaude session new`).
//
// The default direction matters as much as the opt-in: an unarmed source must
// carry NOTHING, so the relaunch injects the refusal.

// optInPeerSource records the source row's peer-messaging posture ON, the same
// out-of-band write the spawn path performs.
func optInPeerSource(t *testing.T, label string) {
	t.Helper()
	require.NoError(t, db.SetSessionPeerMessaging(label, true), "opt source into peer messaging")
}

// TestReincarnate_CarriesPeerMessagingOptIn: the successor keeps the opt-in.
func TestReincarnate_CarriesPeerMessagingOptIn(t *testing.T) {
	f := newFlow(t)

	const oldConv = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa812"
	const oldLabel = "spwn-pm-rinc"
	f.HaveAliveSession(oldConv, oldLabel, "tclaude-"+oldLabel, f.TestCwd("work"))
	optInPeerSource(t, oldLabel)

	r := f.AsHuman().Reincarnate(oldConv, "fresh start")

	got, ok := f.World.SpawnPeerMessaging(r.NewConv)
	require.True(t, ok, "no spawn recorded for successor conv %s", r.NewConv)
	assert.True(t, got, "an opted-in source must thread --peer-messaging onto the reincarnated pane")
}

// TestReincarnate_DefaultCarriesNoPeerMessaging: the recommended posture is what
// an untouched agent keeps — no flag, so the successor gets the refusal.
func TestReincarnate_DefaultCarriesNoPeerMessaging(t *testing.T) {
	f := newFlow(t)

	const oldConv = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbb812"
	const oldLabel = "spwn-pm-rinc-def"
	f.HaveAliveSession(oldConv, oldLabel, "tclaude-"+oldLabel, f.TestCwd("work"))

	r := f.AsHuman().Reincarnate(oldConv, "fresh start")

	got, ok := f.World.SpawnPeerMessaging(r.NewConv)
	require.True(t, ok, "no spawn recorded for successor conv %s", r.NewConv)
	assert.False(t, got, "a source that never opted in must not thread --peer-messaging")
}

// TestResume_CarriesPeerMessagingOptIn: resume is the path clone, reincarnate
// and the dashboard all fork through, so it is the one that matters most.
func TestResume_CarriesPeerMessagingOptIn(t *testing.T) {
	f := newFlow(t)

	const conv = "eeeeeeee-eeee-4eee-8eee-eeeeeeeee812"
	const label = "spwn-pm-rsme"
	const tmux = "tclaude-" + label
	f.HaveAliveSession(conv, label, tmux, f.World.HomeDir)
	optInPeerSource(t, label)
	f.MarkOffline(tmux)

	r := f.AsHuman().Resume(conv)
	require.Equal(t, "resumed", r.Action, "resume action: %s", r.Raw)

	got, ok := f.World.SpawnPeerMessaging(conv)
	require.True(t, ok, "no resume-spawn recorded for conv %s", conv)
	assert.True(t, got, "an opted-in source must thread --peer-messaging onto the resumed pane")
}

// TestResume_DefaultCarriesNoPeerMessaging: an untouched agent resumes with the
// refusal, not with whatever Claude Code would have defaulted to.
func TestResume_DefaultCarriesNoPeerMessaging(t *testing.T) {
	f := newFlow(t)

	const conv = "ffffffff-ffff-4fff-8fff-fffffffff812"
	const label = "spwn-pm-rsme-def"
	const tmux = "tclaude-" + label
	f.HaveAliveSession(conv, label, tmux, f.World.HomeDir)
	f.MarkOffline(tmux)

	r := f.AsHuman().Resume(conv)
	require.Equal(t, "resumed", r.Action, "resume action: %s", r.Raw)

	got, ok := f.World.SpawnPeerMessaging(conv)
	require.True(t, ok, "no resume-spawn recorded for conv %s", conv)
	assert.False(t, got, "a source that never opted in must not thread --peer-messaging")
}

// TestCloneFresh_CarriesPeerMessagingOptIn: the no-copy-conv clone branch.
func TestCloneFresh_CarriesPeerMessagingOptIn(t *testing.T) {
	f := newFlow(t)

	const conv = "cccccccc-cccc-4ccc-8ccc-ccccccccc812"
	const label = "spwn-pm-clone"
	f.HaveAliveSession(conv, label, "tclaude-"+label, f.TestCwd("work"))
	optInPeerSource(t, label)

	r := f.AsHuman().CloneFresh(conv)

	got, ok := f.World.SpawnPeerMessaging(r.NewConv)
	require.True(t, ok, "no spawn recorded for clone conv %s", r.NewConv)
	assert.True(t, got, "a clone of an opted-in agent must thread --peer-messaging")
}

// TestCloneCopyConv_CarriesPeerMessagingOptIn: the DEFAULT clone. `--no-copy-conv`
// is the opt-OUT, so a plain `tclaude agent clone` takes the copy-conversation
// branch — a second, independent argv assembly inside cloneSpawnOnce.
//
// This is deliberately not redundant with TestCloneFresh above: the two branches
// build proofArgs separately, and a field added to only one of them produces
// exactly this bug — a clone that silently drops the source's opt-in, then
// records the loss as its own durable posture so it never comes back. The
// auto-memory suite covers only CloneFresh, which is why that gap went unnoticed
// until a cold review found it. Both branches now have a test.
func TestCloneCopyConv_CarriesPeerMessagingOptIn(t *testing.T) {
	f := newFlow(t)

	const conv = "dddddddd-dddd-4ddd-8ddd-ddddddddd812"
	const label = "spwn-pm-clone-cc"
	f.HaveAliveSession(conv, label, "tclaude-"+label, f.TestCwd("work"))
	optInPeerSource(t, label)

	r := f.AsHuman().CloneWith(conv, map[string]any{})
	require.Equal(t, 200, r.Code, "default (copy-conv) clone; body=%s", r.Raw)

	got, ok := f.World.SpawnPeerMessaging(r.NewConv)
	require.True(t, ok, "no spawn recorded for clone conv %s", r.NewConv)
	assert.True(t, got,
		"the DEFAULT copy-conversation clone must thread --peer-messaging too, "+
			"not just the --no-copy-conv branch")
}

// TestCloneCopyConv_DefaultCarriesNoPeerMessaging: and the default direction on
// the same branch, so a passing opt-in test cannot be satisfied by a hardcoded
// true.
func TestCloneCopyConv_DefaultCarriesNoPeerMessaging(t *testing.T) {
	f := newFlow(t)

	const conv = "dddddddd-dddd-4ddd-8ddd-ddddddddd813"
	const label = "spwn-pm-clone-cc-def"
	f.HaveAliveSession(conv, label, "tclaude-"+label, f.TestCwd("work"))

	r := f.AsHuman().CloneWith(conv, map[string]any{})
	require.Equal(t, 200, r.Code, "default (copy-conv) clone; body=%s", r.Raw)

	got, ok := f.World.SpawnPeerMessaging(r.NewConv)
	require.True(t, ok, "no spawn recorded for clone conv %s", r.NewConv)
	assert.False(t, got, "a clone of an unarmed source must not thread --peer-messaging")
}
