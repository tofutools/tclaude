package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Scenario: an agent pinned to compact at 450k must STAY pinned across every
// handoff. Each relaunch path (resume / reincarnate / clone) recreates the pane
// with a fresh session row, and a launch that carries nothing resolves to "no
// window pinned" — so without carrying the SOURCE conv's recorded window, the
// first handoff silently hands the successor the model's full 1M window AND
// overwrites the record, making the loss permanent and invisible. The whole
// point of the feature is a long-lived agent, and a long-lived agent is exactly
// one that gets reincarnated.
//
// These pin the carry at the same surface the auto-memory / remote-control
// relaunch tests use: World.SpawnAutoCompactWindow — the simSpawner's recorded
// value, i.e. what was threaded onto the forked `tclaude session new`.
//
// The default direction matters as much as the carry: an unpinned source must
// carry NOTHING, so the successor keeps Claude Code's own per-model threshold
// rather than inheriting a window from somewhere it never asked about.

// pinSource records the source row's auto-compaction window, the same
// out-of-band write the spawn path performs at launch.
func pinSource(t *testing.T, label, window string) {
	t.Helper()
	require.NoError(t, db.SetSessionAutoCompactWindow(label, window), "pin the source window")
}

// TestReincarnate_CarriesAutoCompactWindow: the successor keeps the pin. This is
// the load-bearing case — reincarnation is what a pinned long-lived agent does
// repeatedly, so a leak here compounds.
func TestReincarnate_CarriesAutoCompactWindow(t *testing.T) {
	f := newFlow(t)

	const oldConv = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa156"
	const oldLabel = "spwn-acw-rinc"
	f.HaveAliveSession(oldConv, oldLabel, "tclaude-"+oldLabel, f.TestCwd("work"))
	pinSource(t, oldLabel, "450000")

	r := f.AsHuman().Reincarnate(oldConv, "fresh start")

	got, ok := f.World.SpawnAutoCompactWindow(r.NewConv)
	require.True(t, ok, "no spawn recorded for successor conv %s", r.NewConv)
	assert.Equal(t, "450000", got,
		"a pinned source must thread --auto-compact-window onto the reincarnated pane")
}

// TestReincarnate_DefaultCarriesNoAutoCompactWindow: an unpinned agent's
// successor must not acquire a window out of nowhere.
func TestReincarnate_DefaultCarriesNoAutoCompactWindow(t *testing.T) {
	f := newFlow(t)

	const oldConv = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbb156"
	const oldLabel = "spwn-acw-rinc-def"
	f.HaveAliveSession(oldConv, oldLabel, "tclaude-"+oldLabel, f.TestCwd("work"))

	r := f.AsHuman().Reincarnate(oldConv, "fresh start")

	got, ok := f.World.SpawnAutoCompactWindow(r.NewConv)
	require.True(t, ok, "no spawn recorded for successor conv %s", r.NewConv)
	assert.Empty(t, got, "a source that pinned nothing must not thread a window")
}

// TestResume_CarriesAutoCompactWindow: resume is the path clone / reincarnate
// and the dashboard's relaunch all fork through, so this is the widest carry.
func TestResume_CarriesAutoCompactWindow(t *testing.T) {
	f := newFlow(t)

	const conv = "eeeeeeee-eeee-4eee-8eee-eeeeeeeee156"
	const label = "spwn-acw-rsme"
	const tmux = "tclaude-" + label
	f.HaveAliveSession(conv, label, tmux, f.World.HomeDir)
	pinSource(t, label, "450000")
	f.MarkOffline(tmux)

	r := f.AsHuman().Resume(conv)
	require.Equal(t, "resumed", r.Action, "resume action: %s", r.Raw)

	got, ok := f.World.SpawnAutoCompactWindow(conv)
	require.True(t, ok, "no resume-spawn recorded for conv %s", conv)
	assert.Equal(t, "450000", got,
		"a pinned source must thread --auto-compact-window onto the resumed pane")
}

// TestResume_DefaultCarriesNoAutoCompactWindow: an unpinned agent resumes on the
// model's own threshold.
func TestResume_DefaultCarriesNoAutoCompactWindow(t *testing.T) {
	f := newFlow(t)

	const conv = "ffffffff-ffff-4fff-8fff-fffffffff156"
	const label = "spwn-acw-rsme-def"
	const tmux = "tclaude-" + label
	f.HaveAliveSession(conv, label, tmux, f.World.HomeDir)
	f.MarkOffline(tmux)

	r := f.AsHuman().Resume(conv)
	require.Equal(t, "resumed", r.Action, "resume action: %s", r.Raw)

	got, ok := f.World.SpawnAutoCompactWindow(conv)
	require.True(t, ok, "no resume-spawn recorded for conv %s", conv)
	assert.Empty(t, got, "a source that pinned nothing must not thread a window")
}

