package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestReincarnateHelpDocumentsHarnessSpecificContextPolicy(t *testing.T) {
	long := reincarnateCmd().Long
	assert.Contains(t, long, "primarily a Claude Code context-management tool")
	assert.Contains(t, long, "Codex CLI has effective, efficient automatic compaction")
	assert.Contains(t, long, "run to full context and auto-compact")
	assert.Contains(t, long, "merely to free context space")
	assert.Contains(t, long, "explicit human request")
}
