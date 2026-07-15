package agentd_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Flow coverage for the scribe-summon endpoint behind the dashboard's "Edit
// with agent" buttons (JOH-361): a human summon spawns a pre-briefed,
// pre-granted scribe; an unscoped repeat click starts another fresh agent;
// an exact structured scope safely reuses a compatible live agent; and
// an agent caller is gated exactly like the spawn path
// (groups.spawn + — because a summon carries birth-time grants —
// permissions.grant). Asserted at the real surfaces the dashboard reads:
// db.ListAgentGroupMembers (the agent listing) and
// db.ListAgentPermissionOverridesForConv (the granted slugs).

// scribeSummonResp is the decoded /v1/scribe response.
type scribeSummonResp struct {
	Name      string `json:"name"`
	ConvID    string `json:"conv_id"`
	Reused    bool   `json:"reused"`
	FocusMode string `json:"focus_mode"`
}

// stubScribeTerminal records how many times a scribe window was opened and
// returns success (native), so summonScribe's auto-focus never touches a real
// terminal.
func stubScribeTerminal(t *testing.T) *int {
	t.Helper()
	var opens int
	t.Cleanup(agentd.SetOpenTerminalForTest(func(string) error {
		opens++
		return nil
	}))
	return &opens
}

func decodeScribeResp(t *testing.T, rec *httptest.ResponseRecorder) scribeSummonResp {
	t.Helper()
	var resp scribeSummonResp
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "decode /v1/scribe body=%s", rec.Body.String())
	return resp
}

type permissionAuditRow struct {
	GrantedAt string
	GrantedBy string
}

func permissionAuditRows(t *testing.T, conv string) map[string]permissionAuditRow {
	t.Helper()
	agentID, err := db.AgentIDForConv(conv)
	require.NoError(t, err)
	require.NotEmpty(t, agentID)
	d, err := db.Open()
	require.NoError(t, err)
	rows, err := d.Query(`SELECT slug, granted_at, granted_by FROM agent_permissions
		WHERE agent_id = ? ORDER BY slug`, agentID)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]permissionAuditRow{}
	for rows.Next() {
		var slug string
		var row permissionAuditRow
		require.NoError(t, rows.Scan(&slug, &row.GrantedAt, &row.GrantedBy))
		out[slug] = row
	}
	require.NoError(t, rows.Err())
	return out
}

// Scenario: a human summons a uniquely named scribe. It comes up in the
// shared scribe-kind group, holding exactly the requested slug, and its window
// is opened.
func TestScribeSummon_HumanCreatesGrantedScribe(t *testing.T) {
	// The scribe=true wire assertion below fetches /api/snapshot, whose
	// auth pins the Origin to popupBaseURL — set it so the test handler
	// injects a matching Origin (same shim the group-descr snapshot tests use).
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	opens := stubScribeTerminal(t)

	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe",
		map[string]any{
			"name":  "circle-scribe",
			"slugs": []string{agentd.PermTemplatesManage},
			"brief": "You edit summoning circles on this daemon. Discover them with `tclaude agent templates ls`.",
		})))
	require.Equalf(t, http.StatusOK, rec.Code, "summon body=%s", rec.Body.String())

	resp := decodeScribeResp(t, rec)
	require.NotEmpty(t, resp.ConvID, "summon returned a conv-id")
	assert.Regexp(t, `^circle-scribe-[0-9a-f]{8}$`, resp.Name, "summon returns the unique agent name")
	assert.Equal(t, resp.Name, agent.FreshTitle(resp.ConvID), "the generated name is persisted on the agent")
	assert.False(t, resp.Reused, "a first summon is a fresh spawn, not a reuse")

	// The shared scribe-kind group exists and initially holds one member — the
	// agent-listing surface the dashboard renders.
	g, err := db.GetAgentGroupByName("circle-scribe")
	require.NoError(t, err)
	require.NotNil(t, g, "scribe group was created")
	members, err := db.ListAgentGroupMembers(g.ID)
	require.NoError(t, err)
	require.Len(t, members, 1, "exactly one scribe in the group")
	assert.Equal(t, resp.ConvID, members[0].ConvID, "the member is the summoned scribe")

	// The requested slug is a real persisted grant.
	overrides, err := db.ListAgentPermissionOverridesForConv(resp.ConvID)
	require.NoError(t, err)
	assert.Equal(t, "grant", overrides[agentd.PermTemplatesManage], "templates.manage granted at birth")

	// The snapshot flags the shared scribe-kind group as `scribe` — the wire
	// bit the Groups tab keys off to always show it while live (and to hide the
	// dormant group unless the human enables "show offline scribes").
	assert.True(t, dashGroupByName(t, "circle-scribe").Scribe, "snapshot marks the scribe group scribe=true")

	// The scribe's window was opened.
	assert.Equal(t, 1, *opens, "summon opened the scribe's terminal window")
}

// The group-name vocabulary is intentionally broader than the safe agent-name
// vocabulary. A summon normalizes and truncates only the generated agent name,
// preserving the caller's base as the group key while guaranteeing the suffix
// fits the launch/rename gate.
func TestScribeSummon_UniqueNameClearsAgentNameGate(t *testing.T) {
	tests := []struct {
		name       string
		base       string
		wantPrefix string
	}{
		{name: "max length base", base: strings.Repeat("a", agent.MaxSpawnNameLen), wantPrefix: strings.Repeat("a", 55) + "-"},
		{name: "unicode and spaces", base: "scribe café", wantPrefix: "scribe-caf-"},
		{name: "no safe characters", base: "🎉", wantPrefix: "scribe-"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlow(t)
			stubScribeTerminal(t)
			rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe",
				map[string]any{
					"name":  tc.base,
					"slugs": []string{agentd.PermTemplatesManage},
					"brief": "Edit templates.",
				})))
			require.Equalf(t, http.StatusOK, rec.Code, "summon body=%s", rec.Body.String())
			resp := decodeScribeResp(t, rec)
			assert.Len(t, resp.Name, len(tc.wantPrefix)+8)
			assert.True(t, strings.HasPrefix(resp.Name, tc.wantPrefix), resp.Name)
			assert.LessOrEqual(t, len(resp.Name), agent.MaxSpawnNameLen)
			assert.Equal(t, resp.Name, agent.FreshTitle(resp.ConvID), "generated name persisted")
		})
	}
}

