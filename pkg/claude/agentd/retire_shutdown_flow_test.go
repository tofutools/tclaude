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
	f.HaveAliveSession(conv, "spwn-rwsh", tmuxSes, "/tmp/rwsh")
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
	f.HaveAliveSession(conv, "spwn-rnsh", tmuxSes, "/tmp/rnsh")
	f.HaveEnrolledAgent(conv)

	code, resp := postRetire(t, mux, conv, "shutdown=0")
	require.Equal(t, http.StatusOK, code)
	assert.Nil(t, resp.Shutdown, "shutdown was declined — no shutdown outcome should be reported")

	snap := fetchDashSnapshot(t, mux)
	row := retiredRow(snap, conv)
	require.NotNil(t, row, "the retired agent must appear in retired[]")
	assert.True(t, row.Online, "retire --no-shutdown must leave the session alive")
	assert.True(t, f.World.Tmux.IsAlive(tmuxSes), "the tmux session must still be running")
}

// Revocation is the security boundary of retirement: if either permanent or
// temporary grants cannot be removed, the daemon must not demote the agent or
// return a success response that lets a caller claim its access is gone.
func TestRetire_RevocationFailureDoesNotDemoteAgent(t *testing.T) {
	tests := []struct {
		name       string
		prepare    func(*testing.T, string)
		triggerSQL string
		wantError  string
		assertKept func(*testing.T, string)
	}{
		{
			name: "permanent permission",
			prepare: func(t *testing.T, conv string) {
				require.NoError(t, db.GrantAgentPermission(conv, agentd.PermProcessTemplatesManage, "test"))
			},
			triggerSQL: `CREATE TRIGGER fail_permission_revoke BEFORE DELETE ON agent_permissions
				BEGIN SELECT RAISE(FAIL, 'forced permission revoke failure'); END`,
			wantError: "revoke permission grants",
			assertKept: func(t *testing.T, conv string) {
				overrides, err := db.ListAgentPermissionOverridesForConv(conv)
				require.NoError(t, err)
				assert.Equal(t, "grant", overrides[agentd.PermProcessTemplatesManage])
			},
		},
		{
			name: "sudo grant",
			prepare: func(t *testing.T, conv string) {
				_, err := db.InsertSudoGrant(&db.SudoGrant{
					ConvID: conv, Slug: agentd.PermProcessTemplatesManage,
					GrantedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
					GrantedBy: "test", Reason: "retirement failure coverage",
				})
				require.NoError(t, err)
			},
			triggerSQL: `CREATE TRIGGER fail_sudo_revoke BEFORE UPDATE OF revoked_at ON agent_sudo_grants
				BEGIN SELECT RAISE(FAIL, 'forced sudo revoke failure'); END`,
			wantError: "revoke sudo grants",
			assertKept: func(t *testing.T, conv string) {
				active, err := db.ListActiveSudoGrants(conv)
				require.NoError(t, err)
				assert.Len(t, active, 1)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
			f := newFlow(t)
			const conv = "revf-1111-2222-3333-4444"
			f.HaveConvWithTitle(conv, "revocation-failure-worker")
			f.HaveEnrolledAgent(conv)
			tt.prepare(t, conv)
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
			tt.assertKept(t, conv)
		})
	}
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
	f.HaveAliveSession(conv, "spwn-rdef", tmuxSes, "/tmp/rdef")
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
	f.HaveAliveSession(stopConv, "spwn-rcls", "tmux-rcls", "/tmp/rcls")
	f.HaveAliveSession(keepConv, "spwn-rclk", "tmux-rclk", "/tmp/rclk")
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
