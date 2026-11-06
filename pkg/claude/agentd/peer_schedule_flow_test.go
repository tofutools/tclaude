package agentd_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// operatorCronJob creates a cron job as this node's operator and returns its
// id.
func operatorCronJob(t *testing.T, h *peerProcessHarness, body map[string]any) int64 {
	t.Helper()
	rec := fedHuman(t, h.f, http.MethodPost, "/v1/cron", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var job struct {
		ID int64 `json:"id"`
	}
	testharness.DecodeJSON(t, rec, &job)
	require.NotZero(t, job.ID)
	return job.ID
}

// operatorTrigger stores a trigger rule as this node's operator would author
// it on its dashboard; groupID 0 makes it global.
func operatorTrigger(t *testing.T, name string, groupID int64) int64 {
	t.Helper()
	rule := &db.TriggerRule{Name: name, Enabled: true, OperatorAuthored: true, ScopeKind: db.TriggerScopeGlobal,
		Source: db.TriggerSourcePROpened, DraftFilter: db.TriggerDraftExclude,
		Actions: []db.TriggerAction{{Type: db.TriggerActionSpawn, Spawn: &db.TriggerSpawnAction{Profile: "reviewer",
			NameTemplate: "review-{{pr.number}}", InstructionTemplate: "Review {{pr.url}} with the secret checklist.", MaxLiveWorkers: 2}}}}
	if groupID > 0 {
		rule.ScopeKind, rule.GroupID = db.TriggerScopeGroup, groupID
	}
	id, err := db.InsertTriggerRule(rule)
	require.NoError(t, err)
	return id
}

// Pins: a restricted peer without scheduling or trigger grants reaches none
// of this node's cron jobs or trigger rules, and never authors, changes or
// fires one.
func TestPeerSchedulePins(t *testing.T) {
	h := newPeerProcessHarness(t)
	h.grant(t, agentd.PermGroupsRosterRead, "group=crew")
	job := strconv.FormatInt(operatorCronJob(t, h, map[string]any{"target": "group:crew", "interval": "1h", "body": "standup"}), 10)
	rule := strconv.FormatInt(operatorTrigger(t, "reviews", h.group.ID), 10)
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/v1/cron", nil},
		{"GET", "/v1/cron/" + job + "/logs", nil},
		{"POST", "/v1/cron/" + job + "/disable", map[string]any{}},
		{"POST", "/v1/cron/" + job + "/enable", map[string]any{}},
		{"POST", "/v1/cron/" + job + "/run-now", map[string]any{}},
		{"PATCH", "/v1/cron/" + job, map[string]any{"body": "x"}},
		{"DELETE", "/v1/cron/" + job, nil},
		{"POST", "/v1/cron", map[string]any{"target": "group:crew", "interval": "1h", "body": "x"}},
		{"POST", "/api/cron", map[string]any{"target": "group:crew", "interval": "1h", "body": "x"}},
		{"POST", "/api/cron/" + job + "/disable", map[string]any{}},
		{"GET", "/v1/triggers", nil},
		{"GET", "/v1/triggers/" + rule, nil},
		{"GET", "/api/triggers", nil},
		{"POST", "/api/triggers", map[string]any{"name": "x"}},
		{"POST", "/api/triggers/" + rule + "/disable", map[string]any{}},
	} {
		code, body := h.call(c.method, c.path, c.body)
		require.Equal(t, 403, code, "%s %s: %s", c.method, c.path, body)
	}
}

