package agentd_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Scenario (JOH-319): the living successor keeps the plain base name and
// the retiring predecessor takes the `-x` archive marker.
//
// Setup: a worker running under the plain name "worker" with a live tmux
// pane, in group "alpha".
//
// Action: the human reincarnates the worker.
//
// Expected:
//   - The new instance KEEPS the base name "worker" (new_title == "worker");
//     the successor pane is renamed to "worker", not a "-r-N" form.
//   - The old pane is archive-renamed to "worker-x" and receives `/exit`.
//   - Group membership moves old -> new; the live member shows as "worker".
func TestReincarnate_SuccessorKeepsBaseName_PredecessorArchived(t *testing.T) {
	f := newFlow(t)

	const oldConv = "old0-aaaa-bbbb-cccc-dddd"
	const oldLabel = "spwn-old0-001"
	const oldTmux = "tclaude-spwn-old0-001"

	f.HaveConvWithTitle(oldConv, "worker")
	f.HaveAliveSession(oldConv, oldLabel, oldTmux, "/tmp/work")
	g := f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv)

	r := f.AsHuman().Reincarnate(oldConv, "fresh start")

	// The successor keeps the base name.
	f.AssertReincarnateTitle(r, "worker")
	f.AssertSentContains(r.TmuxTarget(), "/rename worker", 5*time.Second)

	// The retiring predecessor is archive-renamed to "worker-x" and exits.
	assert.True(t, f.World.Tmux.WaitForSendKeys(oldTmux+":0.0", "/rename worker-x", 2*time.Second),
		"old pane should be archive-renamed to worker-x; sent=%+v", f.World.Tmux.Sent())
	assert.True(t, f.World.Tmux.WaitForSendKeys(oldTmux+":0.0", "/exit", 1*time.Second),
		"old pane should have received /exit; sent=%+v", f.World.Tmux.Sent())

	// Surface-level invariants the human would see in `agent groups members`:
	//   - the live member shows with the bare base name "worker";
	//   - the old conv is gone (membership migrated atomically).
	f.AssertGroupMember(g.Name, r.NewConv, "worker", 5*time.Second)
	f.AssertNotGroupMember(g.Name, oldConv)

	// JOH-320: the predecessor is hidden from `conv ls` by the durable
	// conv_index.archived_at column the orchestrator stamps, not by the
	// cosmetic `-x` title — so a live agent whose name merely happens to end
	// in `-x` no longer self-hides. ConvIndexRow.IsArchived() is exactly the
	// signal the listing/dashboard read paths consult.
	oldRow, err := db.GetConvIndex(oldConv)
	require.NoError(t, err, "GetConvIndex(old)")
	require.NotNil(t, oldRow, "predecessor still has a conv_index row")
	assert.True(t, oldRow.IsArchived(), "predecessor archived via conv_index.archived_at, not the -x title")
}

// Scenario: a SECOND retirement of the same base. Because the living
// generation keeps its base name, the predecessor would collide on the
// bare "worker-x"; a `-x-<N>` counter disambiguates. Modelled here with a
// prior `worker-x` already in the index (the first retired generation).
func TestReincarnate_RepeatRetirement_AddsArchiveCounter(t *testing.T) {
	f := newFlow(t)

	const priorRetired = "ret1-aaaa-bbbb-cccc-dddd"
	const oldConv = "old1-aaaa-bbbb-cccc-dddd"
	const oldLabel = "spwn-old1-001"
	const oldTmux = "tclaude-spwn-old1-001"

	// A previous generation already retired as "worker-x".
	f.HaveConvWithTitle(priorRetired, "worker-x")
	f.HaveConvWithTitle(oldConv, "worker")
	f.HaveAliveSession(oldConv, oldLabel, oldTmux, "/tmp/work")
	f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv)

	r := f.AsHuman().Reincarnate(oldConv, "fresh start")

	f.AssertReincarnateTitle(r, "worker")
	assert.True(t, f.World.Tmux.WaitForSendKeys(oldTmux+":0.0", "/rename worker-x-2", 2*time.Second),
		"a second retirement disambiguates with -x-2; sent=%+v", f.World.Tmux.Sent())
}

// Scenario (changeover): a worker still carrying the OLD-scheme living
// name "worker-r-3". On reincarnation the successor sheds the legacy
// suffix back to the base "worker", and the predecessor keeps its full
// title plus "-x" — byte-identical to the pre-JOH-319 archive naming.
func TestReincarnate_LegacyLivingNameShedsSuffix(t *testing.T) {
	f := newFlow(t)

	const oldConv = "old3-aaaa-bbbb-cccc-dddd"
	const oldLabel = "spwn-old3-001"
	const oldTmux = "tclaude-spwn-old3-001"

	f.HaveConvWithTitle(oldConv, "worker-r-3")
	f.HaveAliveSession(oldConv, oldLabel, oldTmux, "/tmp/work")
	g := f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv)

	r := f.AsHuman().Reincarnate(oldConv, "fresh start")

	// Successor falls back to the plain base name.
	f.AssertReincarnateTitle(r, "worker")
	f.AssertSentContains(r.TmuxTarget(), "/rename worker", 5*time.Second)

	// Predecessor keeps its legacy full title, archive-marked.
	assert.True(t, f.World.Tmux.WaitForSendKeys(oldTmux+":0.0", "/rename worker-r-3-x", 2*time.Second),
		"legacy numbered predecessor archives as worker-r-3-x; sent=%+v", f.World.Tmux.Sent())
	assert.True(t, f.World.Tmux.WaitForSendKeys(oldTmux+":0.0", "/exit", 1*time.Second),
		"old pane should have received /exit; sent=%+v", f.World.Tmux.Sent())

	f.AssertGroupMember(g.Name, r.NewConv, "worker", 5*time.Second)
	f.AssertNotGroupMember(g.Name, oldConv)
}
