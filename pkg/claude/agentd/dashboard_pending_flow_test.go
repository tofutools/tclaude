package agentd_test

import (
	"net/http"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Flow coverage for JOH-205 inc4 Part A — the dashboard surfaces PENDING
// spawns (the pending_spawns table) and offers a label-keyed focus button.
//
// A pending spawn is a dashboard Codex spawn whose conv-id hasn't
// materialised yet: it has a live tmux pane but is stuck behind a startup
// gate, so it never took the first turn that exposes its conv-id and is
// NOT an enrolled agent. These scenarios pin (a) that /api/snapshot lists
// it under pending[] with its pane-liveness + gate location, and (b) that
// POST /api/pending/focus/{label} opens a terminal attached to the LABEL
// (never a conv-id, which doesn't exist) so the operator can clear the
// gate. The real terminal spawn is not unit-testable, so the openTerminal
// seam is swapped for a recorder.

// findDashPending returns the pending entry with the given label, or nil.
func findDashPending(snap dashSnapshot, label string) *dashPending {
	for i := range snap.Pending {
		if snap.Pending[i].Label == label {
			return &snap.Pending[i]
		}
	}
	return nil
}

// havePendingSpawn seeds a pending spawn that is ALIVE: a live tmux pane +
// a harness=codex session row whose conv-id is empty (the defining pending
// state), plus the pending_spawns row carrying the enrollment intent. It
// mirrors what executeSpawn's async path records for a gated Codex spawn.
func havePendingSpawn(t *testing.T, f *testharness.Flow, label, tmux, cwd string, groupID int64, role, name string) {
	t.Helper()
	// A live Codex pane. HaveAliveCodexSession registers the tmux session
	// alive and writes a session row; overwrite that row (same id → UPSERT,
	// tmux registration untouched so it stays alive) to clear the conv-id —
	// a pending spawn's conv-id has not materialised yet.
	f.HaveAliveCodexSession("conv-"+label, label, tmux, cwd)
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: label, TmuxSession: tmux, Cwd: cwd, Status: "running", Harness: "codex",
	}), "clear conv-id on pending session row")
	require.NoError(t, db.InsertPendingSpawn(&db.PendingSpawn{
		Label: label, GroupID: groupID, Role: role, Name: name, Descr: "a gated spawn",
	}), "insert pending spawn")
}

// Scenario: /api/snapshot surfaces a pending spawn under pending[] with
// its group, role/name, gate location (cwd) and pane-liveness — the data
// the dashboard's "Pending" virtual group renders. A pending spawn whose
// pane has died (no session row) still lists, but as offline, so the
// dashboard can disable its focus button.
func TestDashboardSnapshot_SurfacesPendingSpawns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
		f := newFlow(t)
		g := f.HaveGroup("alpha")

		havePendingSpawn(t, f, "spwn-pend1", "tmux-pend1", "/tmp/pend1", g.ID, "reviewer", "pending-reviewer")

		// A second pending row whose pane is GONE: a pending_spawns row with no
		// session at all — the stale state the sweeper has not yet cleaned up.
		require.NoError(t, db.InsertPendingSpawn(&db.PendingSpawn{
			Label: "spwn-dead", GroupID: g.ID, Role: "worker", Name: "dead-pane",
		}))

		snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

		alive := findDashPending(snap, "spwn-pend1")
		require.NotNil(t, alive, "alive pending spawn missing from pending[]; have %+v", snap.Pending)
		assert.Equal(t, "alpha", alive.Group, "group resolved from group_id")
		assert.Equal(t, "reviewer", alive.Role)
		assert.Equal(t, "pending-reviewer", alive.Name)
		assert.Equal(t, "/tmp/pend1", alive.Cwd, "gate location from the session row")
		assert.Equal(t, "codex", alive.Harness)
		assert.True(t, alive.Online, "the pane is live, so the focus button stays enabled")

		dead := findDashPending(snap, "spwn-dead")
		require.NotNil(t, dead, "stale pending spawn must still list so the operator can see it")
		assert.False(t, dead.Online, "a gone pane lists as offline → focus button disabled")

		// A pending spawn is NOT an enrolled agent — it must not leak onto the
		// Agents roster or the group's member list.
		assert.Nil(t, findDashAgent(snap, "conv-spwn-pend1"),
			"a pending spawn must not appear as an enrolled agent")
	})
}

