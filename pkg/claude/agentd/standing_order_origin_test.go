package agentd

import (
	"context"
	"encoding/json"
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
	var dropPromptResponse atomic.Bool
	var exposeAcceptedMessage atomic.Bool
	var promptPosts atomic.Int32
	var promptMessageID atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		require.True(t, ok)
		assert.Equal(t, openCodeServerUsername, user)
		assert.Equal(t, password, pass)
		if exposeAcceptedMessage.Load() {
			if id, ok := promptMessageID.Load().(string); ok {
				switch r.URL.Path {
				case "/session/" + convID + "/message/" + id:
					_, _ = w.Write([]byte(`{"info":{"id":"` + id + `","role":"user"}}`))
					return
				case "/session/" + convID + "/message":
					_, _ = w.Write([]byte(
						`[{"info":{"id":"` + id + `","role":"user"}}]`))
					return
				}
			}
		}
		switch r.URL.Path {
		case "/global/health":
			_, _ = w.Write([]byte(`{"healthy":true}`))
		case "/session/" + convID + "/prompt_async":
			promptPosts.Add(1)
			var body struct {
				MessageID string `json:"messageID"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			if body.MessageID != "" {
				promptMessageID.Store(body.MessageID)
			}
			if failPrompt.Load() {
				http.Error(w, "rejected", http.StatusInternalServerError)
				return
			}
			if dropPromptResponse.Load() {
				conn, _, hijackErr := w.(http.Hijacker).Hijack()
				require.NoError(t, hijackErr)
				require.NoError(t, conn.Close())
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
	turnOrigin, err := db.GetStandingOrderTurnOrigin(agentID, convID, time.Now())
	require.NoError(t, err)
	assert.Nil(t, turnOrigin,
		"a held reminder must not leave a pending origin marker")

	state.Status = session.StatusIdle
	require.NoError(t, session.SaveSessionState(state))
	assert.True(t, sendNudgeBracket(convID, trusted, "trusted reminder"))
	turnOrigin, err = db.GetStandingOrderTurnOrigin(agentID, convID, time.Now())
	require.NoError(t, err)
	require.NotNil(t, turnOrigin)
	assert.Equal(t, db.StandingOrderTurnOriginPending, turnOrigin.State)
	assert.Equal(t, promptMessageID.Load(), turnOrigin.OpenCodeMessageID)

	projector := newOpenCodeEventProjector(convID, home)
	assert.True(t, consumeOpenCodeEvent(context.Background(),
		db.OpenCodeRuntime{SessionID: sessionID, ConvID: convID}, projector,
		json.RawMessage(openCodeTestEvent("evt_assistant", "message.updated", convID,
			`"info":{"id":"msg_assistant","sessionID":"`+convID+
				`","role":"assistant","parentID":"`+
				turnOrigin.OpenCodeMessageID+`"}`))))
	turnOrigin, err = db.GetStandingOrderTurnOrigin(agentID, convID, time.Now())
	require.NoError(t, err)
	require.NotNil(t, turnOrigin)
	assert.Equal(t, db.StandingOrderTurnOriginActive, turnOrigin.State,
		"only the assistant whose parent is the submitted reminder activates suppression")
	require.NoError(t, db.CompleteStandingOrderTurnOrigin(agentID, convID))

	spoofedID, err := db.InsertAgentMessage(&db.AgentMessage{
		ToConv: convID, Subject: "[standing-order:spoofed]", Body: "ordinary message",
	})
	require.NoError(t, err)
	spoofed, err := db.GetAgentMessage(spoofedID)
	require.NoError(t, err)
	require.NotNil(t, spoofed)
	assert.True(t, sendNudgeBracket(convID, spoofed, "ordinary reminder"))
	turnOrigin, err = db.GetStandingOrderTurnOrigin(agentID, convID, time.Now())
	require.NoError(t, err)
	assert.Nil(t, turnOrigin,
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
	turnOrigin, err = db.GetStandingOrderTurnOrigin(agentID, convID, time.Now())
	require.NoError(t, err)
	assert.Nil(t, turnOrigin,
		"a rejected prompt must cancel its pending origin marker")

	uncertainID, err := db.InsertStandingOrderAgentMessage(&db.AgentMessage{
		ToConv: convID, Subject: "[standing-order:uncertain]", Body: "hold me",
	}, 9, 1)
	require.NoError(t, err)
	uncertain, err := db.GetAgentMessage(uncertainID)
	require.NoError(t, err)
	require.NotNil(t, uncertain)
	failPrompt.Store(false)
	dropPromptResponse.Store(true)
	assert.False(t, sendNudgeBracket(convID, uncertain, "uncertain reminder"))
	turnOrigin, err = db.GetStandingOrderTurnOrigin(agentID, convID, time.Now())
	require.NoError(t, err)
	require.NotNil(t, turnOrigin)
	assert.Equal(t, db.StandingOrderTurnOriginPending, turnOrigin.State,
		"a transport failure after submission may hide acceptance and must retain suppression")

	postsBeforeReconcile := promptPosts.Load()
	dropPromptResponse.Store(false)
	exposeAcceptedMessage.Store(true)
	assert.True(t, sendNudgeBracket(convID, uncertain, "uncertain reminder"),
		"an authoritative message lookup confirms the prior accepted attempt")
	assert.Equal(t, postsBeforeReconcile, promptPosts.Load(),
		"reconciliation must not resubmit the same OpenCode message ID")
	turnOrigin, err = db.GetStandingOrderTurnOrigin(agentID, convID, time.Now())
	require.NoError(t, err)
	require.NotNil(t, turnOrigin)
	assert.Equal(t, db.StandingOrderTurnOriginPending, turnOrigin.State)
}
