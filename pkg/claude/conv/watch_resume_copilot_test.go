package conv

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

const resumeConvCopilot = "5b2f7c10-1111-4222-8333-444455556666"

// TCL-973 — a resume renders the RECORDED approval posture into the launch
// command, so these pin the production resume path rather than the renderer.

// The recorded posture, not the harness default, is what a resumed Copilot pane
// comes back under: relaunch reconstructs an existing agent and must never
// widen it.
func TestResumeLaunchCmd_CopilotPreservesRecordedApprovalPosture(t *testing.T) {
	for _, tc := range []struct {
		name     string
		recorded string
		wantIn   []string
		wantOut  []string
	}{
		{
			name:     "allow-tools renders the two measured nonblocking flags",
			recorded: harness.CopilotApprovalAllowTools,
			wantIn:   []string{"--allow-all-tools", "--no-ask-user"},
			wantOut:  []string{"--allow-all-paths", "--deny-tool"},
		},
		{
			name:     "inherit renders no permission flags at all",
			recorded: harness.CopilotApprovalInherit,
			wantOut:  []string{"--allow-all-tools", "--no-ask-user", "--allow-all-paths"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupTestDB(t)
			require.NoError(t, db.SaveSession(&db.SessionRow{
				ID: "resume-copilot-source", ConvID: resumeConvCopilot,
				Harness: harness.CopilotName, ApprovalPolicy: tc.recorded,
			}))

			cmd, _, h, err := resumeLaunchCmd(harness.CopilotName,
				resumeConvCopilot[:8], resumeConvCopilot, nil)
			require.NoError(t, err)
			require.NotNil(t, h)
			assert.Equal(t, harness.CopilotName, h.Name)
			assert.Contains(t, cmd, "copilot --resume="+resumeConvCopilot)
			for _, want := range tc.wantIn {
				assert.Contains(t, cmd, want)
			}
			for _, unwanted := range tc.wantOut {
				assert.NotContains(t, cmd, unwanted)
			}
			// Every Copilot launch scrubs the ambient promoter, whatever the
			// recorded posture — including the `inherit` one it would otherwise
			// silently upgrade.
			assert.Contains(t, cmd, "unset COPILOT_ALLOW_ALL;")
		})
	}
}

// A pass-through arg that moves the permission posture must fail the resume
// closed, because the pane would then run broader than the row that records it.
// Ordinary pass-through args must keep working.
func TestResumeLaunchCmd_CopilotRefusesPostureMovingPassThroughArgs(t *testing.T) {
	for _, args := range [][]string{
		{"--allow-all-paths"},
		{"--yolo"},
		{"--allow-all"},
		{"--no-color", "--allow-all-tools"},
		{"--deny-tool=url(github.com)"},
		{"--add-dir", "/srv/elsewhere"},
		{"--disallow-temp-dir"},
	} {
		t.Run(args[0], func(t *testing.T) {
			setupTestDB(t)
			require.NoError(t, db.SaveSession(&db.SessionRow{
				ID: "resume-copilot-source", ConvID: resumeConvCopilot,
				Harness:        harness.CopilotName,
				ApprovalPolicy: harness.CopilotApprovalInherit,
			}))
			_, _, _, err := resumeLaunchCmd(harness.CopilotName,
				resumeConvCopilot[:8], resumeConvCopilot, args)
			require.Errorf(t, err, "pass-through %v must fail the resume closed", args)
			assert.Contains(t, err.Error(), "moves the Copilot permission posture that tclaude records",
				"the refusal must explain WHY, not merely reject")
		})
	}
}

func TestResumeLaunchCmd_CopilotKeepsOrdinaryPassThroughArgs(t *testing.T) {
	setupTestDB(t)
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "resume-copilot-source", ConvID: resumeConvCopilot,
		Harness: harness.CopilotName, ApprovalPolicy: harness.CopilotApprovalAllowTools,
	}))
	cmd, _, _, err := resumeLaunchCmd(harness.CopilotName,
		resumeConvCopilot[:8], resumeConvCopilot, []string{"--log-level=debug", "--no-color"})
	require.NoError(t, err, "the audit is a posture gate, not an allowlist")
	assert.Contains(t, cmd, "--log-level=debug")
	assert.Contains(t, cmd, "--no-color")
}
