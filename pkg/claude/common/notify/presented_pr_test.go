package notify

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFormatPresentedPR(t *testing.T) {
	t.Run("full attribution: url leads the body, summary and group follow", func(t *testing.T) {
		title, body := formatPresentedPR("tclaude-worker", "tofutools",
			"https://github.com/tofutools/tclaude/pull/42", "notification on present-pr")
		assert.Equal(t, "Claude: pull request", title)
		assert.Equal(t, "tclaude-worker presented a pull request\n"+
			"https://github.com/tofutools/tclaude/pull/42\n"+
			"notification on present-pr\n— tofutools", body)
	})

	t.Run("unknown agent title falls back to a generic phrase", func(t *testing.T) {
		_, body := formatPresentedPR("", "", "https://example.com/pr/1", "")
		assert.Equal(t, "An agent presented a pull request\nhttps://example.com/pr/1", body)
	})

	t.Run("surrounding whitespace is trimmed on every field", func(t *testing.T) {
		_, body := formatPresentedPR("  worker  ", "  squad  ", "  https://example.com/pr/2  ", "  fix  ")
		assert.Equal(t, "worker presented a pull request\nhttps://example.com/pr/2\nfix\n— squad", body)
	})

	t.Run("an over-long summary cannot push the body past the banner cap", func(t *testing.T) {
		_, body := formatPresentedPR("worker", "", "https://example.com/pr/3",
			strings.Repeat("x", notifyBodyMaxLen+500))
		assert.LessOrEqual(t, len([]rune(body)), notifyBodyMaxLen)
		assert.Contains(t, body, "https://example.com/pr/3", "the URL survives truncation because it leads")
	})
}
