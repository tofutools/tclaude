package pickup

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
)

func sampleList() agent.AWBPickupList {
	now := time.Now()
	return agent.AWBPickupList{Processes: []agent.AWBPickupProcess{
		{Process: "alpha", Workspace: "tcl", Configured: true, State: agent.AWBPickupStateStuck,
			Hint: "agent retired but the issue is still open",
			Dispatch: &agent.AWBPickupDispatch{IssueID: "tcl-1", Phase: "spawned", AgentID: "agt_1",
				CreatedAt: now.Add(-90 * time.Minute),
				Issue:     &agent.AWBPickupIssue{Status: "in_progress", Title: "Fix it"},
				Agent:     &agent.AWBPickupAgent{Exists: true, Name: "tcl-1", Retired: true}}},
		{Process: "beta", Workspace: "web", Configured: true, State: agent.AWBPickupStatePolling,
			LastPollAt: &now},
	}}
}

func TestRowForRendersLiveIssueAndAgent(t *testing.T) {
	list := sampleList()
	now := time.Now()
	r := rowFor(list.Processes[0], now)
	assert.Equal(t, "tcl-1", r.issue)
	assert.Equal(t, "in_progress", r.issueStatus)
	assert.Equal(t, "tcl-1", r.agent)
	assert.Equal(t, "retired", r.session)
	assert.Equal(t, "1h30m", r.since)

	idle := rowFor(list.Processes[1], now)
	assert.Equal(t, "-", idle.issue)
	assert.Equal(t, "polled 0s ago", idle.since)

	orphan := rowFor(agent.AWBPickupProcess{Process: "old", State: agent.AWBPickupStateOrphaned,
		Dispatch: &agent.AWBPickupDispatch{IssueID: "x-1", IssueError: "boom", CreatedAt: now}}, now)
	assert.Equal(t, "old (unconfigured)", orphan.process)
	assert.Equal(t, "unknown", orphan.issueStatus)
	assert.Equal(t, "AWB: boom", orphan.note)
}

func TestResetSummary(t *testing.T) {
	assert.Contains(t, resetSummary(agent.AWBPickupResetResponse{Process: "a"}), "nothing to reset")
	msg := resetSummary(agent.AWBPickupResetResponse{Process: "a", Reset: true,
		Dispatch: &agent.AWBPickupDispatch{IssueID: "tcl-1", Phase: "spawned", AgentID: "agt_1"}})
	assert.Contains(t, msg, "released issue tcl-1")
	assert.Contains(t, msg, "agt_1 was not touched")
}

func runCmd(t *testing.T, m tea.Model, cmd tea.Cmd) tea.Model {
	t.Helper()
	if cmd == nil {
		return m
	}
	m, _ = m.Update(cmd())
	return m
}

func key(s string) tea.KeyPressMsg {
	if s == "down" {
		return tea.KeyPressMsg{Code: tea.KeyDown}
	}
	return tea.KeyPressMsg{Code: rune(s[0]), Text: s}
}

func TestWatchResetConfirmsAndPinsIssue(t *testing.T) {
	list := sampleList()
	var resetCalls []resetTarget
	fetch := func() (agent.AWBPickupList, error) { return list, nil }
	reset := func(process, issueID string) (agent.AWBPickupResetResponse, error) {
		resetCalls = append(resetCalls, resetTarget{process, issueID})
		return agent.AWBPickupResetResponse{Process: process, Reset: true,
			Dispatch: &agent.AWBPickupDispatch{IssueID: issueID, Phase: "spawned"}}, nil
	}
	var m tea.Model = newWatchModel(fetch, reset, time.Second, newWatchStyles(""))
	m = runCmd(t, m, m.Init())
	view := m.View().Content
	assert.Contains(t, view, "alpha")
	assert.Contains(t, view, "Fix it", "the selected process shows its issue title")

	// Cancel first: any key other than y aborts.
	m, _ = m.Update(key("r"))
	assert.Contains(t, m.View().Content, "Reset alpha and release issue tcl-1?")
	m, _ = m.Update(key("n"))
	assert.Empty(t, resetCalls)

	m, _ = m.Update(key("r"))
	// A refresh landing while the prompt is up must not change the target.
	list.Processes[0].Dispatch.IssueID = "tcl-2"
	m, _ = m.Update(statusMsg{list: list})
	m, cmd := m.Update(key("y"))
	require.NotNil(t, cmd)
	m = runCmd(t, m, cmd)
	require.Len(t, resetCalls, 1)
	assert.Equal(t, resetTarget{"alpha", "tcl-1"}, resetCalls[0])
	assert.Contains(t, m.View().Content, "released issue tcl-1")
}

func TestWatchResetOnIdleProcessIsRefusedLocally(t *testing.T) {
	list := sampleList()
	called := false
	var m tea.Model = newWatchModel(
		func() (agent.AWBPickupList, error) { return list, nil },
		func(string, string) (agent.AWBPickupResetResponse, error) {
			called = true
			return agent.AWBPickupResetResponse{}, nil
		}, time.Second, newWatchStyles(""))
	m = runCmd(t, m, m.Init())
	m, _ = m.Update(key("down"))
	m, cmd := m.Update(key("r"))
	assert.Nil(t, cmd)
	assert.False(t, called)
	assert.Contains(t, m.View().Content, "nothing to reset")
}

func TestWatchShowsFetchError(t *testing.T) {
	var m tea.Model = newWatchModel(
		func() (agent.AWBPickupList, error) { return agent.AWBPickupList{}, errors.New("daemon down") },
		nil, time.Second, newWatchStyles(""))
	m = runCmd(t, m, m.Init())
	assert.True(t, strings.Contains(m.View().Content, "daemon down"))
}
