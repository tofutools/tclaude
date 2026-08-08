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
	"github.com/tofutools/tclaude/pkg/testharness"
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

// copilotAPIProfile is the spawn profile every API-drive scenario below spawns
// through.
//
// It exists because TCL-1056 made the API drive REFUSE a launch into a
// directory Copilot does not trust: the folder-trust modal does not block the
// embedded server, so such a launch would come up drivable and invisible rather
// than failing. Pre-trusting is deliberately never a default, so an API-drive
// spawn has to carry the opt-in — and a profile is how a spawn made through the
// `tclaude agent spawn` surface carries it, since that CLI has no --trust-dir
// flag of its own.
//
// Every scenario here is about the DRIVE threading through, so the profile is
// scaffolding rather than subject matter. The refusal itself is asserted in
// TestCopilotDrive_UntrustedLaunchDirIsRefused.
const copilotAPIProfile = "copilot-api-trusted"

// haveCopilotAPIProfile creates the profile above in the flow's daemon.
func haveCopilotAPIProfile(t *testing.T, f *testharness.Flow) {
	t.Helper()
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": copilotAPIProfile, "harness": harness.CopilotName, "trust_dir": true,
	}).Code)
}

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
	haveCopilotAPIProfile(t, f)

	resp, out := runSpawnCLI(t, f, &agent.SpawnParams{
		Group: "crew", Name: "copilot-worker", Harness: harness.CopilotName, CopilotAPI: true, Profile: copilotAPIProfile,
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
		// trust_dir rides along for the same reason copilotAPIProfile carries
		// it: the API drive refuses an untrusted launch dir (TCL-1056), and this
		// scenario is about the DRIVE arriving from a profile tier, not about
		// trust.
		"name": "copilot-api", "harness": harness.CopilotName,
		"copilot_api": true, "trust_dir": true,
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
		// trust_dir rides along for the same reason copilotAPIProfile carries
		// it: the API drive refuses an untrusted launch dir (TCL-1056), and
		// this scenario is about the DRIVE arriving from the global tier.
		"name": "copilot-global", "harness": harness.CopilotName,
		"copilot_api": true, "trust_dir": true,
	}).Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "copilot-global").Code)

	resp, _ := runSpawnCLI(t, f, &agent.SpawnParams{Group: "crew", Name: "copilot-worker"})

	got, ok := f.World.SpawnCopilotAPI(resp.ConvID)
	require.True(t, ok, "no spawn recorded for conv %s", resp.ConvID)
	assert.True(t, got, "a global default Copilot profile must select the drive")
	assert.Equal(t, agent.ProvGlobalProfileSource("copilot-global"),
		resp.Resolved.CopilotAPI.Source)
}

// TestCopilotDrive_UntrustedLaunchDirIsRefused is TCL-1056's refusal, end to
// end through the production spawn API.
//
// It exists because the failure it prevents does not look like a failure.
// Copilot's folder-trust modal blocks the TUI but NOT the embedded server:
// measured live, a pane parked on the dialog still accepted a connection,
// created a session, foregrounded it and completed a full turn. So an
// unattended API-drive spawn into an untrusted directory would come up working
// and invisible — the human staring at a blocking dialog about an agent that is
// answering prompts — and every surface a debugger would reach for would say
// something true about the wrong thing.
//
// The refusal must name the directory and the remedy, or it converts a silent
// bad state into a loud dead end.
func TestCopilotDrive_UntrustedLaunchDirIsRefused(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")

	// An explicit cwd, because that is what the check can name. A request that
	// names none is answered later, by the backstop inside session.New, against
	// the directory resolveSessionDir picks — refusing here would mean guessing
	// at a path nobody chose.
	resp := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "copilot-worker", "harness": harness.CopilotName, "copilot_api": true,
		"cwd": t.TempDir(),
	})

	require.Equal(t, http.StatusUnprocessableEntity, resp.Code,
		"an API-drive spawn into an untrusted dir must be refused, not launched: body=%s",
		resp.Raw)
	assert.Contains(t, string(resp.Raw), "copilot_api_untrusted_launch_dir")
	assert.Contains(t, string(resp.Raw), "pre-trust",
		"the refusal must name a remedy the operator can actually reach")
}

