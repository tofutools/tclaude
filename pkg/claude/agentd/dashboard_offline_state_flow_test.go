package agentd_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Scenario: an agent finishes its turn (final Stop hook → session row
// Status="idle"), then its tmux session dies. No SessionEnd-style hook
// fires on exit, so the row's Status stays frozen at "idle". The
// dashboard snapshot must NOT pass that stale "idle" through — it
// reports the agent offline with state.status == "exited".
//
// Pins the "agents show up as idle instead of offline on the dashboard"
// bug: stateForConv used to echo the frozen hook status verbatim.
func TestDashboardSnapshot_OfflineAgentReportsExitedNotIdle(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t)

	const onlineConv = "onln-aaaa-bbbb-cccc-dddddddddddd"
	const offlineConv = "offl-aaaa-bbbb-cccc-dddddddddddd"
	f.HaveConvWithTitle(onlineConv, "online-worker")
	f.HaveConvWithTitle(offlineConv, "offline-worker")
	f.HaveAliveSession(onlineConv, "spwn-onln", "tmux-onln", f.TestCwd("onln"))
	f.HaveAliveSession(offlineConv, "spwn-offl", "tmux-offl", f.TestCwd("offl"))

	// Both join a group so they surface in the snapshot (an offline
	// ungrouped conv with no grants is intentionally absent everywhere).
	f.HaveGroup("crew")
	f.HaveMember("crew", onlineConv)
	f.HaveMember("crew", offlineConv)

	// Freeze the offline conv's hook status at "idle" — exactly the row
	// a cleanly-finished agent leaves behind — then kill its tmux
	// session. Without the fix the snapshot would echo "idle".
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:          "spwn-offl",
		TmuxSession: "tmux-offl",
		ConvID:      offlineConv,
		Cwd:         f.TestCwd("offl"),
		Status:      "idle",
		LastHook:    time.Now(),
	}), "freeze offline session status at idle")
	f.MarkOffline("tmux-offl")

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	memberOf := func(conv string) *dashMember {
		for _, g := range snap.Groups {
			for i := range g.Members {
				if g.Members[i].ConvID == conv {
					return &g.Members[i]
				}
			}
		}
		return nil
	}
	agentOf := func(conv string) *dashAgent {
		for i := range snap.Agents {
			if snap.Agents[i].ConvID == conv {
				return &snap.Agents[i]
			}
		}
		return nil
	}

	// Offline member: online=false, and the stale "idle" must be gone.
	off := memberOf(offlineConv)
	require.NotNil(t, off, "offline conv should still be a member of crew")
	assert.False(t, off.Online, "dead tmux session → member should be offline")
	assert.Equal(t, "exited", off.State.Status,
		"offline agent must report exited, not the frozen hook status")
	assert.NotEqual(t, "idle", off.State.Status,
		"the stale 'idle' status must not leak into the snapshot")

	// Same conv via the broader Agents list.
	offA := agentOf(offlineConv)
	require.NotNil(t, offA, "offline conv should appear in Agents")
	assert.False(t, offA.Online, "Agents row should be offline too")
	assert.Equal(t, "exited", offA.State.Status, "Agents row must report exited")

	// Control: the online member keeps its live, non-exited status.
	on := memberOf(onlineConv)
	require.NotNil(t, on, "online conv should be a member of crew")
	assert.True(t, on.Online, "live tmux session → member should be online")
	assert.NotEqual(t, "exited", on.State.Status,
		"a live agent must never be reported as exited")
}
