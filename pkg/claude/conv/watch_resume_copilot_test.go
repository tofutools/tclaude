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
	"github.com/tofutools/tclaude/pkg/claude/session"
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

// A pass-through arg naming an option tclaude renders and records must fail the
// resume closed. On a resume the IDENTITY arms matter most: the command already
// carries `--resume=<recorded-id>`, so a second one would attach the pane to a
// different conversation than the one being resumed, while every downstream
// consumer of the record — hooks, status, the conversation index — kept
// describing the original. Ordinary pass-through args must keep working.
func TestResumeLaunchCmd_CopilotRefusesTclaudeOwnedPassThroughArgs(t *testing.T) {
	for _, args := range [][]string{
		{"--allow-all-paths"},
		{"--yolo"},
		{"--allow-all"},
		{"--no-color", "--allow-all-tools"},
		{"--deny-tool=url(github.com)"},
		{"--add-dir", "/srv/elsewhere"},
		{"--disallow-temp-dir"},
		// Identity: a second conversation selector on a resume.
		{"--resume=99999999-9999-4999-8999-999999999999"},
		{"-r", "99999999-9999-4999-8999-999999999999"},
		{"--session-id", "99999999-9999-4999-8999-999999999999"},
		{"--continue"},
		{"--connect"},
		{"-i", "a second first turn"},
		// Metadata tclaude has dedicated, recorded options for.
		{"--model=gpt-5.4"},
		{"--effort=high"},
		{"--name=renamed"},
		// Headless mode, whose no-TTY permission fallbacks are unmeasured.
		{"-p", "go"},
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
			assert.Contains(t, err.Error(), "which tclaude renders and records for this launch",
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

// A Copilot row that predates the approval catalog carries a blank posture. The
// interactive resume path must reconstruct it as `inherit` — what those launches
// actually did — and never as the new `allow-tools` default.
//
// The promotion this guards against would be durable, not momentary: the
// resumed generation's posture is written back into the new session row, so one
// resume would permanently hand the conversation in-sandbox lineage authority
// that the lineage classifier refuses to credit a blank Copilot row with.
func TestResumeLaunchCmd_CopilotBlankRowDoesNotGainTheNewDefault(t *testing.T) {
	setupTestDB(t)
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "resume-copilot-legacy", ConvID: resumeConvCopilot,
		Harness: harness.CopilotName,
		// ApprovalPolicy deliberately blank: the pre-catalog row shape.
	}))

	h, err := harness.Resolve(harness.CopilotName)
	require.NoError(t, err)
	policy, autoReview, err := resumeApprovalState(h, resumeConvCopilot)
	require.NoError(t, err)
	assert.Equal(t, harness.CopilotApprovalInherit, policy,
		"a blank Copilot row must reconstruct as inherit, not be promoted to the daemon default")
	assert.False(t, autoReview)

	cmd, _, _, err := resumeLaunchCmd(harness.CopilotName,
		resumeConvCopilot[:8], resumeConvCopilot, nil)
	require.NoError(t, err)
	assert.NotContains(t, cmd, "--allow-all-tools",
		"resuming a pre-catalog row must not silently auto-approve every tool")
	assert.NotContains(t, cmd, "--no-ask-user")
}

// The correction is Copilot-specific: another harness's blank-row fallback must
// be untouched. A blank Codex row still reconstructs as `untrusted` (the launch
// profile's own conservative legacy inference), not as Codex's `never` default
// and not as anything this branch introduced.
func TestResumeApprovalState_BlankRowFallbackUnchangedForOtherHarnesses(t *testing.T) {
	setupTestDB(t)
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "resume-codex-legacy", ConvID: resumeConvCodex, Harness: harness.CodexName,
	}))
	h, err := harness.Resolve(harness.CodexName)
	require.NoError(t, err)
	policy, _, err := resumeApprovalState(h, resumeConvCodex)
	require.NoError(t, err)
	assert.Equal(t, harness.ApprovalUntrusted, policy,
		"the Codex legacy path keeps its own conservative reconstruction")
}

