package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// TCL-1084 — `tclaude session new` is the surface that HONOURS the Copilot API
// drive: it resolved --copilot-api for real, allocated a port when none was
// supplied, and rendered `copilot --ui-server`. Correct under agentd, which
// allocates the port and performs the bootstrap. Typed by a human with no daemon
// in the loop it produced a pane with an unauthenticated loopback endpoint that
// nothing would ever dial — and for a live managed agent, a deaf agent whose pane
// looks healthy.

const driveLaunchConv = "9c2f6a10-1111-4222-8333-444455556666"

func copilotLaunchHarness(t *testing.T) *harness.Harness {
	t.Helper()
	h, err := harness.Resolve(harness.CopilotName)
	require.NoError(t, err)
	return h
}

// seedDriveLaunchConv writes the session row a launched Copilot pane leaves
// behind and, when managed, the actor row that makes agentd route its mail.
// Through the production writers, so the predicate under test reads what the
// router reads.
func seedDriveLaunchConv(t *testing.T, convID string, managed bool) {
	t.Helper()
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: convID, ConvID: convID, Harness: harness.CopilotName,
		ApprovalPolicy: harness.CopilotApprovalInherit,
	}))
	if managed {
		_, _, err := db.EnsureAgentForConv(convID, "test")
		require.NoError(t, err)
	}
}

// The whole decision table in one place, because the arms answer DIFFERENT
// questions and only their contrast shows the gate is attached to anything. Three
// arms assert a refusal or a disclosure; three assert that the launch is left
// completely alone. The silent arms are ABSENCES — a gate that returned
// (drive, "", nil) unconditionally would satisfy every one of them — so they are
// only meaningful sitting next to the arms that must not be silent, over the same
// seeding, in the same table. That contrast is their positive control.
func TestResolveCopilotAPIDriveForLaunch(t *testing.T) {
	for _, tc := range []struct {
		name          string
		harness       string
		drive         bool
		carried       bool
		managed       bool
		managedLaunch bool
		port          int
		sendKeys      bool
		wantDrive     bool
		wantRefusal   []string
		wantNotice    []string
		wantSilent    bool
	}{
		{
			// The agentd case, which is the one that works and must come out
			// untouched: the daemon declared itself with --managed-launch and handed
			// over the port it allocated before forking.
			name:    "agentd's own launch keeps the drive and says nothing",
			harness: harness.CopilotName, drive: true,
			managedLaunch: true, port: 41234,
			wantDrive: true, wantSilent: true,
		},
		{
			// Assert-and-refuse. An explicit flag is the operator asserting
			// something; silently downgrading an assertion is the failure TCL-1076
			// exists to stop.
			name:    "a hand-typed --copilot-api is refused, naming what is missing",
			harness: harness.CopilotName, drive: true,
			wantDrive: false,
			wantRefusal: []string{
				"copilot_api_drive_needs_agentd",
				"tclaude agent spawn",
				"without --copilot-api",
			},
		},
		{
			// --managed-launch alone is not enough. This is the case
			// appendCopilotAPIPortFlag's comment in agentd claimed was already
			// refused while the code allocated a port instead.
			name:    "a managed launch that lost its port is still refused",
			harness: harness.CopilotName, drive: true,
			managedLaunch: true, port: 0,
			wantDrive:   false,
			wantRefusal: []string{"copilot_api_drive_needs_agentd"},
		},
		{
			// The compounding case: the posture reads API, agentd routes there, and
			// since TCL-1058 delivery HOLDS rather than falling back to keystrokes.
			name:    "a carried drive on a live managed agent is refused, naming both ways out",
			harness: harness.CopilotName, drive: true, carried: true, managed: true,
			wantDrive: false,
			wantRefusal: []string{
				"copilot_api_drive_needs_agentd",
				"tclaude agent resume " + driveLaunchConv,
				"--send-keys",
				"HOLDS",
			},
		},
		{
			// The override is first-class, spelled as TCL-1076 spelled it: `tclaude
			// agent resume` needs a running daemon, so a refusal routed only through
			// it would wall a human out exactly when agentd is what is broken.
			name:    "--send-keys proceeds on a live managed agent and says what holds",
			harness: harness.CopilotName, drive: true, carried: true, managed: true,
			sendKeys:  true,
			wantDrive: false,
			wantNotice: []string{
				"left untouched",
				"HOLD",
				"tclaude agent resume " + driveLaunchConv,
			},
		},
		{
			// Infer-and-disclose. Nothing routes an unmanaged conversation, so
			// refusing would block a human to protect nothing.
			name:    "a carried drive on an unmanaged conversation is disclosed, not refused",
			harness: harness.CopilotName, drive: true, carried: true,
			wantDrive: false,
			wantNotice: []string{
				"send-keys",
				"left untouched",
			},
		},
		{
			// Inertness, asserted rather than asserted-about: a launch that never
			// asked for the drive reaches no decision at all. This is every launch
			// today unless someone opted in.
			name:    "a launch without the drive is left alone",
			harness: harness.CopilotName, drive: false,
			wantDrive: false, wantSilent: true,
		},
		{
			// Same for a conversation that recorded send-keys and carried it: the
			// carry classifies as a no-op, so nothing reaches here.
			name:    "an unmanaged launch without the drive is left alone even on a resume",
			harness: harness.CopilotName, drive: false, carried: true,
			wantDrive: false, wantSilent: true,
		},
		{
			// This gate is about the channel, not the harness. A harness with no
			// API-backed mode has already been refused upstream by
			// harness.ResolveCopilotAPI, and refusing again here would name a reason
			// that does not apply.
			name:    "a non-Copilot harness is left alone",
			harness: harness.DefaultName, drive: true,
			wantDrive: true, wantSilent: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			carryoverTestHome(t)
			seedDriveLaunchConv(t, driveLaunchConv, tc.managed)
			h, err := harness.Resolve(tc.harness)
			require.NoError(t, err)

			carriedFrom := ""
			if tc.carried {
				carriedFrom = driveLaunchConv
			}
			drive, notice, err := resolveCopilotAPIDriveForLaunch(copilotAPIDriveRequest{
				Harness:         h,
				Drive:           tc.drive,
				CarriedFromConv: carriedFrom,
				ManagedLaunch:   tc.managedLaunch,
				Port:            tc.port,
				SendKeys:        tc.sendKeys,
			})

			if len(tc.wantRefusal) > 0 {
				require.Error(t, err, "this launch must be refused, not started")
				for _, want := range tc.wantRefusal {
					assert.Contains(t, err.Error(), want,
						"the refusal must say this, or the operator cannot act on it")
				}
				assert.False(t, drive, "a refused launch must not carry the drive")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantDrive, drive)
			if tc.wantSilent {
				assert.Empty(t, notice,
					"this launch is unaffected and must not be narrated at all")
				return
			}
			require.NotEmpty(t, notice, "dropping the drive silently is the defect")
			for _, want := range tc.wantNotice {
				assert.Contains(t, notice, want)
			}
		})
	}
}

