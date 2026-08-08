package conv

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// TCL-1076 — `tclaude conv resume` and the watch-mode resume used to launch an
// API-driven Copilot conversation on send-keys silently, and to record
// `copilot_api=false` for that launch: a true statement about the pane and a
// false statement about the conversation, over the record that decides whether
// agentd's messages travel over RPC or are typed into the pane.

const driveConv = "7a1e5c40-2222-4333-8444-555566667777"
const driveConvOther = "7a1e5c40-3333-4444-8555-666677778888"

func copilotHarness(t *testing.T) *harness.Harness {
	t.Helper()
	h, err := harness.Resolve(harness.CopilotName)
	require.NoError(t, err)
	return h
}

// seedDriveConv writes the session row a launched Copilot pane leaves behind,
// then records the drive the way a real launch does — through the production
// writers, so the test reads what agentd's routing would read rather than a
// hand-built profile.
func seedDriveConv(t *testing.T, convID string, api bool, managed bool) {
	t.Helper()
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: convID, ConvID: convID, Harness: harness.CopilotName,
		ApprovalPolicy: harness.CopilotApprovalInherit,
	}))
	require.NoError(t, db.SetConversationCopilotAPI(
		convID, harness.CopilotName, t.TempDir(), api, nil))
	if managed {
		_, _, err := db.EnsureAgentForConv(convID, "test")
		require.NoError(t, err)
	}
}

// The gate's whole decision table in one place, because the arms answer
// DIFFERENT questions and only their contrast shows the gate is attached to
// anything: three arms assert a refusal or a disclosure, and three assert
// silence. The silent arms are absences — a gate that returned ("", nil)
// unconditionally would satisfy them — so they are only meaningful next to the
// arms that must not be silent, in the same table, over the same seeding.
func TestResumeCopilotDriveGate(t *testing.T) {
	for _, tc := range []struct {
		name        string
		harness     string
		recordDrive bool // seed a record at all
		api         bool
		managed     bool
		sendKeys    bool
		wantRefusal []string
		wantNotice  []string
		wantSilent  bool
	}{
		{
			// The defect's headline case. A managed agent resumed here would be
			// deaf: agentd keeps routing to a channel this pane does not have.
			name:    "managed API conversation is refused and names both ways out",
			harness: harness.CopilotName, recordDrive: true, api: true, managed: true,
			wantRefusal: []string{
				"copilot_api_drive_unavailable_outside_agentd",
				"tclaude agent resume " + driveConv,
				resumeOverrideHintCLI,
			},
		},
		{
			// The override is first-class: `tclaude agent resume` needs a running
			// daemon, and a human must not be walled out of a pane at exactly the
			// moment agentd is what is broken.
			name:    "managed API conversation proceeds under --send-keys, saying what holds",
			harness: harness.CopilotName, recordDrive: true, api: true, managed: true,
			sendKeys: true,
			wantNotice: []string{
				"chose the Copilot API drive", "send-keys",
				"HOLD", "tclaude agent resume " + driveConv,
			},
		},
		{
			// Nothing routes an unmanaged conversation, so refusing would block a
			// human to protect nothing.
			name:    "unmanaged API conversation proceeds with a disclosure",
			harness: harness.CopilotName, recordDrive: true, api: true,
			wantNotice: []string{"chose the Copilot API drive", "left untouched"},
		},
		{
			name:    "a Copilot conversation that chose send-keys is untouched",
			harness: harness.CopilotName, recordDrive: true, api: false, managed: true,
			wantSilent: true,
		},
		{
			name:    "a Copilot conversation with no recorded drive is untouched",
			harness: harness.CopilotName, managed: true,
			wantSilent: true,
		},
		{
			name:    "a non-Copilot harness is untouched even with a foreign record",
			harness: harness.DefaultName, recordDrive: true, api: true, managed: true,
			wantSilent: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupTestDB(t)
			if tc.recordDrive {
				seedDriveConv(t, driveConv, tc.api, tc.managed)
			} else {
				require.NoError(t, db.SaveSession(&db.SessionRow{
					ID: driveConv, ConvID: driveConv, Harness: harness.CopilotName,
					ApprovalPolicy: harness.CopilotApprovalInherit,
				}))
				if tc.managed {
					_, _, err := db.EnsureAgentForConv(driveConv, "test")
					require.NoError(t, err)
				}
			}
			h, err := harness.Resolve(tc.harness)
			require.NoError(t, err)

			notice, err := resumeCopilotDriveGate(h, driveConv, tc.sendKeys, resumeOverrideHintCLI)

			if len(tc.wantRefusal) > 0 {
				require.Error(t, err, "a managed API conversation must not launch on send-keys by default")
				for _, want := range tc.wantRefusal {
					assert.Contains(t, err.Error(), want,
						"the refusal must name the reason and the way out, not merely reject")
				}
				assert.Empty(t, notice, "a refusal is not also a disclosure")
				return
			}
			require.NoError(t, err)
			if tc.wantSilent {
				assert.Empty(t, notice,
					"a conversation that did not choose the API drive must take the send-keys path unchanged")
				return
			}
			for _, want := range tc.wantNotice {
				assert.Contains(t, notice, want)
			}
		})
	}
}

