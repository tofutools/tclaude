package agentd_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type externalStewardIssued struct {
	AgentID    string    `json:"agent_id"`
	TokenID    string    `json:"token_id"`
	ExpiresAt  time.Time `json:"expires_at"`
	Credential string    `json:"credential"`
	Grants     []string  `json:"grants"`
}

type externalStewardFlow struct {
	t      *testing.T
	f      *testharness.Flow
	client *http.Client
}

func newExternalStewardFlow(t *testing.T, enabled bool) *externalStewardFlow {
	t.Helper()
	f := newFlow(t)
	enableCollab(t)
	setExternalStewards(t, enabled)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	// The capture gate tests rely on no dashboard having been heard from.
	agentd.ResetDashboardCaptureForTest()
	t.Cleanup(agentd.ResetDashboardCaptureForTest)
	return &externalStewardFlow{t: t, f: f, client: agentd.DaemonSocketClientForTest(t)}
}

func setExternalStewards(t *testing.T, enabled bool) {
	t.Helper()
	cfg, err := config.Load()
	require.NoError(t, err)
	if cfg.Steward == nil {
		cfg.Steward = &config.StewardConfig{}
	}
	cfg.Steward.ExternalEnabled = enabled
	require.NoError(t, config.Save(cfg))
}

func (s *externalStewardFlow) humanPost(path string, body any) (int, string) {
	s.t.Helper()
	rec := testharness.Serve(s.f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(s.t, http.MethodPost, path, body)))
	return rec.Code, rec.Body.String()
}

func (s *externalStewardFlow) create(name string) externalStewardIssued {
	s.t.Helper()
	code, body := s.humanPost("/v1/external-stewards", map[string]any{"name": name})
	require.Equalf(s.t, http.StatusOK, code, "create body=%s", body)
	var out externalStewardIssued
	require.NoError(s.t, json.Unmarshal([]byte(body), &out))
	return out
}

// call goes over a real Unix socket through the production identity middleware.
func (s *externalStewardFlow) call(credential, method, path string, body any, headers ...string) (int, string) {
	s.t.Helper()
	var payload bytes.Buffer
	if body != nil {
		require.NoError(s.t, json.NewEncoder(&payload).Encode(body))
	}
	req, err := http.NewRequest(method, "http://daemon"+path, &payload)
	require.NoError(s.t, err)
	req.Header.Set("Content-Type", "application/json")
	if credential != "" {
		req.Header.Set(agentipc.ExternalStewardCredentialHeader, credential)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := s.client.Do(req)
	require.NoError(s.t, err)
	defer func() { _ = resp.Body.Close() }()
	var out bytes.Buffer
	_, err = out.ReadFrom(resp.Body)
	require.NoError(s.t, err)
	return resp.StatusCode, out.String()
}

func TestExternalSteward_OffByDefault(t *testing.T) {
	s := newExternalStewardFlow(t, false)
	code, body := s.humanPost("/v1/external-stewards", map[string]any{"name": "assistant"})
	assert.Equal(t, http.StatusConflict, code, body)

	setExternalStewards(t, true)
	issued := s.create("assistant")
	code, _ = s.call(issued.Credential, http.MethodGet, "/v1/fleet", nil)
	assert.Equal(t, http.StatusOK, code)

	setExternalStewards(t, false)
	code, body = s.call(issued.Credential, http.MethodGet, "/v1/fleet", nil)
	assert.Equal(t, http.StatusUnauthorized, code, body)
	// Taking access away never needs the feature on.
	code, body = s.humanPost("/v1/external-stewards/assistant/revoke", map[string]any{})
	assert.Equal(t, http.StatusOK, code, body)
}

// With Collab off the routes are absent, as for the Steward, and a credential
// is refused.
func TestExternalSteward_NeedsCollab(t *testing.T) {
	s := newExternalStewardFlow(t, true)
	issued := s.create("assistant")
	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.Features.Collab = false
	require.NoError(t, config.Save(cfg))
	code, _ := s.humanPost("/v1/external-stewards", map[string]any{"name": "other"})
	assert.Equal(t, http.StatusNotFound, code)
	code, _ = s.call(issued.Credential, http.MethodGet, "/v1/fleet", nil)
	assert.Equal(t, http.StatusUnauthorized, code)
}

func TestExternalSteward_OnlyTheHumanManages(t *testing.T) {
	s := newExternalStewardFlow(t, true)
	s.f.HaveConvWithTitle("agent-conv-1", "worker")
	issued := s.create("assistant")
	for _, path := range []string{"/v1/external-stewards", "/v1/external-stewards/assistant/renew", "/v1/external-stewards/assistant/revoke"} {
		rec := testharness.Serve(s.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, path, map[string]any{"name": "x"}), "agent-conv-1"))
		assert.Equal(t, http.StatusForbidden, rec.Code, path)
		code, body := s.call(issued.Credential, http.MethodPost, path, map[string]any{"name": "x"})
		assert.Equal(t, http.StatusForbidden, code, body)
	}
}