// TCL-973 — what actually protects the implicit temp grant.
//
// Copilot grants its temp directory automatically, with no flag, so
// ValidateCopilotAddDirGrants has to know which directory that is. The obvious
// worry is a sandbox profile relocating TMPDIR: the gate would then inspect
// tclaude's ambient temp root while the pane was granted another, and a deny
// nested under the relocated root would sail through.
//
// It cannot happen, and this test pins WHY rather than asserting a refusal that
// could never fire. TMPDIR is a reserved profile environment name — along with
// HOME, PATH and the temp aliases — so a profile that tries to set it is
// refused at resolution time, long before any launch. The gate is nevertheless
// fed from the composed launch environment through the same resolver the
// Copilot sandbox baseline uses (session.CopilotLaunchTempDir), so if that
// reservation is ever relaxed the gate follows the pane instead of tclaude.
func TestResumeLaunchCmd_CopilotImplicitTempGrantCannotBeRelocatedByAProfile(t *testing.T) {
	_, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Global: &sandboxpolicy.Profile{
		Name:        "copilot-relocated-temp",
		Environment: []sandboxpolicy.EnvironmentEntry{{Name: "TMPDIR", Value: t.TempDir()}},
	}})
	require.Error(t, err,
		"a profile that could relocate TMPDIR would move the directory Copilot grants implicitly")
	assert.Contains(t, err.Error(), "reserved for launch or sandbox control")

	// The gate still reads the LAUNCH environment rather than tclaude's own, so
	// the seam is correct by construction and not by relying on that reservation.
	ambient := t.TempDir()
	relocated := t.TempDir()
	t.Setenv("TMPDIR", ambient)
	assert.Equal(t, relocated,
		session.CopilotLaunchTempDir(map[string]string{"TMPDIR": relocated}),
		"the launch environment wins over tclaude's ambient temp root")
	assert.Equal(t, filepath.Clean(ambient),
		session.CopilotLaunchTempDir(map[string]string{}),
		"a launch environment that names no temp directory falls back to the inherited one")
}

// A deny under the temp directory the launch actually uses must refuse the
// launch outside tclaude-layer, and be admitted under it — the implicit grant
// is not visible in the argv, so this is the only place it can be caught.
func TestResumeLaunchCmd_CopilotDenyUnderTheImplicitTempGrant(t *testing.T) {
	for _, tc := range []struct {
		name        string
		outerLayer  bool
		wantRefused bool
	}{
		{name: "outside the outer layer the launch is refused", wantRefused: true},
		{
			// Under tclaude-layer the outer sandbox enforces the deny whatever
			// Copilot's own path check believes.
			name:       "under tclaude-layer the launch is admitted",
			outerLayer: true, wantRefused: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupTestDB(t)
			launchTemp := t.TempDir()
			t.Setenv("TMPDIR", launchTemp)
			denied := filepath.Join(launchTemp, "secret")
			require.NoError(t, os.MkdirAll(denied, 0o755))

			effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Global: &sandboxpolicy.Profile{
				Name: "copilot-temp-deny",
				Filesystem: []sandboxpolicy.FilesystemGrant{
					{Path: denied, Access: sandboxpolicy.AccessDeny},
				},
			}})
			require.NoError(t, err)
			snapshot := sandboxpolicy.NewSnapshot(effective, nil)
			agentID, _, err := db.EnsureAgentForConv(resumeConvCopilot, "test")
			require.NoError(t, err)
			require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, &snapshot))

			implementation := "off"
			if tc.outerLayer {
				implementation = string(sandboxpolicy.ImplementationTclaudeLayer)
			}
			require.NoError(t, db.SaveSession(&db.SessionRow{
				ID: "resume-copilot-temp", ConvID: resumeConvCopilot,
				Harness: harness.CopilotName, SandboxMode: harness.CopilotSandboxOff,
				SandboxImplementation: implementation,
				ApprovalPolicy:        harness.CopilotApprovalAllowTools,
			}))

			_, _, _, err = resumeLaunchCmd(harness.CopilotName,
				resumeConvCopilot[:8], resumeConvCopilot, nil)
			if !tc.wantRefused {
				// Asserted as "not THIS refusal" rather than "no error":
				// building a real tclaude-layer launch needs working
				// unprivileged user namespaces, which a CI container or an
				// agent sandbox may not have, and a test that demanded them
				// would fail for a reason that has nothing to do with the gate.
				if err != nil {
					assert.NotContains(t, err.Error(), denied,
						"the outer sandbox enforces the deny, so this gate must not refuse")
					assert.NotContains(t, err.Error(), "automatically, with no flag")
				}
				return
			}
			require.Error(t, err,
				"the deny sits under the temp root Copilot grants with no flag")
			assert.Contains(t, err.Error(), denied)
			assert.Contains(t, err.Error(), "automatically, with no flag",
				"the refusal must say where the grant came from — nothing in the argv names it")
		})
	}
}
