package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// These scenarios pin AskUserQuestion idle-timeout PRESERVATION across the
// lifecycle verbs that mint a successor CC instance: reincarnate, clone (both
// the fresh and the jsonl-copy path) and resume. Unlike sandbox/approval —
// which the daemon re-DEFAULTS on every relaunch (sandboxForHarness /
// approvalForHarness) — the operator wants a per-agent timeout CARRIED across
// the handoff: an agentic worker set to auto-continue at 5m must come back on
// 5m, not revert to the global settings.json. The predecessor's recorded value
// lives on sessions.ask_user_question_timeout (schema v97); the relaunch reads
// it via askTimeoutForRelaunch → db.AskTimeoutForConv. Assertions sit at the
// Spawner boundary (World.SpawnAskTimeout), the seam where the production path
// threads `tclaude session new --ask-user-question-timeout`.

// stageAskTimeout records the resolved ask-timeout on the source's session row,
// the way production's `session new` does at spawn (SaveSession). HaveAliveSession
// writes the row without one, so we load it, set the field, and re-save — the
// same round-trip the hook callback performs, minus the mutation.
func stageAskTimeout(t *testing.T, label, v string) {
	t.Helper()
	s, err := db.LoadSession(label)
	require.NoError(t, err, "load source session row")
	require.NotNil(t, s, "source session row must exist")
	s.AskUserQuestionTimeout = v
	require.NoError(t, db.SaveSession(s), "record source ask-timeout")
}

// TestReincarnate_PreservesAskTimeout: reincarnating a worker set to 5m
// auto-continue brings the successor back on 5m — not the global default.
func TestReincarnate_PreservesAskTimeout(t *testing.T) {
	f := newFlow(t)

	const oldConv = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa971"
	const oldLabel = "spwn-at-rinc"
	f.HaveAliveSession(oldConv, oldLabel, "tclaude-"+oldLabel, "/tmp/work")
	stageAskTimeout(t, oldLabel, "5m")

	r := f.AsHuman().Reincarnate(oldConv, "fresh start")

	got, ok := f.World.SpawnAskTimeout(r.NewConv)
	require.True(t, ok, "no spawn recorded for successor conv %s", r.NewConv)
	assert.Equal(t, "5m", got, "the successor must inherit the predecessor's 5m ask-timeout")
}

// TestReincarnate_PreservesInheritAskTimeout: the tri-state composes — a source
// whose RESOLVED ask-timeout is the first-class `inherit` sentinel carries
// `inherit` across, so the successor stays on settings.json (and is still not
// something a group default could override). Distinct from the no-timeout case,
// which threads "".
func TestReincarnate_PreservesInheritAskTimeout(t *testing.T) {
	f := newFlow(t)

	const oldConv = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbb971"
	const oldLabel = "spwn-at-rinc-inh"
	f.HaveAliveSession(oldConv, oldLabel, "tclaude-"+oldLabel, "/tmp/work")
	stageAskTimeout(t, oldLabel, "inherit")

	r := f.AsHuman().Reincarnate(oldConv, "fresh start")

	got, ok := f.World.SpawnAskTimeout(r.NewConv)
	require.True(t, ok, "no spawn recorded for successor conv %s", r.NewConv)
	assert.Equal(t, "inherit", got, "an explicit inherit must be preserved verbatim across reincarnate")
}

// TestReincarnate_NoAskTimeoutThreadsEmpty: a source that recorded no timeout
// (a pre-column row, or a spawn that never chose one) threads "" — the successor
// keeps the operator's own settings.json, never an error or a bogus default.
func TestReincarnate_NoAskTimeoutThreadsEmpty(t *testing.T) {
	f := newFlow(t)

	const oldConv = "cccccccc-cccc-4ccc-8ccc-ccccccccc971"
	const oldLabel = "spwn-at-rinc-none"
	f.HaveAliveSession(oldConv, oldLabel, "tclaude-"+oldLabel, "/tmp/work")

	r := f.AsHuman().Reincarnate(oldConv, "fresh start")

	got, ok := f.World.SpawnAskTimeout(r.NewConv)
	require.True(t, ok, "no spawn recorded for successor conv %s", r.NewConv)
	assert.Equal(t, "", got, `no recorded ask-timeout must thread "" (spawn omits the override)`)
}

// TestCloneFresh_PreservesAskTimeout: a no-copy clone (fresh CC) inherits the
// original's ask-timeout, the same as its model/effort.
func TestCloneFresh_PreservesAskTimeout(t *testing.T) {
	f := newFlow(t)

	const oldConv = "dddddddd-dddd-4ddd-8ddd-ddddddddd971"
	const oldLabel = "spwn-at-clnf"
	f.HaveAliveSession(oldConv, oldLabel, "tclaude-"+oldLabel, "/tmp/work")
	stageAskTimeout(t, oldLabel, "10m")

	c := f.AsHuman().CloneFresh(oldConv)

	got, ok := f.World.SpawnAskTimeout(c.NewConv)
	require.True(t, ok, "no spawn recorded for clone conv %s", c.NewConv)
	assert.Equal(t, "10m", got, "the clone must inherit the original's ask-timeout")
}

// TestCloneCopy_PreservesAskTimeout: the jsonl-copy clone path (`session new -r`
// into the forked conv) must carry the ask-timeout too — `claude --resume` does
// not restore a per-session settings override on its own.
func TestCloneCopy_PreservesAskTimeout(t *testing.T) {
	f := newFlow(t)

	const oldConv = "eeeeeeee-eeee-4eee-8eee-eeeeeeeee971"
	const oldLabel = "spwn-at-clnc"
	f.HaveAliveSession(oldConv, oldLabel, "tclaude-"+oldLabel, "/tmp/work")
	stageAskTimeout(t, oldLabel, "never")

	c := f.AsHuman().CloneWith(oldConv, map[string]any{})
	require.Equal(t, 200, c.Code, "clone (copy path): %s", c.Raw)
	require.NotEmpty(t, c.NewConv, "copy-path clone minted a conv")

	got, ok := f.World.SpawnAskTimeout(c.NewConv)
	require.True(t, ok, "no resume-spawn recorded for clone conv %s", c.NewConv)
	assert.Equal(t, "never", got, "the copy-path clone's resume must carry the original's ask-timeout")
}

// TestAgentResume_PreservesAskTimeout: resuming an offline agent re-opens the
// SAME conversation, so it must come back on the ask-timeout that conv last
// recorded — its freshest session row carries it.
func TestAgentResume_PreservesAskTimeout(t *testing.T) {
	f := newFlow(t)

	const conv = "ffffffff-ffff-4fff-8fff-fffffffff971"
	const label = "spwn-at-rsme"
	const tmux = "tclaude-" + label
	f.HaveAliveSession(conv, label, tmux, f.World.HomeDir)
	stageAskTimeout(t, label, "5m")
	f.MarkOffline(tmux)

	r := f.AsHuman().Resume(conv)
	require.Equal(t, "resumed", r.Action, "resume action: %s", r.Raw)

	got, ok := f.World.SpawnAskTimeout(conv)
	require.True(t, ok, "no resume-spawn recorded for conv %s", conv)
	assert.Equal(t, "5m", got, "the resumed agent must come back on its own ask-timeout")
}