// TestResume_AutoCompactWindowSurvivesRepeatedResumes: the compounding case. A
// relaunch both READS the recorded window and RE-RECORDS it onto the fresh
// session row it mints; if the re-record were missed, the first resume would look
// fine and the SECOND would read an empty row and drop the pin. That
// read-before-write ordering is easy to get wrong, so it gets its own assertion
// on the durable state as well as on the threaded flag.
//
// Each round has to mark the CURRENT pane offline, not the original one — a
// resume mints a new label, so re-using the first tmux name just gets
// "skipped:already_online".
func TestResume_AutoCompactWindowSurvivesRepeatedResumes(t *testing.T) {
	f := newFlow(t)

	const conv = "dddddddd-dddd-4ddd-8ddd-ddddddddd156"
	const label = "spwn-acw-rsme-twice"
	f.HaveAliveSession(conv, label, "tclaude-"+label, f.World.HomeDir)
	pinSource(t, label, "450000")

	live := "tclaude-" + label
	for round := 1; round <= 3; round++ {
		f.MarkOffline(live)
		r := f.AsHuman().Resume(conv)
		require.Equalf(t, "resumed", r.Action, "resume round %d: %s", round, r.Raw)

		got, ok := f.World.SpawnAutoCompactWindow(conv)
		require.Truef(t, ok, "no resume-spawn recorded for conv %s on round %d", conv, round)
		assert.Equalf(t, "450000", got,
			"the pin must survive resume round %d, not decay after the first", round)

		window, err := db.AutoCompactWindowForConv(conv)
		require.NoError(t, err)
		assert.Equalf(t, "450000", window,
			"round %d must RE-RECORD the window, or the next resume reads an empty row", round)

		sess, err := db.FindSessionByConvID(conv)
		require.NoError(t, err)
		require.NotNil(t, sess)
		live = sess.TmuxSession
	}
}

// TestCloneFresh_CarriesAutoCompactWindow: a clone is meant to be the same agent
// working alongside the original, so a source that compacts early must not
// produce a sibling that runs to the model's full window.
func TestCloneFresh_CarriesAutoCompactWindow(t *testing.T) {
	f := newFlow(t)

	const conv = "cccccccc-cccc-4ccc-8ccc-ccccccccc156"
	const label = "spwn-acw-clone"
	f.HaveAliveSession(conv, label, "tclaude-"+label, f.TestCwd("work"))
	pinSource(t, label, "450000")

	r := f.AsHuman().CloneFresh(conv)

	got, ok := f.World.SpawnAutoCompactWindow(r.NewConv)
	require.True(t, ok, "no spawn recorded for clone conv %s", r.NewConv)
	assert.Equal(t, "450000", got, "a clone of a pinned agent must thread the same window")
}

// TestReincarnate_AutoCompactWindowSurvivesGenerations: the pin must hold across
// a CHAIN of successors, not just one hop. A carry that reads the source's
// durable state but forgets to record it onto the successor would pass the
// single-hop test and lose the window on the second reincarnation — the exact
// shape of the agent-spine merge bug this feature already hit once.
func TestReincarnate_AutoCompactWindowSurvivesGenerations(t *testing.T) {
	f := newFlow(t)

	const rootConv = "99999999-9999-4999-8999-999999999156"
	const rootLabel = "spwn-acw-gen"
	f.HaveAliveSession(rootConv, rootLabel, "tclaude-"+rootLabel, f.TestCwd("work"))
	pinSource(t, rootLabel, "450000")

	conv := rootConv
	for generation := 1; generation <= 3; generation++ {
		r := f.AsHuman().Reincarnate(conv, "carry on")
		require.NotEmptyf(t, r.NewConv, "generation %d produced no successor: %s", generation, r.Raw)

		got, ok := f.World.SpawnAutoCompactWindow(r.NewConv)
		require.Truef(t, ok, "no spawn recorded for generation %d conv %s", generation, r.NewConv)
		assert.Equalf(t, "450000", got,
			"the pin must survive to generation %d, not decay along the chain", generation)
		conv = r.NewConv
	}
}
