package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// These tests cover succession-safe delivery for a CONV-target cron
// job: when the job's target conversation has been reincarnated (a fresh
// live conv-id taking over), firing the job must reach the LIVE
// successor, not the dead conv.
//
// Two layers make this safe. Since JOH-26 PR3a the job stores the
// target's stable agent_id, which the fire path resolves to the actor's
// CURRENT conv on every tick — so a reincarnated target tracks the live
// generation automatically, with no stored ref to go stale. And
// fireCronJob's conv branch still walks the succession chain
// (walkSuccession) before delivery — the same resolution the one-shot
// message path and the group fan-out (fanOutToGroup) do — as
// defence-in-depth for a RecordConvSuccession-only edge that never
// advanced the actor's current-conv pointer (Scenario B).

// createCronJobAsHuman POSTs /v1/cron as the human peer (who bypasses
// the auth gate) and returns the new job's id.
func createCronJobAsHuman(t *testing.T, f *testharness.Flow, body map[string]any) int64 {
	t.Helper()
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(
		testharness.JSONRequest(t, http.MethodPost, "/v1/cron", body)))
	require.Equal(t, http.StatusOK, rec.Code, "POST /v1/cron body=%s", rec.Body.String())
	var resp struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "decode create resp")
	require.NotZero(t, resp.ID, "created job id")
	return resp.ID
}

// findCronMsg returns the single agent_messages row delivered to conv
// whose body matches; fails unless there is exactly one. A reincarnated
// conv also carries the reincarnate handoff row, so the cron message is
// identified by its body rather than by being the only row in the inbox.
func findCronMsg(t *testing.T, conv, body string) *db.AgentMessage {
	t.Helper()
	rows, err := db.ListAgentMessagesForConv(conv, 100)
	require.NoError(t, err, "ListAgentMessagesForConv(%s)", conv)
	var match []*db.AgentMessage
	for _, m := range rows {
		if m.Body == body {
			match = append(match, m)
		}
	}
	require.Len(t, match, 1,
		"exactly one cron message with body %q delivered to %s", body, conv)
	return match[0]
}

// Scenario A: a conv-target cron job whose target reincarnates still
// delivers to the live successor. Under the agent-keyed cutover (JOH-26
// PR3a) the job stores the target's stable agent_id, which the fire path
// resolves to the actor's CURRENT conv every tick — so the target can
// never go stale, and delivery tracks the live generation directly (no
// succession redirect needed, so the row carries no Original-To header).
func TestCronSoloSuccession_StaleRef_DeliversToLiveHead(t *testing.T) {
	f := newFlow(t)

	g := f.HaveGroup("team")
	const po = "css1-popo-aaaa-bbbb-cccc-000000000001"
	const oldX = "css1-oldx-aaaa-bbbb-cccc-000000000002"
	f.HaveConvWithTitle(po, "po-agent")
	f.HaveConvWithTitle(oldX, "worker")
	f.HaveMember("team", po)
	f.HaveMember("team", oldX)
	f.HaveAliveSession(oldX, "spwn-css1-oldx", "tclaude-spwn-css1-oldx", "/tmp/work")

	// Human schedules a recurring nudge for the worker, attributed to
	// the PO. owner(po) != target(oldX) and they share "team", so the
	// job is conv-target, group-routed (non-zero group_id).
	id := createCronJobAsHuman(t, f, map[string]any{
		"target":   oldX,
		"owner":    po,
		"interval": "10m",
		"body":     "status please",
	})

	// The worker reincarnates: oldX is superseded, the live head is Y. The
	// job's target_agent never moves; the fire path resolves it to the actor's
	// new current conv.
	r := f.Reincarnate(oldX, "fresh start")
	newY := r.NewConv
	require.NotEqual(t, oldX, newY, "reincarnation produced a fresh conv-id")

	require.Equal(t, "ok", fireCronNow(t, f, id), "fire status")

	// Delivery reached the live head, addressed directly (the agent-keyed
	// target resolved straight to the current conv — never a stale ref), so the
	// row carries no Original-To redirect header.
	msg := findCronMsg(t, newY, "status please")
	assert.Empty(t, msg.OriginalToConv,
		"agent-keyed target resolves to the current conv; no succession redirect")
	assert.Equal(t, g.ID, msg.GroupID, "row stamped with the job's routing group")

	// Nothing landed in the dead conv's inbox.
	assert.Zero(t, msgRowCount(t, oldX), "no message delivered to the superseded conv")
}

