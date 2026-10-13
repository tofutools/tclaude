package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// mp4Header is the opening ftyp box of an ordinary H.264 MP4, padded with
// payload so a ranged read has something past the header to return.
const mp4Header = "\x00\x00\x00\x18ftypisom\x00\x00\x02\x00isomiso2" + "mdat-payload-bytes"

// Scenario: an agent publishes a demo video. The dashboard snapshot offers the
// player for real video containers whatever the sender labelled them, and the
// download route answers the player's Range requests with the sniffed type.
func TestNotifyHuman_VideosArePlayable(t *testing.T) {
	f := newFlow(t)
	const conv = "vid0-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "demo-maker")
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermHumanNotify, "test"))

	rec := postNotifyMultipartAttachments(t, f.Mux, conv, []notifyFile{
		{"demo.mp4", "application/octet-stream", mp4Header},
		{"clip.webm", "video/webm", "\x1a\x45\xdf\xa3\x9f\x42\x86\x81\x01"},
		{"fake.mp4", "video/mp4", "<html>not a video</html>"},
		{"still.avif", "image/avif", "\x00\x00\x00\x1cftypavif\x00\x00\x00\x00"},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	dash := dashHandlerForTest(t)
	var payload struct {
		Messages []struct {
			Attachments []struct {
				URL   string `json:"url"`
				Video bool   `json:"video"`
			} `json:"attachments"`
		} `json:"messages"`
	}
	snap := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil))
	require.Equal(t, http.StatusOK, snap.Code, snap.Body.String())
	testharness.DecodeJSON(t, snap, &payload)
	require.Len(t, payload.Messages, 1)
	attachments := payload.Messages[0].Attachments
	require.Len(t, attachments, 4)

	assert.True(t, attachments[0].Video, "an MP4 is playable even when sent as octet-stream")
	assert.True(t, attachments[1].Video, "a WebM is playable")
	assert.False(t, attachments[2].Video, "non-video bytes cannot claim an .mp4 name")
	assert.False(t, attachments[3].Video, "an AVIF still shares the ftyp box but is not a video")

	// The player seeks with Range requests.
	req := testharness.JSONRequest(t, http.MethodGet, attachments[0].URL, nil)
	req.Header.Set("Range", "bytes=4-11")
	ranged := testharness.Serve(dash, req)
	require.Equal(t, http.StatusPartialContent, ranged.Code, ranged.Body.String())
	assert.Equal(t, "ftypisom", ranged.Body.String())
	assert.Equal(t, "video/mp4", ranged.Header().Get("Content-Type"),
		"a confirmed video is served as its sniffed container type")
	assert.Equal(t, "nosniff", ranged.Header().Get("X-Content-Type-Options"))
	assert.Contains(t, ranged.Header().Get("Content-Disposition"), "attachment")

	// A full GET still returns the whole file.
	full := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet, attachments[0].URL, nil))
	require.Equal(t, http.StatusOK, full.Code)
	assert.Equal(t, mp4Header, full.Body.String())

	// Non-video files keep the type the sender gave them.
	fake := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet, attachments[2].URL, nil))
	require.Equal(t, http.StatusOK, fake.Code)
	assert.Equal(t, "video/mp4", fake.Header().Get("Content-Type"))
	still := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet, attachments[3].URL, nil))
	assert.Equal(t, "image/avif", still.Header().Get("Content-Type"))
}