func TestExternalSteward_CredentialLifecycle(t *testing.T) {
	s := newExternalStewardFlow(t, true)
	first := s.create("assistant")
	require.True(t, strings.HasPrefix(first.Credential, "tcls_"))
	assert.WithinDuration(t, time.Now().AddDate(0, 0, 30), first.ExpiresAt, time.Minute)

	// Stored hashed, and listed without either secret or hash.
	stored, err := db.LookupExternalStewardCredential(first.TokenID)
	require.NoError(t, err)
	assert.NotContains(t, first.Credential, fmt.Sprintf("%x", stored.Hash))
	_, secret, _ := strings.Cut(strings.TrimPrefix(first.Credential, "tcls_"), "_")
	assert.NotContains(t, string(stored.Hash), secret)
	rec := testharness.Serve(s.f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/external-stewards", nil)))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), secret)
	assert.NotContains(t, rec.Body.String(), "credential\"")

	// A renewal leaves the old credential working until told otherwise.
	code, body := s.humanPost("/v1/external-stewards/assistant/renew", map[string]any{"expires_days": 400})
	assert.Equal(t, http.StatusBadRequest, code, body)
	code, body = s.humanPost("/v1/external-stewards/assistant/renew", map[string]any{})
	require.Equal(t, http.StatusOK, code, body)
	var second externalStewardIssued
	require.NoError(t, json.Unmarshal([]byte(body), &second))
	code, _ = s.call(first.Credential, http.MethodGet, "/v1/fleet", nil)
	assert.Equal(t, http.StatusOK, code)

	code, body = s.humanPost("/v1/external-stewards/assistant/renew", map[string]any{"revoke_existing": true})
	require.Equal(t, http.StatusOK, code, body)
	var third externalStewardIssued
	require.NoError(t, json.Unmarshal([]byte(body), &third))
	for _, ended := range []string{first.Credential, second.Credential} {
		code, _ = s.call(ended, http.MethodGet, "/v1/fleet", nil)
		assert.Equal(t, http.StatusUnauthorized, code)
	}
	code, _ = s.call(third.Credential, http.MethodGet, "/v1/fleet", nil)
	assert.Equal(t, http.StatusOK, code)

	// Revoke takes effect on the next request.
	code, body = s.humanPost("/v1/external-stewards/assistant/revoke", map[string]any{"token_id": third.TokenID})
	require.Equal(t, http.StatusOK, code, body)
	code, _ = s.call(third.Credential, http.MethodGet, "/v1/fleet", nil)
	assert.Equal(t, http.StatusUnauthorized, code)
}

func TestExternalSteward_ExpiredAndRetiredAreRefused(t *testing.T) {
	s := newExternalStewardFlow(t, true)
	issued := s.create("assistant")
	d, err := db.Open()
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE external_steward_credentials SET expires_at = ?`, time.Now().Add(-time.Minute).UnixNano())
	require.NoError(t, err)
	code, body := s.call(issued.Credential, http.MethodGet, "/v1/fleet", nil)
	assert.Equal(t, http.StatusUnauthorized, code)
	assert.Contains(t, body, "expired")

	renewed := s.humanRenew("assistant")
	code, _ = s.call(renewed.Credential, http.MethodGet, "/v1/fleet", nil)
	require.Equal(t, http.StatusOK, code)
	// Through the operator's route, which is not the only writer of retired_at.
	code, body = s.humanPost("/v1/agent/"+renewed.AgentID+"/retire", map[string]any{})
	require.Equal(t, http.StatusOK, code, body)
	code, _ = s.call(renewed.Credential, http.MethodGet, "/v1/fleet", nil)
	assert.Equal(t, http.StatusUnauthorized, code)
	// Bringing the agent back does not bring the credential back.
	rows, err := db.ListExternalStewards()
	require.NoError(t, err)
	_, err = db.PromoteAgent(rows[0].ConvID, "test")
	require.NoError(t, err)
	code, body = s.call(renewed.Credential, http.MethodGet, "/v1/fleet", nil)
	assert.Equal(t, http.StatusUnauthorized, code)
	assert.Contains(t, body, "revoked")
}

func (s *externalStewardFlow) humanRenew(name string) externalStewardIssued {
	s.t.Helper()
	code, body := s.humanPost("/v1/external-stewards/"+name+"/renew", map[string]any{})
	require.Equalf(s.t, http.StatusOK, code, "renew body=%s", body)
	var out externalStewardIssued
	require.NoError(s.t, json.Unmarshal([]byte(body), &out))
	return out
}

// A presented credential decides the request: a bad one never falls through to
// the operator token or the process tree.
func TestExternalSteward_BadCredentialFailsClosed(t *testing.T) {
	s := newExternalStewardFlow(t, true)
	issued := s.create("assistant")
	token := "tclo_external_steward_test"
	t.Cleanup(agentd.SetOperatorTokenForTest(token))
	for _, bad := range []string{"nonsense", "tcls_", "tcls_abc_def", issued.Credential + "x", "tcls_" + issued.TokenID + "_wrong"} {
		code, body := s.call(bad, http.MethodGet, "/v1/fleet", nil, "X-Tclaude-Human-Token", token)
		assert.Equal(t, http.StatusUnauthorized, code, bad)
		assert.Contains(t, body, "credential refused", bad)
	}
	// With a good one the token buys nothing: still not the human.
	code, _ := s.call(issued.Credential, http.MethodGet, "/v1/external-stewards", nil, "X-Tclaude-Human-Token", token)
	assert.Equal(t, http.StatusForbidden, code)
}

// A managed agent that read a credential keeps its own identity.
func TestExternalSteward_ManagedAgentCannotWearIt(t *testing.T) {
	s := newExternalStewardFlow(t, true)
	issued := s.create("assistant")
	t.Cleanup(agentd.SetExternalCallerManagedForTest(true))
	code, body := s.call(issued.Credential, http.MethodGet, "/v1/fleet", nil)
	assert.Equal(t, http.StatusForbidden, code)
	assert.Contains(t, body, "managed agent")
}

// The real check goes by tmux's live pane pids. sessions.pid is the harness after
// the first hook, and a double-forked process in a sandbox is the harness's
// sibling under the sandbox init, so only the pane itself is above both.
func TestExternalSteward_ManagedCheckFindsThePane(t *testing.T) {
	f := newFlow(t)
	f.HaveConvWithTitle("pane-conv-1", "worker")
	f.HaveAliveSession("pane-conv-1", "panes", "tclaude-panes", f.TestCwd("w"))
	const pane, bwrap, sandboxInit, harness, forked, shell, assistant = 4100, 4101, 4102, 4103, 4104, 5100, 5101
	f.World.Tmux.SetPaneIdentityForTest("tclaude-panes", "%7", pane)
	row, err := db.FindSessionByConvID("pane-conv-1")
	require.NoError(t, err)
	row.PID = harness
	require.NoError(t, db.SaveSession(row))
	t.Cleanup(agentd.SetProcTreeForTest(
		map[int]string{pane: "sh", bwrap: "bwrap", sandboxInit: "bwrap", harness: "claude", forked: "daemon", shell: "fish", assistant: "node"},
		map[int]int{forked: sandboxInit, harness: sandboxInit, sandboxInit: bwrap, bwrap: pane, pane: 1, assistant: shell, shell: 1}))
	assert.True(t, agentd.ExternalCallerManagedForTest(forked, ""), "under a live pane")
	assert.True(t, agentd.ExternalCallerManagedForTest(assistant, "claimed-session"), "a session claim is agent-shaped")
	assert.False(t, agentd.ExternalCallerManagedForTest(assistant, ""), "the operator's own assistant")

	// A one-shot's pane has no session row, and is still tclaude's.
	d, err := db.Open()
	require.NoError(t, err)
	_, err = d.Exec(`DELETE FROM sessions WHERE tmux_session = 'tclaude-panes'`)
	require.NoError(t, err)
	assert.True(t, agentd.ExternalCallerManagedForTest(forked, ""), "a pane with no session row")
}

// A session row proves nothing: installed hooks can register the operator's own
// claude with whichever tclaude session was current.
func TestExternalSteward_ManagedCheckIgnoresSessionRows(t *testing.T) {
	f := newFlow(t)
	f.HaveConvWithTitle("plain-conv-1", "worker")
	f.HaveAliveSession("plain-conv-1", "plain", "tclaude-plain", f.TestCwd("w"))
	const claude, assistant = 6100, 6101
	row, err := db.FindSessionByConvID("plain-conv-1")
	require.NoError(t, err)
	row.PID = claude
	require.NoError(t, db.SaveSession(row))
	t.Cleanup(agentd.SetProcTreeForTest(map[int]string{claude: "claude", assistant: "tclaude"}, map[int]int{assistant: claude, claude: 1}))
	assert.False(t, agentd.ExternalCallerManagedForTest(assistant, ""))
}

// A peer can connect and exit, leaving a live socket and no ancestry to read.
func TestExternalSteward_ManagedCheckRefusesAGonePeer(t *testing.T) {
	newFlow(t)
	const beyondPidMax = 1 << 23
	assert.True(t, agentd.ExternalCallerManagedForTest(beyondPidMax, ""))
}

// No handler below the identity layer can see the credential, so none can log it
// or send it upstream.
func TestExternalSteward_HandlersNeverSeeTheCredential(t *testing.T) {
	s := newExternalStewardFlow(t, true)
	issued := s.create("assistant")
	seen := make(chan string, 1)
	s.client = agentd.CapturingSocketClientForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get(agentipc.ExternalStewardCredentialHeader) + "|" + fmt.Sprint(len(r.Header.Values(agentipc.ExternalStewardCredentialHeader)))
	}))
	code, body := s.call(issued.Credential, http.MethodGet, "/anything", nil)
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, "|0", <-seen)
}

// capture reports whether the principal got past the dashboard.capture gate:
// with a grant the route goes on to find no dashboard open.
func (s *externalStewardFlow) capture(credential string) int {
	s.t.Helper()
	code, _ := s.call(credential, http.MethodPost, "/v1/dashboard/capture", map[string]any{})
	return code
}

func TestExternalSteward_StartsWithTheStewardLevelsLessOptIns(t *testing.T) {
	s := newExternalStewardFlow(t, true)
	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.Steward.Writes = config.StewardWritesWorkers
	cfg.Agent = &config.AgentConfig{DefaultPermissions: []string{agentd.PermDashboardCapture}}
	require.NoError(t, config.Save(cfg))
	issued := s.create("assistant")
	assert.Contains(t, issued.Grants, agentd.PermFleetRead)
	assert.Contains(t, issued.Grants, agentd.PermMessageDirect)
	assert.Contains(t, issued.Grants, agentd.PermHumanNotify)
	for _, optIn := range []string{agentd.PermDashboardCapture, agentd.PermDashboardFormsCapture, agentd.PermDashboardFormsFill, agentd.PermAgentSpawn} {
		assert.NotContains(t, issued.Grants, optIn)
	}
	// agent.default_permissions is for agents tclaude runs, not this one.
	assert.Equal(t, http.StatusForbidden, s.capture(issued.Credential))
	code, body := s.humanPost("/v1/permissions/grant", map[string]any{"target": issued.AgentID, "slug": agentd.PermDashboardCapture})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, http.StatusNotFound, s.capture(issued.Credential), "with the grant the gate opens")
}

// The Steward role would bring capture and stored grants with it.
func TestExternalSteward_CannotTakeTheStewardRole(t *testing.T) {
	s := newExternalStewardFlow(t, true)
	issued := s.create("assistant")
	rec := testharness.Serve(dashMessageMux(t), testharness.JSONRequest(t, http.MethodPost, "/api/steward/handover",
		map[string]any{"agent": issued.AgentID, "then": "roster"}))
	assert.NotEqual(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "cannot take the Steward role")
	assert.Equal(t, http.StatusForbidden, s.capture(issued.Credential))
}

func TestExternalSteward_NeverListGrantFailsLoudly(t *testing.T) {
	s := newExternalStewardFlow(t, true)
	issued := s.create("assistant")
	dash := dashMessageMux(t)
	for _, slug := range agentd.ExternalStewardNeverForTest() {
		code, body := s.humanPost("/v1/permissions/grant", map[string]any{"target": issued.AgentID, "slug": slug})
		assert.Equalf(t, http.StatusConflict, code, "%s: %s", slug, body)
		assert.Contains(t, body, "can never hold")
		rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost, "/api/permissions", map[string]any{
			"conv": issued.AgentID, "overrides": map[string]string{slug: "grant"},
		}))
		assert.Equalf(t, http.StatusConflict, rec.Code, "%s: %s", slug, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "can never hold")
	}
	// An ordinary opt-in works.
	code, body := s.humanPost("/v1/permissions/grant", map[string]any{"target": issued.AgentID, "slug": agentd.PermDashboardCapture})
	assert.Equal(t, http.StatusOK, code, body)
}

func TestExternalSteward_NoSudo(t *testing.T) {
	s := newExternalStewardFlow(t, true)
	issued := s.create("assistant")
	code, body := s.call(issued.Credential, http.MethodPost, "/v1/sudo", map[string]any{"slugs": []string{agentd.PermAgentSpawn}, "duration": "5m"})
	assert.Equal(t, http.StatusForbidden, code, body)
	assert.Contains(t, body, "cannot hold sudo")

	rec := testharness.Serve(dashMessageMux(t), testharness.JSONRequest(t, http.MethodPost, "/api/sudo", map[string]any{
		"agent_id": issued.AgentID, "slugs": []string{agentd.PermAgentSpawn}, "duration": "5m",
	}))
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "cannot hold sudo")

	// A row that got there anyway is not consulted.
	now := time.Now()
	id, err := db.InsertSudoGrantForAgent(&db.SudoGrant{AgentID: issued.AgentID, Slug: agentd.PermDashboardCapture,
		GrantedAt: now, ExpiresAt: now.Add(time.Hour), GrantedBy: "test"})
	require.NoError(t, err)
	require.NotZero(t, id)
	assert.Equal(t, http.StatusForbidden, s.capture(issued.Credential))
}

// The credential is in no log, no audit row and no response but the one that
// issues it.
func TestExternalSteward_CredentialIsNeverRecorded(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	s := newExternalStewardFlow(t, true)
	issued := s.create("assistant")
	_, secret, _ := strings.Cut(strings.TrimPrefix(issued.Credential, "tcls_"), "_")
	code, body := s.call(issued.Credential, http.MethodPost, "/v1/notify-human", map[string]any{"body": "hello"})
	require.Equal(t, http.StatusOK, code, body)
	s.call(issued.Credential+"x", http.MethodGet, "/v1/fleet", nil)
	s.humanRenew("assistant")

	// What the CLI sends: the replay table must not keep the response either.
	req := testharness.JSONRequest(t, http.MethodPost, "/v1/external-stewards", map[string]any{"name": "second"})
	req.Header.Set(agent.IdempotencyKeyHeader, "3f2b8c1e-5a44-4e7e-9a55-0d6f7f3a1c21")
	req.Header.Set(agent.RequestDigestHeader, "digest")
	rec := testharness.Serve(s.f.Mux, agentd.AsHumanPeer(req))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "tcls_")
	req = testharness.JSONRequest(t, http.MethodPost, "/v1/external-stewards/assistant/renew", map[string]any{})
	req.Header.Set(agent.IdempotencyKeyHeader, "7a1d2e3f-6b55-4f8f-8b66-1e7a8f4b2d32")
	req.Header.Set(agent.RequestDigestHeader, "digest")
	rec = testharness.Serve(s.f.Mux, agentd.AsHumanPeer(req))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	d, err := db.Open()
	require.NoError(t, err)
	var replayable int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM agentd_idempotency WHERE response_body LIKE '%tcls_%'`).Scan(&replayable))
	assert.Zero(t, replayable, "a credential was kept for replay")

	rows, err := db.ListAuditLog(db.AuditLogFilter{})
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	sawExternal := false
	for _, row := range rows {
		dump := fmt.Sprintf("%+v", row)
		assert.NotContains(t, dump, secret)
		assert.NotContains(t, dump, "tcls_")
		sawExternal = sawExternal || row.ActorLabel == "external:assistant"
	}
	assert.True(t, sawExternal, "the audit row names the principal as external")
	assert.NotContains(t, logs.String(), secret)
	assert.NotContains(t, logs.String(), "tcls_")
}

