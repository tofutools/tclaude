package agentd_test

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// TCL-1053. tclaude is growing a SECOND way to drive a Copilot agent — over
// Copilot CLI's embedded JSON-RPC server rather than by typing into its pane
// with tmux send-keys — and the two are meant to run side by side while agents
// migrate one at a time. This file pins the two properties that makes safe:
//
//  1. Off by default. A spawn that does not ask for the API drive threads
//     false, exactly as every Copilot spawn did before the flag existed.
//  2. Sticky. The choice is per-conversation launch intent, so a resume (and
//     therefore reincarnate/clone, which fork through it) must land on the same
//     drive rather than silently reverting to send-keys.
//
// The assertions sit at the simSpawner boundary — what the spawn path threaded
// onto the forked `tclaude session new` — plus the durable relaunch record that
// makes the next hop possible.

// copilotAPIOn is the tri-state "true" a db.SpawnProfile carries.
var copilotAPIOn = true

// TestCopilotDrive_SpawnDefaultsToSendKeys is the headline acceptance bar: a
// Copilot spawn nobody steered must be indistinguishable from one made before
// this flag existed.
func TestCopilotDrive_SpawnDefaultsToSendKeys(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")

	resp, _ := runSpawnCLI(t, f, &agent.SpawnParams{
		Group: "crew", Name: "copilot-worker", Harness: harness.CopilotName,
	})

	got, ok := f.World.SpawnCopilotAPI(resp.ConvID)
	require.True(t, ok, "no spawn recorded for conv %s", resp.ConvID)
	assert.False(t, got, "an unsteered Copilot spawn must stay on tmux send-keys")
	assert.Empty(t, resp.Resolved.CopilotAPI.Value,
		"the echo must stay silent about a drive nobody chose")
}

// TestCopilotDrive_ExplicitFlagThreadsThrough: --copilot-api must survive the
// whole chain — CLI SpawnParams → wire SpawnRequest → handleGroupSpawn →
// spawnParams → the spawner.
func TestCopilotDrive_ExplicitFlagThreadsThrough(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")

	resp, out := runSpawnCLI(t, f, &agent.SpawnParams{
		Group: "crew", Name: "copilot-worker", Harness: harness.CopilotName, CopilotAPI: true,
	})

	got, ok := f.World.SpawnCopilotAPI(resp.ConvID)
	require.True(t, ok, "no spawn recorded for conv %s", resp.ConvID)
	assert.True(t, got, "--copilot-api must thread through to the spawner")
	assert.Equal(t, agent.ResolvedField{Value: "api", Source: agent.ProvExplicit},
		resp.Resolved.CopilotAPI)
	assert.Contains(t, out, "Copilot drive:",
		"an operator debugging two drives must see which one this spawn got")
}

// TestCopilotDrive_ProfileSelectsTheDrive: the flag is not the only tier. A
// Copilot spawn profile carrying the opt-in must fill an unpassed flag, and say
// so in the echo, so an operator can move a whole class of agents at once.
func TestCopilotDrive_ProfileSelectsTheDrive(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "copilot-api", "harness": harness.CopilotName, "copilot_api": true,
	}).Code)

	resp, _ := runSpawnCLI(t, f, &agent.SpawnParams{
		Group: "crew", Name: "copilot-worker", Profile: "copilot-api",
	})

	got, ok := f.World.SpawnCopilotAPI(resp.ConvID)
	require.True(t, ok, "no spawn recorded for conv %s", resp.ConvID)
	assert.True(t, got, "a profile's copilot_api must fill an unpassed flag")
	assert.Equal(t, agent.ResolvedField{
		Value: "api", Source: agent.ProvCLIProfileSource("copilot-api"),
	}, resp.Resolved.CopilotAPI, "the echo must name the tier that chose the drive")
}

// TestCopilotDrive_GlobalDefaultProfileSelectsTheDrive: the ambient tiers speak
// too, which is the whole reason the CLI leaves the pointer nil rather than
// sending an explicit false.
func TestCopilotDrive_GlobalDefaultProfileSelectsTheDrive(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "copilot-global", "harness": harness.CopilotName, "copilot_api": true,
	}).Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "copilot-global").Code)

	resp, _ := runSpawnCLI(t, f, &agent.SpawnParams{Group: "crew", Name: "copilot-worker"})

	got, ok := f.World.SpawnCopilotAPI(resp.ConvID)
	require.True(t, ok, "no spawn recorded for conv %s", resp.ConvID)
	assert.True(t, got, "a global default Copilot profile must select the drive")
	assert.Equal(t, agent.ProvGlobalProfileSource("copilot-global"),
		resp.Resolved.CopilotAPI.Source)
}

// TestCopilotDrive_ForeignProfileNeverReachesAnotherHarness: the drive is
// Copilot-specific on purpose (TCL-1051 declined to generalise it), so a
// default profile carrying it must not follow an operator onto Codex.
func TestCopilotDrive_ForeignProfileNeverReachesAnotherHarness(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{
		Name: "copilot-api", Harness: harness.CopilotName, CopilotAPI: &copilotAPIOn,
	})
	require.NoError(t, err)

	resp, _ := runSpawnCLI(t, f, &agent.SpawnParams{
		Group: "crew", Name: "codex-worker", Harness: harness.CodexName, Profile: "copilot-api",
	})

	got, ok := f.World.SpawnCopilotAPI(resp.ConvID)
	require.True(t, ok, "no spawn recorded for conv %s", resp.ConvID)
	assert.False(t, got, "a Copilot-only drive must not reach a Codex launch")
	assert.Empty(t, resp.Resolved.CopilotAPI.Value)
}

