package agentd_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// revoke takes a grant back.
func (h *peerProcessHarness) revoke(t *testing.T, slug, scope string) {
	t.Helper()
	rec := fedHuman(t, h.f, http.MethodDelete, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug, "scope": scope})
	require.Equal(t, 200, rec.Code, rec.Body.String())
}

// peerCronMessages is what the peer's jobs delivered to conv.
func peerCronMessages(t *testing.T, conv string) []*db.AgentMessage {
	t.Helper()
	msgs, err := db.ListAgentMessagesForConv(conv, 50)
	require.NoError(t, err)
	var out []*db.AgentMessage
	for _, m := range msgs {
		if strings.HasPrefix(m.Subject, "[cron") {
			out = append(out, m)
		}
	}
	return out
}

func createdJobID(t *testing.T, body string) int64 {
	t.Helper()
	var job struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &job))
	require.NotZero(t, job.ID)
	return job.ID
}

// A restricted peer schedules recurring messages here under its grants. The
// job is the peer's, never this node's operator's: it sends messages only,
// each fire rechecks the peer's grants, and what it delivers is the peer's
// mail under the untrusted-content banner.
func TestPeerOwnedCronJobs(t *testing.T) {
	h := newPeerProcessHarness(t)
	h.f.HaveGroup("other")
	h.grant(t, agentd.PermGroupsRosterRead, "group=crew")
	job := map[string]any{"name": "nudge", "target": "group:crew", "interval": "1h", "subject": "standup", "body": "post your status", "queue_when_offline": true}

	code, body := h.call("POST", "/v1/cron", job)
	require.Equal(t, 403, code, "no schedule grant: %s", body)
	h.grant(t, agentd.PermGroupsMessagesSchedule, "group=crew")
	for _, c := range []struct {
		body map[string]any
		code int
	}{
		{map[string]any{"target": "group:crew", "interval": "1h", "body": "x", "action_kind": "spawn"}, 403},
		{map[string]any{"target": "group:crew", "interval": "1h", "body": "x", "spawn_profile": "reviewer"}, 400},
		{map[string]any{"target": "group:crew", "interval": "1h", "body": "x", "owner": "proc-crew-member"}, 400},
		{map[string]any{"target": "group:other", "interval": "1h", "body": "x"}, 404},
		{map[string]any{"target": h.member, "interval": "1h", "body": "x"}, 403},
		{map[string]any{"target": "group:crew", "interval": "1s", "body": "x"}, 400},
	} {
		code, body := h.call("POST", "/v1/cron", c.body)
		require.Equal(t, c.code, code, "%v: %s", c.body, body)
	}

	code, body = h.call("POST", "/v1/cron", job)
	require.Equal(t, 200, code, body)
	id := createdJobID(t, body)
	path := "/v1/cron/" + strconv.FormatInt(id, 10)
	require.Contains(t, body, `"owner_peer":"`+h.peer.id.ID()+`"`)
	stored := operatorJobs(t, h)[id]
	require.Equal(t, h.peer.id.ID(), stored["owner_peer"])
	require.Nil(t, stored["operator_authored"], "never this node's operator's")
	require.Equal(t, "", stored["owner_conv"])

	// A fire delivers the peer's mail, bannered, never as this operator.
	code, body = h.call("POST", path+"/run-now", map[string]any{})
	require.Equal(t, 200, code, body)
	require.Contains(t, body, `"status":"ok"`)
	msgs := peerCronMessages(t, "proc-crew-member")
	require.Len(t, msgs, 1)
	require.False(t, msgs[0].OperatorAuthored)
	require.True(t, strings.HasPrefix(msgs[0].Body, "[remote message from operator@bob"), msgs[0].Body)
	require.Contains(t, msgs[0].Body, "post your status")
	require.Contains(t, msgs[0].Subject, "[cron:nudge] standup")

	// Its own job: changed, but never retargeted or turned into a spawn.
	code, body = h.call("PATCH", path, map[string]any{"body": "post your status, briefly"})
	require.Equal(t, 200, code, body)
	require.Contains(t, body, "briefly")
	code, body = h.call("PATCH", path, map[string]any{"target": "group:other"})
	require.Equal(t, 400, code, body)
	code, body = h.call("PATCH", path, map[string]any{"spawn_instruction_template": "x"})
	require.Equal(t, 400, code, body)

	// The grant taken back: nothing fires, by hand or on schedule, and the
	// run says why. The peer still sees and deletes its job.
	h.revoke(t, agentd.PermGroupsMessagesSchedule, "group=crew")
	code, body = h.call("POST", path+"/run-now", map[string]any{})
	require.Equal(t, 403, code, body)
	code, body = h.call("POST", path+"/enable", map[string]any{})
	require.Equal(t, 403, code, body)
	agentd.RunCronTickForTest(time.Now().Add(2 * time.Hour))
	agentd.WaitForBackgroundForTest()
	require.Len(t, peerCronMessages(t, "proc-crew-member"), 1, "nothing delivered once the grant is gone")
	code, body = h.call("GET", path+"/logs", nil)
	require.Equal(t, 200, code, body)
	require.Contains(t, body, `"status":"peer_denied"`)
	ids, _ := peerJobIDs(t, h)
	require.Equal(t, []int64{id}, ids, "its own job stays listed")
	code, body = h.call("DELETE", path, nil)
	require.Equal(t, http.StatusNoContent, code, body)
	require.NotContains(t, operatorJobs(t, h), id)
}

