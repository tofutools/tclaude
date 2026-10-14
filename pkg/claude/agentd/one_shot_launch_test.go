package agentd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestForegroundAskJournalPreservesLiveOwnerAcrossSweep(t *testing.T) {
	isolateOneShotDirectories(t)
	run, err := newOneShotDirectoryRunForOwner(true)
	require.NoError(t, err)
	root := filepath.Join(run.Base, run.Key)
	require.NoError(t, os.MkdirAll(root, 0700))
	activeOneShotDirectories.Delete(run.Key) // model a separate daemon's map
	require.NoError(t, cleanupRetainedOneShotDirectories())
	_, err = os.Stat(root)
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(oneShotDirectoryJournalRoot(), run.Key+".json"))
	require.ErrorIs(t, err, os.ErrNotExist)
	// PID reuse must not keep an abandoned, never-launched directory alive.
	run.OwnerStart = "a previous incarnation"
	require.NoError(t, run.save())
	require.NoError(t, cleanupRetainedOneShotDirectories())
	_, err = os.Stat(root)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestForegroundAskSharedPreparationPreservesInteractiveResume(t *testing.T) {
	isolateOneShotDirectories(t)
	h, err := harness.Resolve(harness.CodexName)
	require.NoError(t, err)
	previous := EnsureSeanceCodexProfile
	var granted string
	EnsureSeanceCodexProfile = func(_, id string, snapshot *sandboxpolicy.Snapshot) (string, string, *harness.CodexSplitPolicyCapability, error) {
		for _, entry := range snapshot.Effective.Environment {
			if entry.Name == "GOCACHE" {
				granted = entry.Value
			}
		}
		path := filepath.Join(testutil.CanonicalTempDir(t), "profile")
		require.NoError(t, os.WriteFile(path, nil, 0600))
		return "ask-profile", path, nil, nil
	}
	t.Cleanup(func() { EnsureSeanceCodexProfile = previous })
	prepared, err := PrepareAskLaunch(AskLaunchInput{Cwd: testutil.CanonicalTempDir(t), Harness: h, Ask: harness.AskSpec{Print: false, ResumeID: "existing-thread"}, Snapshot: oneShotPolicy(t), Implementation: "harness-builtin", HarnessBuiltinMode: harness.SandboxManagedProfile})
	require.NoError(t, err)
	require.Equal(t, []string{"codex", "-p", "ask-profile"}, prepared.Argv[:3])
	require.Contains(t, prepared.Argv, "resume")
	require.Contains(t, prepared.Argv, "existing-thread")
	require.NotContains(t, prepared.Argv, "exec")
	require.Contains(t, prepared.Env, "GOCACHE="+granted)
	require.True(t, prepared.directory.Foreground)
	require.Equal(t, os.Getpid(), prepared.directory.OwnerPID)
	prepared.Close() // no subprocess started; remove immediately
	_, err = os.Stat(granted)
	require.ErrorIs(t, err, os.ErrNotExist)
}
