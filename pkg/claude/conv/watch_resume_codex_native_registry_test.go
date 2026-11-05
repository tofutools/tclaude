//go:build unix

package conv

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// installTestCodexNativeRegistry installs the opt-in native permission
// registry under home, as `/etc/codex -> ~/.tclaude/data/codex-sb-cfg` would
// be installed on a host, and points the session package at it.
func installTestCodexNativeRegistry(t *testing.T, home string) session.CodexNativeRegistryOptions {
	t.Helper()
	tclaudeDir := filepath.Join(home, ".tclaude")
	dataDir := filepath.Join(tclaudeDir, "data")
	managedDir := filepath.Join(dataDir, "codex-sb-cfg")
	require.NoError(t, os.MkdirAll(managedDir, 0o700))
	for _, path := range []string{tclaudeDir, dataDir, managedDir} {
		require.NoError(t, os.Chmod(path, 0o700))
	}
	systemRoot := filepath.Join(home, "system")
	require.NoError(t, os.Mkdir(systemRoot, 0o755))
	systemDir := filepath.Join(systemRoot, "codex")
	require.NoError(t, os.Symlink(managedDir, systemDir))
	uid := uint32(os.Getuid())
	opts := session.CodexNativeRegistryOptions{
		SystemDir: systemDir, ManagedDir: managedDir, RootUID: uid, UserUID: uid,
	}
	t.Cleanup(session.SetCodexNativeRegistryOptionsForTest(opts))
	return opts
}

// setupCodexManagedResume records an agent's managed Codex conversation whose
// resume renders a generated profile, and installs a native registry.
func setupCodexManagedResume(t *testing.T) (session.CodexNativeRegistryOptions, string) {
	t.Helper()
	home := testutil.CanonicalTempDir(t)
	t.Setenv("HOME", home)
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	t.Setenv("CODEX_HOME", t.TempDir())
	opts := installTestCodexNativeRegistry(t, home)
	workspace := filepath.Join(home, "plain-workspace")
	require.NoError(t, os.MkdirAll(workspace, 0o755))
	writeDir := filepath.Join(home, "scratch")
	require.NoError(t, os.MkdirAll(writeDir, 0o755))

	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Explicit: &sandboxpolicy.Profile{
		Name: "codex-resume-registry", NetworkAccess: sandboxpolicy.NetworkAccessInternet,
		Filesystem: []sandboxpolicy.FilesystemGrant{{Path: writeDir, Access: sandboxpolicy.AccessWrite}},
	}})
	require.NoError(t, err)
	snapshot := sandboxpolicy.NewSnapshot(effective, nil)
	agentID, _, err := db.EnsureAgentForConv(resumeConvCodex, "test")
	require.NoError(t, err)
	require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, &snapshot))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "source-session", ConvID: resumeConvCodex, Harness: harness.CodexName,
		Cwd: workspace, HarnessBuiltinMode: harness.SandboxManagedProfile, Status: session.StatusExited,
	}))
	return opts, agentID
}

func requireNativeProfiles(t *testing.T) []db.CodexNativePermissionProfile {
	t.Helper()
	profiles, err := db.ListCodexNativePermissionProfiles()
	require.NoError(t, err)
	return profiles
}

func readNativeRequirements(t *testing.T, opts session.CodexNativeRegistryOptions) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(opts.ManagedDir, "requirements.toml"))
	require.NoError(t, err)
	return string(data)
}

