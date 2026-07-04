package agent

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRunClipboard_EmptyRejected: a body that is empty or whitespace-only
// is rejected locally before any daemon I/O — copying nothing is a mistake.
func TestRunClipboard_EmptyRejected(t *testing.T) {
	for _, text := range []string{"", "   \n\t "} {
		var stdout, stderr bytes.Buffer
		rc := runClipboard(&clipboardParams{Text: text}, strings.NewReader(""), &stdout, &stderr)
		assert.Equal(t, rcInvalidArg, rc, "empty/whitespace text should be rejected, text=%q", text)
		assert.Contains(t, stderr.String(), "nothing to copy")
	}
}

// TestRunClipboard_TooLongRejected: a payload past the cap is rejected
// locally with a clear message, before hitting the socket.
func TestRunClipboard_TooLongRejected(t *testing.T) {
	var stdout, stderr bytes.Buffer
	huge := strings.Repeat("x", maxClipboardBytes+1)
	rc := runClipboard(&clipboardParams{Text: huge}, strings.NewReader(""), &stdout, &stderr)
	assert.Equal(t, rcInvalidArg, rc)
	assert.Contains(t, stderr.String(), "too long")
}

// TestRunClipboard_SendsVerbatim: content is sent to the daemon exactly as
// given — leading/trailing whitespace and newlines preserved — even though
// the empty-check trims. Stubs the daemon so no socket is needed.
func TestRunClipboard_SendsVerbatim(t *testing.T) {
	prevAvail := DaemonAvailableImpl
	prevReq := DaemonRequestImpl
	t.Cleanup(func() {
		DaemonAvailableImpl = prevAvail
		DaemonRequestImpl = prevReq
	})
	DaemonAvailableImpl = func() bool { return true }

	var capturedBody map[string]any
	var capturedPath string
	DaemonRequestImpl = func(_ /*method*/, path string, in, out any, _ DaemonOpts) error {
		capturedPath = path
		if m, ok := in.(map[string]any); ok {
			capturedBody = m
		}
		if resp, ok := out.(*struct {
			Bytes int `json:"bytes"`
		}); ok {
			resp.Bytes = 7
		}
		return nil
	}

	const verbatim = "  indented line\nsecond line\n" // leading spaces + trailing newline
	var stdout, stderr bytes.Buffer
	rc := runClipboard(&clipboardParams{Text: verbatim}, strings.NewReader(""), &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())
	assert.Equal(t, "/v1/clipboard", capturedPath)
	assert.Equal(t, verbatim, capturedBody["text"],
		"text must reach the daemon verbatim (whitespace preserved), got %#v", capturedBody)
	assert.Contains(t, stdout.String(), "Copied 7 bytes")
}

// TestRunClipboard_InvalidAskHumanRejected: a malformed --ask-human value
// is caught locally as an arg error.
func TestRunClipboard_InvalidAskHumanRejected(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := runClipboard(&clipboardParams{Text: "hello", AskHuman: "not-a-duration"},
		strings.NewReader(""), &stdout, &stderr)
	assert.Equal(t, rcInvalidArg, rc)
	assert.Contains(t, stderr.String(), "ask-human")
}
