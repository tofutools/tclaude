package agentd_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// haveProfileCapture writes a capture directory the way a finished capture
// leaves it: a few artifacts plus meta.json.
func haveProfileCapture(t *testing.T, store, id, source, status string) {
	t.Helper()
	dir := filepath.Join(store, id)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cpu.pprof"), []byte("cpu-profile-bytes"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "summary.txt"), []byte("agentd profile summary\nagentd CPU: 42%\n"), 0o600))
	meta, err := json.Marshal(map[string]any{
		"id": id, "label": "idle fleet", "source": source, "status": status,
		"seconds": 30, "trace_seconds": 0, "started_at": time.Now().UTC(),
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "meta.json"), meta, 0o600))
}

func TestDashboardProfileArchiveAndShare(t *testing.T) {
	f := newFlow(t)
	const target = "profile-share-aaaa-1111"
	f.HaveGroup("team")
	f.HaveMember("team", target)
	f.HaveConvWithTitle(target, "perf-reader")

	store := t.TempDir()
	t.Cleanup(agentd.SetProfileStoreDirForTest(store))
	durable := t.TempDir()
	t.Cleanup(agentd.SetOperatorMessageAttachmentBasesForTest(t.TempDir(), durable))
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	const id = "20261017-120000-abcdef"
	haveProfileCapture(t, store, id, "dashboard", "done")
	mux := agentd.BuildDashboardHandlerForTest()

	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/profiles/"+id+"/archive", nil))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Header().Get("Content-Disposition"), "agentd-profile-"+id+".zip")
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	require.NoError(t, err)
	var names []string
	for _, entry := range zr.File {
		names = append(names, entry.Name)
	}
	prefix := "agentd-profile-" + id + "/"
	assert.ElementsMatch(t, []string{prefix + "cpu.pprof", prefix + "summary.txt", prefix + "meta.json"}, names,
		"the zip unpacks every artifact into one directory")

	rec = testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost,
		"/api/profiles/"+id+"/share", map[string]any{"to": "perf-reader"}))
	require.Equal(t, http.StatusAccepted, rec.Code, "body=%s", rec.Body.String())
	var shared struct {
		ID    int64  `json:"id"`
		To    string `json:"to"`
		Files int    `json:"files"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &shared))
	agentd.WaitForBackgroundForTest()
	assert.Equal(t, target, shared.To)
	assert.Equal(t, 3, shared.Files)
	m, err := db.GetAgentMessage(shared.ID)
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Empty(t, m.FromConv, "a share is operator mail, not impersonating an agent")
	assert.Equal(t, "agentd profile "+id+": idle fleet", m.Subject)
	assert.Contains(t, m.Body, "agentd CPU: 42%", "the summary is inlined so the numbers show without opening files")
	attachments, err := db.ListAgentMessageAttachments(shared.ID)
	require.NoError(t, err)
	require.Len(t, attachments, 3)
	for _, a := range attachments {
		assert.Equal(t, durable, filepath.Dir(filepath.Dir(a.StoragePath)), "attachments are copies in agent-readable message storage")
		_, err := os.Stat(a.StoragePath)
		assert.NoError(t, err, a.Filename)
	}

	rec = testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost,
		"/api/profiles/"+id+"/share", map[string]any{"to": "no-such-agent"}))
	assert.Equal(t, http.StatusNotFound, rec.Code, "body=%s", rec.Body.String())

	const runningID = "20261017-120500-123456"
	haveProfileCapture(t, store, runningID, "cli", "running")
	rec = testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/profiles/"+runningID+"/archive", nil))
	assert.Equal(t, http.StatusConflict, rec.Code, "a CLI capture still being written is not packed")
}
