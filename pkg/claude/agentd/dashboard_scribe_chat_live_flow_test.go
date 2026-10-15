package agentd_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type scribeChatLiveBody struct {
	Messages []map[string]any `json:"messages"`
	Live     *struct {
		Items []struct {
			Kind string `json:"kind"`
			Text string `json:"text"`
		} `json:"items"`
		Truncated   bool   `json:"truncated"`
		Unavailable string `json:"unavailable"`
	} `json:"live"`
}

// While a scribe's turn is open, the chat read streams what the scribe has
// said and done so far in it, decoded from the scribe's own Claude Code
// transcript; the previous, already-recorded turn is not repeated, and the
// live tail disappears once the Stop hook records the turn's reply.
func TestDashboardScribeChat_StreamsTheOpenTurnFromTheTranscript(t *testing.T) {
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
	const scribeLabel = "spwn-scribe-live"
	cwd := f.TestCwd("scribe")
	f.HaveAliveSession(scribe.ConvID, scribeLabel, "tmux-scribe-live", cwd)

	// The scribe's native transcript, indexed like the reindex scan does.
	jsonl := filepath.Join(os.Getenv("HOME"), ".claude", "projects", "scribe-live", scribe.ConvID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(jsonl), 0o755))
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{ConvID: scribe.ConvID, FullPath: jsonl, Harness: "claude", IndexedAt: time.Now()}))
	parent := ""
	record := func(kind string, at time.Time, content any) {
		t.Helper()
		uuid := kind + "-" + at.Format("150405.000000000")
		line := map[string]any{
			"type": kind, "uuid": uuid, "sessionId": scribe.ConvID, "cwd": cwd,
			"timestamp": at.UTC().Format(time.RFC3339Nano),
			"message":   map[string]any{"role": kind, "content": content},
		}
		if parent != "" {
			line["parentUuid"] = parent
		}
		parent = uuid
		raw, err := json.Marshal(line)
		require.NoError(t, err)
		file, err := os.OpenFile(jsonl, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		require.NoError(t, err)
		_, err = file.Write(append(raw, '\n'))
		require.NoError(t, file.Close())
		require.NoError(t, err)
	}
	text := func(s string) []map[string]any { return []map[string]any{{"type": "text", "text": s}} }

	mux := agentd.BuildDashboardHandlerForTest()
	send := func(body string) {
		t.Helper()
		rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost, "/api/operator-message",
			map[string]any{"to": scribe.ConvID, "subject": "process editor", "body": body}))
		require.Equalf(t, http.StatusAccepted, rec.Code, "operator message body=%s", rec.Body.String())
		agentd.WaitForBackgroundForTest()
	}
	stop := func(reply string) {
		t.Helper()
		require.NoError(t, session.ApplyHook(session.HookCallbackInput{
			HookEventName: "Stop", ConvID: scribe.ConvID, Cwd: cwd, LastAssistantMessage: reply,
		}, scribeLabel))
	}
	chat := func() scribeChatLiveBody {
		t.Helper()
		rec := testharness.Serve(mux, httptestGet(t, "/api/scribe/"+scribeAgent+"/chat"))
		require.Equalf(t, http.StatusOK, rec.Code, "chat body=%s", rec.Body.String())
		var body scribeChatLiveBody
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		return body
	}

	// A first, finished turn.
	start := time.Now().Add(-time.Minute)
	send("Rename build.")
	record("user", start, "[system: new agent message #1 from the human operator] Rename build.")
	record("assistant", start.Add(time.Second), text("Renamed build to compile."))
	stop("Renamed build to compile.")
	assert.Nil(t, chat().Live, "no live tail while the scribe is idle")

	// A second turn, still running.
	send("Add a review gate after build.")
	now := time.Now()
	record("user", now, "[system: new agent message #2 from the human operator] Add a review gate after build.")
	body := chat()
	require.NotNil(t, body.Live)
	assert.Empty(t, body.Live.Items, "the finished turn is not repeated as live progress")
	assert.Empty(t, body.Live.Unavailable)

	record("assistant", now.Add(time.Second), text("Reading the template first."))
	record("assistant", now.Add(2*time.Second), []map[string]any{{
		"type": "tool_use", "id": "toolu_1", "name": "Bash",
		"input": map[string]any{"command": "tclaude process template show release-flow\n  --json"},
	}})
	record("user", now.Add(3*time.Second), []map[string]any{{"type": "tool_result", "tool_use_id": "toolu_1", "content": "{\"secret\":\"tool output\"}"}})
	record("assistant", now.Add(4*time.Second), text("Adding the gate."))
	body = chat()
	require.NotNil(t, body.Live)
	got := make([]string, 0, len(body.Live.Items))
	for _, item := range body.Live.Items {
		got = append(got, item.Kind+"|"+item.Text)
	}
	assert.Equal(t, []string{
		"text|Reading the template first.",
		"tool|Bash: tclaude process template show release-flow --json",
		"text|Adding the gate.",
	}, got)
	assert.NotContains(t, strings.Join(got, "\n"), "tool output", "tool results are not streamed")
	require.Len(t, body.Messages, 3)

	stop("Added a review gate after build.")
	body = chat()
	assert.Nil(t, body.Live, "the recorded reply supersedes the live tail")
	require.Len(t, body.Messages, 4)
	assert.Equal(t, "Added a review gate after build.", body.Messages[3]["body"])

	// A transcript that cannot be read degrades to the plain working state.
	require.NoError(t, os.Remove(jsonl))
	send("And a deploy step.")
	body = chat()
	require.NotNil(t, body.Live)
	assert.Empty(t, body.Live.Items)
	assert.NotEmpty(t, body.Live.Unavailable)
}
