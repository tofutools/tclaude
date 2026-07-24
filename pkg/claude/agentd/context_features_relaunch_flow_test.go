package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Scenario: an agent deliberately spawned with a trimmed startup context must
// come back trimmed. Each relaunch path (resume / reincarnate / clone) recreates
// the pane with a fresh session row, and the launch trims nothing unless
// something says otherwise — so without carrying the SOURCE conv's recorded map,
// the first handoff silently hands the successor Claude Code's full startup load
// AND overwrites the record, making the loss permanent. That would defeat the
// point of the feature for exactly the long-running agents that need it most.
//
// These pin the carry at the same surface the auto-memory relaunch tests use:
// World.SpawnContextFeatures (the simSpawner's recorded value, i.e. what was
// threaded onto the forked `tclaude session new`).

// leanSource records the source row's startup-context trims, the same
// out-of-band write the spawn path performs.
func leanSource(t *testing.T, label string, features map[string]string) {
	t.Helper()
	require.NoError(t, db.SetSessionContextFeatures(label, features),
		"record source startup-context trims")
}

var sourceTrims = map[string]string{"bundled-skills": "off", "artifact": "on"}

// TestReincarnate_CarriesContextFeatures: the successor keeps the lean context.
// Reincarnation exists to escape a full window, so handing the successor a
// FATTER startup context than its predecessor would be precisely backwards.
func TestReincarnate_CarriesContextFeatures(t *testing.T) {
	f := newFlow(t)

	const oldConv = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa155"
	const oldLabel = "spwn-cf-rinc"
	f.HaveAliveSession(oldConv, oldLabel, "tclaude-"+oldLabel, f.TestCwd("work"))
	leanSource(t, oldLabel, sourceTrims)

	r := f.AsHuman().Reincarnate(oldConv, "fresh start")

	got, ok := f.World.SpawnContextFeatures(r.NewConv)
	require.True(t, ok, "no spawn recorded for successor conv %s", r.NewConv)
	assert.Equal(t, sourceTrims, got,
		"a trimmed source must hand its successor the same startup context")
}

// TestReincarnate_DefaultCarriesNoContextFeatures: an untouched agent stays
// untouched, so this feature never changes an existing workflow by itself.
func TestReincarnate_DefaultCarriesNoContextFeatures(t *testing.T) {
	f := newFlow(t)

	const oldConv = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbb155"
	const oldLabel = "spwn-cf-rinc-def"
	f.HaveAliveSession(oldConv, oldLabel, "tclaude-"+oldLabel, f.TestCwd("work"))

	r := f.AsHuman().Reincarnate(oldConv, "fresh start")

	got, ok := f.World.SpawnContextFeatures(r.NewConv)
	require.True(t, ok, "no spawn recorded for successor conv %s", r.NewConv)
	assert.Empty(t, got, "a source with no trims must not invent any")
}

// TestResume_CarriesContextFeatures: resume is the path clone/reincarnate and the
// dashboard all fork through, so this is the load-bearing case.
func TestResume_CarriesContextFeatures(t *testing.T) {
	f := newFlow(t)

	const conv = "eeeeeeee-eeee-4eee-8eee-eeeeeeeee155"
	const label = "spwn-cf-rsme"
	const tmux = "tclaude-" + label
	f.HaveAliveSession(conv, label, tmux, f.World.HomeDir)
	leanSource(t, label, sourceTrims)
	f.MarkOffline(tmux)

	r := f.AsHuman().Resume(conv)
	require.Equal(t, "resumed", r.Action, "resume action: %s", r.Raw)

	got, ok := f.World.SpawnContextFeatures(conv)
	require.True(t, ok, "no resume-spawn recorded for conv %s", conv)
	assert.Equal(t, sourceTrims, got, "a resumed pane must keep its trims")
}

// TestSessionContextFeaturesBecomeTheDurableAnswer: the recorded per-session map
// must be what a later relaunch reads back. This is the hinge the three carry
// tests above depend on — the session row is prunable process state, so the write
// has to reach the durable conversation/agent profile to survive at all.
//
// (The no-decay-across-repeated-handoffs half of this lives in
// pkg/claude/common/db: it is a projection property, and the flow harness
// simulates the forked `session new` that performs the re-record.)
func TestSessionContextFeaturesBecomeTheDurableAnswer(t *testing.T) {
	f := newFlow(t)

	const conv = "dddddddd-dddd-4ddd-8ddd-ddddddddd155"
	const label = "spwn-cf-durable"
	f.HaveAliveSession(conv, label, "tclaude-"+label, f.World.HomeDir)
	leanSource(t, label, sourceTrims)

	durable, err := db.ContextFeaturesForConv(conv)
	require.NoError(t, err)
	assert.Equal(t, sourceTrims, durable,
		"the recorded trims must be what a relaunch reads back for this conv")
}

// TestResume_DefaultCarriesNoContextFeatures: nothing recorded, nothing trimmed.
func TestResume_DefaultCarriesNoContextFeatures(t *testing.T) {
	f := newFlow(t)

	const conv = "ffffffff-ffff-4fff-8fff-fffffffff155"
	const label = "spwn-cf-rsme-def"
	const tmux = "tclaude-" + label
	f.HaveAliveSession(conv, label, tmux, f.World.HomeDir)
	f.MarkOffline(tmux)

	r := f.AsHuman().Resume(conv)
	require.Equal(t, "resumed", r.Action, "resume action: %s", r.Raw)

	got, ok := f.World.SpawnContextFeatures(conv)
	require.True(t, ok, "no resume-spawn recorded for conv %s", conv)
	assert.Empty(t, got)
}

// TestCloneFresh_CarriesContextFeatures: a clone is meant to be the same agent
// working alongside the original, so a lean source must not produce a fat
// sibling.
func TestCloneFresh_CarriesContextFeatures(t *testing.T) {
	f := newFlow(t)

	const conv = "cccccccc-cccc-4ccc-8ccc-ccccccccc155"
	const label = "spwn-cf-clone"
	f.HaveAliveSession(conv, label, "tclaude-"+label, f.TestCwd("work"))
	leanSource(t, label, sourceTrims)

	r := f.AsHuman().CloneFresh(conv)

	got, ok := f.World.SpawnContextFeatures(r.NewConv)
	require.True(t, ok, "no spawn recorded for clone conv %s", r.NewConv)
	assert.Equal(t, sourceTrims, got, "a clone must inherit the source's startup context")
}