// Scenario: a repeat click spawns an independently named scribe while the
// first remains alive for parallel editing.
func TestScribeSummon_RepeatSummonKeepsPriorScribeAlive(t *testing.T) {
	f := newFlow(t)
	opens := stubScribeTerminal(t)

	summon := func() scribeSummonResp {
		rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe",
			map[string]any{
				"name":  "circle-scribe",
				"slugs": []string{agentd.PermTemplatesManage},
				"brief": "Edit the circle named feature-team.",
			})))
		require.Equalf(t, http.StatusOK, rec.Code, "summon body=%s", rec.Body.String())
		return decodeScribeResp(t, rec)
	}

	first := summon()
	assert.False(t, first.Reused, "first summon spawns")

	second := summon()
	assert.False(t, second.Reused, "every summon is a fresh spawn")
	assert.NotEqual(t, first.ConvID, second.ConvID, "repeat summon creates a new conversation")
	assert.NotEqual(t, first.Name, second.Name, "each scribe has a unique display name")
	assert.Equal(t, first.Name, agent.FreshTitle(first.ConvID))
	assert.Equal(t, second.Name, agent.FreshTitle(second.ConvID))

	// Both scribes remain in their shared kind-group and can work concurrently.
	g, err := db.GetAgentGroupByName("circle-scribe")
	require.NoError(t, err)
	require.NotNil(t, g)
	members, err := db.ListAgentGroupMembers(g.ID)
	require.NoError(t, err)
	require.Len(t, members, 2, "parallel scribes remain enrolled")

	// Each independent scribe retains its own birth-time grant.
	overrides, err := db.ListAgentPermissionOverridesForConv(first.ConvID)
	require.NoError(t, err)
	assert.Equal(t, "grant", overrides[agentd.PermTemplatesManage])
	state, err := db.AgentState(first.ConvID)
	require.NoError(t, err)
	assert.Equal(t, db.AgentStateActive, state, "prior scribe remains active")
	overrides, err = db.ListAgentPermissionOverridesForConv(second.ConvID)
	require.NoError(t, err)
	assert.Equal(t, "grant", overrides[agentd.PermTemplatesManage], "second scribe is granted at birth")

	// Both fresh summons opened a window.
	assert.Equal(t, 2, *opens, "each summon opened the scribe's window")
}

// A structured scope opts into exact reuse. The current generation belongs in
// the refreshed inbox brief, not the persistent key: a scribe that saved once
// must remain the right conversation for the same template's next generation.
func TestScribeSummon_SameScopeReusesAndRefreshesBrief(t *testing.T) {
	f := newFlow(t)
	opens := stubScribeTerminal(t)
	post := func(ref string) scribeSummonResp {
		rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe",
			map[string]any{
				"name":  "process-scribe",
				"slugs": []string{agentd.PermProcessTemplatesRead, agentd.PermProcessTemplatesManage},
				"scope": map[string]any{"kind": "process-template", "id": "release-flow"},
				"brief": "Canonical currentRef is " + ref + "; reread before CAS-save.",
			})))
		require.Equalf(t, http.StatusOK, rec.Code, "summon body=%s", rec.Body.String())
		return decodeScribeResp(t, rec)
	}
	first := post("release-flow@sha256:1111")
	firstAudit := permissionAuditRows(t, first.ConvID)
	require.Len(t, firstAudit, 2)
	assert.Equal(t, firstAudit[agentd.PermProcessTemplatesRead].GrantedBy,
		firstAudit[agentd.PermProcessTemplatesManage].GrantedBy,
		"one generic summon correlates all birth-time permission rows")
	assert.Regexp(t, `^<human>:scribe-summon:correlation-id=[0-9a-f]{32}$`,
		firstAudit[agentd.PermProcessTemplatesManage].GrantedBy)
	second := post("release-flow@sha256:2222")
	assert.False(t, first.Reused)
	assert.True(t, second.Reused)
	assert.Equal(t, first.ConvID, second.ConvID)
	assert.Equal(t, "native", second.FocusMode, "reuse returns the existing open-conversation handshake")
	assert.Equal(t, 2, *opens, "fresh and reused calls both focus the conversation")
	assert.Equal(t, firstAudit, permissionAuditRows(t, second.ConvID),
		"reuse must preserve the original grant timestamps and approval correlation")

	g, err := db.GetAgentGroupByName("process-scribe")
	require.NoError(t, err)
	members, err := db.ListAgentGroupMembers(g.ID)
	require.NoError(t, err)
	require.Len(t, members, 1, "same-scope calls are idempotent")
	assert.Equal(t, "scribe", members[0].Role)
	assert.Equal(t, "Reusable scribe scope: process-template/release-flow", members[0].Descr)

	messages, err := db.ListAgentMessagesForConv(first.ConvID, 10)
	require.NoError(t, err)
	require.Len(t, messages, 2, "startup plus refreshed handoff are durable inbox messages")
	assert.Equal(t, "Scribe scope refreshed", messages[0].Subject)
	assert.Contains(t, messages[0].Body, "2222", "reuse refreshes transient generation context")
}

