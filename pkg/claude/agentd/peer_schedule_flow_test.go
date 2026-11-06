package agentd_test

import (
	"net/http"
	"strconv"
	"testing"

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
