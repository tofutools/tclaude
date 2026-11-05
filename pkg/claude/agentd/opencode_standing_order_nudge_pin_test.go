package agentd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// These pin sendOpenCodeStandingOrderNudge by the standing-order turn origin it
// leaves and the prompts it submits, against an in-process stand-in for the
// OpenCode server (owned by the test process, so the ownership proof holds):
// the provenance refusals, a fresh delivery (accepted, rejected, and accepted
// despite a lost response), and the reconciliation of a prompt OpenCode
// already holds (completed, active, still queued, and a rival turn).

const pinOpenCodeMessageID = "msg_pin_standing_order"

// standingOrderOpenCodeStub answers the endpoints the nudge uses.
type standingOrderOpenCodeStub struct {
	sync.Mutex
	messageExists bool   // GET /session/{conv}/message/{id}
	history       string // GET /session/{conv}/message
	promptStatus  int    // POST prompt_async; 0 drops the connection
	prompts       []map[string]any
}

func (s *standingOrderOpenCodeStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.Lock()
	defer s.Unlock()
	switch {
	case r.URL.Path == "/global/health":
		_, _ = w.Write([]byte(`{"healthy":true}`))
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/prompt_async"):
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.prompts = append(s.prompts, body)
		if s.promptStatus == 0 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			s.messageExists = true // accepted before the transport failed
			return
		}
		if s.promptStatus == http.StatusNoContent {
			s.messageExists = true
		}
		w.WriteHeader(s.promptStatus)
	case strings.HasSuffix(r.URL.Path, "/message/"+pinOpenCodeMessageID):
		if !s.messageExists {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	case strings.HasSuffix(r.URL.Path, "/message"):
		_, _ = w.Write([]byte(s.history))
	default:
		http.NotFound(w, r)
	}
}

type standingOrderNudgePin struct {
	stub    *standingOrderOpenCodeStub
	convID  string
	agentID string
	origin  *db.StandingOrderAgentMessageOrigin
}

func newStandingOrderNudgePin(t *testing.T, stub *standingOrderOpenCodeStub) standingOrderNudgePin {
	t.Helper()
	setupTestDB(t)
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)
	const convID = "ses_pin_standing_order"
	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	require.NoError(t, db.UpsertOpenCodeRuntime(db.OpenCodeRuntime{
		SessionID: "spwn-pin-standing-order", ConvID: convID, ServerURL: server.URL,
		Password: "pin", PID: os.Getpid(), Cwd: t.TempDir(),
		Transport: db.OpenCodeTransportLoopbackTCP,
	}))
	messageID, err := db.InsertAgentMessage(&db.AgentMessage{
		ToConv: convID, Subject: "[standing-order:pin]", Body: "pinned reminder",
	})
	require.NoError(t, err)
	return standingOrderNudgePin{stub: stub, convID: convID, agentID: agentID,
		origin: &db.StandingOrderAgentMessageOrigin{MessageID: messageID, OpenCodeMessageID: pinOpenCodeMessageID}}
}

func (p standingOrderNudgePin) send() error {
	return sendOpenCodeStandingOrderNudge(p.convID, "", p.origin, "pinned nudge")
}

func (p standingOrderNudgePin) arm(t *testing.T, state string) {
	t.Helper()
	require.NoError(t, db.ArmStandingOrderTurnOrigin(p.agentID, p.convID, p.origin.MessageID,
		p.origin.OpenCodeMessageID, time.Now(), time.Hour))
	if state == db.StandingOrderTurnOriginActive {
		activated, err := db.ActivateStandingOrderTurnOrigin(p.agentID, p.convID,
			p.origin.OpenCodeMessageID, time.Now(), time.Hour)
		require.NoError(t, err)
		require.True(t, activated)
	}
}

func (p standingOrderNudgePin) originState(t *testing.T) string {
	t.Helper()
	origin, err := db.GetStandingOrderTurnOrigin(p.agentID, p.convID, time.Now())
	require.NoError(t, err)
	if origin == nil {
		return ""
	}
	assert.Equal(t, p.origin.MessageID, origin.MessageID)
	assert.Equal(t, p.origin.OpenCodeMessageID, origin.OpenCodeMessageID)
	return origin.State
}

func (p standingOrderNudgePin) promptCount() int {
	p.stub.Lock()
	defer p.stub.Unlock()
	return len(p.stub.prompts)
}

func openCodeHistoryJSON(assistantCompleted *bool) string {
	user := `{"info":{"id":"` + pinOpenCodeMessageID + `","role":"user"}}`
	if assistantCompleted == nil {
		return "[" + user + "]"
	}
	completed := ""
	if *assistantCompleted {
		completed = `,"time":{"completed":1}`
	}
	return "[" + user + `,{"info":{"id":"msg_reply","role":"assistant","parentID":"` +
		pinOpenCodeMessageID + `"` + completed + "}}]"
}

