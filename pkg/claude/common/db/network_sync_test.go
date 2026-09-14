package db

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"testing"
)

func TestNetworkSyncComposedOneOffAndRevision(t *testing.T) {
	setupTestDB(t)
	baseID, err := CreateSandboxProfile(&SandboxProfile{Name: "base", Network: &sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeList}})
	require.NoError(t, err)
	_, err = CreateSandboxProfile(&SandboxProfile{Name: "outer", Includes: []string{"base"}})
	require.NoError(t, err)
	snapshot, err := ResolveEffectiveSandboxSnapshot(0, "outer")
	require.NoError(t, err)
	require.NoError(t, RegisterNetworkSyncLaunch("manual", "session", snapshot))
	require.NoError(t, BeginNetworkSyncLaunch("manual"))
	snapshot.NetworkAutoSync = true
	require.NoError(t, RegisterNetworkSyncLaunch("automatic", "session2", snapshot))
	require.NoError(t, BeginNetworkSyncLaunch("automatic"))
	queued, err := QueueProfileNetworkSync(baseID, false)
	require.NoError(t, err)
	require.Len(t, queued, 1)
	require.Equal(t, "automatic", queued[0].ID)
	queued, err = QueueProfileNetworkSync(baseID, true)
	require.NoError(t, err)
	require.Len(t, queued, 2)
	manual, err := ReadNetworkSyncLaunch("manual")
	require.NoError(t, err)
	require.False(t, manual.Automatic)
	require.NotNil(t, manual.Requested)
	require.Contains(t, manual.Dependencies, baseID)
	// A stale acknowledgement cannot overwrite a newer pending save.
	require.NoError(t, AcknowledgeNetworkSync("manual", 0, "applied", "", manual.Requested))
	manual, err = ReadNetworkSyncLaunch("manual")
	require.NoError(t, err)
	require.Equal(t, "pending", manual.Status)
	require.NoError(t, AcknowledgeNetworkSync("manual", manual.Revision, "applied", "", manual.Requested))
	manual, err = ReadNetworkSyncLaunch("manual")
	require.NoError(t, err)
	require.Equal(t, "applied", manual.Status)
	require.False(t, manual.Automatic)
	require.NoError(t, FinishNetworkSyncLaunch("manual"))
	rows, err := NetworkSyncLaunches()
	require.NoError(t, err)
	require.Len(t, rows, 1)
}

func TestNetworkSyncSpawnProfileTriStateAndSnapshot(t *testing.T) {
	setupTestDB(t)
	for _, v := range []*bool{nil, ptrBool(true), ptrBool(false)} {
		p := &SpawnProfile{Name: "sync", NetworkAutoSync: v}
		id, err := CreateSpawnProfile(p)
		require.NoError(t, err)
		got, err := GetSpawnProfile("sync")
		require.NoError(t, err)
		require.Equal(t, v, got.NetworkAutoSync)
		p.ID = id
		p.NetworkAutoSync = ptrBool(false)
		require.NoError(t, UpdateSpawnProfile(p))
		got, err = GetSpawnProfile("sync")
		require.NoError(t, err)
		require.Equal(t, ptrBool(false), got.NetworkAutoSync)
		_, err = DeleteSpawnProfile("sync")
		require.NoError(t, err)
	}
	snapshot, err := ResolveEffectiveSandboxSnapshot(0, "")
	require.NoError(t, err)
	snapshot.NetworkAutoSync = true
	revalidated, err := sandboxpolicy.RevalidateSnapshot(snapshot)
	require.NoError(t, err)
	require.True(t, revalidated.NetworkAutoSync)
}

func ptrBool(v bool) *bool { return &v }
