package agentd

import (
	"net/http/httptest"
	"runtime"
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

// TestLinuxClipboardTools_FixedArgsAndOrder locks the command-injection
// safety invariant: the platform copy tools carry only compile-time
// constant args (the untrusted text goes via stdin, never argv), and
// wl-copy leads only when the session advertises Wayland.
func TestLinuxClipboardTools_FixedArgsAndOrder(t *testing.T) {
	want := map[string][]string{
		"wl-copy": nil,
		"xclip":   {"-selection", "clipboard"},
		"xsel":    {"--clipboard", "--input"},
	}

	t.Setenv("WAYLAND_DISPLAY", "wayland-0")
	wayland := linuxClipboardTools()
	assert.Equal(t, "wl-copy", wayland[0].name, "wl-copy leads under Wayland")

	t.Setenv("WAYLAND_DISPLAY", "")
	x11 := linuxClipboardTools()
	assert.Equal(t, "xclip", x11[0].name, "xclip leads without Wayland")

	// Whatever the order, every tool's args are exactly the known constants —
	// no path or user text is ever spliced into argv.
	for _, tools := range [][]clipboardTool{wayland, x11} {
		assert.Len(t, tools, len(want))
		for _, tool := range tools {
			w, ok := want[tool.name]
			assert.True(t, ok, "unexpected clipboard tool %q", tool.name)
			assert.Equal(t, w, tool.args, "tool %q must carry only its fixed args", tool.name)
		}
	}
}

// TestClipboardCommand_NoTextInArgv proves that whatever command
// clipboardCommand resolves on this host, the argv beyond the tool path
// never carries request-controlled text (it can only be a fixed flag from
// the known set) — the text is delivered on stdin by writeToClipboard.
func TestClipboardCommand_NoTextInArgv(t *testing.T) {
	cmd, err := clipboardCommand()
	if err != nil {
		t.Skipf("no clipboard tool on this host (%v) — nothing to assert", err)
	}
	allowed := map[string]bool{
		"-selection": true, "clipboard": true, // xclip
		"--clipboard": true, "--input": true, // xsel
	}
	// cmd.Args[0] is the tool path; the rest must all be known constants.
	for _, a := range cmd.Args[1:] {
		assert.True(t, allowed[a], "argv element %q is not a known constant flag on %s", a, runtime.GOOS)
	}
}
