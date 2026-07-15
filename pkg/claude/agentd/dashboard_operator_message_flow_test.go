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

func TestDashboardOperatorMessageQueuesSenderlessMail(t *testing.T) {
	f := newFlow(t)
	const target = "target-aaaa-bbbb-cccc-1111"
	f.HaveGroup("team")
	f.HaveMember("team", target)
	f.HaveConvWithTitle(target, "worker")

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	mux := agentd.BuildDashboardHandlerForTest()
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost,
		"/api/operator-message", map[string]any{
			"to": "worker", "body": "Please check the failing test.",
		}))
	require.Equal(t, http.StatusAccepted, rec.Code, "body=%s", rec.Body.String())
	var response struct {
		ID      int64 `json:"id"`
		Queued  bool  `json:"queued"`
		Pending int   `json:"pending"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	assert.True(t, response.Queued)
	assert.GreaterOrEqual(t, response.Pending, 1)
	m, err := db.GetAgentMessage(response.ID)
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Empty(t, m.FromConv, "operator mail must not impersonate an agent")
	assert.Empty(t, m.Subject, "an omitted subject must remain omitted")
	assert.Equal(t, target, m.ToConv)
	assert.Equal(t, "Please check the failing test.", m.Body)
	assert.True(t, m.DeliveredAt.IsZero(), "offline target remains durably queued")
}

func TestDashboardOperatorMessageRequiresDashboardAuth(t *testing.T) {
	mux := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(mux)
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost,
		"/api/operator-message", map[string]any{"to": "nobody", "body": "hello"}))
	assert.Equal(t, http.StatusForbidden, rec.Code)
}
