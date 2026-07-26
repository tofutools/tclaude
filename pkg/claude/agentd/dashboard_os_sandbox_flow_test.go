package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// requireDashAgentState / requireDashMemberState fetch one row's state from the
// two surfaces the sandbox badge draws from, failing rather than nil-panicking
// when the row is missing — the assertions below read the same fields off both.
func requireDashAgentState(t *testing.T, snap dashSnapshot, convID string) dashState {
	t.Helper()
	agent := findDashAgent(snap, convID)
	require.NotNil(t, agent, "agent %s missing from Agents[]", convID)
	return agent.State
}

func requireDashMemberState(t *testing.T, snap dashSnapshot, group, convID string) dashState {
	t.Helper()
	member := findDashMember(snap, group, convID)
	require.NotNil(t, member, "agent %s missing from group %s", convID, group)
	return member.State
}

// Scenario (TCL-729): the Groups tab could not tell a Claude agent confined by
// the operator's own settings.json from one confined by nothing. Both spawn
// under the default `inherit` mode, both record an empty sandbox_mode, and the
// badge — driven off that mode alone — rendered nothing for either.
//
// The launch boundary now resolves the question once and records the verdict on
// the session row. This pins the READ path that carries it to the browser: both
// surfaces the badge draws from (the Agents[] roster and a group's Members[]
// rows) must expose the verdict and the file that decided it, for a live agent
// and for a dead one — what a finished agent ran under is still what the
// operator needs to know.
func TestDashboardSnapshot_InheritedOSSandboxVerdictSurfaces(t *testing.T) {
	const convID = "sbx1-1111-2222-3333-4444"
	const label = "spwn-sbx1"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveGroup("confined")
	f.HaveAliveSession(convID, label, "tmux-sbx1", f.TestCwd("sbx1"))
	// Stamp the row the way a launch under `inherit` against a sandbox-enabling
	// settings.json does: no explicit mode, but a resolved "on" naming the file
	// that decided it. Same row id → UPSERT, tmux unchanged so it stays alive.
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:              label,
		TmuxSession:     "tmux-sbx1",
		ConvID:          convID,
		Cwd:             f.TestCwd("sbx1"),
		Status:          "running",
		Harness:         "claude",
		SandboxMode:     "",
		OSSandboxState:  "on",
		OSSandboxSource: "~/.claude/settings.json",
	}), "stamp the inherited sandbox verdict")
	f.HaveMember("confined", convID)

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	agent := findDashAgent(snap, convID)
	require.NotNil(t, agent, "agent missing from Agents[]")
	assert.Empty(t, agent.State.SandboxMode,
		"the launch requested no explicit mode — which is exactly why the mode cannot answer the question")
	assert.Equal(t, "on", agent.State.OSSandboxState, "Agents[] carries the resolved verdict")
	assert.Equal(t, "~/.claude/settings.json", agent.State.OSSandboxSource,
		"Agents[] names the file that decided it")

	member := findDashMember(snap, "confined", convID)
	require.NotNil(t, member, "agent missing from group members")
	assert.Equal(t, "on", member.State.OSSandboxState, "Members[] carries the resolved verdict")
	assert.Equal(t, "~/.claude/settings.json", member.State.OSSandboxSource,
		"Members[] names the file that decided it")
}

// A verdict is a launch property, not a liveness property: an exited agent's
// row must still report what confined it, exactly as harness and sandbox_mode
// already do.
func TestDashboardSnapshot_OSSandboxVerdictSurvivesExit(t *testing.T) {
	const convID = "sbx2-1111-2222-3333-4444"
	const label = "spwn-sbx2"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveGroup("finished")
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:              label,
		ConvID:          convID,
		Cwd:             f.TestCwd("sbx2"),
		Status:          "exited",
		Harness:         "claude",
		OSSandboxState:  "unconfigured",
		OSSandboxSource: "",
	}), "stamp an exited unconfined agent")
	f.HaveMember("finished", convID)

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	agent := findDashAgent(snap, convID)
	require.NotNil(t, agent, "agent missing from Agents[]")
	// "unconfigured" is not "off": nothing tclaude could read enabled the
	// sandbox, and nothing disabled it either. The distinction is what lets the
	// operator-facing copy avoid blaming a file that does not exist.
	assert.Equal(t, "unconfigured", agent.State.OSSandboxState,
		"an exited agent still reports what it ran under")
	assert.Empty(t, agent.State.OSSandboxSource, "nothing decided it, so nothing is named")
}

