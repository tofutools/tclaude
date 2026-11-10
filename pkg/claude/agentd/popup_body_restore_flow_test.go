package agentd_test

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

const overRestoreBound = 3 << 20 // past maxApprovalRestoreBody

func askHumanAs(f *testharness.Flow, conv, path, contentType string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Tclaude-Ask-Human", "30s")
	return testharness.Serve(f.Mux, agentd.AsAgentPeer(req, conv))
}

// An approved request reaches its handler whole, however large: approving must
// not cost it the part past what a popup can show.
func TestAskHumanAgentImportPastTheRestoreBoundIsWhole(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("receiver")
	f.HaveMember("receiver", "caller")
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	popups, restore := agentd.StubCapturingApprovalForTest(true)
	t.Cleanup(restore)

	noise := make([]byte, overRestoreBound)
	_, err := rand.Read(noise)
	require.NoError(t, err)
	b := &agentbundle.Bundle{Manifest: agentbundle.Manifest{Format: agentbundle.Format, FormatVersion: 1, Agent: agentbundle.Definition{
		Name: "large", Harness: "claude", Profile: json.RawMessage(`{}`),
		Paths: agentbundle.Paths{Cwd: testutil.CanonicalTempDir(t)},
	}}}
	b.SetHistory("jsonl", "source-conversation", []byte(`{"text":"`+hex.EncodeToString(noise)+`"}`+"\n"))
	raw, err := b.Encode()
	require.NoError(t, err)
	require.Greater(t, len(raw), overRestoreBound)

	rec := askHumanAs(f, "caller", "/v1/agent-bundle/import?keep_paths=true", "application/zip", raw)
	require.Len(t, *popups, 1)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"large"`)
}

func TestAskHumanTemplateSavePastTheRestoreBoundIsWhole(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	const conv = "popup-restore-aaaa-bbbb-000000000001"
	f.HaveAliveSession(conv, "popup-restore", "tmux-popup-restore", f.TestCwd("popup"))
	_, _, err := db.EnsureAgentForConv(conv, "test")
	require.NoError(t, err)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	popups, restore := agentd.StubCapturingApprovalForTest(true)
	t.Cleanup(restore)

	body, err := json.Marshal(map[string]any{"template": map[string]any{"name": "large", "description": strings.Repeat("x", overRestoreBound)}})
	require.NoError(t, err)
	rec := askHumanAs(f, conv, "/v1/process/templates", "application/json", body)
	require.Len(t, *popups, 1)
	assert.NotEqual(t, "json", errorCode(rec), "the handler got the whole body: %s", rec.Body.String())
}

func errorCode(rec *httptest.ResponseRecorder) string {
	var body struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Code
}

// A body past what a handler accepts is refused by the handler, as it would be
// with no popup, not cut to fit.
func TestAskHumanBodyPastTheHandlersLimitIsRefusedByIt(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	const conv = "popup-restore-aaaa-bbbb-000000000002"
	f.HaveAliveSession(conv, "popup-limit", "tmux-popup-limit", f.TestCwd("popup"))
	_, _, err := db.EnsureAgentForConv(conv, "test")
	require.NoError(t, err)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_, restore := agentd.StubCapturingApprovalForTest(true)
	t.Cleanup(restore)

	body, err := json.Marshal(map[string]any{"template": map[string]any{"name": "large", "description": strings.Repeat("x", 8<<20)}})
	require.NoError(t, err)
	rec := askHumanAs(f, conv, "/v1/process/templates", "application/json", body)
	assert.NotEqual(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "unexpected EOF", "the limit is reported, not a cut body")
}
