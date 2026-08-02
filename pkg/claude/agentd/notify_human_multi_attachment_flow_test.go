package agentd_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// A 1x1 PNG — the smallest honest raster payload, so the daemon's content
// sniffing has something real to confirm.
var onePixelPNG = string(mustDecodeBase64(
	"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg"))

func mustDecodeBase64(s string) []byte {
	data, err := base64.RawStdEncoding.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return data
}

type notifyFile struct{ name, contentType, data string }

func postNotifyMultipartAttachments(t *testing.T, mux http.Handler, conv string, files []notifyFile) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	for _, file := range files {
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", `form-data; name="files"; filename="`+file.name+`"`)
		header.Set("Content-Type", file.contentType)
		part, err := writer.CreatePart(header)
		require.NoError(t, err)
		_, err = part.Write([]byte(file.data))
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	metadata, err := json.Marshal(map[string]string{"body": "concept art ready", "subject": "art"})
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, "/v1/notify-human/attachment", &buf)
	require.NoError(t, err)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-Tclaude-Notify-Metadata", base64.RawURLEncoding.EncodeToString(metadata))
	return testharness.Serve(mux, agentd.AsAgentPeer(req, conv))
}

// Scenario: an agent publishes three files with one notification. Each keeps
// its own name and type, each has its own authenticated download, and the
// legacy single-artifact URL still resolves to the first file.
func TestNotifyHuman_SeparateAttachmentsEachDownloadable(t *testing.T) {
	f := newFlow(t)
	const conv = "mult-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "art-maker")
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermHumanNotify, "test"))

	rec := postNotifyMultipartAttachments(t, f.Mux, conv, []notifyFile{
		{"world.png", "image/png", onePixelPNG},
		{"guild.png", "image/png", onePixelPNG},
		{"notes.md", "text/markdown; charset=utf-8", "# Notes\n"},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var delivered struct {
		ID          int64    `json:"id"`
		Attachment  string   `json:"attachment"`
		Attachments []string `json:"attachments"`
	}
	testharness.DecodeJSON(t, rec, &delivered)
	assert.Equal(t, []string{"world.png", "guild.png", "notes.md"}, delivered.Attachments)
	assert.Equal(t, "world.png", delivered.Attachment, "the legacy field names the first file")

	msgs, err := db.ListHumanMessages()
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Len(t, msgs[0].Attachments, 3, "one message carries every published file")

	dash := dashHandlerForTest(t)
	var payload struct {
		Messages []struct {
			ID          int64 `json:"id"`
			Attachments []struct {
				ID          int64  `json:"id"`
				Filename    string `json:"filename"`
				ContentType string `json:"content_type"`
				URL         string `json:"url"`
				Previewable bool   `json:"previewable"`
			} `json:"attachments"`
			Attachment *struct {
				Filename string `json:"filename"`
			} `json:"attachment"`
		} `json:"messages"`
	}
	snap := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil))
	require.Equal(t, http.StatusOK, snap.Code, snap.Body.String())
	testharness.DecodeJSON(t, snap, &payload)
	require.Len(t, payload.Messages, 1)
	message := payload.Messages[0]
	require.Len(t, message.Attachments, 3)
	require.NotNil(t, message.Attachment)
	assert.Equal(t, "world.png", message.Attachment.Filename, "the legacy field stays populated")
	assert.True(t, message.Attachments[0].Previewable, "a real PNG can be shown inline")
	assert.False(t, message.Attachments[2].Previewable, "markdown is a download, not a preview")

	for _, attachment := range message.Attachments {
		download := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet, attachment.URL, nil))
		require.Equal(t, http.StatusOK, download.Code, download.Body.String())
		assert.Contains(t, download.Header().Get("Content-Disposition"), attachment.Filename)
	}
	legacy := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet,
		"/api/human-messages/"+strconv.FormatInt(message.ID, 10)+"/attachment", nil))
	require.Equal(t, http.StatusOK, legacy.Code, legacy.Body.String())
	assert.Contains(t, legacy.Header().Get("Content-Disposition"), "world.png")

	// Deleting the message reclaims every file it published.
	paths := make([]string, 0, 3)
	for _, a := range msgs[0].Attachments {
		paths = append(paths, a.StoragePath)
	}
	del := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost,
		"/api/human-messages/delete", map[string]any{"id": msgs[0].ID}))
	require.Equal(t, http.StatusOK, del.Code, del.Body.String())
	for _, path := range paths {
		assert.NoFileExists(t, path)
	}
}

