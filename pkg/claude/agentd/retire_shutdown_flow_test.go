package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Flow coverage for retire + optional shutdown: every retire surface
// can also soft-stop the agent's running session, defaulting to ON.
// These scenarios drive the dashboard surfaces the browser uses — the
// per-row retire button (POST /api/agents/{conv}/retire) and the bulk
// cleanup modal's retire tier (POST /api/cleanup/agents) — and assert
// the demotion (retired[]) and the session liveness independently.

// retireShutdownResp decodes the parts of POST .../retire this feature
// added: the shutdown sub-object is present only when shutdown ran.
type retireShutdownResp struct {
	ConvID   string `json:"conv_id"`
	Shutdown *struct {
		Action string `json:"action"`
		Detail string `json:"detail"`
	} `json:"shutdown"`
}

// postRetire fires the per-row retire button's request at the
// dashboard mux. query is the raw query string (e.g. "shutdown=0"),
// empty for none.
func postRetire(t *testing.T, mux http.Handler, conv, query string) (int, retireShutdownResp) {
	t.Helper()
	path := "/api/agents/" + conv + "/retire"
	if query != "" {
		path += "?" + query
	}
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost, path, nil))
	var resp retireShutdownResp
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp),
			"decode retire response: %s", rec.Body.String())
	}
	return rec.Code, resp
}

// retiredRow returns the retired[] snapshot entry for conv, or nil.
func retiredRow(snap dashSnapshot, conv string) *dashRetired {
	for i := range snap.Retired {
		if snap.Retired[i].ConvID == conv {
			return &snap.Retired[i]
		}
	}
	return nil
}

// Scenario: retire with shutdown ON (the default UI choice) — the
// agent is demoted to a retired conversation AND its running tmux
// session is soft-exited. Retire semantics stay intact: the agent
// leaves the roster, lands in retired[], and is reinstatable.
func TestRetire_WithShutdownStopsRunningSession(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	const conv = "rwsh-1111-2222-3333-4444"
	const tmuxSes = "tmux-rwsh"
	f.HaveConvWithTitle(conv, "doomed-worker")
	f.HaveAliveSession(conv, "spwn-rwsh", tmuxSes, f.TestCwd("rwsh"))
	f.HaveEnrolledAgent(conv)
	require.True(t, f.World.Tmux.IsAlive(tmuxSes), "pre: the agent's session is alive")

	code, resp := postRetire(t, mux, conv, "shutdown=1")
	require.Equal(t, http.StatusOK, code)
	require.NotNil(t, resp.Shutdown, "shutdown was requested — the response must report its outcome")
	assert.Equal(t, "soft_stopped", resp.Shutdown.Action,
		"a live session must be soft-exited, never force-killed; detail=%s", resp.Shutdown.Detail)

	snap := fetchDashSnapshot(t, mux)
	assert.False(t, agentInSnap(snap.Agents, conv), "a retired agent leaves the active roster")
	row := retiredRow(snap, conv)
	require.NotNil(t, row, "the retired agent must appear in retired[]")
	assert.False(t, row.Online, "retire-with-shutdown must leave the session stopped")
	assert.False(t, f.World.Tmux.IsAlive(tmuxSes), "the tmux session must be gone")
}

// Scenario: retire with shutdown OFF (the --no-shutdown / unticked
// checkbox path) — the agent is demoted but its session keeps
// running. The response carries no shutdown outcome at all.
func TestRetire_WithoutShutdownKeepsSessionAlive(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	const conv = "rnsh-1111-2222-3333-4444"
	const tmuxSes = "tmux-rnsh"
	f.HaveConvWithTitle(conv, "kept-worker")
	f.HaveAliveSession(conv, "spwn-rnsh", tmuxSes, f.TestCwd("rnsh"))
	f.HaveEnrolledAgent(conv)

	code, resp := postRetire(t, mux, conv, "shutdown=0")
	require.Equal(t, http.StatusOK, code)
	assert.Nil(t, resp.Shutdown, "shutdown was declined — no shutdown outcome should be reported")

	snap := fetchDashSnapshot(t, mux)
	row := retiredRow(snap, conv)
	require.NotNil(t, row, "the retired agent must appear in retired[]")
	assert.True(t, row.Online, "retire --no-shutdown must leave the session alive")
	assert.True(t, f.World.Tmux.IsAlive(tmuxSes), "the tmux session must still be running")

	// A deliberately still-running retired pane cannot retain authority through
	// global defaults. Enrollment state gates every permission source first.
	cfg, err := config.Load()
	require.NoError(t, err)
	if cfg.Agent == nil {
		cfg.Agent = &config.AgentConfig{}
	}
	cfg.Agent.DefaultPermissions = []string{agentd.PermGroupsSpawn}
	require.NoError(t, config.Save(cfg))
	denied := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe", map[string]any{
		"name": "retired-scribe", "slugs": []string{agentd.PermTemplatesManage}, "brief": "must not run",
	}), conv))
	assert.Equal(t, http.StatusForbidden, denied.Code, denied.Body.String())
	assert.Contains(t, denied.Body.String(), "retired agent")
}

