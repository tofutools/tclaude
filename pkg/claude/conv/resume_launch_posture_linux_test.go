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
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// Behind tclaude's sandbox a resumed pane's hook callbacks go through agentd,
// and a constructed root selects the canonical agentd socket for the agent,
// in both the launch environment and the pane's shell environment.
func TestResumeLaunchCmd_TclaudeLayerConstructedRootSelectsAgentdForHooksAndSocket(t *testing.T) {
	home := agentipctest.ShortSocketDir(t)
	t.Setenv("HOME", home)
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	clearAmbientResumeOverride(t)
	t.Setenv(agentipc.SocketEnv, "")
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
	bin := filepath.Join(root, "bin")
	require.NoError(t, os.MkdirAll(bin, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\n"), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{
		Global: &sandboxpolicy.Profile{Name: "separate-root", FilesystemRoot: sandboxpolicy.FilesystemRootSeparate},
	})
	require.NoError(t, err)
	snapshot := sandboxpolicy.NewSnapshot(effective, nil)
	agentID, _, err := db.EnsureAgentForConv(resumeConvClaude, "test")
	require.NoError(t, err)
	require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, &snapshot))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: resumeConvClaude[:8], ConvID: resumeConvClaude, Cwd: root,
		Harness:               harness.DefaultName,
		HarnessBuiltinMode:    harness.ClaudeSandboxOff,
		SandboxImplementation: string(sandboxpolicy.ImplementationTclaudeLayer),
	}))

	cmd, _, _, err := resumeLaunchCmd(harness.DefaultName, resumeConvClaude[:8], resumeConvClaude, nil)
	require.NoError(t, err)
	assert.Contains(t, cmd, "export "+session.HookBrokerEnvVar+"="+session.HookBrokerAgentd)
	assert.Contains(t, cmd, "export "+agentipc.SocketEnv+"=")
}