// "Managed" must mean what ROUTING means by it, not merely "an actor row
// exists". Both shapes here were reproduced by a cold review against the first
// version of this gate, which used db.GetAgentByConv:
//
//   - a SUPERSEDED generation — the conv an agent had before reincarnating —
//     still resolves to its actor through agent_conversations, but nothing
//     delivers to it;
//   - a RETIRED actor likewise.
//
// Refusing either was wrong twice over: the refusal asserts "agentd routes its
// messages over that channel", which is false for both, and it points the human
// at `tclaude agent resume <thatConv>`, which redirects forward to the chain head
// — so following the advice would resume a DIFFERENT conversation while the one
// they asked for stays unresumable without --send-keys.
//
// The notice arm is right for both, by this gate's own stated rule: nothing
// routes them, so their recorded drive buys them nothing.
func TestResumeCopilotDriveGateDiscloseRatherThanRefuseWhenNothingRoutes(t *testing.T) {
	const successor = "7a1e5c40-4444-4555-8666-777788889999"

	for _, tc := range []struct {
		name  string
		setup func(t *testing.T) string // returns the conv to resume
	}{
		{
			name: "a superseded generation is not the live agent",
			setup: func(t *testing.T) string {
				seedDriveConv(t, driveConv, true, true)
				agentID, err := db.AgentIDForConv(driveConv)
				require.NoError(t, err)
				require.NotEmpty(t, agentID)
				require.NoError(t, db.SaveSession(&db.SessionRow{
					ID: successor, ConvID: successor, Harness: harness.CopilotName,
					ApprovalPolicy: harness.CopilotApprovalInherit,
				}))
				require.NoError(t, db.LinkConvToAgent(successor, agentID, "", "test"))
				_, err = db.SetAgentCurrentConv(agentID, driveConv, successor)
				require.NoError(t, err)
				return driveConv
			},
		},
		{
			name: "a retired actor routes nothing",
			setup: func(t *testing.T) string {
				seedDriveConv(t, driveConv, true, true)
				agentID, err := db.AgentIDForConv(driveConv)
				require.NoError(t, err)
				_, err = db.RetireAgentByID(agentID, "test", "cold-review probe")
				require.NoError(t, err)
				return driveConv
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupTestDB(t)
			convID := tc.setup(t)

			// Positive control for the seeding, not decoration: if the drive were not
			// recorded on this conversation the gate would be silent for a reason that
			// has nothing to do with liveness, and the assertions below would pass
			// over a test that exercised nothing.
			api, err := db.CopilotAPIForConv(convID)
			require.NoError(t, err)
			require.True(t, api, "the conversation under test must still read as API-driven")

			notice, err := resumeCopilotDriveGate(
				copilotHarness(t), convID, false, resumeOverrideHintCLI)
			require.NoError(t, err,
				"nothing routes this conversation, so refusing protects nothing and blocks a human")
			assert.Contains(t, notice, "chose the Copilot API drive",
				"the downgrade must still be disclosed")
			assert.NotContains(t, notice, "HOLD",
				"promising held mail for a conversation nothing delivers to is a false statement")
		})
	}
}