// The refusal is about the CHANNEL, not about Copilot. A send-keys Copilot
// spawn into the same untrusted directory has always been allowed to launch and
// stop on the modal — a human clears it, and the dashboard focus button exists
// for exactly that — so this must not have quietly become a trust requirement
// for every Copilot agent.
func TestCopilotDrive_SendKeysStillLaunchesInAnUntrustedDir(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")

	resp, _ := spawnCopilot(t, f, "crew", map[string]any{"name": "copilot-worker"})

	got, ok := f.World.SpawnCopilotAPI(resp.ConvID)
	require.True(t, ok)
	assert.False(t, got)
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
	haveCopilotAPIProfile(t, f)

	resp, _ := runSpawnCLI(t, f, &agent.SpawnParams{
		Group: "crew", Name: "copilot-worker", Harness: harness.CopilotName, CopilotAPI: true, Profile: copilotAPIProfile,
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
		// trust_dir rides along for the same reason copilotAPIProfile carries
		// it: the API drive refuses an untrusted launch dir (TCL-1056), and this
		// scenario is about the DRIVE arriving from a profile tier, not about
		// trust.
		"name": "copilot-api", "harness": harness.CopilotName,
		"copilot_api": true, "trust_dir": true,
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
	haveCopilotAPIProfile(t, f)

	resp, _ := runSpawnCLI(t, f, &agent.SpawnParams{
		Group: "crew", Name: "copilot-worker", Harness: harness.CopilotName, CopilotAPI: true, Profile: copilotAPIProfile,
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

	// Threading the flag onto the launch is only half of it, and the half that
	// was already true. The other half is that the NEW conversation id carries
	// the posture DURABLY, because since TCL-1058 that record is what decides
	// whether the successor's and the sibling's mail travels over RPC or is
	// typed into their panes. A successor launched with --copilot-api whose
	// record says nothing is an API-driven agent routed onto keystrokes, and it
	// looks entirely healthy from every surface.
	//
	// The clone is the arm that could not survive on the old writers at all: it
	// is a NEW agent, nothing on the clone path freezes an agent-level posture,
	// and the launched process's own record is best-effort and lands late.
	for _, minted := range []struct {
		verb   string
		convID string
	}{
		{"reincarnate", reincarnated.NewConv},
		{"clone", cloned.NewConv},
	} {
		assert.Truef(t, copilotDriveIsRecordedFor(t, minted.convID),
			"%s minted conv %s with no recorded Copilot drive; its messages would "+
				"route as send-keys", minted.verb, minted.convID)
	}
}

// copilotDriveIsRecordedFor answers the question the daemon's routing predicate
// asks: does anything DURABLE say this conversation took the API drive?
//
// Both records are consulted, in the same order copilotLaunchIntentForConv uses
// — the stable agent's frozen posture first, then the conversation fallback —
// because reincarnate and clone leave the answer in different places. A
// successor inherits its predecessor's agent row; a clone is a new agent with
// no frozen posture at all, so only the fallback can speak for it.
func copilotDriveIsRecordedFor(t *testing.T, convID string) bool {
	t.Helper()
	if profile, err := db.AgentRelaunchProfileForConv(convID); err == nil &&
		profile != nil && profile.CopilotAPI != nil {
		return *profile.CopilotAPI
	}
	conversation, err := db.ConversationResumeProfileForConv(convID)
	require.NoError(t, err)
	if conversation == nil || conversation.FallbackRelaunch == nil ||
		conversation.FallbackRelaunch.CopilotAPI == nil {
		return false
	}
	return *conversation.FallbackRelaunch.CopilotAPI
}

// TestCopilotDrive_ExplicitOffBeatsAnAmbientProfile is the regression guard for
// the default-off promise at its weakest point. executeSpawn runs a safety-net
// overlay that re-resolves the whole tier stack, and it can only tell "the
// operator said off" from "nobody spoke" if the spawn boundary hands it the
// SET flag alongside the value. Without that, an explicit false falls through
// and an ambient default profile turns the API drive on underneath the
// operator — and, because the resolved-launch echo is computed before the
// overlay runs, the echo would still claim send-keys while the pane ran on the
// API.
//
// The dashboard spawn modal sends an explicit false on every Copilot spawn, so
// this is the ordinary path, not a hand-built request.
func TestCopilotDrive_ExplicitOffBeatsAnAmbientProfile(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "copilot-global", "harness": harness.CopilotName, "copilot_api": true,
	}).Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "copilot-global").Code)

	resp, _ := spawnCopilot(t, f, "crew", map[string]any{
		"name": "copilot-worker", "copilot_api": false,
	})

	got, ok := f.World.SpawnCopilotAPI(resp.ConvID)
	require.True(t, ok, "no spawn recorded for conv %s", resp.ConvID)
	assert.False(t, got,
		"an explicit copilot_api=false must beat a global default profile's opt-in")

	// The frozen record must agree, or the unwanted drive would become permanent
	// across every later resume/reincarnate/clone.
	recorded, err := db.AgentRelaunchProfileForConv(resp.ConvID)
	require.NoError(t, err)
	require.NotNil(t, recorded)
	require.NotNil(t, recorded.CopilotAPI)
	assert.False(t, *recorded.CopilotAPI,
		"the durable record must freeze the drive the operator actually chose")
}
