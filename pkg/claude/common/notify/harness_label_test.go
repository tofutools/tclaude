package notify

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The banner title must name the session's harness so a Codex agent's
// notification reads "Codex: …" rather than the historical "Claude: …".
// Unknown/empty harnesses keep the Claude default (the task runner and
// rate-limit warnings go through Send, which passes "").
func TestNotificationTitle_HarnessAttribution(t *testing.T) {
	tests := []struct {
		name    string
		harness string
		status  string
		want    string
	}{
		{"codex permission", "codex", "Awaiting permission", "Codex: Awaiting permission"},
		{"codex idle", "codex", "Idle", "Codex: Idle"},
		{"codex exited", "codex", "Exited", "Codex: Exited"},
		{"claude explicit", "claude", "Working", "Claude: Working"},
		{"empty defaults to claude", "", "Working", "Claude: Working"},
		{"unknown defaults to claude", "gemini", "Idle", "Claude: Idle"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, notificationTitle(tt.harness, tt.status))
		})
	}
}

func TestHarnessLabel(t *testing.T) {
	assert.Equal(t, "Codex", harnessLabel("codex"))
	assert.Equal(t, "Claude", harnessLabel("claude"))
	assert.Equal(t, "Claude", harnessLabel(""), "empty harness keeps the historical Claude default")
	assert.Equal(t, "Claude", harnessLabel("something-else"))
}