func TestScribeSummon_ProcessScopePersistsTaskAndApprovalAudit(t *testing.T) {
	f, _ := processEngineFlow(t)
	stubScribeTerminal(t)
	taskURL := "https://dashboard.example/processes/templates"
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe",
		map[string]any{
			"name": "process-scribe", "exclusive": true,
			"slugs":          []string{agentd.PermProcessTemplatesRead, agentd.PermProcessTemplatesManage},
			"scope":          map[string]any{"kind": "process-template", "id": "release-flow"},
			"task_ref_url":   taskURL,
			"task_ref_label": "process: release-flow",
			"brief":          "Use the safe process-template CAS workflow and never run a process.",
		})))
	require.Equalf(t, http.StatusOK, rec.Code, "summon body=%s", rec.Body.String())
	resp := decodeScribeResp(t, rec)

	agentID, err := db.AgentIDForConv(resp.ConvID)
	require.NoError(t, err)
	require.NotEmpty(t, agentID)
	ref, err := db.GetAgentTaskRef(agentID)
	require.NoError(t, err)
	assert.Equal(t, taskURL, ref.URL)
	assert.Equal(t, "process: release-flow", ref.Label)

	d, err := db.Open()
	require.NoError(t, err)
	rows, err := d.Query(`SELECT slug, granted_by FROM agent_permissions
		WHERE agent_id = ? AND effect = ? AND slug IN (?, ?) ORDER BY slug`,
		agentID, db.PermEffectGrant, agentd.PermProcessTemplatesManage, agentd.PermProcessTemplatesRead)
	require.NoError(t, err)
	defer rows.Close()
	grants := map[string]string{}
	for rows.Next() {
		var slug, grantedBy string
		require.NoError(t, rows.Scan(&slug, &grantedBy))
		grants[slug] = grantedBy
	}
	require.NoError(t, rows.Err())
	require.Len(t, grants, 2)
	assert.Equal(t, grants[agentd.PermProcessTemplatesRead], grants[agentd.PermProcessTemplatesManage],
		"one explicit human approval correlates both required grants")
	assert.Regexp(t, `^<human>:scribe-summon:correlation-id=[0-9a-f]{32}$`, grants[agentd.PermProcessTemplatesManage])

	// The same approved scribe authors the version attributed to its stable
	// actor. Summon and save are authoring-only: no process run appears.
	tmpl := processRESTTemplate("release-flow", "authored by approved scribe", 10)
	tmpl.Layout = nil
	source, err := model.CanonicalYAML(tmpl)
	require.NoError(t, err)
	created := agentReq(t, f, resp.ConvID, http.MethodPost, "/v1/process/templates/release-flow", map[string]any{
		"source": string(source),
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var saved struct {
		Actor string `json:"actor"`
	}
	testharness.DecodeJSON(t, created, &saved)
	assert.Equal(t, "agent:"+agentID, saved.Actor)
	listed := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates", nil)
	require.Equal(t, http.StatusOK, listed.Code, listed.Body.String())
	assert.Contains(t, listed.Body.String(), saved.Actor, "version/change-review readback retains the exact scribe actor")
	runs := processTemplateRequest(t, f, http.MethodGet, "/v1/process/runs", nil)
	require.Equal(t, http.StatusOK, runs.Code, runs.Body.String())
	assert.JSONEq(t, `{"runs":[]}`, runs.Body.String(), "summoning and authoring never instantiate or execute a process")

	// Reuse refreshes the task reference but does not mint or relabel grants.
	secondTask := "https://dashboard.example/processes/templates?refreshed=1"
	rec = testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe",
		map[string]any{
			"name": "process-scribe", "exclusive": true,
			"slugs":        []string{agentd.PermProcessTemplatesRead, agentd.PermProcessTemplatesManage},
			"scope":        map[string]any{"kind": "process-template", "id": "release-flow"},
			"task_ref_url": secondTask, "task_ref_label": "process: release-flow",
			"brief": "Refresh canonical state before the next CAS save.",
		})))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	second := decodeScribeResp(t, rec)
	assert.True(t, second.Reused)
	assert.Equal(t, resp.ConvID, second.ConvID)
	ref, err = db.GetAgentTaskRef(agentID)
	require.NoError(t, err)
	assert.Equal(t, secondTask, ref.URL)
	reusedAudit := permissionAuditRows(t, second.ConvID)
	assert.Equal(t, grants[agentd.PermProcessTemplatesRead],
		reusedAudit[agentd.PermProcessTemplatesRead].GrantedBy)
	assert.Equal(t, grants[agentd.PermProcessTemplatesManage],
		reusedAudit[agentd.PermProcessTemplatesManage].GrantedBy)
}

func TestScribeSummon_ScopedExclusiveDoesNotReuseOrRevokeLaterSudo(t *testing.T) {
	f := newFlow(t)
	stubScribeTerminal(t)
	post := func() scribeSummonResp {
		rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe",
			map[string]any{
				"name": "process-scribe", "exclusive": true,
				"slugs": []string{agentd.PermProcessTemplatesRead, agentd.PermProcessTemplatesManage},
				"scope": map[string]any{"kind": "process-template", "id": "release-flow"},
				"brief": "Exclusive process-template authoring brief.",
			})))
		require.Equalf(t, http.StatusOK, rec.Code, "summon body=%s", rec.Body.String())
		return decodeScribeResp(t, rec)
	}
	first := post()
	_, err := db.InsertSudoGrant(&db.SudoGrant{
		ConvID: first.ConvID, Slug: agentd.PermGroupsSpawn,
		GrantedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
		GrantedBy: "test", Reason: "verify exclusive reuse",
	})
	require.NoError(t, err)
	active, err := db.ListActiveSudoGrants(first.ConvID)
	require.NoError(t, err)
	require.Len(t, active, 1, "test elevation is active before reuse")

	second := post()
	assert.False(t, second.Reused)
	assert.NotEqual(t, first.ConvID, second.ConvID, "sudo-elevated candidate is incompatible with exact scoped reuse")
	active, err = db.ListActiveSudoGrants(first.ConvID)
	require.NoError(t, err)
	assert.Len(t, active, 1, "summon authority does not cross the separate permission-revoke boundary")
}

func TestScribeSummon_ScopeAndPermissionCompatibility(t *testing.T) {
	f := newFlow(t)
	stubScribeTerminal(t)
	post := func(id string, slugs []string) scribeSummonResp {
		rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe",
			map[string]any{
				"name": "process-scribe", "slugs": slugs,
				"scope": map[string]any{"kind": "process-template", "id": id},
				"brief": "Safe process-template authoring brief.",
			})))
		require.Equalf(t, http.StatusOK, rec.Code, "summon body=%s", rec.Body.String())
		return decodeScribeResp(t, rec)
	}
	required := []string{agentd.PermProcessTemplatesRead, agentd.PermProcessTemplatesManage}
	library := post("", required)
	template := post("release-flow", required)
	assert.NotEqual(t, library.ConvID, template.ConvID, "library and exact-template scopes never cross-reuse")

	require.NoError(t, db.GrantAgentPermission(template.ConvID, agentd.PermTemplatesManage, "test-extra"))
	incompatible := post("release-flow", required)
	assert.False(t, incompatible.Reused)
	assert.NotEqual(t, template.ConvID, incompatible.ConvID, "an agent with widened overrides is incompatible")
	assert.Equal(t, map[string]string{
		agentd.PermProcessTemplatesRead:   db.PermEffectGrant,
		agentd.PermProcessTemplatesManage: db.PermEffectGrant,
	}, mustOverrides(t, incompatible.ConvID), "fresh replacement gets only the requested overrides")
}

