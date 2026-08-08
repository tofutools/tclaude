package agentd_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// TCL-1082's decisive case, and the reason the command exists at all: a rollback
// path for an unverified mechanism that itself requires the mechanism owner to be
// healthy is the rollback path failing in the case it exists for.
//
// So `set-drive send-keys` works with agentd down, and `set-drive api` does not.
// The asymmetry is asserted in BOTH directions here, because a fallback that
// quietly also escalates would hand the daemon-down path the one power it must
// not have.

// withDaemonDown makes DaemonAvailable report false for one test, which is the
// production seam the command branches on.
func withDaemonDown(t *testing.T) {
	t.Helper()
	previous := agent.DaemonAvailableImpl
	agent.DaemonAvailableImpl = func() bool { return false }
	t.Cleanup(func() { agent.DaemonAvailableImpl = previous })
}

// runSetDriveCLIDirect drives the command with no daemon reachable.
func runSetDriveCLIDirect(t *testing.T, selector, drive string) (string, string, int) {
	t.Helper()
	withDaemonDown(t)
	stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
	rc := agent.RunSetDrive(&agent.SetDriveParams{Agent: selector, Drive: drive}, stdout, stderr)
	return stdout.String(), stderr.String(), rc
}

// TestSetDriveDaemonDown_DeEscalationStillWorks is the property the whole
// command is shaped around.
func TestSetDriveDaemonDown_DeEscalationStillWorks(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")
	haveCopilotAPIProfile(t, f)

	resp, _ := runSpawnCLI(t, f, &agent.SpawnParams{
		Group: "crew", Name: "copilot-worker", Harness: harness.CopilotName,
		CopilotAPI: true, Profile: copilotAPIProfile,
	})
	conv := resp.ConvID
	before, err := db.CopilotDriveTargetForConv(conv)
	require.NoError(t, err)
	require.True(t, before.Value, "the scenario needs an agent recorded ON the drive")

	stdout, stderr, rc := runSetDriveCLIDirect(t, conv, "send-keys")
	require.Equalf(t, 0, rc, "de-escalation must not need the daemon: %s", stderr)

	after, err := db.CopilotDriveTargetForConv(conv)
	require.NoError(t, err)
	assert.False(t, after.Value, "the record must be off after a daemon-down rollback")
	assert.Equal(t, db.CopilotDriveRecordAgentProfile, after.Record)
	assert.Contains(t, stdout, "send-keys")
	assert.Contains(t, stdout, string(db.CopilotDriveRecordAgentProfile),
		"the daemon-down path owes the operator the same facts the daemon path does")
}

// TestSetDriveDaemonDown_EscalationIsRefused: the fallback must not become a
// second authoring surface for the drive. Recording "api" with no daemon claims
// a channel nothing can create — and an API-driven conversation HOLDS its mail
// rather than falling back to keystrokes, so the agent would go quiet.
func TestSetDriveDaemonDown_EscalationIsRefused(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")

	resp, _ := spawnCopilot(t, f, "crew", map[string]any{"name": "copilot-worker"})
	conv := resp.ConvID

	_, stderr, rc := runSetDriveCLIDirect(t, conv, "api")
	assert.NotEqual(t, 0, rc, "escalation must need the daemon")
	assert.Contains(t, stderr, "agentd is not running")
	assert.Contains(t, stderr, "works with it down",
		"the refusal must point at the direction that DOES work, or an operator "+
			"mid-rollback reads it as 'nothing works'")

	after, err := db.CopilotDriveTargetForConv(conv)
	require.NoError(t, err)
	assert.False(t, after.Value, "a refused escalation must write nothing")
}

// TestSetDriveDaemonDown_RefusesToInventARecord: with nothing recorded there is
// no record to edit, and seeding one needs the conversation's harness and cwd —
// the daemon's resolution, not this process's. Refusing while naming the pin the
// operator probably wants is better than writing a record from a guess.
func TestSetDriveDaemonDown_RefusesToInventARecord(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")

	resp, _ := spawnCopilot(t, f, "crew", map[string]any{"name": "copilot-worker"})
	conv := resp.ConvID

	agentID, err := db.AgentIDForConv(conv)
	require.NoError(t, err)
	profile, err := db.AgentRelaunchProfileForConv(conv)
	require.NoError(t, err)
	require.NotNil(t, profile)
	profile.CopilotAPI = nil
	require.NoError(t, db.SetAgentRelaunchProfile(agentID, *profile))
	conversation, err := db.ConversationResumeProfileForConv(conv)
	require.NoError(t, err)
	require.NotNil(t, conversation)
	if conversation.FallbackRelaunch != nil {
		conversation.FallbackRelaunch.CopilotAPI = nil
		require.NoError(t, db.SetConversationResumeProfile(conv, *conversation))
	}
	require.NoError(t, err)

	_, stderr, rc := runSetDriveCLIDirect(t, conv, "send-keys")
	assert.NotEqual(t, 0, rc)
	assert.Contains(t, stderr, "nothing records a Copilot drive")
	assert.Contains(t, stderr, "PIN",
		"the operator wanted a pin; the refusal must say where to get one rather than "+
			"reading as 'this agent is fine'")

	after, err := db.CopilotDriveTargetForConv(conv)
	require.NoError(t, err)
	assert.Equal(t, db.CopilotDriveRecordNone, after.Record, "the refusal must invent nothing")
}
