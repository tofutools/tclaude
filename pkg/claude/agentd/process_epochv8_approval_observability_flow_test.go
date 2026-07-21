package agentd_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestEpochV8ApprovalObservabilityUsesOnlyRouteTemplates(t *testing.T) {
	for _, tc := range []struct {
		name     string
		method   string
		path     string
		wantPath string
		perm     string
		body     any
	}{
		{
			name: "settlement", method: http.MethodPost,
			path:     "/v1/process/runs/private-settlement-run/unblock?ref=private-settlement-query",
			wantPath: "/v1/process/runs/{id}/unblock", perm: agentd.PermProcessAdvance,
			body: map[string]any{
				"baseBinding": map[string]any{"revision": 1, "digest": strings.Repeat("a", 64)},
				"token":       strings.Repeat("b", 64), "decision": "retry",
				"reason": "private-settlement-reason", "evidenceRef": "private-settlement-evidence",
			},
		},
		{
			name: "settlement-literal-template", method: http.MethodPost,
			path:     "/v1/process/runs/{id}/unblock?ref=private-settlement-query-literal",
			wantPath: "/v1/process/runs/{id}/unblock", perm: agentd.PermProcessAdvance,
			body: map[string]any{
				"baseBinding": map[string]any{"revision": 1, "digest": strings.Repeat("a", 64)},
				"token":       strings.Repeat("b", 64), "decision": "retry",
				"reason": "private-settlement-reason", "evidenceRef": "private-settlement-evidence",
			},
		},
		{
			name: "settlement-encoded-template", method: http.MethodPost,
			path:     "/v1/process/runs/%7Bid%7D/unblock?ref=private-settlement-query-encoded",
			wantPath: "/v1/process/runs/{id}/unblock", perm: agentd.PermProcessAdvance,
			body: map[string]any{
				"baseBinding": map[string]any{"revision": 1, "digest": strings.Repeat("a", 64)},
				"token":       strings.Repeat("b", 64), "decision": "retry",
				"reason": "private-settlement-reason", "evidenceRef": "private-settlement-evidence",
			},
		},
		{
			name: "artifact", method: http.MethodGet,
			path:     "/v1/process/runs/private-artifact-run/epochs/private-artifact-epoch/reason?ref=private-artifact-query",
			wantPath: "/v1/process/runs/{id}/epochs/{epoch}/{artifact}", perm: agentd.PermProcessRunsUnlockRead,
		},
		{
			name: "artifact-literal-template", method: http.MethodGet,
			path:     "/v1/process/runs/{id}/epochs/{epoch}/{artifact}?ref=private-artifact-query-literal",
			wantPath: "/v1/process/runs/{id}/epochs/{epoch}/{artifact}", perm: agentd.PermProcessRunsUnlockRead,
		},
		{
			name: "artifact-encoded-template", method: http.MethodGet,
			path:     "/v1/process/runs/%7Bid%7D/epochs/%7Bepoch%7D/%7Bartifact%7D?ref=private-artifact-query-encoded",
			wantPath: "/v1/process/runs/{id}/epochs/{epoch}/{artifact}", perm: agentd.PermProcessRunsUnlockRead,
		},
		{
			name: "unlock-apply", method: http.MethodPost,
			path:     "/v1/process/runs/private-apply-run/unlock/apply?ref=private-apply-query",
			wantPath: "/v1/process/runs/{id}/unlock/apply", perm: agentd.PermProcessRunsUnlock,
			body: map[string]any{
				"baseBinding": map[string]any{"revision": 1, "digest": strings.Repeat("a", 64)},
				"applyToken":  strings.Repeat("b", 64), "candidateSource": "private-apply-source",
				"reason": "private-apply-reason", "handoffs": []any{"private-apply-handoff"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
			f, _ := processEngineFlow(t)
			cfg, err := config.Load()
			require.NoError(t, err)
			cfg.Agent = &config.AgentConfig{AccessRequestSystemNotification: true}
			require.NoError(t, config.Save(cfg))

			var logs bytes.Buffer
			previousLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(previousLogger) })

			var notifyMu sync.Mutex
			notifiedPath := ""
			t.Cleanup(agentd.SetAccessRequestNotifyForTest(func(_, _, _, _, path string) {
				notifyMu.Lock()
				notifiedPath = path
				notifyMu.Unlock()
			}))

			const caller = "epoch-v8-observability-caller"
			result := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				req := agentd.AsAgentPeer(testharness.JSONRequest(t, tc.method, tc.path, tc.body), caller)
				req.Header.Set("X-Tclaude-Ask-Human", "5s")
				result <- testharness.Serve(f.Mux, req)
			}()

			dashboard := agentd.BuildDashboardHandlerForTest()
			pendingID := ""
			require.Eventually(t, func() bool {
				for _, request := range fetchAccessReqSnapshot(t, dashboard).AccessRequests {
					if request.Status == db.AccessRequestStatusPending && request.Perm == tc.perm {
						pendingID = request.ID
						return request.Path == tc.wantPath
					}
				}
				return false
			}, 10*time.Second, 10*time.Millisecond, "approval did not expose only the safe route template")

			decision := testharness.Serve(dashboard, testharness.JSONRequest(t, http.MethodPost,
				"/api/access-requests/"+pendingID+"/decision", map[string]any{"decision": "deny"}))
			require.Equal(t, http.StatusOK, decision.Code, decision.Body.String())
			response := <-result
			require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())

			notifyMu.Lock()
			gotNotifiedPath := notifiedPath
			notifyMu.Unlock()
			assert.Equal(t, tc.wantPath, gotNotifiedPath)

			history, err := db.ListRecentHandledAccessRequests(10)
			require.NoError(t, err)
			require.NotEmpty(t, history)
			assert.Equal(t, tc.wantPath, history[0].Path)
			assert.Empty(t, history[0].RawQuery)

			audits, err := db.ListAuditLog(db.AuditLogFilter{})
			require.NoError(t, err)
			encodedAudits, err := json.Marshal(audits)
			require.NoError(t, err)
			observed := string(encodedAudits) + "\n" + logs.String() + "\n" + gotNotifiedPath
			for _, sentinel := range []string{
				"private-settlement-run", "private-settlement-query", "private-settlement-reason", "private-settlement-evidence",
				"private-artifact-run", "private-artifact-epoch", "private-artifact-query",
				"private-apply-run", "private-apply-query", "private-apply-source", "private-apply-reason", "private-apply-handoff",
			} {
				assert.NotContains(t, observed, sentinel)
			}
			assert.Contains(t, observed, tc.wantPath)
		})
	}
}
