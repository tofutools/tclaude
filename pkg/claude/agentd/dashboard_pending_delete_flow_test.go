package agentd_test

import (
	"database/sql"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Flow coverage for the pending-spawn CLEANUP escape hatch — the mirror of
// the focus path. A pending spawn (the pending_spawns table, JOH-205) that
// is wedged behind a startup gate it will never clear can be discarded from
// the dashboard, either via the per-row 🗑 delete button or by dragging the
// row to the trash; both invoke POST /api/pending/delete/{label}. These
// scenarios pin that the endpoint (a) kills the live tmux pane, (b) drops
// the pending + session rows, (c) is self-healing (404) for an already-gone
// label, (d) still cleans up rows when the pane is already dead, and (e) is
// POST-only.

// Scenario: deleting a live pending spawn kills its tmux pane and removes
// both its pending_spawns row and its session row — a full teardown of the
// three things a pending spawn owns, so it vanishes from the pending list.
func TestPendingDelete_KillsPaneAndDropsRows(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()
	g := f.HaveGroup("alpha")

	const label = "spwn-del1"
	havePendingSpawn(t, f, label, "tmux-del1", f.TestCwd("del1"), g.ID, "worker", "delete-me")
	require.True(t, f.World.Tmux.IsAlive("tmux-del1"), "precondition: pane is alive")

	rec := testharness.Serve(mux,
		testharness.JSONRequest(t, http.MethodPost, "/api/pending/delete/"+label, nil))
	require.Equal(t, http.StatusOK, rec.Code, "delete body=%s", rec.Body.String())

	assert.False(t, f.World.Tmux.IsAlive("tmux-del1"), "the gated pane is killed")

	p, err := db.GetPendingSpawn(label)
	require.NoError(t, err)
	assert.Nil(t, p, "the pending_spawns row is gone")

	sess, err := db.LoadSession(label)
	assert.ErrorIs(t, err, sql.ErrNoRows, "the session row is gone")
	assert.Nil(t, sess)

	// And it no longer surfaces in the dashboard's pending[] list.
	snap := fetchDashSnapshot(t, mux)
	assert.Nil(t, findDashPending(snap, label), "deleted spawn drops out of pending[]")
}

// Scenario: deleting a label with no pending row is a self-healing 404 —
// the sweeper may have enrolled + cleaned it up between the operator's
// click and this request, in which case it's now a normal agent reachable
// via the conv-keyed retire path. Mirrors the focus endpoint's 404.
func TestPendingDelete_404ForUnknownLabel(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_ = newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	rec := testharness.Serve(mux,
		testharness.JSONRequest(t, http.MethodPost, "/api/pending/delete/spwn-nope", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code, "unknown label is a 404; body=%s", rec.Body.String())
}

// Scenario: deleting a pending spawn whose pane has already died still
// succeeds — the dead pane is skipped and the pending + session rows are
// dropped anyway. A dead-pane row is exactly the stuck state the operator
// most needs to clear, so it must not 404 like focus does.
func TestPendingDelete_CleansUpDeadPane(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()
	g := f.HaveGroup("alpha")

	const label = "spwn-del-dead"
	havePendingSpawn(t, f, label, "tmux-del-dead", f.TestCwd("deldead"), g.ID, "worker", "dead-pane")
	f.MarkOffline("tmux-del-dead") // the pane died after the spawn was recorded

	rec := testharness.Serve(mux,
		testharness.JSONRequest(t, http.MethodPost, "/api/pending/delete/"+label, nil))
	require.Equal(t, http.StatusOK, rec.Code, "dead-pane delete still succeeds; body=%s", rec.Body.String())

	p, err := db.GetPendingSpawn(label)
	require.NoError(t, err)
	assert.Nil(t, p, "the pending_spawns row is gone even with a dead pane")
}

// Scenario: the pending-delete endpoint is POST-only — a GET is rejected
// with a 405 and leaves the pending row intact.
func TestPendingDelete_RejectsNonPost(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()
	g := f.HaveGroup("alpha")

	const label = "spwn-del-meth"
	havePendingSpawn(t, f, label, "tmux-del-meth", f.TestCwd("delmeth"), g.ID, "worker", "method")

	rec := testharness.Serve(mux,
		testharness.JSONRequest(t, http.MethodGet, "/api/pending/delete/"+label, nil))
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, "GET must be rejected; body=%s", rec.Body.String())

	p, err := db.GetPendingSpawn(label)
	require.NoError(t, err)
	assert.NotNil(t, p, "a rejected method must leave the pending row intact")
}
