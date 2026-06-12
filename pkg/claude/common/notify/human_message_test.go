package notify

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFormatHumanMessage(t *testing.T) {
	t.Run("subject present: title carries subject, body leads with sender", func(t *testing.T) {
		title, body := formatHumanMessage("tclaude-PO", "tclaude-dev", "status", "build is green")
		assert.Equal(t, "Claude: status", title)
		assert.Equal(t, "tclaude-PO: build is green\n— tclaude-dev", body)
	})

	t.Run("no subject: title carries the sender attribution", func(t *testing.T) {
		title, body := formatHumanMessage("tclaude-PO", "tclaude-dev", "", "ping")
		assert.Equal(t, "Claude: tclaude-PO messaged you", title)
		// Sender is in the title, so the body is just message + group.
		assert.Equal(t, "ping\n— tclaude-dev", body)
	})

	t.Run("no group: body omits the group trailer", func(t *testing.T) {
		_, body := formatHumanMessage("tclaude-PO", "", "", "ping")
		assert.Equal(t, "ping", body)
	})

	t.Run("empty sender title falls back to a generic phrase", func(t *testing.T) {
		title, _ := formatHumanMessage("", "", "", "ping")
		assert.Equal(t, "Claude: An agent messaged you", title)
	})

	t.Run("surrounding whitespace is trimmed", func(t *testing.T) {
		title, body := formatHumanMessage("  PO  ", "  dev  ", "  subj  ", "  hi  ")
		assert.Equal(t, "Claude: subj", title)
		assert.Equal(t, "PO: hi\n— dev", body)
	})

	t.Run("over-long body is truncated to the body cap", func(t *testing.T) {
		_, body := formatHumanMessage("PO", "", "", strings.Repeat("x", notifyBodyMaxLen+500))
		assert.LessOrEqual(t, len([]rune(body)), notifyBodyMaxLen)
		assert.True(t, strings.HasSuffix(body, "…"), "a truncated body ends with the ellipsis")
	})

	t.Run("over-long subject is truncated to the title cap", func(t *testing.T) {
		title, _ := formatHumanMessage("PO", "", strings.Repeat("s", notifyTitleMaxLen+50), "hi")
		assert.LessOrEqual(t, len([]rune(title)), notifyTitleMaxLen)
	})
}
