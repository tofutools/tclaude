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

// Under tclaude's sandbox the outer wall is Copilot's only enforcement
// boundary, so every tclaude-layer launch boundary verifies Copilot's own
// sandbox stays off (session.ValidateTclaudeLayerHarnessPosture): `session new`
// and the daemon spawn guard both run it. `--experimental` re-enables
// Copilot's in-pane `/sandbox` command from the argv side, so a resume that
// passed it through would start the pane under a second boundary its record
// says it does not have.
func TestResumeLaunchCmd_TclaudeLayerCopilotRefusesExperimental(t *testing.T) {
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
	work := filepath.Join(base, "work")
	require.NoError(t, os.MkdirAll(work, 0o755))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "resume-copilot-posture", ConvID: resumeConvCopilot, Cwd: work,
		Harness: harness.CopilotName, HarnessBuiltinMode: harness.CopilotSandboxOff,
		SandboxImplementation: string(sandboxpolicy.ImplementationTclaudeLayer),
		ApprovalPolicy:        harness.CopilotApprovalAllowTools,
	}))

	cmd, _, _, err := resumeLaunchCmd(harness.CopilotName, resumeConvCopilot[:8], resumeConvCopilot,
		[]string{"--experimental"})
	require.Error(t, err, "resumed command: %s", cmd)
	assert.Contains(t, err.Error(), "--experimental")
}