// A harness whose recorded mode already IS its posture records no verdict, and
// its badge keeps rendering off the mode exactly as it did before the columns
// existed. This is the regression guard for the "did adding a verdict change
// Codex?" question.
func TestDashboardSnapshot_CodexRecordsNoOSSandboxVerdict(t *testing.T) {
	const convID = "sbx3-1111-2222-3333-4444"
	const label = "spwn-sbx3"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveGroup("codexish")
	f.HaveAliveCodexSession(convID, label, "tmux-sbx3", f.TestCwd("sbx3"))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:          label,
		TmuxSession: "tmux-sbx3",
		ConvID:      convID,
		Cwd:         f.TestCwd("sbx3"),
		Status:      "running",
		Harness:     "codex",
		SandboxMode: "workspace-write",
	}), "stamp the codex launch sandbox")
	f.HaveMember("codexish", convID)

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	agent := findDashAgent(snap, convID)
	require.NotNil(t, agent, "agent missing from Agents[]")
	assert.Equal(t, "workspace-write", agent.State.SandboxMode, "the mode still drives the codex badge")
	assert.Empty(t, agent.State.OSSandboxState, "codex records no separate verdict")
	assert.Empty(t, agent.State.OSSandboxSource)
}

// The hedge has to survive the trip to the browser, not merely reach the row.
// Its two ends were covered independently — the launch tests assert the DB row,
// the jstest asserts a hand-built member state — leaving the join between them
// unpinned, which is exactly where it can be dropped silently: without it every
// unverifiable verdict renders as a plain padlock claiming "Bash is confined",
// which is the failure the flag exists to prevent.
func TestDashboardSnapshot_UnverifiedOSSandboxVerdictSurfaces(t *testing.T) {
	const convID = "sbx4-1111-2222-3333-4444"
	const label = "spwn-sbx4"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveGroup("doubtful")
	f.HaveAliveSession(convID, label, "tmux-sbx4", f.TestCwd("sbx4"))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:                  label,
		TmuxSession:         "tmux-sbx4",
		ConvID:              convID,
		Cwd:                 f.TestCwd("sbx4"),
		Status:              "running",
		Harness:             "claude",
		SandboxMode:         "on",
		OSSandboxState:      "on",
		OSSandboxSource:     "this launch (sandbox `on`)",
		OSSandboxUnverified: true,
	}), "stamp an unverifiable verdict")
	f.HaveMember("doubtful", convID)

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	agent := findDashAgent(snap, convID)
	require.NotNil(t, agent, "agent missing from Agents[]")
	assert.True(t, agent.State.OSSandboxUnverified, "Agents[] carries the doubt")

	member := findDashMember(snap, "doubtful", convID)
	require.NotNil(t, member, "agent missing from group members")
	assert.True(t, member.State.OSSandboxUnverified, "Members[] carries the doubt")
}

// The converse, so the flag is not simply always true: a fully-resolved verdict
// reaches the browser without the hedge.
func TestDashboardSnapshot_VerifiedOSSandboxVerdictCarriesNoDoubt(t *testing.T) {
	const convID = "sbx5-1111-2222-3333-4444"
	const label = "spwn-sbx5"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveGroup("certain")
	f.HaveAliveSession(convID, label, "tmux-sbx5", f.TestCwd("sbx5"))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: label, TmuxSession: "tmux-sbx5", ConvID: convID, Cwd: f.TestCwd("sbx5"),
		Status: "running", Harness: "claude",
		OSSandboxState: "on", OSSandboxSource: "~/.claude/settings.json",
	}), "stamp a fully-resolved verdict")
	f.HaveMember("certain", convID)

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	agent := findDashAgent(snap, convID)
	require.NotNil(t, agent)
	assert.False(t, agent.State.OSSandboxUnverified,
		"a verdict every tier confirmed must not wear the hedge")
}

