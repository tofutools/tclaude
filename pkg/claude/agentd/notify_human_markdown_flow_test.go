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

// Scenario: an agent publishes a Markdown document alongside other files. The
// dashboard snapshot marks only the document renderable, the verdict is on the
// bytes rather than on the name, and the viewer's plain GET returns the source
// it will lay out.
func TestNotifyHuman_MarkdownDocumentsAreRenderable(t *testing.T) {
	f := newFlow(t)
	const conv = "mkdn-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "doc-writer")
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermHumanNotify, "test"))

	const report = "# Report\n\nAll green.\n"
	rec := postNotifyMultipartAttachments(t, f.Mux, conv, []notifyFile{
		{"report.md", "text/markdown; charset=utf-8", report},
		{"shot.png", "image/png", onePixelPNG},
		{"run.log", "text/plain; charset=utf-8", "started\n"},
		{"trojan.md", "text/markdown", "# hi\x00\x01binary"},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	dash := dashHandlerForTest(t)
	var payload struct {
		Messages []struct {
			Attachments []struct {
				Filename    string `json:"filename"`
				URL         string `json:"url"`
				Previewable bool   `json:"previewable"`
				Markdown    bool   `json:"markdown"`
			} `json:"attachments"`
		} `json:"messages"`
	}
	snap := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil))
	require.Equal(t, http.StatusOK, snap.Code, snap.Body.String())
	testharness.DecodeJSON(t, snap, &payload)
	require.Len(t, payload.Messages, 1)
	attachments := payload.Messages[0].Attachments
	require.Len(t, attachments, 4)

	assert.True(t, attachments[0].Markdown, "a Markdown document is offered to the viewer")
	assert.False(t, attachments[0].Previewable, "a document is not an image")
	assert.False(t, attachments[1].Markdown, "an image is not a document")
	assert.True(t, attachments[1].Previewable)
	assert.False(t, attachments[2].Markdown, "a plain log is a download, not a document")
	assert.False(t, attachments[3].Markdown, "binary bytes cannot claim a .md name")

	// The viewer reads the document with an ordinary authenticated GET, and the
	// bytes it gets back are the ones the agent published.
	read := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet, attachments[0].URL, nil))
	require.Equal(t, http.StatusOK, read.Code, read.Body.String())
	assert.Equal(t, report, read.Body.String())
	assert.Equal(t, "nosniff", read.Header().Get("X-Content-Type-Options"))
	assert.Contains(t, read.Header().Get("Content-Disposition"), "attachment",
		"the route still hands the browser a download, never a page to navigate to")
}