func TestScribeSummon_ScopedDeadAndRetiredAreNotReused(t *testing.T) {
	for _, tc := range []struct {
		name            string
		makeUnavailable func(*testing.T, *testharness.Flow, string)
	}{
		{name: "dead", makeUnavailable: func(t *testing.T, f *testharness.Flow, conv string) {
			sessions, err := db.FindSessionsByConvID(conv)
			require.NoError(t, err)
			require.NotEmpty(t, sessions)
			f.MarkOffline(sessions[0].TmuxSession)
		}},
		{name: "retired", makeUnavailable: func(t *testing.T, _ *testharness.Flow, conv string) {
			retired, err := db.RetireAgent(conv, "test", "retired scribe")
			require.NoError(t, err)
			require.True(t, retired)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlow(t)
			stubScribeTerminal(t)
			post := func() scribeSummonResp {
				rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe", map[string]any{
					"name": "process-scribe", "slugs": []string{agentd.PermProcessTemplatesRead, agentd.PermProcessTemplatesManage},
					"scope": map[string]any{"kind": "process-template", "id": "release-flow"}, "brief": "Edit safely.",
				})))
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				return decodeScribeResp(t, rec)
			}
			first := post()
			tc.makeUnavailable(t, f, first.ConvID)
			second := post()
			assert.False(t, second.Reused)
			assert.NotEqual(t, first.ConvID, second.ConvID)
		})
	}
}

func TestScribeSummon_ConcurrentSameScopeSpawnsOnce(t *testing.T) {
	f := newFlow(t)
	stubScribeTerminal(t)
	const callers = 4
	results := make(chan scribeSummonResp, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe", map[string]any{
				"name": "process-scribe", "slugs": []string{agentd.PermProcessTemplatesRead, agentd.PermProcessTemplatesManage},
				"scope": map[string]any{"kind": "process-template", "id": "release-flow"}, "brief": "Edit safely.",
			})))
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			results <- decodeScribeResp(t, rec)
		}()
	}
	wg.Wait()
	close(results)
	convs := map[string]bool{}
	fresh := 0
	for result := range results {
		convs[result.ConvID] = true
		if !result.Reused {
			fresh++
		}
	}
	assert.Len(t, convs, 1)
	assert.Equal(t, 1, fresh, "lock-serialized lookup prevents double-spawn")
}

// A capability-reducing scribe can pin explicit denies as well as its narrow
// grant. A concurrent scribe receives the complete deny set independently at
// birth without disturbing the first scribe.
func TestScribeSummon_ConcurrentScribeGetsExplicitDenies(t *testing.T) {
	f := newFlow(t)
	stubScribeTerminal(t)
	summon := func() scribeSummonResp {
		rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe",
			map[string]any{
				"name":      "sandbox-scribe",
				"slugs":     []string{agentd.PermSandboxProfilesDraft},
				"exclusive": true,
				"brief":     "Prepare a draft only; never save or launch anything.",
			})))
		require.Equalf(t, http.StatusOK, rec.Code, "summon body=%s", rec.Body.String())
		return decodeScribeResp(t, rec)
	}

	first := summon()
	require.NoError(t, db.GrantAgentPermission(first.ConvID, agentd.PermSandboxProfilesManage, "simulated-stale-grant"))
	_, err := db.InsertSudoGrant(&db.SudoGrant{
		ConvID: first.ConvID, Slug: agentd.PermSandboxProfilesManage,
		ExpiresAt: time.Now().Add(time.Hour), GrantedBy: "test", Reason: "simulated stale elevation",
	})
	require.NoError(t, err)
	second := summon()
	require.False(t, second.Reused)
	require.NotEqual(t, first.ConvID, second.ConvID)

	overrides, err := db.ListAgentPermissionOverridesForConv(second.ConvID)
	require.NoError(t, err)
	assert.Equal(t, db.PermEffectGrant, overrides[agentd.PermSandboxProfilesDraft])
	assert.Equal(t, db.PermEffectDeny, overrides[agentd.PermSandboxProfilesManage])
	assert.Equal(t, db.PermEffectDeny, overrides[agentd.PermGroupsSpawn])
	assert.Equal(t, db.PermEffectDeny, overrides[agentd.PermTemplatesUse])
	assert.Equal(t, db.PermEffectDeny, overrides[agentd.PermSelfClone])
	assert.Equal(t, db.PermEffectDeny, overrides[agentd.PermSelfReincarnate])
	assert.Equal(t, db.PermEffectDeny, overrides[agentd.PermPermissionsGrant])
	assert.Equal(t, db.PermEffectDeny, overrides[agentd.PermPermissionsRevoke])
	oldOverrides, err := db.ListAgentPermissionOverridesForConv(first.ConvID)
	require.NoError(t, err)
	assert.Equal(t, db.PermEffectGrant, oldOverrides[agentd.PermSandboxProfilesManage],
		"existing scribe is not mutated while it works")
	activeSudo, err := db.ListActiveSudoGrants(first.ConvID)
	require.NoError(t, err)
	assert.NotEmpty(t, activeSudo, "existing scribe's active task is left untouched")
}

