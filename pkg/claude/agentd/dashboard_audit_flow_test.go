package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Wire-shape mirror of agentd's /api/audit response — the Audit tab
// renders straight from these fields.
type auditResp struct {
	Entries         []auditEntryResp `json:"entries"`
	Page            int              `json:"page"`
	PageSize        int              `json:"page_size"`
	Total           int              `json:"total"`
	TotalUnfiltered int              `json:"total_unfiltered"`
	Sort            string           `json:"sort"`
	Dir             string           `json:"dir"`
	RetentionDays   int              `json:"retention_days"`
	PruningOn       bool             `json:"pruning_on"`
}
type auditEntryResp struct {
	ID              int64  `json:"id"`
	At              string `json:"at"`
	ActorKind       string `json:"actor_kind"`
	ActorAgent      string `json:"actor_agent"`
	ActorLabel      string `json:"actor_label"`
	Verb            string `json:"verb"`
	TargetAgent     string `json:"target_agent"`
	TargetLabel     string `json:"target_label"`
	GroupName       string `json:"group_name"`
	Detail          string `json:"detail"`
	Status          int    `json:"status"`
	Source          string `json:"source"`
	EventID         string `json:"event_id"`
	RelatedEventID  string `json:"related_event_id"`
	SessionID       string `json:"session_id"`
	Observer        string `json:"observer"`
	CauseKind       string `json:"cause_kind"`
	ObservedProcess string `json:"observed_process"`
	LaunchPhase     string `json:"launch_phase"`
	ExitCode        *int   `json:"exit_code"`
	Signal          string `json:"signal"`
	LifecycleAction string `json:"lifecycle_action"`
}

func fetchAudit(t *testing.T, mux http.Handler, query string) auditResp {
	t.Helper()
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/audit"+query, nil))
	require.Equal(t, http.StatusOK, rec.Code, "/api/audit body=%s", rec.Body.String())
	var out auditResp
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), "decode audit")
	return out
}

// Scenario: the Audit tab opens. It lists rows newest-first, surfaces the
// retention policy, and honours the verb + outcome filters.
func TestAuditEndpoint_ListsFiltersAndRetention(t *testing.T) {
	newFlow(t) // HOME + fresh DB; no config → default 30-day retention
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	_, err := db.InsertAuditLog(db.AuditLogEntry{
		ActorKind: db.AuditActorHuman, ActorLabel: "operator", Verb: "spawn",
		GroupName: "crew", TargetLabel: "worker", Status: 200, Source: db.AuditSourceCLI,
	})
	require.NoError(t, err)
	_, err = db.InsertAuditLog(db.AuditLogEntry{
		ActorKind: db.AuditActorAgent, ActorLabel: "po", Verb: "message",
		TargetLabel: "worker", Detail: "rebasing now", Status: 200, Source: db.AuditSourceCLI,
	})
	require.NoError(t, err)
	_, err = db.InsertAuditLog(db.AuditLogEntry{
		ActorKind: db.AuditActorAgent, ActorLabel: "intruder", Verb: "retire",
		TargetLabel: "worker", Status: 403, Source: db.AuditSourceCLI,
	})
	require.NoError(t, err)

	mux := agentd.BuildDashboardHandlerForTest()

	// All rows, newest-first.
	all := fetchAudit(t, mux, "")
	require.Len(t, all.Entries, 3)
	assert.Equal(t, "retire", all.Entries[0].Verb, "newest first")
	assert.Equal(t, "spawn", all.Entries[2].Verb)
	assert.Equal(t, 30, all.RetentionDays)
	assert.True(t, all.PruningOn)

	// Verb filter.
	msgs := fetchAudit(t, mux, "?verb=message")
	require.Len(t, msgs.Entries, 1)
	assert.Equal(t, "rebasing now", msgs.Entries[0].Detail)

	// Outcome filter — failures only.
	fails := fetchAudit(t, mux, "?outcome=failure")
	require.Len(t, fails.Entries, 1)
	assert.Equal(t, "retire", fails.Entries[0].Verb)
	assert.Equal(t, 403, fails.Entries[0].Status)

	// Search (server-side substring) — only the "rebasing now" message.
	search := fetchAudit(t, mux, "?q=rebasing")
	require.Len(t, search.Entries, 1)
	assert.Equal(t, "message", search.Entries[0].Verb)
	assert.Equal(t, 3, search.TotalUnfiltered, "total_unfiltered counts all rows even while searching")
}

// Scenario (PR3c-web): the Audit tab surfaces the stable agent_id of an
// enrolled actor/target, so the dashboard renders the command trail by the
// rotation-immune handle instead of a conv-id prefix.
func TestAuditEndpoint_SurfacesAgentID(t *testing.T) {
	newFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	const actorConv = "audit-actor-1111"
	const targetConv = "audit-target-2222"
	actorAgent, _, err := db.EnsureAgentForConv(actorConv, "spawn")
	require.NoError(t, err)
	targetAgent, _, err := db.EnsureAgentForConv(targetConv, "spawn")
	require.NoError(t, err)

	_, err = db.InsertAuditLog(db.AuditLogEntry{
		ActorKind: db.AuditActorAgent, ActorConv: actorConv, ActorLabel: "po",
		Verb: "message", TargetConv: targetConv, TargetLabel: "worker",
		Status: 200, Source: db.AuditSourceCLI,
	})
	require.NoError(t, err)

	mux := agentd.BuildDashboardHandlerForTest()
	got := fetchAudit(t, mux, "")
	require.Len(t, got.Entries, 1)
	assert.Equal(t, actorAgent, got.Entries[0].ActorAgent, "actor agent_id on the wire")
	assert.Equal(t, targetAgent, got.Entries[0].TargetAgent, "target agent_id on the wire")
}

