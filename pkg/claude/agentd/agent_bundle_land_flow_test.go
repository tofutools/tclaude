package agentd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/paf/legacy"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

type landingResult struct {
	Applied  bool     `json:"applied"`
	History  bool     `json:"history"`
	Warnings []string `json:"warnings"`
	Agent    struct {
		Harness string `json:"harness"`
	} `json:"agent"`
	Landing struct {
		SourceHarness string `json:"source_harness"`
		Harness       string `json:"harness"`
		Model         string `json:"model"`
		Rung          string `json:"rung"`
		Losses        *struct {
			Target string `json:"target"`
		} `json:"losses"`
	} `json:"history_landing"`
	Spawn struct {
		ConvID string `json:"conv_id"`
	} `json:"spawn"`
}

func importAgentArchive(t *testing.T, f *testharness.Flow, raw []byte, query url.Values) landingResult {
	t.Helper()
	r := agentd.AsHumanPeer(httptest.NewRequest(http.MethodPost, "/v1/agent-bundle/import?"+query.Encode(), bytes.NewReader(raw)))
	rec := httptest.NewRecorder()
	f.Mux.ServeHTTP(rec, r)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var res landingResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &res))
	return res
}

// seedClaudeConversation gives source a short Claude Code conversation.
func seedClaudeConversation(t *testing.T, f *testharness.Flow, source, label, cwd string) {
	t.Helper()
	f.HaveAliveSession(source, label, label+"-pane", cwd)
	cc := f.World.CCs.GetByLabel(label)
	require.NotNil(t, cc)
	turn := func(typ, id, parent string, msg map[string]any) {
		require.NoError(t, cc.AppendTurn(map[string]any{"type": typ, "uuid": id, "parentUuid": parent, "cwd": cwd, "timestamp": "2026-10-01T10:00:00.000Z", "message": msg}))
	}
	turn("user", "u1", "", map[string]any{"role": "user", "content": morphSecret})
	turn("assistant", "a1", "u1", map[string]any{"id": "m1", "role": "assistant", "model": "claude-opus-5-5", "content": []any{map[string]any{"type": "text", "text": "There are two files."}}})
	convops.ScanAndUpsertFile(cc.JsonlPath)
}

