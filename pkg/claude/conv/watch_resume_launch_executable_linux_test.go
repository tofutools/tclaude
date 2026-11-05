//go:build linux

package conv

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc/agentipctest"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// A native Claude install puts only a symlink on PATH; the versioned
// executable lives elsewhere (~/.local/share), which a constructed root
// omits. A fresh launch behind tclaude's sandbox therefore pins the resolved
// executable before entering the namespace (session.ResolveTclaudeLayerExecutable);
// the operator-side resume of the same agent must launch the same file.
func TestResumeLaunchCmd_TclaudeLayerClaudeRunsTheResolvedExecutable(t *testing.T) {
	// A constructed root needs the canonical agentd socket live; keep HOME
	// short enough for a Unix socket path.
	home := agentipctest.ShortSocketDir(t)
	t.Setenv("HOME", home)
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	clearAmbientResumeOverride(t)
	socket := agentipc.CanonicalSocketPath()
	require.NoError(t, os.MkdirAll(filepath.Dir(socket), 0o700))
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	prevResolve := resolveResumeTclaudeLayer
	resolveResumeTclaudeLayer = func(sandboxpolicy.NetworkPosture, sandboxpolicy.RootPosture,
		sandboxpolicy.NetworkEngine, bool) (string, harness.LaunchOSSandbox, error) {
		return "/usr/bin/bwrap", harness.LaunchOSSandbox{}, nil
	}
	t.Cleanup(func() { resolveResumeTclaudeLayer = prevResolve })
	root := testutil.CanonicalTempDir(t)
	real := filepath.Join(root, "share", "claude", "versions", "9.9.9")
	require.NoError(t, os.MkdirAll(filepath.Dir(real), 0o755))
	require.NoError(t, os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755))
	bin := filepath.Join(root, "bin")
	require.NoError(t, os.MkdirAll(bin, 0o755))
	require.NoError(t, os.Symlink(real, filepath.Join(bin, "claude")))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	var effective sandboxpolicy.EffectiveProfile
	effective, err = sandboxpolicy.Resolve(sandboxpolicy.Scopes{
		Global: &sandboxpolicy.Profile{Name: "separate-root", FilesystemRoot: sandboxpolicy.FilesystemRootSeparate},
	})
	require.NoError(t, err)
	snapshot := sandboxpolicy.NewSnapshot(effective, nil)
	agentID, _, err := db.EnsureAgentForConv(resumeConvClaude, "test")
	require.NoError(t, err)
	require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, &snapshot))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "layer-claude-source", ConvID: resumeConvClaude, Cwd: root,
		Harness:               harness.DefaultName,
		HarnessBuiltinMode:    harness.ClaudeSandboxOff,
		SandboxImplementation: string(sandboxpolicy.ImplementationTclaudeLayer),
	}))

	cmd, _, _, err := resumeLaunchCmd(harness.DefaultName, resumeConvClaude[:8], resumeConvClaude, nil)
	require.NoError(t, err)
	// The exec token, not merely a read grant for the file.
	assert.Contains(t, cmd, real+" --resume "+resumeConvClaude,
		"the resumed pane must run the resolved Claude executable, as a fresh launch does")
}
