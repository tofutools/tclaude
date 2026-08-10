package agentd

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

func TestCleanupRetiredCodexNativeProfilesDefersUntilActorIsRetired(t *testing.T) {
	setupTestDB(t)
	const convID = "native-cleanup-retired-conv"
	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	require.NoError(t, db.UpsertCodexAppServerRuntime(db.CodexAppServerRuntime{
		Generation: "native-cleanup-generation", LaunchID: "native-cleanup-launch",
		AgentID: agentID, ConvID: convID, SocketPath: filepath.Join(t.TempDir(), "app.sock"),
		State: db.CodexAppServerDead,
	}))
	require.NoError(t, db.UpsertCodexNativePermissionProfile(db.CodexNativePermissionProfile{
		Generation: "native-cleanup-generation", ProfileName: "tclaude-agent-1234567890abcdef",
		ProfileTOML: "default_permissions = \"tclaude-agent-1234567890abcdef\"\n",
	}))

	cleanupRetiredCodexNativeProfiles(convID)
	profile, err := db.GetCodexNativePermissionProfile("native-cleanup-generation")
	require.NoError(t, err)
	require.NotNil(t, profile)
	assert.False(t, profile.CleanupPending, "active/resumable actor keeps its definition")

	retired, err := db.RetireAgent(convID, "human", "test")
	require.NoError(t, err)
	require.True(t, retired)
	previousOnline := codexNativeConvOnline
	t.Cleanup(func() { codexNativeConvOnline = previousOnline })
	codexNativeConvOnline = func(string) bool { return true }
	cleanupRetiredCodexNativeProfiles(convID)
	profile, err = db.GetCodexNativePermissionProfile("native-cleanup-generation")
	require.NoError(t, err)
	require.NotNil(t, profile)
	assert.False(t, profile.CleanupPending, "online retirement must not alter a live turn's profile")

	codexNativeConvOnline = func(string) bool { return false }
	cleanupRetiredCodexNativeProfiles(convID)
	profile, err = db.GetCodexNativePermissionProfile("native-cleanup-generation")
	require.NoError(t, err)
	require.NotNil(t, profile)
	assert.True(t, profile.CleanupPending,
		"missing host topology defers publication but must durably retain cleanup intent")
}

func TestReinstateUsesResumeLaunchLock(t *testing.T) {
	setupTestDB(t)
	const convID = "native-cleanup-reinstate-conv"
	_, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	_, err = db.RetireAgent(convID, "human", "test")
	require.NoError(t, err)

	lock := resumeLaunchLock(convID)
	lock.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = reinstateAgentUnderLaunchLock(convID)
	}()
	select {
	case <-done:
		t.Fatal("reinstate raced past retirement/resume launch lock")
	default:
	}
	lock.Unlock()
	<-done
	state, err := db.AgentState(convID)
	require.NoError(t, err)
	assert.Equal(t, db.AgentStateActive, state)
}

func TestRetiredProfileCleanupSharesReinstateLaunchLock(t *testing.T) {
	setupTestDB(t)
	const predecessorConv = "native-cleanup-race-predecessor"
	const currentConv = "native-cleanup-race-current"
	agentID, _, err := db.EnsureAgentForConv(predecessorConv, "test")
	require.NoError(t, err)
	_, err = db.RotateAgentConv(predecessorConv, currentConv, "reincarnate")
	require.NoError(t, err)
	_, err = db.RetireAgent(currentConv, "human", "test")
	require.NoError(t, err)
	require.NoError(t, db.UpsertCodexNativePermissionProfile(db.CodexNativePermissionProfile{
		Generation: "native-cleanup-race", ProfileName: "tclaude-agent-3234567890abcdef",
		ProfileTOML:  "default_permissions = \"tclaude-agent-3234567890abcdef\"\n",
		OwnerAgentID: agentID, OwnerConvID: currentConv,
	}))
	previousOnline := codexNativeConvOnline
	codexNativeConvOnline = func(string) bool { return false }
	t.Cleanup(func() { codexNativeConvOnline = previousOnline })

	lock := resumeLaunchLock(currentConv)
	require.Same(t, lock, resumeLaunchLock(predecessorConv),
		"every generation of one stable actor must share its launch lock")
	lock.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		cleanupRetiredCodexNativeProfiles(predecessorConv)
	}()
	select {
	case <-done:
		t.Fatal("profile cleanup raced past the reinstate/resume launch lock")
	default:
	}
	_, err = db.ReinstateAgent(currentConv)
	require.NoError(t, err)
	lock.Unlock()
	<-done
	profile, err := db.GetCodexNativePermissionProfile("native-cleanup-race")
	require.NoError(t, err)
	require.NotNil(t, profile)
	assert.False(t, profile.CleanupPending,
		"cleanup that loses to reinstate must preserve the newly active actor's profile")
}