// Fail closed. An unreadable actor row is not evidence that nothing routes this
// conversation, and proceeding on that assumption is what produces the deaf
// agent. Reproduced by dropping the table the predicate reads.
func TestResolveCopilotAPIDriveForLaunchRefusesOnAnUnreadableActorRow(t *testing.T) {
	carryoverTestHome(t)
	seedDriveLaunchConv(t, driveLaunchConv, true)

	d, err := db.Open()
	require.NoError(t, err)
	_, err = d.Exec(`DROP TABLE agent_conversations`)
	require.NoError(t, err)

	_, _, err = resolveCopilotAPIDriveForLaunch(copilotAPIDriveRequest{
		Harness:         copilotLaunchHarness(t),
		Drive:           true,
		CarriedFromConv: driveLaunchConv,
	})
	require.Error(t, err,
		"an unreadable actor row must refuse rather than assume nothing routes this conversation")
	assert.Contains(t, err.Error(), driveLaunchConv,
		"the error must name the conversation it could not resolve")
}

// The predicate itself, driven over its four combinations. Both halves earn their
// place and this is where that is checked rather than argued: a port alone is a
// side effect of the daemon's involvement (and is typable by hand), and
// --managed-launch alone leaves no number for anything to wait on.
func TestCopilotAPIDriveIsDaemonOwnedNeedsBothHalves(t *testing.T) {
	assert.True(t, copilotAPIDriveIsDaemonOwned(true, 41234),
		"agentd declares itself AND hands over the port it allocated")
	assert.False(t, copilotAPIDriveIsDaemonOwned(false, 41234),
		"a port without the declaration is a number a human can type")
	assert.False(t, copilotAPIDriveIsDaemonOwned(true, 0),
		"a declaration without a port leaves nothing for the daemon to wait on")
	assert.False(t, copilotAPIDriveIsDaemonOwned(false, 0),
		"the ordinary send-keys launch")
}

