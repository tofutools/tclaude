package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// findDashAgent returns the snapshot's Agents[] row for a conv-id.
func findDashAgent(snap dashSnapshot, convID string) *dashAgent {
	for i := range snap.Agents {
		if snap.Agents[i].ConvID == convID {
			return &snap.Agents[i]
		}
	}
	return nil
}

// findDashMember returns the group-member row for a conv-id within a
// named group.
func findDashMember(snap dashSnapshot, group, convID string) *dashMember {
	for gi := range snap.Groups {
		if snap.Groups[gi].Name != group {
			continue
		}
		for mi := range snap.Groups[gi].Members {
			if snap.Groups[gi].Members[mi].ConvID == convID {
				return &snap.Groups[gi].Members[mi]
			}
		}
	}
	return nil
}

// Scenario: an agent's context-window usage — written to the sessions
// row by the statusline hook (UpdateContextSnapshot) — must surface on
// /api/snapshot so the dashboard's vertical context-meter can render
// it. The meter rides entirely on this already-persisted data; no new
// poller, no new data source.
//
// Asserts the snapshot carries context_pct + the absolute token counts
// for the agent BOTH on the Agents[] roster and on the group Members[]
// row (the two places memberRowHTML / the agents table draw the
// meter). A known 60% value is the JS contract: with 5 segments at 20%
// each, round(60/20)=3 segments light bottom-up (2 green + 1 yellow).
func TestDashboardSnapshot_ContextMeterUsageSurfaced(t *testing.T) {
	const conv = "ctxm-1111-2222-3333-4444"
	const label = "spwn-ctxm"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveGroup("squad")
	f.HaveAliveSession(conv, label, "tmux-ctxm", "/tmp/ctxm")
	f.HaveMember("squad", conv)

	// The statusline hook's write path: context_pct + abs token counts
	// land on the sessions row keyed by tclaude session ID (the label).
	require.NoError(t,
		db.UpdateContextSnapshot(label, 60.0, 110000, 10000, 200000),
		"UpdateContextSnapshot")

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	agentRow := findDashAgent(snap, conv)
	require.NotNil(t, agentRow, "agent %s missing from snapshot Agents[]", conv)
	assert.Equal(t, 60.0, agentRow.State.ContextPct, "Agents[] context_pct")
	assert.Equal(t, int64(110000), agentRow.State.TokensInput, "Agents[] tokens_input")
	assert.Equal(t, int64(10000), agentRow.State.TokensOutput, "Agents[] tokens_output")
	assert.Equal(t, int64(200000), agentRow.State.ContextWindowSize, "Agents[] context_window_size")

	memberRow := findDashMember(snap, "squad", conv)
	require.NotNil(t, memberRow, "agent %s missing from group squad members", conv)
	assert.Equal(t, 60.0, memberRow.State.ContextPct, "Members[] context_pct")
	assert.Equal(t, int64(200000), memberRow.State.ContextWindowSize, "Members[] context_window_size")
}

// Regression: Codex has no command-backed statusline, so waiting for the
// turn-ending hook could leave the dashboard meter stale until a later hook
// happened to persist rollout telemetry. A live Codex session should refresh
// from its latest rollout token_count when /api/snapshot reads it.
func TestDashboardSnapshot_CodexContextRefreshesFromRolloutOnRead(t *testing.T) {
	const conv = "019ec004-4250-79b1-9ade-ebaea4170180"
	const label = "spwn-codexctx"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveGroup("codex-squad")
	cx := f.HaveAliveCodexSession(conv, label, "tmux-codexctx", "/tmp/codexctx")
	cx.ContextWindow = 200000
	f.HaveMember("codex-squad", conv)

	require.NoError(t, cx.WriteUserInput("do the codex thing"))
	require.NoError(t, cx.WriteTokenCount(
		testharness.CodexTokenUsage{InputTokens: 120000, OutputTokens: 8000, TotalTokens: 128000},
		testharness.CodexTokenUsage{InputTokens: 49000, OutputTokens: 1000, TotalTokens: 50000},
	))

	// No Stop hook / db.UpdateContextSnapshot call here: the dashboard read
	// itself should lift the latest Codex token_count into the session row.
	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	agentRow := findDashAgent(snap, conv)
	require.NotNil(t, agentRow, "agent %s missing from snapshot Agents[]", conv)
	assert.InDelta(t, 25.0, agentRow.State.ContextPct, 0.001, "Agents[] context_pct")
	assert.Equal(t, int64(49000), agentRow.State.TokensInput, "Agents[] tokens_input")
	assert.Equal(t, int64(1000), agentRow.State.TokensOutput, "Agents[] tokens_output")
	assert.Equal(t, int64(200000), agentRow.State.ContextWindowSize, "Agents[] context_window_size")

	memberRow := findDashMember(snap, "codex-squad", conv)
	require.NotNil(t, memberRow, "agent %s missing from group codex-squad members", conv)
	assert.InDelta(t, 25.0, memberRow.State.ContextPct, 0.001, "Members[] context_pct")
}

