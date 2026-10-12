package notify

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// The banner title must name the session's harness so a Codex agent's
// notification reads "Codex: …" rather than the historical "Claude: …".
// Empty harnesses keep the Claude default (the task runner and
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
		{"shell exited", "shell", "Exited", "Shell: Exited"},
		{"claude explicit", "claude", "Working", "Claude: Working"},
		{"empty defaults to claude", "", "Working", "Claude: Working"},
		{"gemini idle", "gemini", "Idle", "Gemini: Idle"},
		{"copilot idle", "copilot", "Idle", "Copilot: Idle"},
		{"opencode idle", "opencode", "Idle", "OpenCode: Idle"},
		{"unknown neutral label", "unknown", "Idle", "tclaude: Idle"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, notificationTitle(tt.harness, tt.status))
		})
	}
}

func TestHarnessLabel(t *testing.T) {
	assert.Equal(t, "Codex", harnessLabel("codex"))
	assert.Equal(t, "Shell", harnessLabel("shell"))
	assert.Equal(t, "Claude", harnessLabel("claude"))
	assert.Equal(t, "Claude", harnessLabel(""), "empty harness keeps the historical Claude default")
	assert.Equal(t, "tclaude", harnessLabel("something-else"))
}

func TestFormatStatus_AgentRecoveryTransitions(t *testing.T) {
	assert.Equal(t, "Crashed", formatStatus("crashed"))
	assert.Equal(t, "Crash loop / backoff", formatStatus("crash loop / backoff"))
}

func TestRegisteredHarnessNotificationLabels(t *testing.T) {
	for _, name := range harness.Names() {
		label := harnessLabel(name)
		assert.NotEqual(t, "tclaude", label, "declare a notification label for %s", name)
		if name != harness.DefaultName {
			assert.NotEqual(t, "Claude", label, name)
		}
	}
}