// Moving from exclusive to ordinary mode starts a fresh agent, so no deny from
// the previous task can leak into the new scribe's permission set.
func TestScribeSummon_FreshOrdinaryDoesNotInheritExclusiveDenies(t *testing.T) {
	f := newFlow(t)
	stubScribeTerminal(t)
	summon := func(exclusive bool) scribeSummonResp {
		rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe",
			map[string]any{
				"name":      "sandbox-scribe",
				"slugs":     []string{agentd.PermSandboxProfilesDraft},
				"exclusive": exclusive,
				"brief":     "Prepare a draft for human review.",
			})))
		require.Equalf(t, http.StatusOK, rec.Code, "summon body=%s", rec.Body.String())
		return decodeScribeResp(t, rec)
	}

	first := summon(true)
	// Add a human-authored deny to the prior generation; the next agent must not
	// inherit either it or the summon-authored exclusive denies.
	require.NoError(t, db.SetAgentPermissionOverride(first.ConvID, agentd.PermSelfRename,
		db.PermEffectDeny, "<human>"))
	second := summon(false)
	require.False(t, second.Reused)
	require.NotEqual(t, first.ConvID, second.ConvID)

	overrides, err := db.ListAgentPermissionOverridesForConv(second.ConvID)
	require.NoError(t, err)
	assert.Equal(t, db.PermEffectGrant, overrides[agentd.PermSandboxProfilesDraft])
	assert.NotContains(t, overrides, agentd.PermGroupsSpawn, "exclusive deny is not inherited")
	assert.NotContains(t, overrides, agentd.PermTemplatesUse, "generated denies are not inherited")
	assert.NotContains(t, overrides, agentd.PermSelfRename, "human-authored deny is not inherited")
}

// Scenario: after the scribe's session dies (e.g. a daemon restart leaves the
// membership row but kills the tmux session), a re-summon spawns a FRESH scribe
// and prunes the dead one — the group does not accumulate stale members.
func TestScribeSummon_DeadScribePrunedOnResummon(t *testing.T) {
	f := newFlow(t)
	stubScribeTerminal(t)

	summon := func() scribeSummonResp {
		rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe",
			map[string]any{
				"name":  "circle-scribe",
				"slugs": []string{agentd.PermTemplatesManage},
				"brief": "Edit summoning circles.",
			})))
		require.Equalf(t, http.StatusOK, rec.Code, "summon body=%s", rec.Body.String())
		return decodeScribeResp(t, rec)
	}

	first := summon()
	require.False(t, first.Reused, "first summon spawns")

	// Kill the scribe's tmux session but leave its membership row (what a daemon
	// restart does).
	sessions, err := db.FindSessionsByConvID(first.ConvID)
	require.NoError(t, err)
	require.NotEmpty(t, sessions, "the fresh scribe has a session row")
	f.MarkOffline(sessions[0].TmuxSession)

	second := summon()
	assert.False(t, second.Reused, "a dead scribe is not reused — a fresh one is spawned")
	assert.NotEqual(t, first.ConvID, second.ConvID, "the fresh scribe is a new conv")

	// The dead scribe was pruned: exactly one (live) member remains.
	g, err := db.GetAgentGroupByName("circle-scribe")
	require.NoError(t, err)
	require.NotNil(t, g)
	members, err := db.ListAgentGroupMembers(g.ID)
	require.NoError(t, err)
	require.Len(t, members, 1, "the dead scribe was pruned, not accumulated")
	assert.Equal(t, second.ConvID, members[0].ConvID, "the sole member is the fresh scribe")
}

// Scenario: gating parity with the spawn path. An agent caller is refused
// unless it holds groups.spawn AND — because a summon applies birth-time grants
// — permissions.grant; the human always passes.
func TestScribeSummon_AgentGatedLikeSpawn(t *testing.T) {
	f := newFlow(t)
	stubScribeTerminal(t)
	f.HaveGroup("callers")

	post := func(conv string) *httptest.ResponseRecorder {
		req := testharness.JSONRequest(t, http.MethodPost, "/v1/scribe", map[string]any{
			"name":  "circle-scribe",
			"slugs": []string{agentd.PermTemplatesManage},
			"brief": "Edit summoning circles.",
		})
		return testharness.Serve(f.Mux, agentd.AsAgentPeer(req, conv))
	}

	// (a) An agent with NEITHER slug is refused outright.
	const bare = "bare-1111-2222-3333-4444"
	f.HaveMember("callers", bare)
	assert.Equalf(t, http.StatusForbidden, post(bare).Code,
		"an agent without groups.spawn is refused")

	// (b) An agent with groups.spawn but NOT permissions.grant is still refused —
	// a summon carries birth-time grants, so it needs the grant slug too.
	const spawnOnly = "spwn-1111-2222-3333-4444"
	f.HaveMember("callers", spawnOnly)
	require.NoError(t, db.GrantAgentPermission(spawnOnly, agentd.PermGroupsSpawn, "test"))
	assert.Equalf(t, http.StatusForbidden, post(spawnOnly).Code,
		"granting a scribe slugs needs permissions.grant")

	// (c) An agent holding both slugs is allowed — same bar the spawn path sets.
	const granter = "good-1111-2222-3333-4444"
	f.HaveMember("callers", granter)
	require.NoError(t, db.GrantAgentPermission(granter, agentd.PermGroupsSpawn, "test"))
	require.NoError(t, db.GrantAgentPermission(granter, agentd.PermPermissionsGrant, "test"))
	rec := post(granter)
	require.Equalf(t, http.StatusOK, rec.Code, "authorised agent summon body=%s", rec.Body.String())
	assert.Equal(t, "grant",
		mustOverrides(t, decodeScribeResp(t, rec).ConvID)[agentd.PermTemplatesManage],
		"authorised agent's scribe carries the grant")

	// (d) When permissions.grant is sudo-only, every child grant retains the
	// exact sudo row lineage in addition to the server-minted summon id.
	const elevated = "sudo-1111-2222-3333-4444"
	f.HaveMember("callers", elevated)
	require.NoError(t, db.GrantAgentPermission(elevated, agentd.PermGroupsSpawn, "test"))
	sudoID, err := db.InsertSudoGrant(&db.SudoGrant{
		ConvID: elevated, Slug: agentd.PermPermissionsGrant,
		GrantedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
		GrantedBy: "test", Reason: "scribe audit lineage coverage",
	})
	require.NoError(t, err)
	rec = post(elevated)
	require.Equalf(t, http.StatusOK, rec.Code, "sudo-authorised summon body=%s", rec.Body.String())
	elevatedScribe := decodeScribeResp(t, rec)
	assert.Regexp(t, fmt.Sprintf(`^%s:via-sudo:grant-id=%d:scribe-summon:correlation-id=[0-9a-f]{32}$`, elevated, sudoID),
		permissionAuditRows(t, elevatedScribe.ConvID)[agentd.PermTemplatesManage].GrantedBy)
}

