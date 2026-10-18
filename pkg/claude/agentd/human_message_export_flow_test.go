package agentd_test

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario: the operator downloads a notification from the Messages reader to
// share it. With attachments it arrives as a zip of message.md plus every
// file; without, or for the "message only" variant, as plain Markdown. A file
// reclaimed from disk is reported in message.md rather than silently dropped,
// and two files sharing a name both survive.
func TestHumanMessageExport_MarkdownAndZip(t *testing.T) {
	f := newFlow(t)
	const conv = "expo-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "art-maker")
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermHumanNotify, "test"))

	rec := postNotifyMultipartAttachments(t, f.Mux, conv, []notifyFile{
		{"world.png", "image/png", onePixelPNG},
		{"notes.md", "text/markdown; charset=utf-8", "# Notes\n"},
		{"gone.txt", "text/plain", "reclaimed"},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	msgs, err := db.ListHumanMessagesPage(db.HumanMessageFilter{}, 0, db.MaxHumanMessagesPage)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Len(t, msgs[0].Attachments, 3)
	require.NoError(t, os.Remove(msgs[0].Attachments[2].StoragePath))
	exportURL := "/api/human-messages/" + strconv.FormatInt(msgs[0].ID, 10) + "/export"
	dash := dashHandlerForTest(t)

	md := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet, exportURL, nil))
	require.Equal(t, http.StatusOK, md.Code, md.Body.String())
	assert.Contains(t, md.Header().Get("Content-Type"), "text/markdown")
	assert.Contains(t, md.Header().Get("Content-Disposition"), "-art.md")
	body := md.Body.String()
	for _, want := range []string{"# art\n", "**From:** art-maker", "concept art ready", "## Attachments", "- world.png (", "- gone.txt ("} {
		assert.Contains(t, body, want)
	}

	zipped := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet, exportURL+"?attachments=1", nil))
	require.Equal(t, http.StatusOK, zipped.Code, zipped.Body.String())
	assert.Equal(t, "application/zip", zipped.Header().Get("Content-Type"))
	assert.Contains(t, zipped.Header().Get("Content-Disposition"), "-art.zip")
	archive, err := zip.NewReader(bytes.NewReader(zipped.Body.Bytes()), int64(zipped.Body.Len()))
	require.NoError(t, err)
	entries := map[string]string{}
	for _, file := range archive.File {
		r, err := file.Open()
		require.NoError(t, err)
		data, err := io.ReadAll(r)
		require.NoError(t, err)
		entries[file.Name] = string(data)
	}
	assert.Equal(t, onePixelPNG, entries["attachments/world.png"])
	assert.Equal(t, "# Notes\n", entries["attachments/notes.md"])
	assert.NotContains(t, entries, "attachments/gone.txt")
	assert.Contains(t, entries["message.md"], "concept art ready")
	assert.Contains(t, entries["message.md"], "[world.png](<attachments/world.png>)")
	assert.Contains(t, entries["message.md"], "gone.txt (9 B) — no longer available")

	missing := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet, "/api/human-messages/999999/export", nil))
	assert.Equal(t, http.StatusNotFound, missing.Code)
}

// Scenario: a message with no files asked for "with attachments" is still a
// plain Markdown download — a zip holding a single file is just friction.
func TestHumanMessageExport_NoAttachmentsStaysMarkdown(t *testing.T) {
	newFlow(t)
	id, err := db.InsertHumanMessage(&db.HumanMessage{FromConv: "c-1", FromTitle: "Alpha", Subject: "Status", Body: "all green"})
	require.NoError(t, err)
	rec := testharness.Serve(dashHandlerForTest(t), testharness.JSONRequest(t, http.MethodGet,
		"/api/human-messages/"+strconv.FormatInt(id, 10)+"/export?attachments=1", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/markdown")
	assert.Contains(t, rec.Header().Get("Content-Disposition"), "msg-"+strconv.FormatInt(id, 10)+"-status.md")
	assert.Contains(t, rec.Body.String(), "all green")
	assert.NotContains(t, rec.Body.String(), "## Attachments")
}