// Revocation is the security boundary of retirement: if either permanent or
// temporary grants cannot be removed, the daemon must not demote the agent or
// return a success response that lets a caller claim its access is gone.
func TestRetire_RevocationFailureDoesNotDemoteAgent(t *testing.T) {
	tests := []struct {
		name        string
		triggerName string
		triggerSQL  string
		wantError   string
	}{
		{
			name:        "owned cron job",
			triggerName: "fail_cron_disable",
			triggerSQL: `CREATE TRIGGER fail_cron_disable BEFORE UPDATE OF enabled ON agent_cron_jobs
				WHEN OLD.enabled = 1 AND NEW.enabled = 0
				BEGIN SELECT RAISE(FAIL, 'forced cron disable failure'); END`,
			wantError: "disable cron jobs",
		},
		{
			name:        "permanent permission",
			triggerName: "fail_permission_revoke",
			triggerSQL: `CREATE TRIGGER fail_permission_revoke BEFORE DELETE ON agent_permissions
				BEGIN SELECT RAISE(FAIL, 'forced permission revoke failure'); END`,
			wantError: "revoke permission grants",
		},
		{
			name:        "sudo grant",
			triggerName: "fail_sudo_revoke",
			triggerSQL: `CREATE TRIGGER fail_sudo_revoke BEFORE UPDATE OF revoked_at ON agent_sudo_grants
				BEGIN SELECT RAISE(FAIL, 'forced sudo revoke failure'); END`,
			wantError: "revoke sudo grants",
		},
		{
			name:        "retirement state",
			triggerName: "fail_agent_retire",
			triggerSQL: `CREATE TRIGGER fail_agent_retire BEFORE UPDATE OF retired_at ON agents
				WHEN NEW.retired_at <> '' BEGIN SELECT RAISE(FAIL, 'forced retirement failure'); END`,
			wantError: "retire",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
			f := newFlow(t)
			const conv = "revf-1111-2222-3333-4444"
			f.HaveConvWithTitle(conv, "revocation-failure-worker")
			group := f.HaveGroup("retirement-rollback")
			f.HaveMember(group.Name, conv)
			require.NoError(t, db.AddAgentGroupOwner(group.ID, conv, "test"))
			require.NoError(t, db.GrantAgentPermission(conv, agentd.PermProcessTemplatesManage, "test"))
			_, err := db.InsertSudoGrant(&db.SudoGrant{
				ConvID: conv, Slug: agentd.PermProcessTemplatesManage,
				GrantedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
				GrantedBy: "test", Reason: "retirement failure coverage",
			})
			require.NoError(t, err)
			cronID, err := db.InsertAgentCronJob(&db.AgentCronJob{
				Name: "retirement-rollback", OwnerConv: conv, TargetKind: db.CronTargetGroup,
				GroupID: group.ID, IntervalSeconds: 60, Body: "must roll back", Enabled: true,
			})
			require.NoError(t, err)
			d, err := db.Open()
			require.NoError(t, err)
			_, err = d.Exec(tt.triggerSQL)
			require.NoError(t, err)

			rec := testharness.Serve(agentd.BuildDashboardHandlerForTest(),
				testharness.JSONRequest(t, http.MethodPost, "/api/agents/"+conv+"/retire?shutdown=1&delete_worktree=0", nil))
			assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), tt.wantError)
			live, err := db.IsLiveAgentConv(conv)
			require.NoError(t, err)
			assert.True(t, live, "revocation failure must leave the agent active for a safe retry")
			overrides, err := db.ListAgentPermissionOverridesForConv(conv)
			require.NoError(t, err)
			assert.Equal(t, "grant", overrides[agentd.PermProcessTemplatesManage],
				"either failure must roll back the permanent permission revoke")
			active, err := db.ListActiveSudoGrants(conv)
			require.NoError(t, err)
			assert.Len(t, active, 1, "either failure must roll back the sudo revoke")
			groups, err := db.ListGroupsForConv(conv)
			require.NoError(t, err)
			require.Len(t, groups, 1, "failure must roll back group membership")
			assert.Equal(t, group.Name, groups[0].Name)
			owner, err := db.IsAgentGroupOwner(group.ID, conv)
			require.NoError(t, err)
			assert.True(t, owner, "failure must roll back group ownership")
			cron, err := db.GetAgentCronJob(cronID)
			require.NoError(t, err)
			require.NotNil(t, cron)
			assert.True(t, cron.Enabled, "failure must roll back the owned cron disable")

			_, err = d.Exec(`DROP TRIGGER ` + tt.triggerName)
			require.NoError(t, err)
			retry := testharness.Serve(agentd.BuildDashboardHandlerForTest(),
				testharness.JSONRequest(t, http.MethodPost, "/api/agents/"+conv+"/retire?shutdown=1&delete_worktree=0", nil))
			require.Equal(t, http.StatusOK, retry.Code, retry.Body.String())
			var retryResp struct {
				Outcome struct {
					GroupsLeft   []string `json:"groups_left"`
					PermsRevoked int64    `json:"perms_revoked"`
					SudoRevoked  int64    `json:"sudo_revoked"`
					CronDisabled int64    `json:"cron_disabled"`
					Retired      bool     `json:"retired"`
				} `json:"outcome"`
			}
			require.NoError(t, json.Unmarshal(retry.Body.Bytes(), &retryResp))
			assert.Equal(t, []string{group.Name}, retryResp.Outcome.GroupsLeft)
			assert.Equal(t, int64(1), retryResp.Outcome.PermsRevoked)
			assert.Equal(t, int64(1), retryResp.Outcome.SudoRevoked)
			assert.Equal(t, int64(1), retryResp.Outcome.CronDisabled)
			assert.True(t, retryResp.Outcome.Retired, "retry response truthfully reports the demotion")
			live, err = db.IsLiveAgentConv(conv)
			require.NoError(t, err)
			assert.False(t, live, "retry after the transient failure must retire the agent")
			overrides, err = db.ListAgentPermissionOverridesForConv(conv)
			require.NoError(t, err)
			assert.Empty(t, overrides, "successful retry removes permanent overrides")
			active, err = db.ListActiveSudoGrants(conv)
			require.NoError(t, err)
			assert.Empty(t, active, "successful retry revokes sudo grants")
			groups, err = db.ListGroupsForConv(conv)
			require.NoError(t, err)
			assert.Empty(t, groups, "successful retry removes group membership")
			owner, err = db.IsAgentGroupOwner(group.ID, conv)
			require.NoError(t, err)
			assert.False(t, owner, "successful retry removes group ownership")
			cron, err = db.GetAgentCronJob(cronID)
			require.NoError(t, err)
			require.NotNil(t, cron)
			assert.False(t, cron.Enabled, "successful retry disables owned cron authority")
			assert.Equal(t, db.CronDisabledReasonAgentRetired, cron.DisabledReason)
		})
	}
}

