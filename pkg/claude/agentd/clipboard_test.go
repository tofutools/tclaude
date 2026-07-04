package agentd

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestClipboardApprovalPreview covers the popup-preview extractor: for a
// clipboard write it returns the RAW text (so the popup shows the exact
// content, not the JSON envelope); for anything else, or an unparseable /
// truncated body, it declines so the caller keeps the generic preview.
func TestClipboardApprovalPreview(t *testing.T) {
	// A clipboard write: the raw text is surfaced, newlines intact.
	got, ok := clipboardApprovalPreview(PermHumanClipboard, "{\n  \"text\": \"line1\\nline2\"\n}")
	assert.True(t, ok)
	assert.Equal(t, "line1\nline2", got)

	// A non-clipboard perm: declined regardless of body shape.
	_, ok = clipboardApprovalPreview(PermHumanNotify, `{"text":"hi"}`)
	assert.False(t, ok, "non-clipboard perm should not get the clipboard preview")

	// A truncated / unparseable body (e.g. a payload past the preview cap):
	// declined, so the generic JSON preview is kept.
	_, ok = clipboardApprovalPreview(PermHumanClipboard, `{"text":"unterminated`)
	assert.False(t, ok, "unparseable body should be declined")

	// An empty text field: declined (nothing meaningful to preview).
	_, ok = clipboardApprovalPreview(PermHumanClipboard, `{"text":""}`)
	assert.False(t, ok, "empty text should be declined")
}

// TestRenderApprovalPage_EscapesClipboardContent proves the injection gate:
// untrusted clipboard text rendered into the approval page is HTML-escaped,
// so agent output can't inject markup/script into the human's popup. The
// custom "Clipboard content" label also appears.
func TestRenderApprovalPage_EscapesClipboardContent(t *testing.T) {
	const evil = `</pre><script>alert('xss')</script>`
	req := &approvalRequest{
		id:          "abc123",
		perm:        PermHumanClipboard,
		convID:      "cccc-1111",
		convTitle:   "worker",
		method:      "POST",
		path:        "/v1/clipboard",
		bodyPreview: evil,
		bodyLabel:   "Clipboard content",
		timeout:     30 * time.Second,
	}

	rec := httptest.NewRecorder()
	renderApprovalPage(rec, req)
	body := rec.Body.String()

	assert.NotContains(t, body, "<script>alert('xss')</script>",
		"raw agent markup must NOT reach the page verbatim")
	assert.Contains(t, body, "&lt;script&gt;",
		"the script tag must be HTML-escaped")
	assert.Contains(t, body, "Clipboard content",
		"the clipboard body row carries its dedicated label")
	// The label itself is also escaped through html.EscapeString; a plain
	// label survives unchanged.
	assert.True(t, strings.Contains(body, "<dt>Clipboard content</dt>"),
		"expected the labelled body row, got:\n%s", body)
}
