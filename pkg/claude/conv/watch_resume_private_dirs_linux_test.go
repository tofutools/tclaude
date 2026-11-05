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
	tclcommon "github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// tclaude's sandbox hides the daemon-owned attachment and morph parents from
// every launch, and each launch reopens only its own children: the session's
// attachment directory, whose paths the agent was handed at spawn, and the
// agent's morph directory, which it is pointed at on every later launch.
// `session new` grants both (prepareTclaudeLayerLaunch); a resume of the same
// agent must too, or those files vanish from the agent's view.
func TestResumeLaunchCmd_TclaudeLayerReopensTheAgentsPrivateDirs(t *testing.T) {
	home := testutil.CanonicalTempDir(t)
	t.Setenv("HOME", home)
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	clearAmbientResumeOverride(t)
	prevResolve := resolveResumeTclaudeLayer
	resolveResumeTclaudeLayer = func(sandboxpolicy.NetworkPosture, sandboxpolicy.RootPosture,
		sandboxpolicy.NetworkEngine, bool) (string, harness.LaunchOSSandbox, error) {
		return "/usr/bin/bwrap", harness.LaunchOSSandbox{}, nil
	}
	t.Cleanup(func() { resolveResumeTclaudeLayer = prevResolve })
	bin := filepath.Join(home, "bin")
	require.NoError(t, os.MkdirAll(bin, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\n"), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	work := filepath.Join(home, "work")
	require.NoError(t, os.MkdirAll(work, 0o755))

	sessionID := resumeConvClaude[:8]
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{})
	require.NoError(t, err)
	snapshot := sandboxpolicy.NewSnapshot(effective, nil)
	agentID, _, err := db.EnsureAgentForConv(resumeConvClaude, "test")
	require.NoError(t, err)
	require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, &snapshot))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: sessionID, ConvID: resumeConvClaude, Cwd: work,
		Harness:               harness.DefaultName,
		HarnessBuiltinMode:    harness.ClaudeSandboxOff,
		SandboxImplementation: string(sandboxpolicy.ImplementationTclaudeLayer),
	}))
	attachments, _, err := tclcommon.PrepareSpawnAttachmentsPrivateDir(sessionID)
	require.NoError(t, err)
	morph := tclcommon.MorphArtifactsDir(agentID)
	require.NoError(t, os.MkdirAll(morph, 0o700))

	cmd, _, _, err := resumeLaunchCmd(harness.DefaultName, sessionID, resumeConvClaude, nil)
	require.NoError(t, err)
	// Each parent is hidden, so no other session's attachments or other
	// agent's morph history show through, and only this agent's child is
	// reopened.
	for _, dir := range []struct{ parent, child, what string }{
		{tclcommon.SpawnAttachmentsPrivateBase(), attachments, "attachment"},
		{tclcommon.MorphArtifactsBase(), morph, "morph"},
	} {
		assert.Contains(t, cmd, "--tmpfs "+dir.parent+" ",
			"the %s parent must be hidden from the resumed pane", dir.what)
		assert.Contains(t, cmd, "--bind "+dir.child+" "+dir.child+" ",
			"the agent's own %s directory must stay reachable", dir.what)
	}
}