// Scenario: the badge tooltip named the settings file that ENABLED the sandbox
// and nothing else, which reads as "this is your whole sandbox configuration"
// — when the rules the agent actually runs under came from a tclaude sandbox
// profile the operator never sees mentioned. The profile is orthogonal to the
// state: it does not decide whether the agent is sandboxed, it supplies the
// rules (for Claude Code, compiled into the harness's own sandbox.filesystem.*
// through `--settings`).
//
// The launch already freezes its resolved policy on the session row, so this
// pins the READ path: both surfaces the badge draws from must name the applied
// profiles and the tier each came from, in resolution order.
func TestDashboardSnapshot_AppliedSandboxProfilesSurface(t *testing.T) {
	const convID = "sbx6-1111-2222-3333-4444"
	const label = "spwn-sbx6"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveGroup("profiled")
	f.HaveAliveSession(convID, label, "tmux-sbx6", f.TestCwd("sbx6"))
	snapshot := sandboxpolicy.EmptySnapshot()
	// Recorded in the order the snapshot BUILDER writes them
	// (db.resolveEffectiveSandboxSnapshot walks global → group → explicit). The
	// read path preserves that order rather than imposing one, so this fixture
	// mirrors a real record instead of proving a sort that does not exist.
	snapshot.Applied = []sandboxpolicy.AppliedProfile{
		{Scope: sandboxpolicy.ScopeGlobal, ID: 1, Name: "tclaude-agent"},
		{Scope: sandboxpolicy.ScopeGroup, ID: 2, Name: "squad-tight"},
	}
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: label, TmuxSession: "tmux-sbx6", ConvID: convID, Cwd: f.TestCwd("sbx6"),
		Status: "running", Harness: "claude",
		OSSandboxState: "on", OSSandboxSource: "~/.claude/settings.json",
		EffectiveSandbox: &snapshot,
	}), "stamp a launch whose rules came from sandbox profiles")
	f.HaveMember("profiled", convID)

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	for name, state := range map[string]dashState{
		"Agents[]":  requireDashAgentState(t, snap, convID),
		"Members[]": requireDashMemberState(t, snap, "profiled", convID),
	} {
		require.Len(t, state.SandboxProfiles, 2, "%s: both applied profiles reach the browser", name)
		assert.Equal(t, "global", state.SandboxProfiles[0].Scope,
			"%s: the recorded tier order survives to the browser", name)
		assert.Equal(t, "tclaude-agent", state.SandboxProfiles[0].Name, "%s: names the global profile", name)
		assert.Equal(t, "group", state.SandboxProfiles[1].Scope, "%s: group tier second", name)
		assert.Equal(t, "squad-tight", state.SandboxProfiles[1].Name, "%s: names the group profile", name)
		assert.True(t, state.SandboxProfilesRecorded, "%s: the launch recorded a resolved policy", name)
	}
}

