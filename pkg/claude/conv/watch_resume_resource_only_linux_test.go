//go:build linux

// resource-only is a Linux-only implementation: sandboxImplementationHostFailure
// refuses it off Linux rather than degrading to `off`, so every test that drives
// a real resource-only launch or resume belongs here. The predicate, mode and
// enforcement-table tests stay cross-platform — they take an explicit platform
// argument and assert the same answer everywhere.

package conv

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// The resume seam is the third of the three that plan access, and the one that
// stayed broken longest: the shared ResolveAccessEnforcement fix let it past
// the capability call, and it then failed on the NEXT call in the same block.
// PlanAccessEnforcement refuses a closed network no mechanism can hold — right
// for every caller that expected a wall, wrong for one that declared it has
// none — so a resource-only conversation carrying a deny-baseline `network:`
// block could be spawned but never resumed.
//
// A closed network is the shape that matters here: `list` already degrades to
// a no_mechanism notice rather than an error, and `open` needs nothing. The
// chain also carries a filesystem grant, because the resume path gates
// filesystem rules TWICE — once over authored rows and again over rendered
// ones — and Codex refuses them on a mode that is not its managed profile,
// which is exactly the mode an unconfined resume resolves.
func TestResumeLaunchCmd_ResourceOnlyResumesWithAClosedNetworkChain(t *testing.T) {
	for _, tc := range []struct{ harnessName, mode string }{
		{harness.DefaultName, harness.ClaudeSandboxOff},
		{harness.CodexName, harness.SandboxDangerFull},
	} {
		t.Run(tc.harnessName, func(t *testing.T) {
			resumeResourceOnlyClosedNetwork(t, tc.harnessName, tc.mode)
		})
	}
}

func resumeResourceOnlyClosedNetwork(t *testing.T, harnessName, mode string) {
	t.Helper()
	setupTestDB(t)
	clearAmbientResumeOverride(t)
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{
		Global: &sandboxpolicy.Profile{
			Name:    "closed-net-limits",
			Network: &sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeClosed},
			Filesystem: []sandboxpolicy.FilesystemGrant{
				{Path: "/usr/share", Access: sandboxpolicy.AccessRead},
			},
			UnixSockets: &sandboxpolicy.UnixSocketRules{
				Mode: sandboxpolicy.AccessModeClosed,
			},
			ResourceLimits: sandboxpolicy.ResourceLimits{Memory: "8GiB"},
		},
	})
	require.NoError(t, err)
	snapshot := sandboxpolicy.NewSnapshot(effective, nil)
	agentID, _, agentErr := db.EnsureAgentForConv(resumeConvClaude, "test")
	require.NoError(t, agentErr)
	require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, &snapshot))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "resource-only-source", ConvID: resumeConvClaude,
		Harness:               harnessName,
		HarnessBuiltinMode:    mode,
		SandboxImplementation: string(sandboxpolicy.ImplementationResourceOnly),
	}))

	cmd, _, h, err := resumeLaunchCmd(
		harnessName, resumeConvClaude[:8], resumeConvClaude, nil)
	require.NoError(t, err,
		"a resource-only conversation must resume; refusing here strands an agent "+
			"that spawned cleanly under the same chain")
	require.NotNil(t, h)
	assert.Equal(t, harnessName, h.Name)
	// Harness-agnostic: Claude spells it --resume=<id>, Codex `resume <id>`.
	assert.Contains(t, cmd, resumeConvClaude,
		"the built command must actually resume this conversation")
}

