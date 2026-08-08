package session

import (
	"io"
	"strings"
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
			//
			// NOT a reachable runNew state, and the arm says so rather than implying a
			// contract: runNew cannot deliver drive=true for Claude Code, because
			// ResolveCopilotAPI errors first. What it pins is that the DEFENSIVE branch
			// is a pass-through — if a future caller reaches this function some other
			// way, it must not refuse a launch for a Copilot-only reason. Flagged by
			// cold review as documenting an unreachable state; kept, with the reason.
			name:    "a non-Copilot harness is left alone (defensive branch, unreachable from runNew)",
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
			decision, err := resolveCopilotAPIDriveForLaunch(copilotAPIDriveRequest{
				Harness:         h,
				Drive:           tc.drive,
				CarriedFromConv: carriedFrom,
				ManagedLaunch:   tc.managedLaunch,
				Port:            tc.port,
				SendKeys:        tc.sendKeys,
			})

			drive, notice := decision.Drive, decision.Notice
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

	_, err = resolveCopilotAPIDriveForLaunch(copilotAPIDriveRequest{
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

// The WIRING, driven behaviourally. This is the test whose absence let the cold
// review land three separate beats: the decision table calls the gate directly, and
// runNew launches panes so nothing exercised the path between them. Deleting
// `params.copilotAPIDriveDropped = ...`, wrapping the gate in `if params.Resume !=
// ""`, and discarding the refusal into a trailing blank each left the whole package
// green. An AST guard asserting those lines exist would only have restated them;
// what was missing was a function that takes params and can be called from a test.
//
// So the applier owns every consequence and this asserts all of them together:
// the drive params runs with, the drop marker the posture write reads, the port, and
// what the operator is told.
func TestApplyCopilotAPIDriveDecisionWiresEveryConsequence(t *testing.T) {
	for _, tc := range []struct {
		name          string
		harness       string
		drive         bool
		carried       bool
		managed       bool
		managedLaunch bool
		port          int
		sendKeys      bool
		resume        string
		joinGroup     string
		wantErr       string
		wantDrive     bool
		wantDropped   bool
		wantPort      int
		wantNotice    []string
	}{
		{
			// The path that works, and the assertion that it is untouched.
			name:    "agentd's launch keeps drive, port and silence",
			harness: harness.CopilotName, drive: true, managedLaunch: true, port: 41234,
			wantDrive: true, wantDropped: false, wantPort: 41234,
		},
		{
			// The defect. Note wantDropped is false: a REFUSED launch never reaches the
			// posture write, and marking it dropped would be a claim about a launch that
			// did not happen.
			name:    "a hand-typed drive is refused",
			harness: harness.CopilotName, drive: true,
			wantErr: "copilot_api_drive_needs_agentd", wantPort: 0,
		},
		{
			// The consequence the review found: dropping the drive while leaving the
			// port set manufactured a port-without-drive combination the operator never
			// typed, so the launch died on an unrelated error one line later — AFTER
			// being told it would proceed on send-keys.
			name:    "a dropped carried drive clears the port it no longer needs",
			harness: harness.CopilotName, drive: true, carried: true, port: 4599,
			wantDrive: false, wantDropped: true, wantPort: 0,
			wantNotice: []string{"send-keys", "left untouched"},
		},
		{
			// The record half: dropped must be set, or copilotAPIPostureToRecord asserts
			// false over the conversation's drive. This is the assignment whose deletion
			// left the package green.
			name:    "a dropped drive is marked so the posture write abstains",
			harness: harness.CopilotName, drive: true, carried: true, managed: true, sendKeys: true,
			wantDrive: false, wantDropped: true, wantPort: 0,
			wantNotice: []string{"HOLD"},
		},
		{
			// A launch that never asked for the drive must NOT be marked dropped: it has
			// dropped nothing, and it must still be able to record its own false.
			name:    "a send-keys launch is untouched and not marked dropped",
			harness: harness.CopilotName, drive: false,
			wantDrive: false, wantDropped: false, wantPort: 0,
		},
		{
			// The resume arm's remedies, which used to be fresh-launch advice that fails
			// twice over on this invocation.
			name:    "an explicit drive on a resume names remedies that work from here",
			harness: harness.CopilotName, drive: true, resume: driveLaunchConv,
			wantErr: "tclaude agent resume " + driveLaunchConv,
		},
		{
			// The join-group wording. This path starts NO local pane — RunJoinGroup POSTs
			// to the daemon — so a refusal claiming "the pane would come up with the
			// endpoint listening" would describe something that does not happen. The
			// refusal stands (the request carries no drive field, so the flag is discarded
			// in transit), but it has to say the true thing. Found by cold review.
			name:    "a join-group refusal describes the handoff, not a pane",
			harness: harness.CopilotName, drive: true, joinGroup: "reviewers",
			wantErr: "discarded in transit",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			carryoverTestHome(t)
			seedDriveLaunchConv(t, driveLaunchConv, tc.managed)
			h, err := harness.Resolve(tc.harness)
			require.NoError(t, err)

			params := &NewParams{
				CopilotAPI:     tc.drive,
				CopilotAPIPort: tc.port,
				ManagedLaunch:  tc.managedLaunch,
				SendKeys:       tc.sendKeys,
				Resume:         tc.resume,
				JoinGroup:      tc.joinGroup,
			}
			if tc.carried {
				params.copilotAPIDriveCarriedFrom = driveLaunchConv
			}
			var notices strings.Builder

			err = applyCopilotAPIDriveDecision(params, h, &notices)

			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.False(t, params.CopilotAPI,
					"a refused launch must not be left holding the drive")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantDrive, params.CopilotAPI, "the drive params runs with")
			assert.Equal(t, tc.wantDropped, params.copilotAPIDriveDropped,
				"the marker copilotAPIPostureToRecord reads: wrong here and the "+
					"conversation's recorded drive is authored over")
			assert.Equal(t, tc.wantPort, params.CopilotAPIPort, "the port")
			if len(tc.wantNotice) == 0 {
				assert.Empty(t, notices.String(), "this launch must not be narrated")
				return
			}
			for _, want := range tc.wantNotice {
				assert.Contains(t, notices.String(), want)
			}
		})
	}
}

// The two halves must agree end to end: the applier sets the marker, and the posture
// helper turns it into an abstention. Asserted together because each was tested in
// isolation while the line joining them was deletable without a failure.
func TestADroppedDriveReachesThePostureWriteAsNil(t *testing.T) {
	carryoverTestHome(t)
	seedDriveLaunchConv(t, driveLaunchConv, false)
	params := &NewParams{CopilotAPI: true}
	params.copilotAPIDriveCarriedFrom = driveLaunchConv

	require.NoError(t, applyCopilotAPIDriveDecision(params, copilotLaunchHarness(t), io.Discard))
	require.True(t, params.copilotAPIDriveDropped, "precondition: the drive was dropped")
	assert.Nil(t, copilotAPIPostureToRecord(params),
		"a dropped drive must reach RecordLaunchPosture as nil, or the launch un-chooses "+
			"the conversation's drive on the authority of having failed to provide it")
}