// operatorJobs reads this node's cron jobs as its operator, by id.
func operatorJobs(t *testing.T, h *peerProcessHarness) map[int64]map[string]any {
	t.Helper()
	rec := fedHuman(t, h.f, http.MethodGet, "/v1/cron", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var out struct {
		Jobs []map[string]any `json:"jobs"`
	}
	testharness.DecodeJSON(t, rec, &out)
	byID := map[int64]map[string]any{}
	for _, j := range out.Jobs {
		byID[int64(j["id"].(float64))] = j
	}
	return byID
}

// peerJobIDs lists the cron jobs the peer sees.
func peerJobIDs(t *testing.T, h *peerProcessHarness) ([]int64, string) {
	t.Helper()
	code, body := h.call("GET", "/v1/cron", nil)
	require.Equal(t, 200, code, body)
	var out struct {
		Jobs []struct {
			ID int64 `json:"id"`
		} `json:"jobs"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	ids := []int64{}
	for _, j := range out.Jobs {
		ids = append(ids, j.ID)
	}
	return ids, body
}

// A restricted peer lists, reads and disables this node's cron jobs in its
// reach, and nothing else: a group job under groups.messages.schedule there,
// a member's job under groups.members.schedule in every group it is in.
func TestPeerCronJobsUnderGrants(t *testing.T) {
	h := newPeerProcessHarness(t)
	h.f.HaveGroup("other")
	h.grant(t, agentd.PermGroupsRosterRead, "group=crew")
	group := operatorCronJob(t, h, map[string]any{"target": "group:crew", "interval": "1h", "body": "standup"})
	member := operatorCronJob(t, h, map[string]any{"target": h.member, "interval": "1h", "body": "check in"})
	hidden := operatorCronJob(t, h, map[string]any{"target": "group:other", "interval": "1h", "body": "not yours"})
	rec := fedHuman(t, h.f, http.MethodPost, "/v1/cron/"+strconv.FormatInt(group, 10)+"/run-now", map[string]any{})
	require.Equal(t, 200, rec.Code, rec.Body.String())

	h.grant(t, agentd.PermGroupsMessagesSchedule, "group=crew")
	ids, body := peerJobIDs(t, h)
	require.Equal(t, []int64{group}, ids, "only the group job: the member job needs groups.members.schedule")
	require.Contains(t, body, `"body":"standup"`)
	require.NotContains(t, body, "owner_conv", "conv ids stay this node's")
	require.NotContains(t, body, "target_conv")

	h.grant(t, agentd.PermGroupsMembersSchedule, "group=crew")
	ids, _ = peerJobIDs(t, h)
	require.ElementsMatch(t, []int64{group, member}, ids)

	// Runs: their statuses, never their error text.
	code, body := h.call("GET", "/v1/cron/"+strconv.FormatInt(group, 10)+"/logs", nil)
	require.Equal(t, 200, code, body)
	require.Contains(t, body, `"status":`)
	require.NotContains(t, body, "error_msg")
	code, body = h.call("GET", "/v1/cron/"+strconv.FormatInt(hidden, 10)+"/logs", nil)
	require.Equal(t, 404, code, "a job out of reach: %s", body)

	// Disable, which is reversible; nothing that authors or fires.
	code, body = h.call("POST", "/v1/cron/"+strconv.FormatInt(member, 10)+"/disable", map[string]any{})
	require.Equal(t, http.StatusNoContent, code, body)
	require.Equal(t, false, operatorJobs(t, h)[member]["enabled"])
	code, body = h.call("POST", "/v1/cron/"+strconv.FormatInt(hidden, 10)+"/disable", map[string]any{})
	require.Equal(t, 404, code, body)
	require.Equal(t, true, operatorJobs(t, h)[hidden]["enabled"])
	for _, c := range []struct{ method, path string }{
		{"POST", "/v1/cron/" + strconv.FormatInt(member, 10) + "/enable"},
		{"POST", "/v1/cron/" + strconv.FormatInt(group, 10) + "/run-now"},
		{"PATCH", "/v1/cron/" + strconv.FormatInt(group, 10)},
		{"DELETE", "/v1/cron/" + strconv.FormatInt(group, 10)},
		{"POST", "/v1/cron"},
	} {
		code, body := h.call(c.method, c.path, map[string]any{"target": "group:crew", "interval": "1h", "body": "x"})
		require.Equal(t, 403, code, "%s %s: %s", c.method, c.path, body)
	}
	require.Len(t, operatorJobs(t, h), 3, "nothing created or deleted")

	// Unrestricted trust reads as this node's operator.
	h.trustUnrestricted(t)
	ids, body = peerJobIDs(t, h)
	require.ElementsMatch(t, []int64{group, member, hidden}, ids)
	require.Contains(t, body, "owner_conv")
}

// A restricted peer reads the trigger rules of a visible group it holds
// groups.triggers.read on, without spawn instructions or error detail; global
// rules and other groups' stay this node's, and no rule is authored.
func TestPeerTriggersUnderGrant(t *testing.T) {
	h := newPeerProcessHarness(t)
	other := h.f.HaveGroup("other")
	h.grant(t, agentd.PermGroupsRosterRead, "group=crew")
	crew := operatorTrigger(t, "crew-reviews", h.group.ID)
	global := operatorTrigger(t, "global-reviews", 0)
	hidden := operatorTrigger(t, "other-reviews", other.ID)
	// A PR by a crew member fires the crew rule; its spawn fails (there is
	// no reviewer profile), with detail the peer must not read.
	_, err := db.UpsertAgentPRDetails(h.member, "https://github.com/o/r/pull/7", "ready", "open", "topic", false)
	require.NoError(t, err)
	agentd.RunTriggerTickForTest(time.Now())
	agentd.WaitForBackgroundForTest()
	firings, err := db.ListTriggerFirings(crew, 10)
	require.NoError(t, err)
	require.NotEmpty(t, firings)
	require.NotEmpty(t, firings[0].Actions)
	detail := firings[0].Actions[0].Detail
	require.NotEmpty(t, detail)
	outcome := firings[0].Actions[0].Outcome

	code, body := h.call("GET", "/v1/triggers", nil)
	require.Equal(t, 403, code, "no grant: %s", body)
	h.grant(t, agentd.PermGroupsTriggersRead, "group=crew")
	code, body = h.call("GET", "/v1/triggers", nil)
	require.Equal(t, 200, code, body)
	require.Contains(t, body, "crew-reviews")
	require.NotContains(t, body, "global-reviews")
	require.NotContains(t, body, "other-reviews")
	require.Contains(t, body, "review-{{pr.number}}")
	require.Contains(t, body, `"outcome":"`+outcome+`"`)
	require.NotContains(t, body, "secret checklist", "a spawn's instruction template stays this node's")
	require.NotContains(t, body, detail, "firing detail stays this node's")
	require.NotContains(t, body, "github.com/o/r/pull/7", "a firing's event names the PR, which only groups.members.pr reads")

	code, body = h.call("GET", "/v1/triggers/"+strconv.FormatInt(crew, 10), nil)
	require.Equal(t, 200, code, body)
	require.Contains(t, body, `"firings":[`)
	require.NotContains(t, body, "secret checklist")
	require.NotContains(t, body, detail)
	for _, id := range []int64{global, hidden} {
		code, body = h.call("GET", "/v1/triggers/"+strconv.FormatInt(id, 10), nil)
		require.Equal(t, 404, code, body)
	}
	code, body = h.call("POST", "/api/triggers/"+strconv.FormatInt(crew, 10)+"/disable", map[string]any{})
	require.Equal(t, 403, code, "trigger rules are this node's operator's: %s", body)

	h.trustUnrestricted(t)
	code, body = h.call("GET", "/v1/triggers", nil)
	require.Equal(t, 200, code, body)
	require.Contains(t, body, "global-reviews")
	require.Contains(t, body, "secret checklist")
}

// The member rule holds in every group the agent is in, with
// groups.members.schedule alone; a job's owner and routing group are named
// only when the peer sees them; and a peer's disable keeps an automatic
// pause's marker, so the job still comes back.
func TestPeerCronReachAndReversibleDisable(t *testing.T) {
	h := newPeerProcessHarness(t)
	h.f.HaveGroup("side")
	h.f.HaveGroup("leads")
	lead := havePerformerAgent(t, h.f, "leads", "proc-lead", "lead", "lead", "idle")
	both := havePerformerAgent(t, h.f, "crew", "proc-both", "both", "dev", "idle")
	h.f.HaveMemberWithRole("side", "proc-both", "dev")
	h.grant(t, agentd.PermGroupsRosterRead, "group=crew")
	h.grant(t, agentd.PermGroupsRosterRead, "group=side")
	group := operatorCronJob(t, h, map[string]any{"target": "group:crew", "interval": "1h", "body": "standup", "owner": "proc-lead"})
	member := operatorCronJob(t, h, map[string]any{"target": h.member, "interval": "1h", "body": "check in", "owner": "proc-lead"})
	inSide := operatorCronJob(t, h, map[string]any{"target": both, "interval": "1h", "body": "two groups"})

	// groups.members.schedule alone: the member's job, not the group's, and
	// not one whose target is also in a group the grant leaves out.
	h.grant(t, agentd.PermGroupsMembersSchedule, "group=crew")
	ids, body := peerJobIDs(t, h)
	require.Equal(t, []int64{member}, ids)
	require.Equal(t, lead, operatorJobs(t, h)[member]["owner_agent"])
	require.NotContains(t, body, lead, "the owner sits in a group the peer cannot see")
	code, body := h.call("POST", "/v1/cron/"+strconv.FormatInt(inSide, 10)+"/disable", map[string]any{})
	require.Equal(t, 404, code, body)
	h.grant(t, agentd.PermGroupsMembersSchedule, "group=side")
	ids, _ = peerJobIDs(t, h)
	require.ElementsMatch(t, []int64{member, inSide}, ids)

	// A retire emptied the crew: its rhythm is paused automatically. The
	// peer's disable leaves that pause as it is, so the resume brings it back.
	h.grant(t, agentd.PermGroupsMessagesSchedule, "group=crew")
	n, err := db.DisableGroupTargetCronJobsForRetire(h.group.ID)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	code, body = h.call("POST", "/v1/cron/"+strconv.FormatInt(group, 10)+"/disable", map[string]any{})
	require.Equal(t, http.StatusNoContent, code, body)
	n, err = db.ReenableGroupRetiredCronJobs(h.group.ID)
	require.NoError(t, err)
	require.Equal(t, 1, n, "the automatic pause survived the peer's disable")
	require.Equal(t, true, operatorJobs(t, h)[group]["enabled"])
	// An enabled job is paused by hand: a resume leaves it off.
	code, body = h.call("POST", "/v1/cron/"+strconv.FormatInt(group, 10)+"/disable", map[string]any{})
	require.Equal(t, http.StatusNoContent, code, body)
	_, err = db.ReenableGroupRetiredCronJobs(h.group.ID)
	require.NoError(t, err)
	require.Equal(t, false, operatorJobs(t, h)[group]["enabled"])
}