func TestFirstNativeActivationAdoptsLiveOrdinaryCodexProfile(t *testing.T) {
	setupTestDB(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	profileName, profilePath, err := harness.EnsureCodexAgentLaunchProfile(nil, "1234567890abcdef")
	require.NoError(t, err)
	const convID = "ordinary-live-codex-conv"
	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "ordinary-live-launch", ConvID: convID, TmuxSession: "ordinary-live-tmux",
		Harness: harness.CodexName, Status: "running",
	}))
	launchScript := filepath.Join(t.TempDir(), "launch-scripts", "launch-proof.sh")
	require.NoError(t, os.MkdirAll(filepath.Dir(launchScript), 0o700))
	previousStarts := codexNativePaneStarts
	previousRegister := registerCodexNativePermissionProfilesIfInstalled
	t.Cleanup(func() {
		codexNativePaneStarts = previousStarts
		registerCodexNativePermissionProfilesIfInstalled = previousRegister
	})
	codexNativePaneStarts = func() ([]byte, error) {
		return []byte(fmt.Sprintf("ordinary-live-tmux\t0\t/bin/sh %s %s\n", launchScript, profilePath)), nil
	}
	var captured db.CodexNativePermissionProfile
	registerCodexNativePermissionProfilesIfInstalled = func(registrations []session.CodexNativePermissionProfileRegistration) (bool, error) {
		require.Len(t, registrations, 1)
		captured = registrations[0].Profile
		assert.Equal(t, profilePath, registrations[0].ProfilePath)
		return true, nil
	}

	require.NoError(t, adoptLiveCodexProfilesIntoInstalledRegistry())
	assert.Equal(t, profileName, captured.ProfileName)
	assert.Equal(t, convID, captured.OwnerConvID)
	assert.Equal(t, agentID, captured.OwnerAgentID)
	assert.Equal(t, "ordinary-live-launch", captured.LaunchID)
	assert.True(t, captured.LaunchReady)
}

func TestFirstNativeActivationAdoptsStartingPaneBeforeSessionRow(t *testing.T) {
	setupTestDB(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	profileName, profilePath, err := harness.EnsureCodexAgentLaunchProfile(nil, "2234567890abcdef")
	require.NoError(t, err)
	launchScript := filepath.Join(t.TempDir(), "launch-scripts", "launch-proof.sh")
	require.NoError(t, os.MkdirAll(filepath.Dir(launchScript), 0o700))
	previousStarts := codexNativePaneStarts
	previousRegister := registerCodexNativePermissionProfilesIfInstalled
	t.Cleanup(func() {
		codexNativePaneStarts = previousStarts
		registerCodexNativePermissionProfilesIfInstalled = previousRegister
	})
	codexNativePaneStarts = func() ([]byte, error) {
		return []byte(fmt.Sprintf("starting-tmux\t0\t/bin/sh %s %s\n", launchScript, profilePath)), nil
	}
	var captured db.CodexNativePermissionProfile
	registerCodexNativePermissionProfilesIfInstalled = func(registrations []session.CodexNativePermissionProfileRegistration) (bool, error) {
		require.Len(t, registrations, 1)
		captured = registrations[0].Profile
		assert.Equal(t, profilePath, registrations[0].ProfilePath)
		return true, nil
	}

	require.NoError(t, adoptLiveCodexProfilesIntoInstalledRegistry())
	assert.Equal(t, profileName, captured.ProfileName)
	assert.Equal(t, "starting-tmux", captured.LaunchID)
	assert.Empty(t, captured.OwnerConvID)
	assert.Empty(t, captured.OwnerAgentID)
	assert.True(t, captured.LaunchReady)
}
