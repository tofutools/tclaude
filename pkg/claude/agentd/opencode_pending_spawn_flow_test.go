package agentd_test

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Flow coverage for the deferred OpenCode spawn path: the managed
// `opencode serve` boot dominates spawn latency and runs BEFORE the pane
// fork, so an async (dashboard) OpenCode spawn must not hold the spawn
// dialog open for it. Instead the endpoint reserves a pending_spawns row +
// stable actor id, answers within openCodeAsyncSpawnResponseGrace, and the
// launch continues in the background — claiming the reservation once the
// server-issued conv-id binds, or deleting it and surfacing the failure to
// the operator's Messages tab when the launch dies.
//
// These scenarios pin the three outcomes end to end through the production
// HTTP spawn endpoint, with only the OpenCode runtime boundary swapped
// (SetOpenCodeRuntimeForTest): inline fast path (warm server), pending →
// background enrollment, and pending → background failure.

// blockedOpenCodeRuntime swaps in an OpenCode runtime whose boot blocks
// until release closes, modelling a cold `opencode serve` start. failWith
// non-nil makes the boot fail (after release) instead of succeeding.
func blockedOpenCodeRuntime(t *testing.T, release <-chan struct{}, failWith error) {
	t.Cleanup(agentd.SetOpenCodeRuntimeForTest(func(sessionID, _, _, _ string) (agentd.OpenCodeRuntimeFixture, error) {
		<-release
		if failWith != nil {
			return agentd.OpenCodeRuntimeFixture{}, failWith
		}
		return agentd.OpenCodeRuntimeFixture{
			SessionID: sessionID,
			ConvID:    "ses_" + sessionID,
			ServerURL: "http://127.0.0.1:43210",
			Password:  "test-password",
			PID:       1234,
		}, nil
	}))
}

// Scenario: a fast OpenCode launch (the warm-server case — the default flow
// fixture returns instantly) still answers INLINE with its real conv-id, and
// leaves no pending_spawns residue: the deferred path's reservation is
// claimed by the launch-enrollment return before the response goes out.
func TestOpenCodeAgent_FastSpawnStillAnswersInline(t *testing.T) {
	f := newFlow(t)
	// A generous grace makes the inline outcome deterministic under
	// scheduler jitter; production keeps sub-second.
	t.Cleanup(agentd.SetOpenCodeAsyncSpawnResponseGraceForTest(30 * time.Second))

	g := f.HaveGroup("oc-crew")
	resp := f.AsHuman().SpawnWith("oc-crew", map[string]any{
		"name":    "oc-worker",
		"harness": "opencode",
	})
	require.Equal(t, 200, resp.Code, "inline spawn stays 200 (raw=%s)", resp.Raw)
	require.True(t, strings.HasPrefix(resp.ConvID, "ses_"), "fast launch returns the server-issued conv-id inline, got %q", resp.ConvID)
	require.True(t, strings.HasPrefix(resp.AgentID, db.AgentIDPrefix), "inline outcome carries the reserved stable identity")

	ps, err := db.GetPendingSpawn(resp.Label)
	require.NoError(t, err)
	assert.Nil(t, ps, "inline completion claims the reservation before responding")

	m, err := db.FindMemberInGroup(g.ID, resp.ConvID)
	require.NoError(t, err)
	require.NotNil(t, m, "inline completion enrolled the agent")
	boundAgentID, err := db.AgentIDForConv(resp.ConvID)
	require.NoError(t, err)
	assert.Equal(t, resp.AgentID, boundAgentID, "enrollment bound the reserved identity")
}