// And the live case still refuses, in the same file, so the fix above cannot be
// mistaken for "the gate stopped refusing". A live managed agent — current
// generation, not retired — is the one shape that must not launch here.
func TestResumeCopilotDriveGateStillRefusesTheLiveGeneration(t *testing.T) {
	setupTestDB(t)
	seedDriveConv(t, driveConv, true, true)

	live, err := db.IsLiveAgentConv(driveConv)
	require.NoError(t, err)
	require.True(t, live, "positive control: this conv must be the actor's live generation")

	_, err = resumeCopilotDriveGate(copilotHarness(t), driveConv, false, resumeOverrideHintCLI)
	require.Error(t, err, "a live managed API agent must still be refused")
	assert.Contains(t, err.Error(), "copilot_api_drive_unavailable_outside_agentd")
}

// A read failure must refuse rather than fall through to a silent send-keys
// launch: an unreadable record is not evidence that the conversation chose
// keystrokes. Reproduced by making the actor lookup fail — the discriminator
// between the refusal and the disclosure — while the drive itself reads true.
func TestResumeCopilotDriveGateRefusesOnAnUnreadableRecord(t *testing.T) {
	setupTestDB(t)
	seedDriveConv(t, driveConv, true, false)
	d, err := db.Open()
	require.NoError(t, err)
	_, err = d.Exec(`DROP TABLE agent_conversations`)
	require.NoError(t, err)

	notice, err := resumeCopilotDriveGate(copilotHarness(t), driveConv, false, resumeOverrideHintCLI)
	require.Error(t, err, "an unreadable managed-agent lookup must refuse, not guess unmanaged")
	assert.Contains(t, err.Error(), driveConv)
	assert.Empty(t, notice)
}

// The record-preservation half, exercised through the production write path
// rather than by inspecting the literal: seed a conversation that chose the API
// drive with a pinned meter cap, replay what a plain-CLI resume does to the
// durable record (a fresh session row, then RecordLaunchPosture with the posture
// resumeLaunchPosture builds), and read the record back the way agentd's routing
// reads it.
//
// The mutation that proves it is attached: give resumeLaunchPosture's
// ContextWindowMax and CopilotAPI a non-nil zero — the pre-TCL-1076 behaviour —
// and this goes red on both.
func TestPlainCLIResumeLeavesTheRecordedCopilotDriveAlone(t *testing.T) {
	setupTestDB(t)
	cwd := t.TempDir()
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: driveConv, ConvID: driveConv, Cwd: cwd, Harness: harness.CopilotName,
		ApprovalPolicy: harness.CopilotApprovalInherit,
	}))
	require.NoError(t, db.SetConversationCopilotAPI(driveConv, harness.CopilotName, cwd, true, nil))
	require.NoError(t, db.SetSessionConfiguredContextWindowMax(driveConv, 128000))

	before, err := db.CopilotAPIForConv(driveConv)
	require.NoError(t, err)
	require.True(t, before, "positive control: the seeded conversation must read as API-driven "+
		"before the resume, or this test proves nothing about preserving it")

	// The resumed generation: a new session row (a later launch of the same
	// conversation), then the posture write both plain-CLI resume surfaces do.
	const resumedSession = "resumed-generation"
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: resumedSession, ConvID: driveConv, Cwd: cwd, Harness: harness.CopilotName,
		ApprovalPolicy: harness.CopilotApprovalInherit,
	}))
	session.RecordLaunchPosture(resumedSession, copilotHarness(t),
		resumeLaunchPosture(false, nil, "", false))

	api, err := db.CopilotAPIForConv(driveConv)
	require.NoError(t, err)
	assert.True(t, api,
		"a send-keys resume must not un-choose the conversation's drive: this record routes "+
			"every later message, and a false here is what TCL-1076 was")

	posture, err := db.RecordedLaunchPostureForConv(driveConv)
	require.NoError(t, err)
	require.NotNil(t, posture)
	require.NotNil(t, posture.ConfiguredContextWindowMax,
		"the configured meter denominator must survive a plain-CLI resume")
	assert.Equal(t, int64(128000), *posture.ConfiguredContextWindowMax)
}

