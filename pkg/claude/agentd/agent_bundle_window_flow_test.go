package agentd_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// seedLongClaudeConversation gives source a Claude Code conversation of
// n exchanges whose user turns carry about size bytes each.
func seedLongClaudeConversation(t *testing.T, f *testharness.Flow, source, label, cwd string, n, size int) {
	t.Helper()
	f.HaveAliveSession(source, label, label+"-pane", cwd)
	cc := f.World.CCs.GetByLabel(label)
	require.NotNil(t, cc)
	parent := ""
	for i := range n {
		u, a := fmt.Sprintf("u%d", i), fmt.Sprintf("a%d", i)
		text := fmt.Sprintf("turn %d: ", i) + strings.Repeat("lorem ipsum dolor sit amet ", size/27)
		if i == 0 {
			text = morphSecret + " " + text
		}
		require.NoError(t, cc.AppendTurn(map[string]any{"type": "user", "uuid": u, "parentUuid": parent, "cwd": cwd, "timestamp": "2026-10-01T10:00:00.000Z", "message": map[string]any{"role": "user", "content": text}}))
		require.NoError(t, cc.AppendTurn(map[string]any{"type": "assistant", "uuid": a, "parentUuid": u, "cwd": cwd, "timestamp": "2026-10-01T10:00:01.000Z", "message": map[string]any{"id": "m" + a, "role": "assistant", "model": "claude-opus-5-5", "content": []any{map[string]any{"type": "text", "text": fmt.Sprintf("done with turn %d", i)}}}}))
		parent = a
	}
	convops.ScanAndUpsertFile(cc.JsonlPath)
}

func importRaw(f *testharness.Flow, raw []byte, query url.Values) *httptest.ResponseRecorder {
	r := agentd.AsHumanPeer(httptest.NewRequest(http.MethodPost, "/v1/agent-bundle/import?"+query.Encode(), bytes.NewReader(raw)))
	rec := httptest.NewRecorder()
	f.Mux.ServeHTTP(rec, r)
	return rec
}

// An agent exported with its own summary lands on a model with a much
// smaller window: the cut needs confirmation, the summary stands in for
// what is cut, and the whole conversation is saved beside the agent, which
// is told where.
func TestAgentImportOntoSmallerWindowCarriesSummary(t *testing.T) {
	f := newFlow(t)
	codexHome := testutil.CanonicalTempDir(t)
	t.Setenv("CODEX_HOME", codexHome)
	require.NoError(t, os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte("model = \"gpt-5.5\"\nmodel_context_window = 6000\n"), 0o600))
	f.HaveGroup("receiver")
	const source = "0f0e0d0c-0b0a-4908-8706-050403020c01"
	cwd := testutil.CanonicalTempDir(t)
	seedLongClaudeConversation(t, f, source, "window-src", cwd, 30, 1000)
	f.HaveEnrolledAgent(source)

	// The operator asks for a summary; the agent writes it with the
	// command a morph uses.
	rec := profileReq(t, f, http.MethodPost, "/v1/agent-bundle/summary", map[string]any{"agent": source})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var asked struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &asked))
	inbox, err := db.ListAgentMessagesForConv(source, 10)
	require.NoError(t, err)
	require.NotEmpty(t, inbox)
	assert.Contains(t, inbox[0].Body, "tclaude agent morph-handoff")
	const summary = "SUMMARY: we refactored the billing module; next step is the migration."
	sub := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/morph/handoff", map[string]any{"body": summary}), source)
	rec = testharness.Serve(f.Mux, sub)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = profileReq(t, f, http.MethodGet, "/v1/agent-bundle/summary/"+asked.ID, nil)
	require.Contains(t, rec.Body.String(), `"ready"`)

	rec = profileReq(t, f, http.MethodGet, "/v1/agent-bundle/export?agent="+source+"&history=true&format=paf&summary="+asked.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	raw := rec.Body.Bytes()

	dest := testutil.CanonicalTempDir(t)
	query := url.Values{"harness": {"codex"}, "model": {"gpt-5.5"}, "cwd": {dest}}
	preview := importAgentArchive(t, f, raw, query)
	assert.Equal(t, "translated", preview.Landing.Rung)
	joined := strings.Join(preview.Warnings, "\n")
	assert.Contains(t, joined, "cuts about")
	assert.Contains(t, joined, "the agent's own summary")
	assert.Contains(t, joined, "--confirm-history-loss")

	query.Set("apply", "true")
	query.Set("group", "receiver")
	rec = importRaw(f, raw, query)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "history_confirmation_required")
	assert.Contains(t, rec.Body.String(), "--model")

	query.Set("confirm_history_loss", "true")
	applied := importAgentArchive(t, f, raw, query)
	require.True(t, applied.Applied)
	text := historyText(t, mustGetHarness(t, harness.CodexName), applied.Spawn.ConvID, dest)
	assert.Contains(t, text, summary, "the summary stands in for the cut history")
	assert.Contains(t, text, morphSecret, "the pinned start is kept")

	msgs, err := db.ListAgentMessagesForConv(applied.Spawn.ConvID, 20)
	require.NoError(t, err)
	var pointer string
	for _, m := range msgs {
		if m.Subject == "Your full imported history" {
			pointer = m.Body
		}
	}
	require.NotEmpty(t, pointer, "the agent is told where its full history is")
	path := regexp.MustCompile(`(/\S+/transcript\.md)`).FindString(pointer)
	require.NotEmpty(t, path, pointer)
	full, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(full), "turn 15:", "the saved transcript has the turns the landing cut")
}

// A native history that does not fit the landing model's budget is not
// installed as it is: it lands translated and trimmed.
func TestAgentImportBudgetsNativeHistory(t *testing.T) {
	f := newFlow(t)
	const source = "0f0e0d0c-0b0a-4908-8706-050403020c02"
	cwd := testutil.CanonicalTempDir(t)
	seedLongClaudeConversation(t, f, source, "native-src", cwd, 60, 6000)
	rec := profileReq(t, f, http.MethodGet, "/v1/agent-bundle/export?agent="+source+"&history=true&format=paf", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	claude := mustGetHarness(t, harness.DefaultName)
	var model string
	for _, m := range claude.Models.Models() {
		if m != "" && !strings.HasSuffix(m, "[1m]") {
			model = m
			break
		}
	}
	require.NotEmpty(t, model)

	preview := importAgentArchive(t, f, rec.Body.Bytes(), url.Values{"cwd": {testutil.CanonicalTempDir(t)}, "model": {model}})
	assert.Equal(t, "translated", preview.Landing.Rung)
	assert.Contains(t, strings.Join(preview.Warnings, "\n"), "exceeds")
}