// The two ways a row can carry no profile names are NOT the same fact, and the
// tooltip says different things about them: a launch that resolved to no
// profile can be reported as such, while a row predating the snapshot must stay
// silent rather than claim an absence tclaude never observed.
func TestDashboardSnapshot_NoSandboxProfilesDistinguishesEmptyFromUnrecorded(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveGroup("plain")

	const emptyConv = "sbx7-1111-2222-3333-4444"
	f.HaveAliveSession(emptyConv, "spwn-sbx7", "tmux-sbx7", f.TestCwd("sbx7"))
	empty := sandboxpolicy.EmptySnapshot()
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "spwn-sbx7", TmuxSession: "tmux-sbx7", ConvID: emptyConv, Cwd: f.TestCwd("sbx7"),
		Status: "running", Harness: "claude", OSSandboxState: "on",
		EffectiveSandbox: &empty,
	}), "stamp a launch that resolved to no sandbox profile")
	f.HaveMember("plain", emptyConv)

	const legacyConv = "sbx8-1111-2222-3333-4444"
	f.HaveAliveSession(legacyConv, "spwn-sbx8", "tmux-sbx8", f.TestCwd("sbx8"))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "spwn-sbx8", TmuxSession: "tmux-sbx8", ConvID: legacyConv, Cwd: f.TestCwd("sbx8"),
		Status: "running", Harness: "claude", OSSandboxState: "on",
	}), "stamp a row from before the policy snapshot existed")
	f.HaveMember("plain", legacyConv)

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	resolved := requireDashAgentState(t, snap, emptyConv)
	assert.Empty(t, resolved.SandboxProfiles, "nothing applied")
	assert.True(t, resolved.SandboxProfilesRecorded,
		"a resolved policy with no profiles is an observed absence the tooltip may report")

	legacy := requireDashAgentState(t, snap, legacyConv)
	assert.Empty(t, legacy.SandboxProfiles, "nothing to name")
	assert.False(t, legacy.SandboxProfilesRecorded,
		"a row with no recorded policy must not be reported as 'no profile applied'")
}

// Scenario: `sandbox: on` reaches a launch either because someone typed it or
// because a spawn profile they never opened carried it — a named profile, their
// group's default, or the global default. The resolved VERDICT is identical
// either way, so the badge called both "forced ON for this launch" and credited
// the containment to the operator reading it.
//
// The spawn boundary already resolves that tier (resolveStringLaunchField) and
// used to discard it. This pins the recorded end of the path: the attribution
// reaches both snapshot surfaces, and an explicit choice is NOT dressed up as a
// profile's doing.
func TestDashboardSnapshot_SandboxModeAttributionSurfaces(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveGroup("attributed")

	const profileConv = "sbx9-1111-2222-3333-4444"
	f.HaveAliveSession(profileConv, "spwn-sbx9", "tmux-sbx9", f.TestCwd("sbx9"))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "spwn-sbx9", TmuxSession: "tmux-sbx9", ConvID: profileConv, Cwd: f.TestCwd("sbx9"),
		Status: "running", Harness: "claude",
		SandboxMode: "on", SandboxModeSource: `global default profile "agents"`,
		OSSandboxState: "on", OSSandboxSource: "global default profile \"agents\" (sandbox `on`)",
	}), "stamp a launch whose sandbox a default profile chose")
	f.HaveMember("attributed", profileConv)

	const explicitConv = "sbxa-1111-2222-3333-4444"
	f.HaveAliveSession(explicitConv, "spwn-sbxa", "tmux-sbxa", f.TestCwd("sbxa"))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "spwn-sbxa", TmuxSession: "tmux-sbxa", ConvID: explicitConv, Cwd: f.TestCwd("sbxa"),
		Status: "running", Harness: "claude",
		SandboxMode: "on", SandboxModeSource: "explicit",
		OSSandboxState: "on", OSSandboxSource: "this launch (sandbox `on`)",
	}), "stamp a launch whose sandbox the caller chose")
	f.HaveMember("attributed", explicitConv)

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	for name, state := range map[string]dashState{
		"Agents[]":  requireDashAgentState(t, snap, profileConv),
		"Members[]": requireDashMemberState(t, snap, "attributed", profileConv),
	} {
		assert.Equal(t, "global default profile \"agents\" (sandbox `on`)", state.OSSandboxSource,
			"%s: the badge can name the profile that forced the sandbox", name)
	}

	explicit := requireDashAgentState(t, snap, explicitConv)
	assert.Equal(t, "this launch (sandbox `on`)", explicit.OSSandboxSource,
		"a caller's own choice stays 'this launch' — it IS this launch")
}