// Scenario: the JOH-369 fix. A summon spawns the scribe into the stable,
// shared, pre-trusted workdir (~/.tclaude/scribe) — NOT $HOME — so its
// detached pane can start unprompted; the CC folder-trust store is pre-seeded
// for that dir; and every fresh scribe uses that same trusted cwd.
func TestScribeSummon_SpawnsInSharedTrustedWorkdir(t *testing.T) {
	f := newFlow(t)
	stubScribeTerminal(t)

	// Paths derived from the flow's temp HOME (World.New t.Setenv's HOME to it,
	// so summonScribe's os.UserHomeDir() resolves here — no real ~ is touched).
	wantCwd := filepath.Join(f.World.HomeDir, ".tclaude", "scribe")
	claudeJSON := filepath.Join(f.World.HomeDir, ".claude.json")

	summon := func() scribeSummonResp {
		rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe",
			map[string]any{
				"name":  "circle-scribe",
				"slugs": []string{agentd.PermTemplatesManage},
				"brief": "Edit summoning circles.",
			})))
		require.Equalf(t, http.StatusOK, rec.Code, "summon body=%s", rec.Body.String())
		return decodeScribeResp(t, rec)
	}

	first := summon()
	require.False(t, first.Reused, "first summon spawns")

	// (1) The scribe spawned with cwd = the shared workdir — observed at the
	// SessionRow surface (what conv/session lookups walk), exactly how other
	// spawn flow tests observe the launch cwd.
	sessions, err := db.FindSessionsByConvID(first.ConvID)
	require.NoError(t, err)
	require.NotEmpty(t, sessions, "the scribe has a session row")
	assert.Equal(t, wantCwd, sessions[0].Cwd, "scribe spawns in the shared ~/.tclaude/scribe workdir, not $HOME")

	// (2) The workdir was actually created on disk.
	fi, err := os.Stat(wantCwd)
	require.NoError(t, err, "the scribe workdir exists")
	assert.True(t, fi.IsDir(), "the scribe workdir is a directory")

	// (3) The CC folder-trust store was pre-seeded for that dir, so an
	// interactive CC start there won't raise the trust dialog.
	assert.True(t, claudeDirTrusted(t, claudeJSON, wantCwd),
		"~/.claude.json marks the scribe workdir hasTrustDialogAccepted=true")

	// (4) The second scribe is fresh but uses the SAME stable trusted cwd.
	second := summon()
	require.False(t, second.Reused, "second summon is also fresh")
	require.NotEqual(t, first.ConvID, second.ConvID)
	sessions2, err := db.FindSessionsByConvID(second.ConvID)
	require.NoError(t, err)
	require.NotEmpty(t, sessions2)
	assert.Equal(t, wantCwd, sessions2[0].Cwd, "second scribe uses the same stable workdir")
}

// Scenario: trust-seeding is best-effort. A malformed ~/.claude.json (which
// makes the CC trust editor refuse) must NOT fail the summon — the scribe
// still spawns (worst case it sees a one-time trust dialog), and the daemon
// leaves the broken config untouched rather than corrupting it further.
func TestScribeSummon_ProceedsWhenClaudeConfigMalformed(t *testing.T) {
	f := newFlow(t)
	stubScribeTerminal(t)

	claudeJSON := filepath.Join(f.World.HomeDir, ".claude.json")
	const garbage = "{ this is not valid json ][" // parse fails → editor refuses
	require.NoError(t, os.WriteFile(claudeJSON, []byte(garbage), 0o600))

	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe",
		map[string]any{
			"name":  "circle-scribe",
			"slugs": []string{agentd.PermTemplatesManage},
			"brief": "Edit summoning circles.",
		})))
	require.Equalf(t, http.StatusOK, rec.Code, "summon must succeed despite a malformed CC config; body=%s", rec.Body.String())
	resp := decodeScribeResp(t, rec)
	require.NotEmpty(t, resp.ConvID, "the scribe still spawned")

	// The daemon did not touch the broken file (best-effort skip, not a rewrite).
	after, err := os.ReadFile(claudeJSON)
	require.NoError(t, err)
	assert.Equal(t, garbage, string(after), "a malformed CC config is left untouched, not corrupted further")
}

// claudeDirTrusted reports whether ~/.claude.json marks dir as
// hasTrustDialogAccepted=true — the surface Claude Code reads at startup.
func claudeDirTrusted(t *testing.T, claudeJSONPath, dir string) bool {
	t.Helper()
	data, err := os.ReadFile(claudeJSONPath)
	require.NoErrorf(t, err, "read %s", claudeJSONPath)
	var root struct {
		Projects map[string]struct {
			HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	require.NoError(t, json.Unmarshal(data, &root))
	return root.Projects[dir].HasTrustDialogAccepted
}

// mustOverrides is a tiny read helper for the per-conv override map.
func mustOverrides(t *testing.T, conv string) map[string]string {
	t.Helper()
	m, err := db.ListAgentPermissionOverridesForConv(conv)
	require.NoError(t, err)
	return m
}

// Scenario: confused-deputy guard. A summon whose name collides with a real,
// non-scribe group must NOT resolve that group — it fails closed rather than
// spawning a privileged stray scribe into it.
func TestScribeSummon_RefusesForeignGroupCollision(t *testing.T) {
	f := newFlow(t)
	stubScribeTerminal(t)

	// A real working group + a live member that happens to share the scribe name.
	f.HaveGroup("circle-scribe")
	const foreigner = "frgn-1111-2222-3333-4444"
	f.HaveMember("circle-scribe", foreigner)

	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe",
		map[string]any{
			"name":  "circle-scribe",
			"slugs": []string{agentd.PermTemplatesManage},
			"brief": "Edit summoning circles.",
		})))
	assert.Equalf(t, http.StatusConflict, rec.Code, "summon into a non-scribe group must 409; body=%s", rec.Body.String())

	// The foreign member was neither granted the scribe slug nor pulled in.
	overrides, err := db.ListAgentPermissionOverridesForConv(foreigner)
	require.NoError(t, err)
	assert.Empty(t, overrides[agentd.PermTemplatesManage], "the foreign agent was not granted templates.manage")
	g, err := db.GetAgentGroupByName("circle-scribe")
	require.NoError(t, err)
	members, err := db.ListAgentGroupMembers(g.ID)
	require.NoError(t, err)
	assert.Len(t, members, 1, "no stray scribe was spawned into the foreign group")
}