func TestRetire_CachedDueCronCannotFireAfterCommit(t *testing.T) {
	f := newFlow(t)
	group := f.HaveGroup("cron-retire-race")
	const owner = "crrr-owner-aaaa-bbbb-cccc"
	const worker = "crrr-worker-aaaa-bbbb-cccc"
	f.HaveConvWithTitle(owner, "cron owner")
	f.HaveConvWithTitle(worker, "cron worker")
	f.HaveMember(group.Name, owner)
	f.HaveMember(group.Name, worker)
	jobID, err := db.InsertAgentCronJob(&db.AgentCronJob{
		Name: "cached-before-retire", OwnerConv: owner, TargetKind: db.CronTargetGroup,
		GroupID: group.ID, IntervalSeconds: 60, Body: "must not arrive", Enabled: true,
	})
	require.NoError(t, err)

	var retireCode int
	var retireBody string
	restore := agentd.SetCronAfterDueListForTest(func() {
		rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(
			testharness.JSONRequest(t, http.MethodPost, "/v1/agent/"+owner+"/retire?shutdown=0&delete_worktree=0", nil)))
		retireCode = rec.Code
		retireBody = rec.Body.String()
	})
	t.Cleanup(restore)
	agentd.RunCronTickForTest(time.Now().Add(time.Hour))
	require.Equal(t, http.StatusOK, retireCode, retireBody)

	messages, err := db.ListAgentMessagesForConv(worker, 100)
	require.NoError(t, err)
	assert.Empty(t, messages, "cached due row is revalidated after retirement")
	runs, err := db.ListAgentCronRunsForJob(jobID, 10)
	require.NoError(t, err)
	assert.Empty(t, runs, "skipped cached row records no false execution")
}