// Scenario: previewability is the daemon's verdict on the BYTES, per published
// file — a payload that only claims to be an image never reaches the preview
// surface, and every attachment is still downloadable through its own URL.
func TestNotifyHuman_PreviewableIsPerFileAndByteVerified(t *testing.T) {
	f := newFlow(t)
	const conv = "snif-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "sniffer")
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermHumanNotify, "test"))

	rec := postNotifyMultipartAttachments(t, f.Mux, conv, []notifyFile{
		{"real.png", "image/png", onePixelPNG},
		{"fake.png", "image/png", "<html><script>alert(1)</script></html>"},
		{"art.svg", "image/svg+xml", "<svg xmlns=\"http://www.w3.org/2000/svg\"/>"},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	msgs, err := db.ListHumanMessages()
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Len(t, msgs[0].Attachments, 3)

	dash := dashHandlerForTest(t)
	var payload struct {
		Messages []struct {
			ID          int64 `json:"id"`
			Attachments []struct {
				Filename    string `json:"filename"`
				URL         string `json:"url"`
				Previewable bool   `json:"previewable"`
			} `json:"attachments"`
		} `json:"messages"`
	}
	snap := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil))
	require.Equal(t, http.StatusOK, snap.Code, snap.Body.String())
	testharness.DecodeJSON(t, snap, &payload)
	require.Len(t, payload.Messages, 1)
	attachments := payload.Messages[0].Attachments
	require.Len(t, attachments, 3)
	assert.True(t, attachments[0].Previewable, "a real PNG is previewable")
	assert.False(t, attachments[1].Previewable, "bytes that are not an image never preview")
	assert.False(t, attachments[2].Previewable, "SVG is an active document, not a picture")

	// Each file is still its own download, and the viewer's HEAD preflight
	// works against the per-file route.
	for _, attachment := range attachments {
		head := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodHead, attachment.URL, nil))
		require.Equal(t, http.StatusOK, head.Code, attachment.Filename)
		assert.Equal(t, "nosniff", head.Header().Get("X-Content-Type-Options"))
		assert.Contains(t, head.Header().Get("Content-Disposition"), "attachment",
			"agent bytes are downloaded, never rendered as a document")
	}
}

// Scenario: an attachment id that belongs to another message is not reachable
// through this message's download route.
func TestNotifyHuman_AttachmentDownloadIsScopedToItsMessage(t *testing.T) {
	f := newFlow(t)
	const conv = "scop-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "scoper")
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermHumanNotify, "test"))
	require.Equal(t, http.StatusOK, postNotifyAttachment(t, f.Mux, conv, "first.txt", "one").Code)
	require.Equal(t, http.StatusOK, postNotifyAttachment(t, f.Mux, conv, "second.txt", "two").Code)

	msgs, err := db.ListHumanMessages()
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	other := msgs[0].Attachments[0]
	mine := msgs[1]

	dash := dashHandlerForTest(t)
	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet,
		"/api/human-messages/"+strconv.FormatInt(mine.ID, 10)+
			"/attachments/"+strconv.FormatInt(other.ID, 10), nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// A malformed upload is the sender's mistake: it must answer 400, the way the
// single-body path already does, rather than being reported as a daemon fault.
func TestNotifyHuman_MalformedMultipartIsARequestError(t *testing.T) {
	f := newFlow(t)
	const conv = "malf-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "malformer")
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermHumanNotify, "test"))

	// A multipart body carrying only non-file fields publishes nothing.
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	require.NoError(t, writer.WriteField("note", "no files here"))
	require.NoError(t, writer.Close())
	metadata, err := json.Marshal(map[string]string{"body": "nothing attached"})
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, "/v1/notify-human/attachment", &buf)
	require.NoError(t, err)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-Tclaude-Notify-Metadata", base64.RawURLEncoding.EncodeToString(metadata))
	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(req, conv))
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	// A part whose content type cannot be parsed is equally a bad request.
	rec = postNotifyMultipartAttachments(t, f.Mux, conv, []notifyFile{
		{"broken.png", "image/png; charset=", onePixelPNG},
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	msgs, err := db.ListHumanMessages()
	require.NoError(t, err)
	assert.Empty(t, msgs, "a refused upload publishes no message")
}
