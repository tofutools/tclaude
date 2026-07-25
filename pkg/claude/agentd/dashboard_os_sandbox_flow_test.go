package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

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