// Scenario: an absent shutdown param defaults to ON. Every retire
// surface inherits the shutdown-by-default behaviour, so a request
// that omits the flag still stops the session.
func TestRetire_AbsentShutdownParamDefaultsToOn(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	const conv = "rdef-1111-2222-3333-4444"
	const tmuxSes = "tmux-rdef"
	f.HaveConvWithTitle(conv, "default-worker")
	f.HaveAliveSession(conv, "spwn-rdef", tmuxSes, f.TestCwd("rdef"))
	f.HaveEnrolledAgent(conv)

	code, resp := postRetire(t, mux, conv, "" /* no shutdown param */)
	require.Equal(t, http.StatusOK, code)
	require.NotNil(t, resp.Shutdown, "an absent shutdown param must default to ON")
	assert.Equal(t, "soft_stopped", resp.Shutdown.Action)
	assert.False(t, f.World.Tmux.IsAlive(tmuxSes), "the default must stop the session")
}

// Scenario: the bulk cleanup modal's retire tier honours the same
// "also shut down" toggle — one checkbox governs the whole batch.
// include_online lifts the skip-online guard so the retire reaches a
// running agent; shutdown then decides whether its pane is stopped.
func TestRetire_CleanupTierShutdownToggle(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	const stopConv = "rcls-1111-2222-3333-4444"
	const keepConv = "rclk-1111-2222-3333-4444"
	f.HaveConvWithTitle(stopConv, "stop-me")
	f.HaveConvWithTitle(keepConv, "keep-me")
	f.HaveAliveSession(stopConv, "spwn-rcls", "tmux-rcls", f.TestCwd("rcls"))
	f.HaveAliveSession(keepConv, "spwn-rclk", "tmux-rclk", f.TestCwd("rclk"))
	f.HaveEnrolledAgent(stopConv)
	f.HaveEnrolledAgent(keepConv)

	// shutdown:true — the retired agent's live session is soft-stopped.
	stopResp := postCleanup(t, mux, "/api/cleanup/agents",
		`{"agents":["`+stopConv+`"],"mode":"retire","include_online":true,"shutdown":true}`)
	assert.Equal(t, 1, stopResp.Retired, "the agent was retired; outcomes=%+v", stopResp.Outcomes)
	require.Len(t, stopResp.Outcomes, 1)
	assert.Contains(t, stopResp.Outcomes[0].Detail, "session soft-stopped")
	assert.False(t, f.World.Tmux.IsAlive("tmux-rcls"), "shutdown:true must stop the session")

	// shutdown:false — the retired agent keeps its running session.
	keepResp := postCleanup(t, mux, "/api/cleanup/agents",
		`{"agents":["`+keepConv+`"],"mode":"retire","include_online":true,"shutdown":false}`)
	assert.Equal(t, 1, keepResp.Retired, "the agent was retired; outcomes=%+v", keepResp.Outcomes)
	require.Len(t, keepResp.Outcomes, 1)
	assert.NotContains(t, keepResp.Outcomes[0].Detail, "soft-stopped")
	assert.True(t, f.World.Tmux.IsAlive("tmux-rclk"), "shutdown:false must keep the session alive")
}
