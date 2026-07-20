package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Scenario: an operator who explicitly opted an agent INTO Claude Code's auto
// memory must keep it across a handoff. Each relaunch path (resume /
// reincarnate / clone) recreates the pane with a fresh session row, and the
// launch resolves auto memory OFF unless something says otherwise — so without
// carrying the SOURCE conv's recorded posture, the first handoff silently
// reverts the opt-in AND overwrites the record, making the loss permanent.
//
// These pin the carry at the same surface the remote-control relaunch tests
// use: World.SpawnAutoMemory (the simSpawner's recorded value, i.e. whether
// `--auto-memory` was threaded onto the forked `tclaude session new`).
//
// The default direction matters as much as the opt-in: an unarmed source must
// carry NOTHING, so the relaunch injects CLAUDE_CODE_DISABLE_AUTO_MEMORY=1.

// optInSource records the source row's auto-memory posture ON, the same
// out-of-band write the spawn path performs.
func optInSource(t *testing.T, label string) {
	t.Helper()
	require.NoError(t, db.SetSessionAutoMemory(label, true), "opt source into auto memory")
}

// TestReincarnate_CarriesAutoMemoryOptIn: the successor keeps the opt-in.
func TestReincarnate_CarriesAutoMemoryOptIn(t *testing.T) {
	f := newFlow(t)

	const oldConv = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa139"
	const oldLabel = "spwn-am-rinc"
	f.HaveAliveSession(oldConv, oldLabel, "tclaude-"+oldLabel, f.TestCwd("work"))
	optInSource(t, oldLabel)

	r := f.AsHuman().Reincarnate(oldConv, "fresh start")

	got, ok := f.World.SpawnAutoMemory(r.NewConv)
	require.True(t, ok, "no spawn recorded for successor conv %s", r.NewConv)
	assert.True(t, got, "an opted-in source must thread --auto-memory onto the reincarnated pane")
}

// TestReincarnate_DefaultCarriesNoAutoMemory: the recommended posture is what
// an untouched agent keeps — no flag, so the successor gets the disable.
func TestReincarnate_DefaultCarriesNoAutoMemory(t *testing.T) {
	f := newFlow(t)

	const oldConv = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbb139"
	const oldLabel = "spwn-am-rinc-def"
	f.HaveAliveSession(oldConv, oldLabel, "tclaude-"+oldLabel, f.TestCwd("work"))

	r := f.AsHuman().Reincarnate(oldConv, "fresh start")

	got, ok := f.World.SpawnAutoMemory(r.NewConv)
	require.True(t, ok, "no spawn recorded for successor conv %s", r.NewConv)
	assert.False(t, got, "a source that never opted in must not thread --auto-memory")
}

// TestResume_CarriesAutoMemoryOptIn: the regression this suite exists for —
// resume is the path clone/reincarnate and the dashboard all fork through.
func TestResume_CarriesAutoMemoryOptIn(t *testing.T) {
	f := newFlow(t)

	const conv = "eeeeeeee-eeee-4eee-8eee-eeeeeeeee139"
	const label = "spwn-am-rsme"
	const tmux = "tclaude-" + label
	f.HaveAliveSession(conv, label, tmux, f.World.HomeDir)
	optInSource(t, label)
	f.MarkOffline(tmux)

	r := f.AsHuman().Resume(conv)
	require.Equal(t, "resumed", r.Action, "resume action: %s", r.Raw)

	got, ok := f.World.SpawnAutoMemory(conv)
	require.True(t, ok, "no resume-spawn recorded for conv %s", conv)
	assert.True(t, got, "an opted-in source must thread --auto-memory onto the resumed pane")
}

// TestResume_DefaultCarriesNoAutoMemory: an untouched agent resumes with the
// disable, not with whatever Claude Code would have defaulted to.
func TestResume_DefaultCarriesNoAutoMemory(t *testing.T) {
	f := newFlow(t)

	const conv = "ffffffff-ffff-4fff-8fff-fffffffff139"
	const label = "spwn-am-rsme-def"
	const tmux = "tclaude-" + label
	f.HaveAliveSession(conv, label, tmux, f.World.HomeDir)
	f.MarkOffline(tmux)

	r := f.AsHuman().Resume(conv)
	require.Equal(t, "resumed", r.Action, "resume action: %s", r.Raw)

	got, ok := f.World.SpawnAutoMemory(conv)
	require.True(t, ok, "no resume-spawn recorded for conv %s", conv)
	assert.False(t, got, "a source that never opted in must not thread --auto-memory")
}

// TestCloneFresh_CarriesAutoMemoryOptIn: a clone inherits the posture too, so
// the sibling behaves like the agent it was cloned from.
func TestCloneFresh_CarriesAutoMemoryOptIn(t *testing.T) {
	f := newFlow(t)

	const conv = "cccccccc-cccc-4ccc-8ccc-ccccccccc139"
	const label = "spwn-am-clone"
	f.HaveAliveSession(conv, label, "tclaude-"+label, f.TestCwd("work"))
	optInSource(t, label)

	r := f.AsHuman().CloneFresh(conv)

	got, ok := f.World.SpawnAutoMemory(r.NewConv)
	require.True(t, ok, "no spawn recorded for clone conv %s", r.NewConv)
	assert.True(t, got, "a clone of an opted-in agent must thread --auto-memory")
}