// The durable half: a resumed agent replays the attribution rather than losing
// it. Without this, an agent that has restarted once reports an anonymous "this
// launch" for containment a profile imposed — the same misattribution, arrived
// at by a slower route.
func TestSandboxModeAttributionSurvivesTheDurableProjection(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	const convID = "sbxb-1111-2222-3333-4444"
	f.HaveAliveSession(convID, "spwn-sbxb", "tmux-sbxb", f.TestCwd("sbxb"))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "spwn-sbxb", TmuxSession: "tmux-sbxb", ConvID: convID, Cwd: f.TestCwd("sbxb"),
		Status: "running", Harness: "claude",
		SandboxMode: "on", SandboxModeSource: `group default profile "squad"`,
	}), "stamp an attributed launch")

	launch, err := db.SessionLaunchProfileForConv(convID)
	require.NoError(t, err)
	assert.Equal(t, "on", launch.SandboxMode, "the mode is replayed")
	assert.Equal(t, `group default profile "squad"`, launch.SandboxModeSource,
		"and so is who chose it — a resume that dropped this would go anonymous")
}

// The projection above is only half the contract. A DAEMON relaunch — a resume,
// a crash recovery, a reincarnation, a clone — does not read that projection at
// spawn time; it builds a fresh argv from the durable relaunch config. A path
// that carries the MODE but not its attribution mints a session row with an
// empty sandbox_mode_source, and the projection then asserts that emptiness as
// intent, permanently erasing who chose the containment. The badge would be
// back to crediting "this launch" — the exact misattribution this surface
// exists to remove, now arrived at by way of a restart.
func TestSandboxModeAttributionSurvivesADaemonRelaunch(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveGroup("relaunch")
	const convID = "sbxd-1111-2222-3333-4444"
	f.HaveAliveSession(convID, "spwn-sbxd", "tmux-sbxd", f.TestCwd("sbxd"))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "spwn-sbxd", TmuxSession: "tmux-sbxd", ConvID: convID, Cwd: f.TestCwd("sbxd"),
		Status: "running", Harness: "claude",
		SandboxMode: "on", SandboxModeSource: `group default profile "squad"`,
	}), "stamp an attributed launch")
	f.HaveMember("relaunch", convID)

	f.MarkOffline("tmux-sbxd")
	f.AssertResumeSpawned(f.Resume(convID))

	state := requireDashMemberState(t, fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest()), "relaunch", convID)
	assert.Equal(t, "on", state.SandboxMode, "the relaunch preserved the mode")
	assert.Equal(t, `group default profile "squad"`, state.SandboxModeSource,
		"and the tier that chose it — a relaunch that dropped this re-credits the operator")
	assert.Contains(t, state.OSSandboxSource, `group default profile "squad"`,
		"the replayed attribution reaches the verdict the badge actually renders")
}

// A launch mode can discard the profile tiers outright — a Codex
// danger-full-access spawn takes the raw --sandbox opt-out, which cannot carry
// the managed permission profile that renders filesystem rules. That is not the
// same fact as "no profile was assigned", and the tooltip says so, which it can
// only do if the distinction survives to the browser.
func TestDashboardSnapshot_SuppressedSandboxProfilesAreDistinguishable(t *testing.T) {
	const convID = "sbxc-1111-2222-3333-4444"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveGroup("omitted")
	f.HaveAliveCodexSession(convID, "spwn-sbxc", "tmux-sbxc", f.TestCwd("sbxc"))
	omitted := sandboxpolicy.OmittedProfilesSnapshot()
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "spwn-sbxc", TmuxSession: "tmux-sbxc", ConvID: convID, Cwd: f.TestCwd("sbxc"),
		Status: "running", Harness: "codex", SandboxMode: "danger-full-access",
		EffectiveSandbox: &omitted,
	}), "stamp a launch whose mode suppresses the profile tiers")
	f.HaveMember("omitted", convID)

	state := requireDashAgentState(t, fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest()), convID)
	assert.Empty(t, state.SandboxProfiles, "nothing was applied")
	assert.True(t, state.SandboxProfilesRecorded, "a policy WAS resolved for this launch")
	assert.True(t, state.SandboxProfilesOmitted,
		"the mode discarded the tiers — distinct from nobody having configured one")
}
