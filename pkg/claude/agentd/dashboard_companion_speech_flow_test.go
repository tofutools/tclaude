package agentd_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// The companion's voice output reads what an agent said after a cursor:
// its text only, never its tool calls, from the agent's own transcript.
func TestDashboardCompanionSpeech_ReturnsTheAgentsTextAfterTheCursor(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	enableCollab(t)
	stubScribeTerminal(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe",
		map[string]any{"name": "speaker", "slugs": []string{agentd.PermProcessTemplatesRead}, "brief": "Help."})))
	require.Equalf(t, http.StatusOK, rec.Code, "summon body=%s", rec.Body.String())
	conv := decodeScribeResp(t, rec).ConvID
	agentID, err := db.AgentIDForConv(conv)
	require.NoError(t, err)
	cwd := f.TestCwd("speech")
	f.HaveAliveSession(conv, "spwn-speech", "tmux-speech", cwd)

	jsonl := filepath.Join(os.Getenv("HOME"), ".claude", "projects", "speech", conv+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(jsonl), 0o755))
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{ConvID: conv, FullPath: jsonl, Harness: "claude", IndexedAt: time.Now()}))
	parent := ""
	record := func(kind string, at time.Time, content any) {
		t.Helper()
		uuid := kind + "-" + at.Format("150405.000000000")
		line := map[string]any{
			"type": kind, "uuid": uuid, "sessionId": conv, "cwd": cwd,
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
	start := time.Now().Add(-time.Minute)
	record("user", start, "What groups do I have?")
	record("assistant", start.Add(time.Second), []map[string]any{{"type": "text", "text": "Old answer."}})
	cursor := start.Add(2 * time.Second)
	record("user", start.Add(3*time.Second), "And profiles?")
	record("assistant", start.Add(4*time.Second), []map[string]any{
		{"type": "text", "text": "Let me look."},
		{"type": "tool_use", "id": "t1", "name": "Bash", "input": map[string]any{"command": "tclaude agent profiles ls"}},
	})
	record("assistant", start.Add(5*time.Second), []map[string]any{{"type": "text", "text": "You have three profiles."}})

	mux := agentd.BuildDashboardHandlerForTest()
	var body struct {
		Items []struct {
			Text string    `json:"text"`
			At   time.Time `json:"at"`
		} `json:"items"`
		Now int64 `json:"now"`
	}
	rec = testharness.Serve(mux, httptestGet(t, "/api/companion/"+agentID+"/speech?since="+strconv.FormatInt(cursor.UnixNano(), 10)))
	require.Equalf(t, http.StatusOK, rec.Code, "speech body=%s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	var texts []string
	for _, it := range body.Items {
		texts = append(texts, it.Text)
	}
	assert.Equal(t, []string{"Let me look.", "You have three profiles."}, texts, "text after the cursor, no tool calls")
	assert.Greater(t, body.Now, cursor.UnixNano())

	rec = testharness.Serve(mux, httptestGet(t, "/api/companion/"+agentID+"/speech?since=yesterday"))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	rec = testharness.Serve(mux, httptestGet(t, "/api/companion/not-an-agent/speech"))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}