// A resumed managed Codex conversation joins an installed native permission
// registry, as `session new` does. Before this, the resume launched with
// `-p tclaude-agent-<id>` while requirements.toml did not allow that profile,
// so Codex fell back to another allowed profile instead of the one rendered
// from the conversation's sandbox (awb tcl-7kcuq1).
//
// The registration is owned by the conversation's agent and kept live by the
// resume's own session row: it is activated once that row is saved, a later
// resume's ready profile supersedes it, and the startup sweep retires it once
// the session has exited and its agent can no longer resume.
func TestResumeCodexManagedProfileJoinsTheNativeRegistry(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("native Codex registry targets Linux and macOS")
	}
	opts, agentID := setupCodexManagedResume(t)
	const sessionID = resumeConvCodex

	relaunch := &standaloneRelaunch{convID: resumeConvCodex}
	cmd, profilePath, _, err := resumeLaunchCmdWithEffectiveSandbox(
		harness.CodexName, sessionID, resumeConvCodex, nil, nil, relaunch)
	require.NoError(t, err)
	require.NotEmpty(t, profilePath)
	profiles := requireNativeProfiles(t)
	require.Len(t, profiles, 1, "the resume registers its generated profile")
	registered := profiles[0]
	assert.Contains(t, cmd, registered.ProfileName, "the pane starts with the registered profile")
	assert.Equal(t, agentID, registered.OwnerAgentID)
	assert.Equal(t, resumeConvCodex, registered.OwnerConvID)
	assert.Equal(t, sessionID, registered.LaunchID)
	assert.False(t, registered.LaunchReady, "not ready before the pane's row is saved")
	assert.Contains(t, readNativeRequirements(t, opts), registered.ProfileName,
		"requirements.toml allows the profile the pane is started with")

	// The pane starts, and its row is saved: the generation is ready.
	relaunch.settle(true)
	row := &db.SessionRow{
		ID: sessionID, ConvID: resumeConvCodex, Harness: harness.CodexName,
		Cwd: filepath.Join(os.Getenv("HOME"), "plain-workspace"), Status: session.StatusIdle,
		HarnessBuiltinMode: harness.SandboxManagedProfile,
	}
	require.NoError(t, db.SaveSession(row))
	relaunch.activateNativeProfile()
	stored, err := db.GetCodexNativePermissionProfile(registered.Generation)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.True(t, stored.LaunchReady)

	// The sweep keeps it while the session runs.
	require.NoError(t, session.ReconcileCodexNativePermissionRegistry())
	require.Len(t, requireNativeProfiles(t), 1, "a running resume keeps its profile")

	// The next resume's ready profile supersedes it.
	row.Status = session.StatusExited
	require.NoError(t, db.SaveSession(row))
	next := &standaloneRelaunch{convID: resumeConvCodex}
	_, _, _, err = resumeLaunchCmdWithEffectiveSandbox(
		harness.CodexName, sessionID, resumeConvCodex, nil, nil, next)
	require.NoError(t, err)
	require.Len(t, requireNativeProfiles(t), 2, "the predecessor stays until its successor is ready")
	next.settle(true)
	row.Status = session.StatusIdle
	require.NoError(t, db.SaveSession(row))
	next.activateNativeProfile()
	profiles = requireNativeProfiles(t)
	require.Len(t, profiles, 1, "a ready successor prunes the resume it superseded")
	successor := profiles[0]
	assert.NotEqual(t, registered.Generation, successor.Generation)
	requirements := readNativeRequirements(t, opts)
	assert.NotContains(t, requirements, registered.ProfileName)
	assert.Contains(t, requirements, successor.ProfileName)

	// An exited session whose agent can still resume keeps its profile, as a
	// `session new` launch's does. Once the agent is retired, the startup
	// sweep retires it.
	row.Status = session.StatusExited
	require.NoError(t, db.SaveSession(row))
	require.NoError(t, session.ReconcileCodexNativePermissionRegistry())
	require.Len(t, requireNativeProfiles(t), 1, "a resumable owner keeps its exited profile")
	_, err = db.RetireAgent(resumeConvCodex, "test", "gone")
	require.NoError(t, err)
	require.NoError(t, session.ReconcileCodexNativePermissionRegistry())
	assert.Empty(t, requireNativeProfiles(t), "the sweep retires an exited resume's profile")
	assert.NotContains(t, readNativeRequirements(t, opts), successor.ProfileName)
}

// A resume whose pane never starts leaves the registry as it found it.
func TestResumeCodexManagedProfileLeavesTheRegistryWhenNotLaunched(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("native Codex registry targets Linux and macOS")
	}
	opts, _ := setupCodexManagedResume(t)
	relaunch := &standaloneRelaunch{convID: resumeConvCodex}
	_, _, _, err := resumeLaunchCmdWithEffectiveSandbox(
		harness.CodexName, resumeConvCodex, resumeConvCodex, nil, nil, relaunch)
	require.NoError(t, err)
	profiles := requireNativeProfiles(t)
	require.Len(t, profiles, 1)

	relaunch.settle(false)
	assert.Empty(t, requireNativeProfiles(t))
	assert.NotContains(t, readNativeRequirements(t, opts), profiles[0].ProfileName)
}
