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

const resumeConvPi = "7c3e9a10-2222-4333-8444-555566667777"

// A pi resumed behind tclaude's sandbox runs as `session new` launched it:
// against tclaude's walled agent directory for this conversation, not the
// operator's, and with the live-status extension loaded and mounted for the
// pane.
func TestResumeLaunchCmd_TclaudeLayerPiKeepsWalledDirAndStatusExtension(t *testing.T) {
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

	cmd, _, _, err := resumeLaunchCmd(harness.PiName, resumeConvPi[:8], resumeConvPi, nil)
	require.NoError(t, err)
	walled, err := harness.PreparePiWalledAgentDir(resumeConvPi)
	require.NoError(t, err)
	assert.Contains(t, cmd, "export "+harness.PiAgentDirEnvVar+"="+walled+"; pi ",
		"a walled pi runs against tclaude's walled agent dir, not the operator's")
	extension, err := harness.MaterializePiExtension()
	require.NoError(t, err)
	assert.Contains(t, cmd, "--extension "+extension, "the status extension is a launch flag")
	assert.Contains(t, cmd, "--ro-bind "+filepath.Dir(extension),
		"tclaude's sandbox mounts the extension's directory")
}
