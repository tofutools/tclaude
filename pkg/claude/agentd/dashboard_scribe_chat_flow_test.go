package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// The process editor's docked scribe chat: the human writes through operator
// mail, the scribe's final message of each turn is captured by the production
// Stop hook path, and GET /api/scribe/{agent}/chat merges both. Replies from
// agents that are not process scribes are never stored or served.
func TestDashboardScribeChat_MergesOperatorMailAndStopReplies(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	stubScribeTerminal(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe",
		map[string]any{
			"name": "process-scribe", "exclusive": true,
			"slugs": []string{agentd.PermProcessTemplatesRead, agentd.PermProcessTemplatesManage},
			"scope": map[string]any{"kind": "process-template", "id": "release-flow"},
			"brief": "Edit the release-flow template.",
		})))
	require.Equalf(t, http.StatusOK, rec.Code, "summon body=%s", rec.Body.String())
	scribe := decodeScribeResp(t, rec)
	scribeAgent, err := db.AgentIDForConv(scribe.ConvID)
	require.NoError(t, err)
	const scribeLabel = "spwn-scribe-chat"
	f.HaveAliveSession(scribe.ConvID, scribeLabel, "tmux-scribe-chat", f.TestCwd("scribe"))

	const other = "other-1111-2222-3333-444444444444"
	const otherLabel = "spwn-other-chat"
	f.HaveGroup("squad")
	f.HaveAliveSession(other, otherLabel, "tmux-other-chat", f.TestCwd("other"))
	f.HaveMember("squad", other)
	otherAgent, err := db.AgentIDForConv(other)
	require.NoError(t, err)

	mux := agentd.BuildDashboardHandlerForTest()
	rec = testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost, "/api/operator-message",
		map[string]any{"to": scribe.ConvID, "subject": "process editor", "body": "Add a review gate after build."}))
	require.Equalf(t, http.StatusAccepted, rec.Code, "operator message body=%s", rec.Body.String())
	agentd.WaitForBackgroundForTest()

	stop := func(conv, label, reply string) {
		t.Helper()
		require.NoError(t, session.ApplyHook(session.HookCallbackInput{
			HookEventName: "Stop", ConvID: conv, Cwd: f.TestCwd("scribe"), LastAssistantMessage: reply,
		}, label))
	}
	stop(scribe.ConvID, scribeLabel, "Added a review gate after build; saved as a new version.")
	stop(other, otherLabel, "private work")

	chat := func(agentID string) (int, []map[string]any) {
		t.Helper()
		rec := testharness.Serve(mux, httptestGet(t, "/api/scribe/"+agentID+"/chat"))
		var body struct {
			Messages []map[string]any `json:"messages"`
		}
		if rec.Code == http.StatusOK {
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		}
		return rec.Code, body.Messages
	}
	code, messages := chat(scribeAgent)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, messages, 2)
	assert.Equal(t, "human", messages[0]["role"])
	assert.Equal(t, "Add a review gate after build.", messages[0]["body"])
	assert.Equal(t, "scribe", messages[1]["role"])
	assert.Equal(t, "Added a review gate after build; saved as a new version.", messages[1]["body"])

	code, _ = chat(otherAgent)
	assert.Equal(t, http.StatusNotFound, code, "a non-scribe's replies are not served")
	stored, err := db.ListScribeTurnReplies(otherAgent, 0)
	require.NoError(t, err)
	assert.Empty(t, stored, "a non-scribe's Stop reply is never stored")
	code, _ = chat("not-an-agent")
	assert.Equal(t, http.StatusBadRequest, code)
}

func httptestGet(t *testing.T, path string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, path, nil)
	require.NoError(t, err)
	return req
}
