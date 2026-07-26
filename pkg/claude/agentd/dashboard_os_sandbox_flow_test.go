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
	// Deliberately out of tier order on the way in: the read path must present
	// them the way resolution applies them (global, then group), not the order
	// they happen to sit in the record.
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
		assert.Equal(t, "global", state.SandboxProfiles[0].Scope, "%s: global tier first", name)
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