// A principal has no pane, so nothing is ever nudged and mail waits unread. The
// backlog cap still counts that mail, as for any offline agent: senders are told
// plainly once it is full, and reading a message makes room.
func TestExternalSteward_InboxWithoutAPane(t *testing.T) {
	s := newExternalStewardFlow(t, true)
	issued := s.create("assistant")
	s.f.HaveConvWithTitle("worker-conv-1", "worker")
	s.f.HaveAliveSession("worker-conv-1", "workers", "tclaude-workers", s.f.TestCwd("w"))
	require.NoError(t, db.GrantAgentPermissionWithScope("worker-conv-1", agentd.PermMessageDirect, "", "test"))
	send := func() *httptest.ResponseRecorder {
		return testharness.Serve(s.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/messages",
			map[string]any{"to": "assistant", "body": "report"}), "worker-conv-1"))
	}
	limit := agentd.RegularAgentMessageQueueLimitForTest
	for i := range limit {
		rec := send()
		require.Equalf(t, http.StatusOK, rec.Code, "send %d body=%s", i, rec.Body.String())
	}
	rec := send()
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "queue_full")

	code, body := s.call(issued.Credential, http.MethodGet, fmt.Sprintf("/v1/inbox?unread=1&limit=%d", limit+5), nil)
	require.Equal(t, http.StatusOK, code, body)
	var inbox []struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal([]byte(extractInboxItems(t, body)), &inbox))
	require.Len(t, inbox, limit)

	code, body = s.call(issued.Credential, http.MethodGet, fmt.Sprintf("/v1/messages/%d", inbox[0].ID), nil)
	require.Equal(t, http.StatusOK, code, body)
	rec = send()
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// And it can answer.
	code, body = s.call(issued.Credential, http.MethodPost, "/v1/messages", map[string]any{"to": "worker", "body": "thanks"})
	require.Equal(t, http.StatusOK, code, body)
}

