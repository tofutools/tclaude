package agentd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestPlacementOrderUsesStableInstanceTies(t *testing.T) {
	a, b := float64(1), float64(1)
	ramA, ramB := uint64(100), uint64(200)
	rows := []fedPlacementCandidate{{Instance: "inst-b", Eligible: true, LoadPerCore: &b, RAMAvailable: &ramB}, {Instance: "inst-a", Eligible: true, LoadPerCore: &a, RAMAvailable: &ramA}, {Instance: "inst-c", Eligible: false}}
	require.Equal(t, []int{1, 0}, placementOrder(rows, "least-loaded"))
	require.Equal(t, []int{0, 1}, placementOrder(rows, "most-free-ram"))
}

type enrollingCapacityTmux struct {
	clcommon.Tmux
	enroll func()
	alive  map[string]struct{}
}

func (t enrollingCapacityTmux) ListSessions() (map[string]struct{}, error) {
	t.enroll()
	return t.alive, nil // snapshot may precede the late pane
}
func TestNodeCapacityCountsEnrollmentDuringObservation(t *testing.T) {
	setupTestDB(t)
	id := db.NewAgentID()
	require.NoError(t, db.InsertPendingSpawn(&db.PendingSpawn{Label: "spwn-cap-transition", AgentID: id, GroupID: 1}))
	prior := clcommon.Default
	clcommon.Default = enrollingCapacityTmux{Tmux: prior, enroll: func() {
		claimed, err := db.ClaimPendingSpawnAndBindAgent("spwn-cap-transition", "cap-conv", id, "spawn")
		require.NoError(t, err)
		require.True(t, claimed)
	}}
	t.Cleanup(func() { clcommon.Default = prior })
	nodeAdmission.Lock()
	used, err := nodeCapacityUsed("")
	nodeAdmission.Unlock()
	require.NoError(t, err)
	require.Equal(t, 1, used, "a reservation becoming an actor cannot disappear between snapshots")
}

func TestNodeCapacityUsesPaneLivenessDespiteCachedExit(t *testing.T) {
	setupTestDB(t)
	_, _, err := db.EnsureAgentForConv("cap-exited-conv", "spawn")
	require.NoError(t, err)
	require.NoError(t, db.SaveSession(&db.SessionRow{ID: "cap-exited", TmuxSession: "cap-exited-pane", ConvID: "cap-exited-conv", Status: "exited", CreatedAt: time.Now()}))
	prior := clcommon.Default
	clcommon.Default = enrollingCapacityTmux{Tmux: prior, enroll: func() {}, alive: map[string]struct{}{"cap-exited-pane": {}}}
	t.Cleanup(func() { clcommon.Default = prior })
	nodeAdmission.Lock()
	used, err := nodeCapacityUsed("")
	nodeAdmission.Unlock()
	require.NoError(t, err)
	require.Equal(t, 1, used, "a late pane still counts after premature attach cached an exited state")
}