// THIS TEST PINS CURRENT BEHAVIOUR, AND THE BEHAVIOUR IS WRONG. TCL-1085 fixes
// it; invert the assertion then and delete this header. Said in capitals because
// a test that pins a known defect is only safe while it says so — otherwise the
// next reader takes it for a specification and defends the bug.
//
// The Codex service tier rides the same literal as the two fields TCL-1076 fixed
// and is NOT fixed here, and this test exists so that stays a measured statement
// rather than an impression.
//
// A plain-CLI resume still loses a pinned tier, and the mechanism is not the one
// TCL-1076 fixed: FastMode's carry-forward in projectSessionRelaunchProfilesTx
// is gated on the source generation, so the fresh session row drops it before
// RecordLaunchPosture is reached. Passing nil instead of "" would therefore
// change nothing except how the code reads — the loss would look preserved. It
// needs a carry plus a launch that renders --fast-mode, which this surface does
// not do, so it is filed rather than half-fixed.
//
// If this test starts failing because the tier survives, that is the fix
// landing: assert preservation here and delete this comment.
func TestPlainCLIResumeStillLosesAPinnedCodexTier(t *testing.T) {
	setupTestDB(t)
	cwd := t.TempDir()
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: driveConvOther, ConvID: driveConvOther, Cwd: cwd, Harness: harness.CodexName,
	}))
	require.NoError(t, db.SetSessionFastMode(driveConvOther, "on"))

	before, err := db.RecordedLaunchPostureForConv(driveConvOther)
	require.NoError(t, err)
	require.NotNil(t, before)
	require.NotNil(t, before.FastMode, "positive control: the pin must exist before the resume")
	require.True(t, *before.FastMode)

	codex, err := harness.Resolve(harness.CodexName)
	require.NoError(t, err)
	const resumedSession = "resumed-codex-generation"
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: resumedSession, ConvID: driveConvOther, Cwd: cwd, Harness: harness.CodexName,
	}))
	session.RecordLaunchPosture(resumedSession, codex,
		resumeLaunchPosture(false, nil, "", false))

	after, err := db.RecordedLaunchPostureForConv(driveConvOther)
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.Nil(t, after.FastMode,
		"documented gap, not a fix (TCL-1085): the pinned Codex tier is still lost by a "+
			"plain-CLI resume, through the generation-gated projection rather than the "+
			"posture write. If this now fails, the fix has landed — assert preservation")
}

// The other direction, and the reason RecordLaunchPosture's skip is keyed on nil
// rather than on the caller's package: a surface that DID resolve the drive must
// still be able to record every value including false, or turning the drive off
// would be unrepresentable.
func TestARecordedFalseStillOverwritesTheDrive(t *testing.T) {
	setupTestDB(t)
	cwd := t.TempDir()
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: driveConv, ConvID: driveConv, Cwd: cwd, Harness: harness.CopilotName,
		ApprovalPolicy: harness.CopilotApprovalInherit,
	}))
	require.NoError(t, db.SetConversationCopilotAPI(driveConv, harness.CopilotName, cwd, true, nil))

	off := false
	zero := int64(0)
	session.RecordLaunchPosture(driveConv, copilotHarness(t), session.LaunchPosture{
		ContextWindowMax: &zero,
		CopilotAPI:       &off,
	})

	api, err := db.CopilotAPIForConv(driveConv)
	require.NoError(t, err)
	assert.False(t, api,
		"an explicit false from a surface that resolved the drive must still land, "+
			"or `session new -r --copilot-api=false` could never turn it off")
}
