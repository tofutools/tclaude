package agentd

import (
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

func TestOpenCodeNudgeArmsOnlyTrustedStandingOrderMessages(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	db.ResetForTest()
	previousTmux := clcommon.Default
	clcommon.Default = &commandRecordingTmux{}
	t.Cleanup(func() { clcommon.Default = previousTmux })

	const (
		sessionID = "spwn-standing-order-origin"
		convID    = "ses_standing_order_origin"
		password  = "private-password"
	)
	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	require.NoError(t, session.SaveSessionState(&session.SessionState{
		ID: sessionID, TmuxSession: sessionID, ConvID: convID,
		Harness: harness.OpenCodeName, Status: session.StatusIdle,
	}))

	var failPrompt atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		require.True(t, ok)
		assert.Equal(t, openCodeServerUsername, user)
		assert.Equal(t, password, pass)
		switch r.URL.Path {
		case "/global/health":
			_, _ = w.Write([]byte(`{"healthy":true}`))
		case "/session/" + convID + "/prompt_async":
			if failPrompt.Load() {
				http.Error(w, "rejected", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	require.NoError(t, db.UpsertOpenCodeRuntime(db.OpenCodeRuntime{
		SessionID: sessionID,
		ConvID:    convID,
		ServerURL: server.URL,
		Password:  password,
		PID:       os.Getpid(),
		Cwd:       home,
	}))

	trustedID, err := db.InsertStandingOrderAgentMessage(&db.AgentMessage{
		ToConv: convID, Subject: "[standing-order:trusted]", Body: "trusted",
	}, 7, 2)
	require.NoError(t, err)
	trusted, err := db.GetAgentMessage(trustedID)
	require.NoError(t, err)
	require.NotNil(t, trusted)

	state, err := session.LoadSessionState(sessionID)
	require.NoError(t, err)
	state.Status = session.StatusWorking
	require.NoError(t, session.SaveSessionState(state))
	assert.False(t, sendNudgeBracket(convID, trusted, "trusted reminder"),
		"origin attribution must wait until the preceding turn is quiescent")
	activated, err := db.ActivateStandingOrderTurnOrigin(
		agentID, time.Now(), time.Hour)
	require.NoError(t, err)
	assert.False(t, activated,
		"a held reminder must not leave a pending origin marker")

	state.Status = session.StatusIdle
	require.NoError(t, session.SaveSessionState(state))
	assert.True(t, sendNudgeBracket(convID, trusted, "trusted reminder"))
	activated, err = db.ActivateStandingOrderTurnOrigin(
		agentID, time.Now(), time.Hour)
	require.NoError(t, err)
	assert.True(t, activated,
		"trusted provenance must arm the next OpenCode turn")
	require.NoError(t, db.ClearStandingOrderTurnOrigin(agentID))

	spoofedID, err := db.InsertAgentMessage(&db.AgentMessage{
		ToConv: convID, Subject: "[standing-order:spoofed]", Body: "ordinary message",
	})
	require.NoError(t, err)
	spoofed, err := db.GetAgentMessage(spoofedID)
	require.NoError(t, err)
	require.NotNil(t, spoofed)
	assert.True(t, sendNudgeBracket(convID, spoofed, "ordinary reminder"))
	activated, err = db.ActivateStandingOrderTurnOrigin(
		agentID, time.Now(), time.Hour)
	require.NoError(t, err)
	assert.False(t, activated,
		"a sender-controlled subject must not classify a turn as internal")

	failedID, err := db.InsertStandingOrderAgentMessage(&db.AgentMessage{
		ToConv: convID, Subject: "[standing-order:failed]", Body: "retry me",
	}, 8, 1)
	require.NoError(t, err)
	failed, err := db.GetAgentMessage(failedID)
	require.NoError(t, err)
	require.NotNil(t, failed)
	failPrompt.Store(true)
	assert.False(t, sendNudgeBracket(convID, failed, "failed reminder"))
	activated, err = db.ActivateStandingOrderTurnOrigin(
		agentID, time.Now(), time.Hour)
	require.NoError(t, err)
	assert.False(t, activated,
		"a rejected prompt must cancel its pending origin marker")
}