// Scenario: a freshly-spawned agent whose statusline hook has not yet
// fired has no context snapshot row. The dashboard meter must render a
// neutral / empty indicator, never a broken one — so /api/snapshot
// must report a clean zero (context_pct == 0, no token counts) rather
// than garbage. The JS treats this all-zero state as "unknown" and
// dims every segment.
func TestDashboardSnapshot_ContextMeterUnknownWhenNoUsage(t *testing.T) {
	const conv = "ctxu-1111-2222-3333-4444"
	const label = "spwn-ctxu"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveAliveSession(conv, label, "tmux-ctxu", "/tmp/ctxu")
	f.HaveEnrolledAgent(conv)

	// No UpdateContextSnapshot call — the statusline hook never fired.

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	agentRow := findDashAgent(snap, conv)
	require.NotNil(t, agentRow, "agent %s missing from snapshot Agents[]", conv)
	assert.Zero(t, agentRow.State.ContextPct, "no-usage agent should report 0 context_pct")
	assert.Zero(t, agentRow.State.TokensInput, "no-usage agent should report 0 tokens_input")
	assert.Zero(t, agentRow.State.TokensOutput, "no-usage agent should report 0 tokens_output")
	assert.Zero(t, agentRow.State.ContextWindowSize, "no-usage agent should report 0 window size")
}

// Regression: the dashboard context meter flickered empty because the
// statusline hook clobbered a good snapshot. Claude Code emits
// statusline renders whose context_window block is empty (e.g. before
// a turn's first API response); the hook turned those into an
// UpdateContextSnapshot(0,0,0,0) call that overwrote the row's good
// data with zeros. The next populated render restored it — hence the
// intermittent empty meter, and `tclaude agent context-info` "working"
// only when read during a good window.
//
// The fix makes the write non-destructive at the DB chokepoint: an
// all-zero snapshot is skipped, never written. This test reproduces
// the bug — a good snapshot, then an empty render — and asserts the
// good snapshot still surfaces on /api/snapshot. Pre-fix it fails (the
// empty write zeroes the row); post-fix the data survives.
func TestDashboardSnapshot_ContextMeterSurvivesEmptyStatuslineRender(t *testing.T) {
	const conv = "ctxc-1111-2222-3333-4444"
	const label = "spwn-ctxc"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveAliveSession(conv, label, "tmux-ctxc", "/tmp/ctxc")
	f.HaveEnrolledAgent(conv)

	// A populated statusline render writes a good snapshot.
	require.NoError(t,
		db.UpdateContextSnapshot(label, 15.0, 146000, 2000, 1000000),
		"good snapshot")

	// A subsequent render whose context_window block is empty arrives
	// as an all-zero write. It must NOT clobber the good snapshot.
	require.NoError(t,
		db.UpdateContextSnapshot(label, 0, 0, 0, 0),
		"empty statusline render")

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	agentRow := findDashAgent(snap, conv)
	require.NotNil(t, agentRow, "agent %s missing from snapshot Agents[]", conv)
	assert.Equal(t, 15.0, agentRow.State.ContextPct,
		"context_pct must survive an empty statusline render")
	assert.Equal(t, int64(146000), agentRow.State.TokensInput,
		"tokens_input must survive an empty statusline render")
	assert.Equal(t, int64(2000), agentRow.State.TokensOutput,
		"tokens_output must survive an empty statusline render")
	assert.Equal(t, int64(1000000), agentRow.State.ContextWindowSize,
		"context_window_size must survive an empty statusline render")
}

// Regression: the dashboard context meter dropped to empty whenever an
// agent's state changed — going to 0 exactly on "idle" and "working:
// UserPromptSubmit", correct only while "working: Bash/Write".
//
// Root cause: the state-tracking hooks (Stop, UserPromptSubmit, every
// PreToolUse tick) call db.SaveSession to update the row's status, and
// SaveSession's INSERT OR REPLACE re-created the whole row — resetting
// the context columns the statusline hook owns back to DEFAULT 0. The
// next statusline render restored them, hence the flicker; an idle
// agent has no further renders, so it stayed empty. The statusbar
// display never blipped because it reads Claude Code's input directly,
// not the DB. The fix makes SaveSession an UPSERT that leaves the
// out-of-band context columns untouched.
//
// This writes a good snapshot, then fires a SaveSession (what a Stop /
// UserPromptSubmit hook does), and asserts /api/snapshot still carries
// the usage. Pre-fix it fails — the SaveSession zeroes the row.
func TestDashboardSnapshot_ContextMeterSurvivesStateHookWrite(t *testing.T) {
	const conv = "ctxs-1111-2222-3333-4444"
	const label = "spwn-ctxs"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveAliveSession(conv, label, "tmux-ctxs", "/tmp/ctxs")
	f.HaveEnrolledAgent(conv)

	// The statusline hook writes a good context snapshot.
	require.NoError(t,
		db.UpdateContextSnapshot(label, 24.0, 241000, 5000, 1000000),
		"context snapshot")

	// A state-tracking hook fires — e.g. Stop (-> idle) or
	// UserPromptSubmit — re-saving the session row to update its
	// status. It must not wipe the context columns.
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:          label,
		TmuxSession: "tmux-ctxs",
		ConvID:      conv,
		Cwd:         "/tmp/ctxs",
		Status:      "idle",
	}), "state-update SaveSession")

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	agentRow := findDashAgent(snap, conv)
	require.NotNil(t, agentRow, "agent %s missing from snapshot Agents[]", conv)
	assert.Equal(t, 24.0, agentRow.State.ContextPct,
		"context_pct must survive a state-tracking hook's SaveSession")
	assert.Equal(t, int64(241000), agentRow.State.TokensInput,
		"tokens_input must survive a state-tracking hook's SaveSession")
	assert.Equal(t, int64(1000000), agentRow.State.ContextWindowSize,
		"context_window_size must survive a state-tracking hook's SaveSession")
}