// An agent exported from Claude Code lands on Codex: its conversation is
// translated into a Codex rollout and resumes there as a new agent.
func TestAgentImportLandsOnAnotherHarness(t *testing.T) {
	f := newFlow(t)
	t.Setenv("CODEX_HOME", "")
	f.HaveGroup("receiver")
	const source = "0f0e0d0c-0b0a-4908-8706-050403020a01"
	cwd := testutil.CanonicalTempDir(t)
	seedClaudeConversation(t, f, source, "land-src", cwd)
	rec := profileReq(t, f, http.MethodGet, "/v1/agent-bundle/export?agent="+source+"&history=true&format=paf", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	raw := rec.Body.Bytes()

	dest := testutil.CanonicalTempDir(t)
	preview := importAgentArchive(t, f, raw, url.Values{"harness": {"codex"}, "model": {"gpt-5.5"}, "cwd": {dest}})
	assert.False(t, preview.Applied)
	assert.True(t, preview.History)
	assert.Equal(t, "claude", preview.Landing.SourceHarness)
	assert.Equal(t, "codex", preview.Landing.Harness)
	assert.Equal(t, "gpt-5.5", preview.Landing.Model)
	assert.Equal(t, "translated", preview.Landing.Rung)
	require.NotNil(t, preview.Landing.Losses)
	assert.Equal(t, "codex", preview.Landing.Losses.Target)
	assert.Equal(t, "codex", preview.Agent.Harness)
	assert.Contains(t, preview.Warnings[0], "launch posture recorded for claude is not applied on codex")

	applied := importAgentArchive(t, f, raw, url.Values{"harness": {"codex"}, "model": {"gpt-5.5"}, "cwd": {dest}, "apply": {"true"}, "group": {"receiver"}, "name": {"landed"}})
	require.True(t, applied.Applied)
	require.NotEmpty(t, applied.Spawn.ConvID)
	text := historyText(t, mustGetHarness(t, harness.CodexName), applied.Spawn.ConvID, dest)
	assert.Contains(t, text, morphSecret)
	assert.Contains(t, text, "There are two files.")

	// The same harness still installs the native history unchanged.
	same := importAgentArchive(t, f, raw, url.Values{"cwd": {dest}})
	assert.Equal(t, "native", same.Landing.Rung)
	assert.Nil(t, same.Landing.Losses)
}

// A PAF that carries only the neutral context (exported with no_native, or
// written by another tool) lands by translation instead of importing
// configuration only.
func TestAgentImportLandsContextOnlyPAF(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("receiver")
	const source = "0f0e0d0c-0b0a-4908-8706-050403020a02"
	cwd := testutil.CanonicalTempDir(t)
	seedClaudeConversation(t, f, source, "ctx-src", cwd)
	rec := profileReq(t, f, http.MethodGet, "/v1/agent-bundle/export?agent="+source+"&history=true&format=paf&no_native=true", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var buf bytes.Buffer
	buf.Write(rec.Body.Bytes())
	a, err := paf.Open(bytes.NewReader(buf.Bytes()), int64(buf.Len()), paf.Options{Purpose: paf.PurposeImport})
	require.NoError(t, err)
	ag, _ := a.Agent(a.Manifest.Agents[0])
	require.Nil(t, ag.Native, "no_native leaves the native history out")
	claude := mustGetHarness(t, harness.DefaultName)

	rec = profileReq(t, f, http.MethodGet, "/v1/agent-bundle/export?agent="+source+"&history=true&no_native=true", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, "no_native needs format=paf")
	rec = profileReq(t, f, http.MethodGet, "/v1/agent-bundle/export?agent="+source+"&history=true&format=paf&no_native=true&no_context=true", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	dest := testutil.CanonicalTempDir(t)
	// With no model at all the budget would be a guess, so the conversation
	// does not land and the preview says how to fix it.
	unbudgeted := importAgentArchive(t, f, buf.Bytes(), url.Values{"cwd": {dest}})
	assert.Equal(t, "none", unbudgeted.Landing.Rung)
	assert.False(t, unbudgeted.History)
	assert.Contains(t, strings.Join(unbudgeted.Warnings, "\n"), "name one with --model")

	const model = "claude-opus-5-5[1m]"
	preview := importAgentArchive(t, f, buf.Bytes(), url.Values{"cwd": {dest}, "model": {model}})
	assert.Equal(t, "translated", preview.Landing.Rung)
	assert.NotContains(t, preview.Warnings, legacy.WarningContextOnly)

	applied := importAgentArchive(t, f, buf.Bytes(), url.Values{"cwd": {dest}, "model": {model}, "apply": {"true"}, "group": {"receiver"}, "name": {"from-context"}})
	require.True(t, applied.Applied)
	assert.Contains(t, historyText(t, claude, applied.Spawn.ConvID, dest), morphSecret)
}

// The import preview rescans a PAF's whole content, so a credential that
// only the neutral context carries is still found.
func TestAgentImportRescansNeutralContext(t *testing.T) {
	f := newFlow(t)
	tr := transcript.New(transcript.Source{Harness: "claude"})
	tr.Entries = []transcript.Entry{{Role: transcript.RoleUser, Origin: transcript.OriginHuman, Blocks: []transcript.Block{transcript.Text("token ghp_abcdefghijklmnopqrstuvwxyz0123")}}}
	var buf bytes.Buffer
	w := paf.NewWriter(&buf, paf.KindAgent, paf.Limits{})
	require.NoError(t, w.AddAgent("ctx", paf.Agent{Name: "ctx"}, paf.Environment{Harness: paf.Harness{Name: "claude"}}))
	require.NoError(t, paf.WriteTranscript(w, "ctx", paf.ScopeEffective, tr))
	require.NoError(t, w.WriteJSON(paf.AgentPath("ctx", "context/losses.json"), paf.LossesFromReport(tr.Losses)))
	_, err := w.Close(paf.Producer{Name: "other-tool"})
	require.NoError(t, err)

	r := agentd.AsHumanPeer(httptest.NewRequest(http.MethodPost, "/v1/agent-bundle/import?cwd="+url.QueryEscape(testutil.CanonicalTempDir(t)), bytes.NewReader(buf.Bytes())))
	rec := httptest.NewRecorder()
	f.Mux.ServeHTTP(rec, r)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var preview struct {
		Findings []struct {
			Kind      string   `json:"kind"`
			Locations []string `json:"locations"`
		} `json:"findings"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &preview))
	require.Len(t, preview.Findings, 1)
	assert.Equal(t, "github-token", preview.Findings[0].Kind)
	assert.Contains(t, preview.Findings[0].Locations[0], "context/transcript.jsonl")
}