// Scenario B: direct group_id=0 inbox delivery is succession-safe too. A solo
// job whose target_conv is superseded queues mail for the live successor.
func TestCronSoloSuccession_DirectInbox_FollowsChain(t *testing.T) {
	f := newFlow(t)

	const oldX = "css2-oldx-aaaa-bbbb-cccc-000000000001"
	const newY = "css2-newy-aaaa-bbbb-cccc-000000000002"

	// A solo, self-targeted cron job stored against the conv-id that is
	// about to be superseded. group_id 0 means direct inbox mail.
	id, err := db.InsertAgentCronJob(&db.AgentCronJob{
		Name:            "self-nudge",
		OwnerConv:       oldX,
		TargetKind:      db.CronTargetConv,
		TargetConv:      oldX,
		GroupID:         0,
		IntervalSeconds: 600,
		Body:            "remember to check the build",
		Enabled:         true,
	})
	require.NoError(t, err, "InsertAgentCronJob")

	// oldX is superseded by the live head newY; only newY has a pane.
	require.NoError(t, db.RecordConvSuccession(oldX, newY, "reincarnate"))
	f.HaveAliveSession(newY, "spwn-css2-newy", "tclaude-spwn-css2-newy", "/tmp/work")

	require.Equal(t, "ok", fireCronNow(t, f, id),
		"solo fire resolves the chain and finds the live pane")

	// The body was send-keys'd into the LIVE successor's pane.
	f.AssertSentContains("tclaude-spwn-css2-newy:0.0",
		"remember to check the build", 2*time.Second)
	findCronMsg(t, newY, "remember to check the build")
}

func TestCronSolo_OfflineTargetIsDiscardedUnlessJobOptsIntoQueue(t *testing.T) {
	f := newFlow(t)
	const target = "csol-offl-aaaa-bbbb-cccc-000000000001"
	f.HaveConvWithTitle(target, "offline-worker")
	f.HaveEnrolledAgent(target)

	id, err := db.InsertAgentCronJob(&db.AgentCronJob{
		Name: "offline-nudge", OwnerConv: target,
		TargetKind: db.CronTargetConv, TargetConv: target,
		IntervalSeconds: 600, Body: "discard stale tick", Enabled: true,
	})
	require.NoError(t, err)

	require.Equal(t, "skipped_offline", fireCronNow(t, f, id))
	assert.Zero(t, msgRowCount(t, target), "default offline tick creates no inbox debt")

	queuedID, err := db.InsertAgentCronJob(&db.AgentCronJob{
		Name: "offline-durable", OwnerConv: target,
		TargetKind: db.CronTargetConv, TargetConv: target,
		IntervalSeconds: 600, Body: "check this after resume", Enabled: true,
		QueueWhenOffline: true,
	})
	require.NoError(t, err)
	require.Equal(t, "ok", fireCronNow(t, f, queuedID))
	msg := findCronMsg(t, target, "check this after resume")
	assert.True(t, msg.DeliveredAt.IsZero(), "offline delivery remains queued")
}

// Scenario C: the end-to-end happy path — reincarnate the target
// between job creation and fire, delivery reaches the live agent. The
// stored target_agent is stable, so GetAgentCronJob resolves it to the
// actor's new current conv with no rewrite needed; the fire-time walk is
// then a no-op.
func TestCronSoloSuccession_EndToEnd_ReincarnateBetweenCreateAndFire(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("team")
	const po = "css3-popo-aaaa-bbbb-cccc-000000000001"
	const oldX = "css3-oldx-aaaa-bbbb-cccc-000000000002"
	f.HaveConvWithTitle(po, "po-agent")
	f.HaveConvWithTitle(oldX, "worker")
	f.HaveMember("team", po)
	f.HaveMember("team", oldX)
	f.HaveAliveSession(oldX, "spwn-css3-oldx", "tclaude-spwn-css3-oldx", "/tmp/work")

	id := createCronJobAsHuman(t, f, map[string]any{
		"target":   oldX,
		"owner":    po,
		"interval": "10m",
		"body":     "daily check-in",
	})

	// Reincarnate the target between create and fire.
	r := f.Reincarnate(oldX, "fresh start")
	newY := r.NewConv

	// The stored target_agent is stable; GetAgentCronJob resolves it to the
	// actor's current conv, which reincarnate advanced to the live successor.
	job, err := db.GetAgentCronJob(id)
	require.NoError(t, err)
	require.NotNil(t, job)
	assert.Equal(t, newY, job.TargetConv,
		"target_agent resolves to the actor's live current conv")

	require.Equal(t, "ok", fireCronNow(t, f, id), "fire status")

	findCronMsg(t, newY, "daily check-in")
	assert.Zero(t, msgRowCount(t, oldX), "nothing delivered to the superseded conv")
}