func TestSendOpenCodeStandingOrderNudge_RefusesWithoutProvenance(t *testing.T) {
	p := newStandingOrderNudgePin(t, &standingOrderOpenCodeStub{promptStatus: http.StatusNoContent})

	err := sendOpenCodeStandingOrderNudge(p.convID, "", nil, "pinned nudge")
	require.ErrorContains(t, err, "standing-order OpenCode delivery requires trusted provenance")

	err = sendOpenCodeStandingOrderNudge("ses_not_an_agent", "", p.origin, "pinned nudge")
	require.ErrorContains(t, err, "standing-order OpenCode delivery requires a stable target agent")

	assert.Zero(t, p.promptCount(), "nothing is submitted")
	assert.Empty(t, p.originState(t))
}

func TestSendOpenCodeStandingOrderNudge_FreshDelivery(t *testing.T) {
	t.Run("accepted", func(t *testing.T) {
		p := newStandingOrderNudgePin(t, &standingOrderOpenCodeStub{promptStatus: http.StatusNoContent})

		require.NoError(t, p.send())

		require.Equal(t, 1, p.promptCount())
		p.stub.Lock()
		messageID := p.stub.prompts[0]["messageID"]
		p.stub.Unlock()
		assert.Equal(t, pinOpenCodeMessageID, messageID,
			"the prompt carries the origin's stable message ID")
		assert.Equal(t, db.StandingOrderTurnOriginPending, p.originState(t))
	})
	t.Run("rejected", func(t *testing.T) {
		p := newStandingOrderNudgePin(t, &standingOrderOpenCodeStub{promptStatus: http.StatusBadRequest})

		require.ErrorContains(t, p.send(), "submit OpenCode launch prompt: HTTP 400")

		assert.Equal(t, 1, p.promptCount())
		assert.Empty(t, p.originState(t), "a rejected prompt cancels the pending origin")
	})
	t.Run("accepted despite a lost response", func(t *testing.T) {
		p := newStandingOrderNudgePin(t, &standingOrderOpenCodeStub{promptStatus: 0})

		require.NoError(t, p.send(), "the exact lookup confirms the delivery")

		assert.Equal(t, 1, p.promptCount())
		assert.Equal(t, db.StandingOrderTurnOriginPending, p.originState(t))
	})
}

func TestSendOpenCodeStandingOrderNudge_ReconcilesAnAlreadyAcceptedPrompt(t *testing.T) {
	completed, active := true, false
	for _, tc := range []struct {
		name    string
		history string
		armed   string // the origin before the retry; "" for none
		want    string // the origin after it
		err     string
	}{
		{"completed with no origin", openCodeHistoryJSON(&completed), "", "", ""},
		{"completed while active", openCodeHistoryJSON(&completed), db.StandingOrderTurnOriginActive, "", ""},
		{"completed while pending", openCodeHistoryJSON(&completed), db.StandingOrderTurnOriginPending, "", ""},
		{"active with no origin", openCodeHistoryJSON(&active), "", db.StandingOrderTurnOriginActive, ""},
		{"active while pending", openCodeHistoryJSON(&active), db.StandingOrderTurnOriginPending, db.StandingOrderTurnOriginActive, ""},
		{"still queued with no origin", openCodeHistoryJSON(nil), "", db.StandingOrderTurnOriginPending, ""},
		{"still queued while pending", openCodeHistoryJSON(nil), db.StandingOrderTurnOriginPending, db.StandingOrderTurnOriginPending, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newStandingOrderNudgePin(t, &standingOrderOpenCodeStub{
				messageExists: true, history: tc.history, promptStatus: http.StatusNoContent,
			})
			if tc.armed != "" {
				p.arm(t, tc.armed)
			}

			require.NoError(t, p.send())

			assert.Zero(t, p.promptCount(), "an accepted prompt is never resubmitted")
			assert.Equal(t, tc.want, p.originState(t))
		})
	}
}

func TestSendOpenCodeStandingOrderNudge_RefusesBehindARivalTurn(t *testing.T) {
	p := newStandingOrderNudgePin(t, &standingOrderOpenCodeStub{
		messageExists: true, history: openCodeHistoryJSON(nil), promptStatus: http.StatusNoContent,
	})
	rival, err := db.InsertAgentMessage(&db.AgentMessage{
		ToConv: p.convID, Subject: "[standing-order:rival]", Body: "rival reminder",
	})
	require.NoError(t, err)
	require.NoError(t, db.ArmStandingOrderTurnOrigin(p.agentID, p.convID, rival,
		"msg_rival", time.Now(), time.Hour))

	require.ErrorContains(t, p.send(), "another standing-order turn is pending or active")

	assert.Zero(t, p.promptCount())
	origin, err := db.GetStandingOrderTurnOrigin(p.agentID, p.convID, time.Now())
	require.NoError(t, err)
	require.NotNil(t, origin)
	assert.Equal(t, rival, origin.MessageID, "the rival turn is left alone")
}