func TestAuditEndpoint_SurfacesTypedExitFieldsAdditively(t *testing.T) {
	newFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	code := 0
	_, err := db.InsertAuditLog(db.AuditLogEntry{
		ActorKind: db.AuditActorSystem, ActorLabel: "tclaude",
		Verb: db.AuditVerbAgentExit, Status: 200, Source: db.AuditSourceTmux,
		EventID: "evt_1234567890abcdef12345678", SessionID: "session-exit",
		Observer: db.AgentExitObserverTmux, CauseKind: db.AgentExitCauseNormal,
		ObservedProcess: db.AgentExitObservedProcessPaneBootstrap,
		LaunchPhase:     db.AgentExitLaunchPhaseRuntime,
		ExitCode:        &code, LifecycleAction: db.AgentExitActionStop,
	})
	require.NoError(t, err)

	got := fetchAudit(t, agentd.BuildDashboardHandlerForTest(), "?verb="+db.AuditVerbAgentExit)
	require.Len(t, got.Entries, 1)
	entry := got.Entries[0]
	assert.Equal(t, "evt_1234567890abcdef12345678", entry.EventID)
	assert.Equal(t, "session-exit", entry.SessionID)
	assert.Equal(t, db.AgentExitObserverTmux, entry.Observer)
	assert.Equal(t, db.AgentExitCauseNormal, entry.CauseKind)
	assert.Equal(t, db.AgentExitObservedProcessPaneBootstrap, entry.ObservedProcess)
	assert.Equal(t, db.AgentExitLaunchPhaseRuntime, entry.LaunchPhase)
	require.NotNil(t, entry.ExitCode)
	assert.Zero(t, *entry.ExitCode)
	assert.Equal(t, db.AgentExitActionStop, entry.LifecycleAction)
}

func TestAuditEndpoint_LegacyCommandRowOmitsAdditiveExitFields(t *testing.T) {
	newFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_, err := db.InsertAuditLog(db.AuditLogEntry{
		ActorKind: db.AuditActorHuman, ActorLabel: "operator", Verb: "message",
		Status: 200, Source: db.AuditSourceCLI,
	})
	require.NoError(t, err)
	got := fetchAudit(t, agentd.BuildDashboardHandlerForTest(), "")
	require.Len(t, got.Entries, 1)
	assert.Empty(t, got.Entries[0].ObservedProcess)
	assert.Empty(t, got.Entries[0].LaunchPhase)
	assert.Nil(t, got.Entries[0].ExitCode)
}

// The endpoint pages and sorts server-side: a page_size of 1 returns one
// row with the correct pager totals, and sort=verb&dir=asc reorders.
func TestAuditEndpoint_PaginatesAndSorts(t *testing.T) {
	newFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	for _, v := range []string{"spawn", "message", "retire"} {
		_, err := db.InsertAuditLog(db.AuditLogEntry{
			ActorKind: db.AuditActorHuman, ActorLabel: "operator", Verb: v,
			Status: 200, Source: db.AuditSourceCLI,
		})
		require.NoError(t, err)
	}
	mux := agentd.BuildDashboardHandlerForTest()

	// Page 1 of size 1: one row, total 3, 3 pages.
	p1 := fetchAudit(t, mux, "?page_size=1&page=1")
	require.Len(t, p1.Entries, 1)
	assert.Equal(t, 1, p1.Page)
	assert.Equal(t, 1, p1.PageSize)
	assert.Equal(t, 3, p1.Total)
	assert.Equal(t, "retire", p1.Entries[0].Verb, "default sort is newest first")

	// A page past the end clamps back to the last page (no empty page).
	pLast := fetchAudit(t, mux, "?page_size=1&page=99")
	require.Len(t, pLast.Entries, 1)
	assert.Equal(t, 3, pLast.Page, "stale page clamps to the last page")
	assert.Equal(t, "spawn", pLast.Entries[0].Verb, "oldest row on the last page")

	// Sort by verb ascending.
	byVerb := fetchAudit(t, mux, "?sort=verb&dir=asc")
	require.Len(t, byVerb.Entries, 3)
	assert.Equal(t, "asc", byVerb.Dir)
	assert.Equal(t, "verb", byVerb.Sort)
	assert.Equal(t, "message", byVerb.Entries[0].Verb)
	assert.Equal(t, "spawn", byVerb.Entries[2].Verb)
}

// The endpoint refuses an uncookied request — same dashboard-auth gate
// as the rest of /api/*.
func TestAuditEndpoint_RequiresAuth(t *testing.T) {
	newFlow(t)
	mux := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(mux)

	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/audit", nil))
	assert.NotEqual(t, http.StatusOK, rec.Code, "uncookied /api/audit must be refused")
}
