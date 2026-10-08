package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// The app-server observer keeps its source label in the session row so
// diagnostics can distinguish a snapshot from a hook. The dashboard snapshot
// is a regular user-facing projection, however: source changes must not turn
// canonical idle/working states into labels such as "idle: app-server
// snapshot". Meaningful operational detail still belongs in the projection.
func TestDashboardSnapshot_HidesAppServerStatusProvenance(t *testing.T) {
	const (
		idleConv    = "status-idle-1111-2222-3333-4444"
		workingConv = "status-work-1111-2222-3333-4444"
		labelIdle   = "spwn-status-idle"
		labelWork   = "spwn-status-work"
	)

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("squad")
	f.HaveAliveSession(idleConv, labelIdle, "tmux-status-idle", f.TestCwd("status-idle"))
	f.HaveAliveSession(workingConv, labelWork, "tmux-status-work", f.TestCwd("status-work"))
	f.HaveMember("squad", idleConv)
	f.HaveMember("squad", workingConv)

	idle, err := db.LoadSession(labelIdle)
	require.NoError(t, err)
	idle.Status = session.StatusIdle
	idle.StatusDetail = "app-server snapshot"
	require.NoError(t, db.SaveSession(idle))

	working, err := db.LoadSession(labelWork)
	require.NoError(t, err)
	working.Status = session.StatusWorking
	working.StatusDetail = "app-server reconnect"
	require.NoError(t, db.SaveSession(working))

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	idleMember := findDashMember(snap, "squad", idleConv)
	workingMember := findDashMember(snap, "squad", workingConv)
	require.NotNil(t, idleMember)
	require.NotNil(t, workingMember)
	assert.Equal(t, session.StatusIdle, idleMember.State.Status)
	assert.Empty(t, idleMember.State.StatusDetail)
	assert.Equal(t, session.StatusWorking, workingMember.State.Status)
	assert.Empty(t, workingMember.State.StatusDetail)

	// A real operational detail remains part of the regular projection.
	working.StatusDetail = "Bash"
	require.NoError(t, db.SaveSession(working))
	snap = fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	workingMember = findDashMember(snap, "squad", workingConv)
	require.NotNil(t, workingMember)
	assert.Equal(t, "Bash", workingMember.State.StatusDetail)
}
