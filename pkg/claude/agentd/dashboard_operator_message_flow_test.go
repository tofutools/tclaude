package agentd_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

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

	read := testharness.Serve(agentd.BuildHandlerForTest(), agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodGet, "/v1/messages/"+strconv.FormatInt(response.ID, 10), nil), target))
	require.Equal(t, http.StatusOK, read.Code, "body=%s", read.Body.String())
	var detail struct {
		Replyable bool   `json:"replyable"`
		ReplyTo   string `json:"reply_to"`
		ReplyCmd  string `json:"reply_cmd"`
	}
	require.NoError(t, json.Unmarshal(read.Body.Bytes(), &detail))
	assert.False(t, detail.Replyable)
	assert.Empty(t, detail.ReplyTo)
	assert.Empty(t, detail.ReplyCmd)
}

func TestDashboardOperatorMessagePreservesMultilineInline(t *testing.T) {
	f := newFlow(t)
	const target = "target-multiline-cccc-2222"
	const tmux = "operator-multiline"
	f.HaveGroup("team")
	f.HaveMember("team", target)
	f.HaveAliveSession(target, "op-multiline", tmux, "/tmp/work")

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	dashHandler := agentd.BuildDashboardHandlerForTest()
	const body = "Please inspect both failures.\n\tKeep this formatting in one turn."
	rec := testharness.Serve(dashHandler, testharness.JSONRequest(t, http.MethodPost,
		"/api/operator-message", map[string]any{"to": target, "body": body}))
	require.Equal(t, http.StatusAccepted, rec.Code, "body=%s", rec.Body.String())

	f.AssertSentContains(tmux+":0.0", "] "+body, 2*time.Second)
	agentd.WaitForBackgroundForTest()
	rows, err := db.ListAgentMessagesForConv(target, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.False(t, rows[0].DeliveredAt.IsZero())
	assert.False(t, rows[0].ReadAt.IsZero(), "the inlined archival copy is consumed")
}

func TestDashboardOperatorMessageRequiresDashboardAuth(t *testing.T) {
	mux := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(mux)
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost,
		"/api/operator-message", map[string]any{"to": "nobody", "body": "hello"}))
	assert.Equal(t, http.StatusForbidden, rec.Code)
}