// Scenario: a cold OpenCode boot blows the response grace. The endpoint
// answers 200 with an EMPTY conv_id but a reserved agent_id + label, and a
// pending_spawns row is already visible — the dashboard's Pending list shows
// the agent while the server is still booting. Once the boot completes, the
// background continuation enrolls the agent under the reserved identity and
// clears the reservation, with no sweeper tick needed.
func TestOpenCodeAgent_SlowBootReturnsPendingThenEnrollsInBackground(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetOpenCodeAsyncSpawnResponseGraceForTest(20 * time.Millisecond))

	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	blockedOpenCodeRuntime(t, release, nil)

	g := f.HaveGroup("oc-crew")
	resp := f.AsHuman().SpawnWith("oc-crew", map[string]any{
		"name":            "oc-worker",
		"harness":         "opencode",
		"initial_message": "Audit the auth module for timing-safe comparison bugs",
	})
	require.Equal(t, 200, resp.Code, "pending spawn still returns 200 (raw=%s)", resp.Raw)
	require.Empty(t, resp.ConvID, "response must not wait for the OpenCode server boot")
	require.True(t, strings.HasPrefix(resp.AgentID, db.AgentIDPrefix), "pending response carries the reserved stable identity")
	require.NotEmpty(t, resp.Label, "pending spawn returns its label")

	ps, err := db.GetPendingSpawn(resp.Label)
	require.NoError(t, err)
	require.NotNil(t, ps, "the reservation is durably visible while the server boots")
	assert.Equal(t, resp.AgentID, ps.AgentID, "pending row persists the returned identity")
	assert.Equal(t, g.ID, ps.GroupID)
	assert.Equal(t, "oc-worker", ps.Name)

	// Nothing is enrolled before the server exists — no conv-id yet.
	assert.Empty(t, f.ListGroupMembers("oc-crew"), "no member before the launch completes")

	// The server comes up; the background continuation finishes the launch.
	releaseOnce.Do(func() { close(release) })

	convID := "ses_" + resp.Label
	f.AssertGroupMember("oc-crew", convID, "oc-worker", 10*time.Second)
	require.Eventually(t, func() bool {
		gone, err := db.GetPendingSpawn(resp.Label)
		return err == nil && gone == nil
	}, 10*time.Second, 20*time.Millisecond, "background enrollment clears the pending row")
	boundAgentID, err := db.AgentIDForConv(convID)
	require.NoError(t, err)
	assert.Equal(t, resp.AgentID, boundAgentID, "background enrollment binds the reserved identity")
}

// Scenario: the OpenCode server never becomes healthy. The spawn already
// answered Pending, so the failure must not strand a forever-Pending ghost:
// the background continuation deletes the reservation and surfaces the
// failure as a daemon-originated message in the operator's Messages tab —
// the remaining trace of what happened to the row they watched appear.
func TestOpenCodeAgent_BackgroundBootFailureClearsPendingAndNotifies(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetOpenCodeAsyncSpawnResponseGraceForTest(20 * time.Millisecond))

	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	blockedOpenCodeRuntime(t, release, errors.New("port already in use"))

	f.HaveGroup("oc-crew")
	resp := f.AsHuman().SpawnWith("oc-crew", map[string]any{
		"name":    "doomed-worker",
		"harness": "opencode",
	})
	require.Equal(t, 200, resp.Code, "the failure lands after the pending response (raw=%s)", resp.Raw)
	require.Empty(t, resp.ConvID)
	require.NotEmpty(t, resp.Label)

	releaseOnce.Do(func() { close(release) })
	agentd.WaitForBackgroundForTest()

	gone, err := db.GetPendingSpawn(resp.Label)
	require.NoError(t, err)
	assert.Nil(t, gone, "a failed deferred launch must not leave a forever-Pending row")
	assert.Empty(t, f.ListGroupMembers("oc-crew"), "nothing was enrolled")

	msgs, err := db.ListHumanMessages()
	require.NoError(t, err)
	found := false
	for _, m := range msgs {
		if strings.Contains(m.Subject, "doomed-worker") && strings.Contains(m.Body, "port already in use") {
			found = true
			assert.Empty(t, m.FromConv, "the failure notice is daemon-originated, not agent-sent")
		}
	}
	assert.True(t, found, "the operator gets a Messages-tab notice for the failed spawn; have %+v", msgs)
}
