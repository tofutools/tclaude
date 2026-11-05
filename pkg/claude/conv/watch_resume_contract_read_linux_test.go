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

// The launch contract's read reopens are computed over HOST paths: each
// candidate is symlink-resolved against the host, so a sandbox-side spelling
// fed in would resolve to whatever sits at that spelling on the host. A
// remapped write grant needs no reopen at all — its mount is applied after the
// deny that would have masked it (session.sandboxLaunchContractReadDirs). A
// resumed pane must not grant a host path a fresh launch would not.
func TestResumeLaunchCmd_RemappedWriteGrantMintsNoHostReadReopen(t *testing.T) {
	resumeMountedPathMintsNoHostReadReopen(t, func(denied, hostData, guest string) sandboxpolicy.Profile {
		return sandboxpolicy.Profile{
			Name: "remapped-under-deny",
			Filesystem: []sandboxpolicy.FilesystemGrant{
				{Path: denied, Access: sandboxpolicy.AccessDeny},
				{Path: hostData, MountPath: guest, Access: sandboxpolicy.AccessWrite},
			},
		}
	})
}

// The same holds for a temporary filesystem: it is mounted after the deny, so
// it needs no reopen, and its sandbox path must not be resolved on the host.
func TestResumeLaunchCmd_TmpfsMountMintsNoHostReadReopen(t *testing.T) {
	resumeMountedPathMintsNoHostReadReopen(t, func(denied, _, guest string) sandboxpolicy.Profile {
		return sandboxpolicy.Profile{
			Name:       "tmpfs-under-deny",
			Filesystem: []sandboxpolicy.FilesystemGrant{{Path: denied, Access: sandboxpolicy.AccessDeny}},
			Tmpfs:      []sandboxpolicy.TmpfsMount{{Path: guest}},
		}
	})
}

func resumeMountedPathMintsNoHostReadReopen(t *testing.T,
	profile func(denied, hostData, guest string) sandboxpolicy.Profile,
) {
	t.Helper()
	// HOME and the temp root kept apart: Copilot's baseline grants the
	// system temp directory and refuses one that covers HOME.
	base := testutil.CanonicalTempDir(t)
	home, tmp := filepath.Join(base, "home"), filepath.Join(base, "tmp")
	require.NoError(t, os.MkdirAll(home, 0o755))
	require.NoError(t, os.MkdirAll(tmp, 0o755))
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", tmp)
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	clearAmbientResumeOverride(t)
	prevResolve := resolveResumeTclaudeLayer
	resolveResumeTclaudeLayer = func(sandboxpolicy.NetworkPosture, sandboxpolicy.RootPosture,
		sandboxpolicy.NetworkEngine, bool) (string, harness.LaunchOSSandbox, error) {
		return "/usr/bin/bwrap", harness.LaunchOSSandbox{}, nil
	}
	t.Cleanup(func() { resolveResumeTclaudeLayer = prevResolve })

	root := filepath.Join(base, "work")
	denied := filepath.Join(root, "secret")
	hostData := filepath.Join(root, "data")
	elsewhere := filepath.Join(denied, "hidden")
	for _, dir := range []string{denied, hostData, elsewhere} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}
	// On the host, the sandbox-side spelling leads to another path under the
	// same deny, which the profile never reopened.
	guest := filepath.Join(denied, "work")
	require.NoError(t, os.Symlink(elsewhere, guest))

	authored := profile(denied, hostData, guest)
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Global: &authored})
	require.NoError(t, err)
	snapshot := sandboxpolicy.NewSnapshot(effective, nil)
	agentID, _, err := db.EnsureAgentForConv(resumeConvCopilot, "test")
	require.NoError(t, err)
	require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, &snapshot))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "resume-remapped", ConvID: resumeConvCopilot, Cwd: root,
		Harness: harness.CopilotName, HarnessBuiltinMode: harness.CopilotSandboxOff,
		SandboxImplementation: string(sandboxpolicy.ImplementationTclaudeLayer),
		ApprovalPolicy:        harness.CopilotApprovalAllowTools,
	}))

	cmd, _, _, err := resumeLaunchCmd(harness.CopilotName, resumeConvCopilot[:8], resumeConvCopilot, nil)
	require.NoError(t, err)
	assert.NotContains(t, cmd, elsewhere,
		"the resumed pane must not be granted the host target behind a mounted path's sandbox spelling")
}