// The central property, through the production write path: a launch that could
// not honour a carried drive must leave the conversation's recorded choice alone.
// Read back with the same accessor agentd's ROUTING reads, not a hand-built
// profile, because the record is a routing decision since TCL-1058 and a test that
// reads it differently proves something about a different value.
//
// Carries a positive control by construction: the same table asserts the opposite
// direction, so a change that made the record simply unwritable would fail the
// second arm rather than passing both.
func TestSessionNewLeavesACarriedCopilotDriveAloneWhenItCannotHonourIt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		dropped bool
		ran     bool
		want    bool
	}{
		{
			// The defect's record half. A drive dropped for want of a daemon is a
			// fact about this launch, not a decision about the conversation.
			name: "a dropped drive is not authored over", dropped: true, ran: false, want: true,
		},
		{
			// The positive control, and the reason the skip cannot be read as "the
			// drive is now unwritable": a launch that RESOLVED the drive to false may
			// still record it, or turning the drive off would be unrepresentable.
			name: "a resolved false still overwrites the record", dropped: false, ran: false, want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			carryoverTestHome(t)
			h := copilotLaunchHarness(t)
			seedDriveLaunchConv(t, driveLaunchConv, false)
			require.NoError(t, db.SetConversationCopilotAPI(
				driveLaunchConv, harness.CopilotName, t.TempDir(), true))

			api, err := db.CopilotAPIForConv(driveLaunchConv)
			require.NoError(t, err)
			require.True(t, api,
				"positive control: the conversation must read API-driven BEFORE the launch, "+
					"or the assertion below passes for the wrong reason")

			params := &NewParams{CopilotAPI: tc.ran}
			params.copilotAPIDriveDropped = tc.dropped
			RecordLaunchPosture(driveLaunchConv, h, LaunchPosture{
				CopilotAPI: copilotAPIPostureToRecord(params),
			})

			api, err = db.CopilotAPIForConv(driveLaunchConv)
			require.NoError(t, err)
			assert.Equal(t, tc.want, api)
		})
	}
}

// The marker the whole policy turns on, driven over BOTH halves that have to be
// right: what the real carryover row puts in the `carried` list for a recorded
// drive, and what the assignment then does with it.
//
// It is deliberately not driven through applyRecordedLaunchPosture. That path
// needs a Copilot conversation its ConvStore can resolve — a workspace.yaml
// fixture, not a conv_index row — and a version of this test that seeded the
// Claude-indexed resolver instead PASSED ITS NEGATIVE ARM FOR THE WRONG REASON:
// the conversation never resolved, so nothing was carried at all and "no marker"
// held trivially. Which half is uncovered, stated rather than left implicit: that
// applyRecordedLaunchPosture calls noteCarriedCopilotDrive at all. The source
// guard in copilot_api_gate_guard_test.go covers exactly that.
func TestCarryingTheDriveRecordsWhichConversationItCameFrom(t *testing.T) {
	for _, tc := range []struct {
		name       string
		recorded   bool
		wantCarry  carryOutcome
		wantMarker bool
	}{
		{
			name:     "a recorded drive is carried and names its conversation",
			recorded: true, wantCarry: carryApplied, wantMarker: true,
		},
		{
			// The one that matters for inertness: an ordinary send-keys conversation
			// must not enter the carried-drive arm. A recorded false is a carried
			// NO-OP, so it never reaches the list the marker is keyed off.
			name:     "a recorded send-keys posture sets no marker",
			recorded: false, wantCarry: carryAppliedDefault, wantMarker: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := copilotLaunchHarness(t)
			recorded := tc.recorded
			posture := &db.AgentRelaunchProfile{
				Version: db.RelaunchProfileVersion, CopilotAPI: &recorded,
			}
			params := &NewParams{}

			// The real carryover row, so what lands in `carried` is what production
			// puts there rather than what this test believes it puts there.
			var row *launchCarryoverField
			for i := range launchCarryoverFields {
				if launchCarryoverFields[i].flag == "copilot-api" {
					row = &launchCarryoverFields[i]
				}
			}
			require.NotNil(t, row, "the copilot-api carryover row must exist")

			outcome := row.classify(row.carry(h, posture, params))
			require.Equal(t, tc.wantCarry, outcome)
			assert.Equal(t, tc.recorded, params.CopilotAPI, "the carry itself must have happened")

			carried := []string{}
			if outcome == carryApplied {
				carried = append(carried, "--"+row.flag)
			}
			noteCarriedCopilotDrive(params, carried, driveLaunchConv)

			if tc.wantMarker {
				assert.Equal(t, driveLaunchConv, params.copilotAPIDriveCarriedFrom,
					"the gate needs the conversation the drive came from to decide "+
						"refuse-versus-disclose")
				return
			}
			assert.Empty(t, params.copilotAPIDriveCarriedFrom,
				"a resume that carried no drive must not enter the carried-drive arm")
		})
	}
}