// Scenario: POST /api/pending/focus/{label} opens a terminal ATTACHED to
// the pending spawn's pane, keyed on the LABEL (a pending agent has no
// conv-id), so the operator can clear the startup gate. The agent process
// is untouched — this only gives its detached pane a window.
func TestPendingFocus_OpensAttachTerminalKeyedOnLabel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
		f := newFlow(t)
		mux := agentd.BuildDashboardHandlerForTest()
		g := f.HaveGroup("alpha")

		const label = "spwn-foc1"
		havePendingSpawn(t, f, label, "tmux-foc1", "/tmp/foc1", g.ID, "reviewer", "focus-me")

		var mu sync.Mutex
		var opened []string
		t.Cleanup(agentd.SetOpenTerminalForTest(func(cmd string) error {
			mu.Lock()
			defer mu.Unlock()
			opened = append(opened, cmd)
			return nil
		}))

		rec := testharness.Serve(mux,
			testharness.JSONRequest(t, http.MethodPost, "/api/pending/focus/"+label, nil))
		require.Equal(t, http.StatusOK, rec.Code, "focus body=%s", rec.Body.String())

		require.Len(t, opened, 1, "exactly one terminal opened")
		assert.Contains(t, opened[0], "attach", "opens an attach terminal")
		assert.Contains(t, opened[0], label, "attach is keyed on the spawn label, not a conv-id")

		// Window-only: the pane keeps running.
		assert.True(t, f.World.Tmux.IsAlive("tmux-foc1"), "focus opens a window only — the pane is untouched")
	})
}

// Scenario: focusing a label with no pending row is a 404 and opens
// nothing — the endpoint is scoped to pending spawns, and a label the
// sweeper already enrolled + cleaned up is "gone" (the dashboard's re-poll
// will have moved it to the agent roster).
func TestPendingFocus_404ForUnknownLabel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
		f := newFlow(t)
		_ = f
		mux := agentd.BuildDashboardHandlerForTest()

		var dispatched bool
		t.Cleanup(agentd.SetOpenTerminalForTest(func(string) error {
			dispatched = true
			return nil
		}))

		rec := testharness.Serve(mux,
			testharness.JSONRequest(t, http.MethodPost, "/api/pending/focus/spwn-nope", nil))
		assert.Equal(t, http.StatusNotFound, rec.Code, "unknown label is a 404; body=%s", rec.Body.String())
		assert.False(t, dispatched, "no terminal opened for an unknown pending label")
	})
}

// Scenario: focusing a pending spawn whose pane has died is a 404 and
// opens nothing — the same offline→404 boundary as the per-agent hide /
// jump endpoints. The pending row exists, but its tmux pane is gone.
func TestPendingFocus_404ForDeadPane(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
		f := newFlow(t)
		mux := agentd.BuildDashboardHandlerForTest()
		g := f.HaveGroup("alpha")

		const label = "spwn-dead2"
		havePendingSpawn(t, f, label, "tmux-dead2", "/tmp/dead2", g.ID, "worker", "dead-pane")
		f.MarkOffline("tmux-dead2") // the pane died after the spawn was recorded

		var dispatched bool
		t.Cleanup(agentd.SetOpenTerminalForTest(func(string) error {
			dispatched = true
			return nil
		}))

		rec := testharness.Serve(mux,
			testharness.JSONRequest(t, http.MethodPost, "/api/pending/focus/"+label, nil))
		assert.Equal(t, http.StatusNotFound, rec.Code, "a dead pane is a 404; body=%s", rec.Body.String())
		assert.False(t, dispatched, "no terminal opened for a dead pane")
	})
}

// Scenario: the pending-focus endpoint is POST-only — a GET is rejected
// with a 405 and never opens a terminal.
func TestPendingFocus_RejectsNonPost(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
		f := newFlow(t)
		mux := agentd.BuildDashboardHandlerForTest()
		g := f.HaveGroup("alpha")

		const label = "spwn-meth"
		havePendingSpawn(t, f, label, "tmux-meth", "/tmp/meth", g.ID, "worker", "method")

		var dispatched bool
		t.Cleanup(agentd.SetOpenTerminalForTest(func(string) error {
			dispatched = true
			return nil
		}))

		rec := testharness.Serve(mux,
			testharness.JSONRequest(t, http.MethodGet, "/api/pending/focus/"+label, nil))
		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, "GET must be rejected; body=%s", rec.Body.String())
		assert.False(t, dispatched, "a rejected method must not open a terminal")
	})
}