// TestCopilotDrive_RejectedForAHarnessWithoutOne: asking a harness that has no
// API-backed mode is a loud client-side refusal, not a silent drop — otherwise
// an operator is left wondering why their agent is still on send-keys.
func TestCopilotDrive_RejectedForAHarnessWithoutOne(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	bridgeAgentClientToMux(t, f.Mux)
	chdirTo(t, resolveSym(t, t.TempDir()))

	stderr := new(bytes.Buffer)
	resp, rc := agent.RunSpawn(&agent.SpawnParams{
		Group: "crew", Name: "claude-worker", Harness: harness.DefaultName, CopilotAPI: true,
	}, new(bytes.Buffer), stderr, new(bytes.Buffer))

	require.NotEqual(t, 0, rc, "--copilot-api on Claude Code must fail")
	assert.Nil(t, resp)
	assert.Contains(t, stderr.String(), "API-backed mode")
}

// TestCopilotDrive_SurvivesResume is the stickiness bar. A resume mints a fresh
// session row and re-records what it resolved, so a drive that is not carried
// is not merely lost for one launch — the blank is asserted as intent and the
// choice is gone permanently. Three rounds, because a carry that reads the
// record but forgets to re-write it passes the first hop and fails the second.
func TestCopilotDrive_SurvivesResume(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")

	resp, _ := runSpawnCLI(t, f, &agent.SpawnParams{
		Group: "crew", Name: "copilot-worker", Harness: harness.CopilotName, CopilotAPI: true,
	})
	conv := resp.ConvID

	recorded, err := db.AgentRelaunchProfileForConv(conv)
	require.NoError(t, err)
	require.NotNil(t, recorded)
	require.NotNil(t, recorded.CopilotAPI, "the spawn must freeze the drive it chose")
	assert.True(t, *recorded.CopilotAPI)

	session, err := db.FindSessionByConvID(conv)
	require.NoError(t, err)
	require.NotNil(t, session)
	live := session.TmuxSession

	for round := 1; round <= 3; round++ {
		f.MarkOffline(live)
		r := f.AsHuman().Resume(conv)
		require.Equalf(t, "resumed", r.Action, "resume round %d: %s", round, r.Raw)

		got, ok := f.World.SpawnCopilotAPI(conv)
		require.Truef(t, ok, "no resume-spawn recorded for conv %s on round %d", conv, round)
		assert.Truef(t, got, "the API drive must survive resume round %d", round)

		after, err := db.AgentRelaunchProfileForConv(conv)
		require.NoError(t, err)
		require.NotNil(t, after)
		require.NotNilf(t, after.CopilotAPI,
			"round %d must RE-RECORD the drive, or the next resume reads an empty row", round)
		assert.Truef(t, *after.CopilotAPI, "round %d must re-record the API drive", round)

		session, err := db.FindSessionByConvID(conv)
		require.NoError(t, err)
		require.NotNil(t, session)
		live = session.TmuxSession
	}
}

// TestCopilotDrive_SendKeysSurvivesResume is the same guarantee in the default
// direction: a send-keys agent must not acquire the experimental drive from a
// profile edit made after it was born.
func TestCopilotDrive_SendKeysSurvivesResume(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")

	resp, _ := runSpawnCLI(t, f, &agent.SpawnParams{
		Group: "crew", Name: "copilot-worker", Harness: harness.CopilotName,
	})
	conv := resp.ConvID

	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "copilot-api", "harness": harness.CopilotName, "copilot_api": true,
	}).Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "copilot-api").Code)

	session, err := db.FindSessionByConvID(conv)
	require.NoError(t, err)
	require.NotNil(t, session)
	f.MarkOffline(session.TmuxSession)

	r := f.AsHuman().Resume(conv)
	require.Equal(t, "resumed", r.Action, "resume: %s", r.Raw)

	got, ok := f.World.SpawnCopilotAPI(conv)
	require.True(t, ok, "no resume-spawn recorded for conv %s", conv)
	assert.False(t, got,
		"a send-keys agent must not be moved onto the API drive by a later profile edit")
}

// TestCopilotDrive_SurvivesReincarnateAndClone: reincarnate and clone both fork
// through their own SpawnArgs assembly rather than the resume one, so each needs
// its own pin. A long-lived API-driven agent is exactly the one that gets
// reincarnated, and a clone is meant to be the same agent alongside the
// original — a sibling that quietly reverted to send-keys would make the two
// drives impossible to compare.
func TestCopilotDrive_SurvivesReincarnateAndClone(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")

	resp, _ := runSpawnCLI(t, f, &agent.SpawnParams{
		Group: "crew", Name: "copilot-worker", Harness: harness.CopilotName, CopilotAPI: true,
	})

	reincarnated := f.AsHuman().Reincarnate(resp.ConvID, "carry on")
	require.NotEmptyf(t, reincarnated.NewConv, "no successor: %s", reincarnated.Raw)
	got, ok := f.World.SpawnCopilotAPI(reincarnated.NewConv)
	require.True(t, ok, "no spawn recorded for successor conv %s", reincarnated.NewConv)
	assert.True(t, got, "a successor must inherit the API drive")

	cloned := f.AsHuman().CloneFresh(reincarnated.NewConv)
	require.NotEmptyf(t, cloned.NewConv, "no clone: %s", cloned.Raw)
	got, ok = f.World.SpawnCopilotAPI(cloned.NewConv)
	require.True(t, ok, "no spawn recorded for clone conv %s", cloned.NewConv)
	assert.True(t, got, "a clone must inherit the API drive")
}
