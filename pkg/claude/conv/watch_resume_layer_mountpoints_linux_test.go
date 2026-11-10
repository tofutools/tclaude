//go:build linux

package conv

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// A resume behind tclaude's sandbox prepares the protected mountpoints its
// bwrap command hides, as `session new` does. bwrap runs over a read-only
// root, so a hide target that no longer exists on the host (here
// ~/.claude/sessions, which a Codex- or pi-only host may never have had, or
// an operator may have removed since the launch) cannot be created at mount
// time and the resumed pane would fail to start.
func TestResumeLaunchCmd_TclaudeLayerPreparesProtectedMountpoints(t *testing.T) {
	home := testutil.CanonicalTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv(harness.PiAgentDirEnvVar, filepath.Join(home, "operator-pi"))
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	clearAmbientResumeOverride(t)
	prevResolve := resolveResumeTclaudeLayer
	resolveResumeTclaudeLayer = func(sandboxpolicy.NetworkPosture, sandboxpolicy.RootPosture,
		sandboxpolicy.NetworkEngine, bool) (string, harness.LaunchOSSandbox, error) {
		return "/usr/bin/bwrap", harness.LaunchOSSandbox{}, nil
	}
	t.Cleanup(func() { resolveResumeTclaudeLayer = prevResolve })
	root := testutil.CanonicalTempDir(t)
	bin := filepath.Join(root, "bin")
	require.NoError(t, os.MkdirAll(bin, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "pi"), []byte("#!/bin/sh\n"), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: resumeConvPi[:8], ConvID: resumeConvPi, Cwd: root,
		Harness:               harness.PiName,
		SandboxImplementation: string(sandboxpolicy.ImplementationTclaudeLayer),
	}))
	sessions := filepath.Join(home, ".claude", "sessions")
	require.NoError(t, os.RemoveAll(filepath.Join(home, ".claude")))

	cmd, _, _, err := resumeLaunchCmd(harness.PiName, resumeConvPi[:8], resumeConvPi, nil)
	require.NoError(t, err)
	require.Contains(t, cmd, sessions, "the resumed bwrap command hides ~/.claude/sessions")
	protected, err := sandboxpolicy.ProtectedPaths()
	require.NoError(t, err)
	for _, path := range protected {
		info, statErr := os.Stat(path)
		if assert.NoError(t, statErr, "protected mountpoint %s must exist before bwrap starts", path) {
			assert.True(t, info.IsDir(), "%s must be a directory", path)
		}
	}
}