// The same resume must still SAY that those rules are inert, in the words a
// fresh launch uses. A resumed pane looking quieter than the one it replaced is
// how an operator concludes the policy came back.
func TestResumeLaunchCmd_ResourceOnlyDisclosesInertAccessRules(t *testing.T) {
	setupTestDB(t)
	clearAmbientResumeOverride(t)
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{
		Global: &sandboxpolicy.Profile{
			Name:           "closed-net-notice",
			Network:        &sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeClosed},
			ResourceLimits: sandboxpolicy.ResourceLimits{Memory: "4GiB"},
		},
	})
	require.NoError(t, err)
	snapshot := sandboxpolicy.NewSnapshot(effective, nil)
	agentID, _, agentErr := db.EnsureAgentForConv(resumeConvClaude, "test")
	require.NoError(t, agentErr)
	require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, &snapshot))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "resource-only-notice", ConvID: resumeConvClaude,
		Harness:               harness.DefaultName,
		HarnessBuiltinMode:    harness.ClaudeSandboxOff,
		SandboxImplementation: string(sandboxpolicy.ImplementationResourceOnly),
	}))

	var effectiveSandbox *sandboxpolicy.Snapshot
	_, _, _, resumeErr := resumeLaunchCmdWithStackedProof(
		harness.DefaultName, resumeConvClaude[:8], resumeConvClaude, nil,
		nil, &effectiveSandbox,
	)
	require.NoError(t, resumeErr)
	require.NotNil(t, effectiveSandbox)

	var disclosed bool
	for _, notice := range effectiveSandbox.Effective.AccessNotices {
		if notice.Reason == sandboxpolicy.AccessNoticeReasonUnconfinedImplementation {
			disclosed = true
			assert.Equal(t, sandboxpolicy.AccessNoticeEffectNotEnforced, notice.Effect)
		}
	}
	assert.True(t, disclosed,
		"the resumed snapshot must carry the same inert-rules notice a fresh launch does")
}

// The narrow-but-real refusal the socket skip actually closes.
//
// RevalidateSnapshot and the materializer diverge on one axis:
// validateSocketAllowEntry checks a path_glob by resolving its LITERAL
// ANCESTOR only, while MaterializeUnixSocketList globs that directory and then
// Lstats every matched CHILD. Listing a directory needs r; stat-ing its
// children needs x. A directory left readable-but-not-executable (a tightened
// socket dir, a tmpfiles.d mode) separates the two: revalidation passes and the
// materializer refuses.
//
// That is the one measured case where skipping the materialization changes the
// outcome for an unconfined resume, rather than merely avoiding work something
// else refuses first. An explicit `path` row under the same directory does NOT
// reach it — revalidation resolves that one itself and refuses first, which is
// the residual limitation documented at the skip.
func TestResumeLaunchCmd_ResourceOnlyResumesOverAGlobTheMaterializerWouldRefuse(t *testing.T) {
	setupTestDB(t)
	clearAmbientResumeOverride(t)

	dir := filepath.Join(t.TempDir(), "sockets")
	require.NoError(t, os.Mkdir(dir, 0o755))
	listener, err := net.Listen("unix", filepath.Join(dir, "agent.sock"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{
		Global: &sandboxpolicy.Profile{
			Name: "socket-glob-limits",
			UnixSockets: &sandboxpolicy.UnixSocketRules{
				Mode:  sandboxpolicy.AccessModeList,
				Allow: []sandboxpolicy.SocketAllowEntry{{PathGlob: filepath.Join(dir, "*.sock")}},
			},
			ResourceLimits: sandboxpolicy.ResourceLimits{Memory: "8GiB"},
		},
	})
	require.NoError(t, err)
	snapshot := sandboxpolicy.NewSnapshot(effective, nil)
	agentID, _, agentErr := db.EnsureAgentForConv(resumeConvClaude, "test")
	require.NoError(t, agentErr)
	require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, &snapshot))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "resource-only-glob", ConvID: resumeConvClaude,
		Harness:               harness.DefaultName,
		HarnessBuiltinMode:    harness.ClaudeSandboxOff,
		SandboxImplementation: string(sandboxpolicy.ImplementationResourceOnly),
	}))

	// Root bypasses the permission check entirely, so the materializer would
	// succeed and this test would pass while proving nothing — and CI commonly
	// runs as root in a container. Skip loudly rather than assert vacuously.
	if os.Geteuid() == 0 {
		t.Skip("needs a non-root euid: root ignores the missing execute bit this test relies on")
	}
	// Readable, not executable: the glob still lists, the children no longer stat.
	require.NoError(t, os.Chmod(dir, 0o444))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	_, _, _, resumeErr := resumeLaunchCmd(
		harness.DefaultName, resumeConvClaude[:8], resumeConvClaude, nil)
	require.NoError(t, resumeErr,
		"an unconfined resume must not materialize a socket allowlist it does not "+
			"enforce; this row passes revalidation and refuses only in the materializer")
}
