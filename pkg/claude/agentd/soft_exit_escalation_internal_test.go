package agentd

import (
	"strings"
	"testing"
)

func TestFormatPaneScreenTail(t *testing.T) {
	t.Run("drops blank lines and joins on one line", func(t *testing.T) {
		got := formatPaneScreenTail("\n  ❯ prompt  \n\n status bar \n\n")
		want := "❯ prompt | status bar"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("keeps only the last lines of a tall screen", func(t *testing.T) {
		lines := make([]string, 0, 40)
		for r := 'a'; r < 'a'+40; r++ {
			lines = append(lines, strings.Repeat(string(r), 3))
		}
		got := formatPaneScreenTail(strings.Join(lines, "\n"))
		if parts := strings.Split(got, " | "); len(parts) != paneScreenTailLines {
			t.Fatalf("kept %d lines, want %d: %q", len(parts), paneScreenTailLines, got)
		} else if parts[len(parts)-1] != lines[len(lines)-1] {
			t.Fatalf("tail should end with the screen's last line, got %q", parts[len(parts)-1])
		}
	})

	t.Run("clips oversized content", func(t *testing.T) {
		got := formatPaneScreenTail(strings.Repeat("x", 3*paneScreenTailClip))
		if len(got) > paneScreenTailClip+len("…") {
			t.Fatalf("tail not clipped: %d bytes", len(got))
		}
	})

	t.Run("empty screen yields empty tail", func(t *testing.T) {
		if got := formatPaneScreenTail("\n\n  \n"); got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})
}
