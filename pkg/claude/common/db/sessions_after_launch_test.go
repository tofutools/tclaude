package db

import (
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestPostLaunchSavePreservesOnlyCurrentLaunchHooks(t *testing.T) {
	setupTestDB(t)
	started := time.Now()
	hookAt := started.Add(time.Millisecond)
	hook := &SessionRow{ID: "resume", TmuxSession: "pi", ConvID: "conv", Harness: "pi", Status: "awaiting_permission", StatusDetail: "Allow bash?", CreatedAt: started, LastHook: hookAt}
	require.NoError(t, SaveSession(hook))
	metadata := &SessionRow{ID: "resume", TmuxSession: "pi", ConvID: "conv", Harness: "pi", Status: "idle", CreatedAt: started, PID: 4242}
	require.NoError(t, SaveSessionAfterLaunch(metadata))
	saved, err := LoadSession("resume")
	require.NoError(t, err)
	require.Equal(t, hookAt.UnixNano(), saved.LastHook.UnixNano(), "final metadata must not erase the already-delivered hook")
	require.Equal(t, "awaiting_permission", saved.Status, "an open approval must remain blocked")
	require.Equal(t, "Allow bash?", saved.StatusDetail)
	require.Equal(t, 4242, saved.PID, "launch metadata is still recorded")
	metadata.CreatedAt = hookAt.Add(time.Millisecond)
	require.NoError(t, SaveSessionAfterLaunch(metadata))
	saved, err = LoadSession("resume")
	require.NoError(t, err)
	require.Equal(t, "idle", saved.Status, "predecessor hooks are not current-launch evidence")
	require.True(t, saved.LastHook.IsZero())
}
