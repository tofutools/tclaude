package db

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"os"
	"path/filepath"
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

func TestNetworkSyncBrokenUnrelatedProfileDoesNotBlockQueue(t *testing.T) {
	setupTestDB(t)
	id, err := CreateSandboxProfile(&SandboxProfile{Name: "target"})
	require.NoError(t, err)
	_, err = CreateSandboxProfile(&SandboxProfile{Name: "deleted"})
	require.NoError(t, err)
	for _, name := range []string{"deleted", "target"} {
		snapshot, err := ResolveEffectiveSandboxSnapshot(0, name)
		require.NoError(t, err)
		require.NoError(t, RegisterNetworkSyncLaunch(name, name, snapshot))
		require.NoError(t, BeginNetworkSyncLaunch(name))
	}
	_, err = DeleteSandboxProfile("deleted")
	require.NoError(t, err)
	rows, err := QueueProfileNetworkSync(id, true)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "target", rows[0].ID)
}

func TestNetworkSyncPinsHostDatabaseAcrossChangedHome(t *testing.T) {
	setupTestDB(t)
	snapshot, err := ResolveEffectiveSandboxSnapshot(0, "")
	require.NoError(t, err)
	require.NoError(t, RegisterNetworkSyncLaunch("pinned", "session", snapshot))
	hostDB := DBPath()
	Close()
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, PinNetworkSyncDatabase(hostDB))
	require.NoError(t, BeginNetworkSyncLaunch("pinned"))
	row, err := ReadNetworkSyncLaunch("pinned")
	require.NoError(t, err)
	require.Equal(t, "session", row.SessionID)
	_, err = os.Stat(filepath.Join(home, ".tclaude", "data", "db.sqlite"))
	require.True(t, os.IsNotExist(err))
}

func TestNetworkSyncPublishesOnlyNetworkAndGuardsSuccessor(t *testing.T) {
	setupTestDB(t)
	snapshot, err := ResolveEffectiveSandboxSnapshot(0, "")
	require.NoError(t, err)
	require.NoError(t, SaveSession(&SessionRow{ID: "session", EffectiveSandbox: &snapshot}))
	require.NoError(t, RegisterNetworkSyncLaunch("first", "session", snapshot))
	require.NoError(t, BeginNetworkSyncLaunch("first"))
	changed := snapshot
	changed.Effective.Network = &sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeList, Allow: []sandboxpolicy.NetworkAllowEntry{{Host: "example.com"}}}
	changed.Effective.Environment = []sandboxpolicy.EnvironmentEntry{{Name: "UNAPPLIED", Value: "no"}}
	require.NoError(t, AcknowledgeNetworkSync("first", 0, "applied", "", &changed))
	row, err := LoadSession("session")
	require.NoError(t, err)
	require.NotNil(t, row)
	require.Equal(t, changed.Effective.Network, row.EffectiveSandbox.Effective.Network)
	require.Equal(t, snapshot.Effective.Environment, row.EffectiveSandbox.Effective.Environment)
	require.NoError(t, RegisterNetworkSyncLaunch("successor", "session", snapshot))
	require.NoError(t, BeginNetworkSyncLaunch("successor"))
	require.NoError(t, AcknowledgeNetworkSync("first", 0, "applied", "", &snapshot))
	row, err = LoadSession("session")
	require.NoError(t, err)
	require.Equal(t, changed.Effective.Network, row.EffectiveSandbox.Effective.Network)
}

func TestNetworkSyncResolvesChangedGlobalAndGroupAssignments(t *testing.T) {
	setupTestDB(t)
	for _, name := range []string{"global-old", "global-new", "group-old", "group-new", "included", "explicit"} {
		p := &SandboxProfile{Name: name}
		if name == "explicit" {
			p.Includes = []string{"included"}
		}
		_, err := CreateSandboxProfile(p)
		require.NoError(t, err)
	}
	groupID, err := CreateAgentGroup("crew", "")
	require.NoError(t, err)
	require.NoError(t, SetGlobalSandboxProfile("global-old"))
	_, err = SetAgentGroupSandboxProfile("crew", "group-old")
	require.NoError(t, err)
	original, err := ResolveEffectiveSandboxSnapshot(groupID, "explicit")
	require.NoError(t, err)
	original.NetworkAutoSync = true
	require.NoError(t, SetGlobalSandboxProfile("global-new"))
	_, err = SetAgentGroupSandboxProfile("crew", "group-new")
	require.NoError(t, err)
	current, err := ResolveNetworkSyncSnapshot(original)
	require.NoError(t, err)
	require.True(t, current.NetworkAutoSync)
	names := []string{}
	for _, p := range current.Applied {
		names = append(names, p.Name)
	}
	require.Contains(t, names, "global-new")
	require.Contains(t, names, "group-new")
	require.Contains(t, names, "explicit")
	require.NotContains(t, names, "global-old")
	require.NotContains(t, names, "group-old")
}
