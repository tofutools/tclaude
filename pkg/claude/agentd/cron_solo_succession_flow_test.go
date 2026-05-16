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
// job: when the job's target conversation has been reincarnated (the
// stored conv superseded, a fresh live conv-id taking over), firing the
// job must reach the LIVE successor, not the dead conv.
//
// fireCronJob's conv branch walks the succession chain
// (walkSuccession) before delivery — the same resolution the one-shot
// message path and the group fan-out (fanOutToGroup) already do.
// MigrateCronJobConvRef re-points target_conv eagerly at reincarnate
// time, but that is best-effort; the fire-time walk is the fallback
// that makes delivery succession-safe regardless.

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

// Scenario A: a conv-target cron job whose stored target_conv is a
// SUPERSEDED conv-id still delivers to the live successor. This is the
// fallback the fix adds — it models the case where the eager
// MigrateCronJobConvRef missed the row (best-effort, no retry) or a
// stale ref arrived via cross-machine sync. Without the fire-time
// walkSuccession the message would land in the dead conv's inbox and
// be silently lost.
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

	// The worker reincarnates: oldX is superseded, the live head is Y.
	r := f.Reincarnate(oldX, "fresh start")
	newY := r.NewConv
	require.NotEqual(t, oldX, newY, "reincarnation produced a fresh conv-id")

	// Model a missed eager migration: re-point the job's target_conv
	// back at the now-dead oldX. (Reincarnate's MigrateCronJobConvRef
	// already moved it to Y; this puts the row into exactly the stale
	// state the fire-time walk has to recover from.)
	stale := oldX
	n, err := db.UpdateAgentCronJobFields(id, db.UpdateCronPatch{TargetConv: &stale})
	require.NoError(t, err)
	require.Equal(t, 1, n, "re-staled the job's target_conv")

	require.Equal(t, "ok", fireCronNow(t, f, id), "fire status")

	// Delivery followed the succession chain to the live head.
	msg := findCronMsg(t, newY, "status please")
	assert.Equal(t, oldX, msg.OriginalToConv,
		"the row records the superseded conv the job addressed")
	assert.Equal(t, g.ID, msg.GroupID, "row stamped with the job's routing group")

	// Nothing landed in the dead conv's inbox.
	assert.Zero(t, msgRowCount(t, oldX), "no message delivered to the superseded conv")
}

// Scenario B: the solo (group_id=0, direct send-keys) delivery
// sub-path is succession-safe too. A solo job whose target_conv is a
// superseded conv-id sends keys into the LIVE successor's pane — before
// the fix it called pickAliveSession on the dead conv, found no pane,
// and silently no-op'd as "no_target".
func TestCronSoloSuccession_SoloSendKeys_FollowsChain(t *testing.T) {
	f := newFlow(t)

	const oldX = "css2-oldx-aaaa-bbbb-cccc-000000000001"
	const newY = "css2-newy-aaaa-bbbb-cccc-000000000002"

	// A solo, self-targeted cron job stored against the conv-id that is
	// about to be superseded. group_id 0 → the scheduler send-keys
	// directly into the target's pane.
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
}

// Scenario C: the end-to-end happy path — reincarnate the target
// between job creation and fire, delivery reaches the live agent. Here
// the eager MigrateCronJobConvRef carries it (target_conv is rewritten
// at reincarnate time); the fire-time walk is then a no-op. Documents
// that the two layers compose.
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

	// The eager migration re-pointed the stored target_conv at the live
	// successor — MigrateCronJobConvRef ran inside the reincarnate flow.
	job, err := db.GetAgentCronJob(id)
	require.NoError(t, err)
	require.NotNil(t, job)
	assert.Equal(t, newY, job.TargetConv,
		"reincarnate eagerly migrated the job's target_conv to the live conv")

	require.Equal(t, "ok", fireCronNow(t, f, id), "fire status")

	findCronMsg(t, newY, "daily check-in")
	assert.Zero(t, msgRowCount(t, oldX), "nothing delivered to the superseded conv")
}
