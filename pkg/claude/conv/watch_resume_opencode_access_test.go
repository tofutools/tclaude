package conv

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

const resumeConvOpenCode = "6c3e8d21-2222-4333-8444-555566667777"

// `session new` admits OpenCode network rules in its soft access-control mode
// (web-tool access control). A resume of the same agent must admit them too,
// or a conversation that spawned fine cannot be resumed.
func TestResumeLaunchCmd_OpenCodeAccessControlAdmitsNetworkRules(t *testing.T) {
	setupTestDB(t)
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Global: &sandboxpolicy.Profile{
		Name: "opencode-no-network", NetworkAccess: sandboxpolicy.NetworkAccessNone,
	}})
	require.NoError(t, err)
	snapshot := sandboxpolicy.NewSnapshot(effective, nil)
	agentID, _, err := db.EnsureAgentForConv(resumeConvOpenCode, "test")
	require.NoError(t, err)
	require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, &snapshot))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "resume-opencode-access", ConvID: resumeConvOpenCode,
		Harness: harness.OpenCodeName, HarnessBuiltinMode: harness.OpenCodeSandboxAccessControl,
	}))

	_, _, _, err = resumeLaunchCmd(harness.OpenCodeName, resumeConvOpenCode[:8], resumeConvOpenCode, nil)
	if err != nil {
		require.NotContains(t, err.Error(), "unsupported_sandbox_profile_network",
			"a posture `session new` admits must stay resumable")
	}
}

const resumeConvGeminiMissing = "7d4f9e32-3333-4444-8555-666677778888"

// The shared gates read the launch view of the snapshot, as `session new`
// does: a read rule whose path does not exist yet is filtered out of the
// launch, so it must not count as a filesystem rule Gemini's own sandbox
// cannot represent.
func TestResumeLaunchCmd_RuleAwaitingItsPathDoesNotTripRepresentationGates(t *testing.T) {
	setupTestDB(t)
	missing := filepath.Join(t.TempDir(), "not-yet")
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Global: &sandboxpolicy.Profile{
		Name:       "awaiting-path",
		Filesystem: []sandboxpolicy.FilesystemGrant{{Path: missing, Access: sandboxpolicy.AccessRead}},
	}})
	require.NoError(t, err)
	snapshot := sandboxpolicy.NewSnapshot(effective, nil)
	agentID, _, err := db.EnsureAgentForConv(resumeConvGeminiMissing, "test")
	require.NoError(t, err)
	require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, &snapshot))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "resume-gemini-missing", ConvID: resumeConvGeminiMissing,
		Harness: harness.GeminiName,
	}))

	_, _, _, err = resumeLaunchCmd(harness.GeminiName, resumeConvGeminiMissing[:8], resumeConvGeminiMissing, nil)
	if err != nil {
		require.NotContains(t, err.Error(), "unsupported_sandbox_profile_filesystem",
			"a rule waiting for its path is not part of the launch")
	}
}