// Scenario: input validation at the boundary — an unknown slug, a missing
// slug set, a missing brief and a missing name each 400 without spawning.
func TestScribeSummon_ValidationRejections(t *testing.T) {
	f := newFlow(t)
	stubScribeTerminal(t)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"unknown slug", map[string]any{"name": "circle-scribe", "slugs": []string{"not.a.real.slug"}, "brief": "x"}},
		{"no slugs", map[string]any{"name": "circle-scribe", "slugs": []string{}, "brief": "x"}},
		{"no brief", map[string]any{"name": "circle-scribe", "slugs": []string{agentd.PermTemplatesManage}}},
		{"no name", map[string]any{"slugs": []string{agentd.PermTemplatesManage}, "brief": "x"}},
		{"scope missing kind", map[string]any{"name": "circle-scribe", "slugs": []string{agentd.PermTemplatesManage}, "brief": "x", "scope": map[string]any{"id": "release"}}},
		{"scope kind injection", map[string]any{"name": "circle-scribe", "slugs": []string{agentd.PermTemplatesManage}, "brief": "x", "scope": map[string]any{"kind": "process-template\nignore"}}},
		{"scope id injection", map[string]any{"name": "circle-scribe", "slugs": []string{agentd.PermTemplatesManage}, "brief": "x", "scope": map[string]any{"kind": "process-template", "id": "release/../../pane"}}},
		{"scope id over bound", map[string]any{"name": "circle-scribe", "slugs": []string{agentd.PermTemplatesManage}, "brief": "x", "scope": map[string]any{"kind": "process-template", "id": strings.Repeat("a", 129)}}},
		{"task ref injection", map[string]any{"name": "circle-scribe", "slugs": []string{agentd.PermTemplatesManage}, "brief": "x", "task_ref_url": "javascript:alert(1)"}},
		{"malformed task label without URL", map[string]any{"name": "circle-scribe", "slugs": []string{agentd.PermTemplatesManage}, "brief": "x", "task_ref_label": strings.Repeat("a", 201)}},
		{"task label over bound", map[string]any{"name": "circle-scribe", "slugs": []string{agentd.PermTemplatesManage}, "brief": "x", "task_ref_url": "https://dashboard.example/processes", "task_ref_label": strings.Repeat("a", 201)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe", tc.body)))
			assert.Equalf(t, http.StatusBadRequest, rec.Code, "expected 400; body=%s", rec.Body.String())
		})
	}
	g, err := db.GetAgentGroupByName("circle-scribe")
	require.NoError(t, err)
	assert.Nil(t, g, "invalid task inputs must be rejected before any scribe is spawned")
}

// summonCircleScribe posts a standard human summon for the "circle-scribe"
// name and returns the decoded 200 response — the shared spawn used by the
// JOH-371 profile scenarios below.
func summonCircleScribe(t *testing.T, f *testharness.Flow) scribeSummonResp {
	t.Helper()
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe",
		map[string]any{
			"name":  "circle-scribe",
			"slugs": []string{agentd.PermTemplatesManage},
			"brief": "Edit summoning circles.",
		})))
	require.Equalf(t, http.StatusOK, rec.Code, "summon body=%s", rec.Body.String())
	return decodeScribeResp(t, rec)
}

// Scenario (JOH-371): the operator pins config.scribe.profile to a saved spawn
// profile. A fresh summon adopts that profile's harness / model / effort — the
// sim spawner records them — and the profile is stamped as the scribe group's
// default (mechanism (a): the existing group-default-profile resolution then
// carries the launch shape with no new spawn logic).
func TestScribeSummon_AppliesConfiguredProfile(t *testing.T) {
	f := newFlow(t)
	stubScribeTerminal(t)

	// A saved spawn profile pinning a specific Claude model + effort.
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{
		Name:    "editor-prof",
		Harness: harness.DefaultName,
		Model:   "sonnet",
		Effort:  "high",
	})
	require.NoError(t, err)

	// The operator's Config-tab choice, on disk in the same config.json the CLI
	// reads (config.Save writes under the flow's temp HOME).
	require.NoError(t, config.Save(&config.Config{
		Scribe: &config.ScribeConfig{Profile: "editor-prof"},
	}))

	resp := summonCircleScribe(t, f)
	require.NotEmpty(t, resp.ConvID)
	require.False(t, resp.Reused, "first summon spawns fresh")

	// The sim spawner observed the profile's harness / model / effort.
	sessions, err := db.FindSessionsByConvID(resp.ConvID)
	require.NoError(t, err)
	require.NotEmpty(t, sessions, "the scribe has a session row")
	assert.Equal(t, harness.DefaultName, sessions[0].Harness, "spawn adopted the profile's harness")

	model, ok := f.World.SpawnModel(resp.ConvID)
	require.True(t, ok, "the spawn was observed by the sim spawner")
	assert.Equal(t, "sonnet", model, "spawn adopted the profile's model")
	effort, ok := f.World.SpawnEffort(resp.ConvID)
	require.True(t, ok)
	assert.Equal(t, "high", effort, "spawn adopted the profile's effort")

	// Mechanism (a): the profile was stamped as the scribe group's default, so
	// scribeSpawnHarness + applyDefaultProfile resolve from one source.
	g, err := db.GetAgentGroupByName("circle-scribe")
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.Equal(t, "editor-prof", g.DefaultProfile, "config.scribe.profile stamped as the group default")
}