func extractInboxItems(t *testing.T, body string) string {
	t.Helper()
	trimmed := strings.TrimSpace(body)
	if strings.HasPrefix(trimmed, "[") {
		return trimmed
	}
	var wrapped map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(trimmed), &wrapped))
	for _, key := range []string{"messages", "items", "inbox"} {
		if raw, ok := wrapped[key]; ok {
			return string(raw)
		}
	}
	t.Fatalf("no message list in %s", body)
	return ""
}

// A principal lists as a named, offline agent on every roster it reaches, with
// no session behind it, and nothing can start one.
func TestExternalSteward_ListedOfflineAndNeverLaunched(t *testing.T) {
	s := newExternalStewardFlow(t, true)
	issued := s.create("assistant")
	rows, err := db.ListExternalStewards()
	require.NoError(t, err)
	conv := rows[0].ConvID
	s.f.HaveConvWithTitle("worker-conv-2", "worker")
	s.f.HaveAliveSession("worker-conv-2", "workers2", "tclaude-workers2", s.f.TestCwd("w"))
	rec := testharness.Serve(s.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/peers", nil), "worker-conv-2"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "assistant")
	dash := dashMessageMux(t)
	found := false
	for _, a := range fetchSnapshotOnly(t, dash).Agents {
		if a.ConvID == conv {
			found = true
			assert.Equal(t, "assistant", a.Title)
			assert.False(t, a.Online)
		}
	}
	assert.True(t, found, "the dashboard roster lists the principal")

	rec = testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost, "/api/agents/"+issued.AgentID+"/resume", nil))
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "outside tclaude")
	// Reincarnating already needs a live pane.
	rec = testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost, "/api/agents/"+issued.AgentID+"/reincarnate", map[string]any{"follow_up": "x"}))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "no_tmux")
}