// A member-targeted job needs groups.members.schedule in every group the
// agent is in; unlinking the peer deletes the jobs it scheduled.
func TestPeerOwnedCronJobsForAMemberGoWithThePeer(t *testing.T) {
	h := newPeerProcessHarness(t)
	h.grant(t, agentd.PermGroupsRosterRead, "group=crew")
	h.grant(t, agentd.PermGroupsMembersSchedule, "group=crew")
	code, body := h.call("POST", "/v1/cron", map[string]any{"target": h.member, "interval": "1h", "body": "check in", "role": "dev"})
	require.Equal(t, 400, code, "a role filters a group: %s", body)
	code, body = h.call("POST", "/v1/cron", map[string]any{"target": h.member, "interval": "1h", "body": "check in", "run_immediately": true, "queue_when_offline": true})
	require.Equal(t, 200, code, body)
	id := createdJobID(t, body)
	require.Contains(t, body, `"last_run_status":"ok"`, "run_immediately fires once")
	msgs := peerCronMessages(t, "proc-crew-member")
	require.Len(t, msgs, 1)
	require.Contains(t, msgs[0].Body, "check in")

	ok, err := db.UntrustFederationPeer(h.peer.id.ID())
	require.NoError(t, err)
	require.True(t, ok)
	require.NotContains(t, operatorJobs(t, h), id, "the jobs a peer scheduled go with it")
}

// A peer's agent schedules under its target's grant and reaches only the
// jobs it scheduled: never this node's, nor its operator's.
func TestPeerOwnedCronJobsAsAgentPrincipal(t *testing.T) {
	h := newPeerProcessHarness(t)
	h.grant(t, agentd.PermGroupsRosterRead, "group=crew")
	h.grant(t, agentd.PermGroupsMessagesSchedule, "group=crew")
	worker := &proto.PeerViewAgent{ID: "agt_bobworker000000000000000", Name: "bob-worker", Groups: []string{"crew"}}
	call := func(method, path string, body any) (int, string) {
		reply := agentPeerViewExchange(t, h.peer, worker, method, path, body)
		return reply.Status, string(reply.Body)
	}
	local := operatorCronJob(t, h, map[string]any{"target": "group:crew", "interval": "1h", "body": "standup"})
	code, body := h.call("POST", "/v1/cron", map[string]any{"target": "group:crew", "interval": "1h", "body": "from bob's operator"})
	require.Equal(t, 200, code, body)
	operators := createdJobID(t, body)

	code, body = call("POST", "/v1/cron", map[string]any{"target": "group:crew", "interval": "1h", "body": "from bob's worker", "queue_when_offline": true})
	require.Equal(t, 200, code, body)
	mine := createdJobID(t, body)
	require.Equal(t, worker.ID, operatorJobs(t, h)[mine]["owner_peer_agent"])
	code, body = call("GET", "/v1/cron", nil)
	require.Equal(t, 200, code, body)
	require.Contains(t, body, `"id":`+strconv.FormatInt(mine, 10)+`,`)
	require.NotContains(t, body, "standup", "an agent lists only its own jobs")
	require.NotContains(t, body, "from bob's operator")
	for _, other := range []int64{local, operators} {
		code, body = call("POST", "/v1/cron/"+strconv.FormatInt(other, 10)+"/disable", map[string]any{})
		require.Equal(t, 404, code, body)
	}
	code, body = call("POST", "/v1/cron/"+strconv.FormatInt(mine, 10)+"/run-now", map[string]any{})
	require.Equal(t, 200, code, body)
	msgs := peerCronMessages(t, "proc-crew-member")
	require.Len(t, msgs, 1)
	require.True(t, strings.HasPrefix(msgs[0].Body, "[remote message from agent "+worker.ID+"@bob"), msgs[0].Body)

	// Its node's operator manages the job its agent scheduled.
	code, body = h.call("POST", "/v1/cron/"+strconv.FormatInt(mine, 10)+"/disable", map[string]any{})
	require.Equal(t, http.StatusNoContent, code, body)
	// With agent views turned off here, the agent's job no longer fires.
	code, body = h.call("POST", "/v1/cron/"+strconv.FormatInt(mine, 10)+"/enable", map[string]any{})
	require.Equal(t, http.StatusNoContent, code, body)
	rec := fedHuman(t, h.f, http.MethodPost, "/v1/federation/peers/agent-views", map[string]any{"instance": "bob", "policy": "deny"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	agentd.RunCronTickForTest(time.Now().Add(2 * time.Hour))
	agentd.WaitForBackgroundForTest()
	fromWorker := 0
	for _, m := range peerCronMessages(t, "proc-crew-member") {
		if strings.Contains(m.Body, "from bob's worker") {
			fromWorker++
		}
	}
	require.Equal(t, 1, fromWorker, "the agent's job no longer fires")
	code, body = h.call("GET", "/v1/cron/"+strconv.FormatInt(mine, 10)+"/logs", nil)
	require.Equal(t, 200, code, body)
	require.Contains(t, body, `"status":"peer_denied"`)
}