// Scenario (JOH-371 coupling to JOH-369): a Codex scribe profile makes the
// summon launch on Codex AND pre-seed the CODEX dir-trust store (not CC's) for
// the scribe workdir — the trust seed follows the harness the spawn actually
// resolves to, because both read the same stamped group default profile.
func TestScribeSummon_CodexProfileSeedsCodexTrust(t *testing.T) {
	f := newFlow(t)
	stubScribeTerminal(t)

	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "codex-prof", Harness: harness.CodexName})
	require.NoError(t, err)
	require.NoError(t, config.Save(&config.Config{
		Scribe: &config.ScribeConfig{Profile: "codex-prof"},
	}))

	wantCwd := filepath.Join(f.World.HomeDir, ".tclaude", "scribe")
	codexTOML := filepath.Join(f.World.HomeDir, ".codex", "config.toml")
	claudeJSON := filepath.Join(f.World.HomeDir, ".claude.json")

	resp := summonCircleScribe(t, f)
	require.NotEmpty(t, resp.ConvID)
	require.False(t, resp.Reused, "first summon spawns fresh")

	// The scribe launched on Codex — observed at the SessionRow harness column.
	sessions, err := db.FindSessionsByConvID(resp.ConvID)
	require.NoError(t, err)
	require.NotEmpty(t, sessions, "the scribe has a session row")
	assert.Equal(t, harness.CodexName, sessions[0].Harness, "codex-profile scribe launched on codex")

	// The CODEX trust store was seeded for the scribe workdir — the harness the
	// spawn resolved to (the JOH-369 coupling this ticket must preserve).
	codexData, err := os.ReadFile(codexTOML)
	require.NoError(t, err, "the codex trust store was written")
	assert.Contains(t, string(codexData), `[projects."`+wantCwd+`"]`)
	assert.Contains(t, string(codexData), `trust_level = "trusted"`)

	// CC's trust store was NOT seeded — a codex scribe must never touch
	// ~/.claude.json (that would be the wrong-harness trust seed the ticket
	// explicitly warns against).
	_, err = os.Stat(claudeJSON)
	assert.Truef(t, os.IsNotExist(err), "codex-profile scribe must not seed the CC trust store (~/.claude.json), got err=%v", err)
}

// Scenario (JOH-371): a config.scribe.profile that names a since-deleted /
// renamed profile self-heals to the no-profile default — the scribe still
// summons, on the harness default (Claude), with CC dir-trust seeding — rather
// than wedging the summon. The dangling name is stamped as-is; resolution
// (groupDefaultProfile → nil) is what self-heals.
func TestScribeSummon_DeletedProfileSelfHeals(t *testing.T) {
	f := newFlow(t)
	stubScribeTerminal(t)

	// Point at a profile that was never created (or was deleted after config
	// was written) — no db.CreateSpawnProfile for "gone-prof".
	require.NoError(t, config.Save(&config.Config{
		Scribe: &config.ScribeConfig{Profile: "gone-prof"},
	}))

	wantCwd := filepath.Join(f.World.HomeDir, ".tclaude", "scribe")
	claudeJSON := filepath.Join(f.World.HomeDir, ".claude.json")

	resp := summonCircleScribe(t, f)
	require.NotEmpty(t, resp.ConvID, "the scribe still summoned despite the dangling profile")
	require.False(t, resp.Reused)

	// Self-healed to the harness default: Claude, with no pinned model/effort.
	sessions, err := db.FindSessionsByConvID(resp.ConvID)
	require.NoError(t, err)
	require.NotEmpty(t, sessions)
	assert.Equal(t, harness.DefaultName, sessions[0].Harness, "dangling profile self-heals to the default harness")
	if model, ok := f.World.SpawnModel(resp.ConvID); ok {
		assert.Empty(t, model, "no model is pinned when the profile self-heals")
	}
	if effort, ok := f.World.SpawnEffort(resp.ConvID); ok {
		assert.Empty(t, effort, "no effort is pinned when the profile self-heals")
	}

	// CC dir-trust was seeded (the default-harness path), not skipped.
	assert.True(t, claudeDirTrusted(t, claudeJSON, wantCwd),
		"the default-harness path still pre-seeds the CC trust store")
}

// Scenario (JOH-371): every summon is fresh, so a config change between
// summons applies immediately to the next independent scribe.
func TestScribeSummon_NextScribeAdoptsConfigChange(t *testing.T) {
	f := newFlow(t)
	stubScribeTerminal(t)

	_, err := db.CreateSpawnProfile(&db.SpawnProfile{
		Name: "prof-a", Harness: harness.DefaultName, Model: "sonnet", Effort: "high",
	})
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{
		Name: "prof-b", Harness: harness.DefaultName, Model: "opus", Effort: "low",
	})
	require.NoError(t, err)

	// First summon under profile A → fresh spawn, born with A's shape.
	require.NoError(t, config.Save(&config.Config{Scribe: &config.ScribeConfig{Profile: "prof-a"}}))
	first := summonCircleScribe(t, f)
	require.False(t, first.Reused, "first summon spawns fresh")
	if model, ok := f.World.SpawnModel(first.ConvID); ok {
		assert.Equal(t, "sonnet", model, "the fresh scribe was born with profile A's model")
	}

	// Operator changes the config to profile B between summons.
	require.NoError(t, config.Save(&config.Config{Scribe: &config.ScribeConfig{Profile: "prof-b"}}))

	// Second summon leaves the old scribe running and launches under profile B.
	second := summonCircleScribe(t, f)
	require.False(t, second.Reused, "second summon is fresh")
	require.NotEqual(t, first.ConvID, second.ConvID)

	// The next independent scribe picks up the changed launch shape.
	if model, ok := f.World.SpawnModel(second.ConvID); ok {
		assert.Equal(t, "opus", model, "second scribe launches with profile B")
	}
	// The group default is re-stamped on every summon.
	g, err := db.GetAgentGroupByName("circle-scribe")
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.Equal(t, "prof-b", g.DefaultProfile, "second summon re-stamped the changed profile")
}
