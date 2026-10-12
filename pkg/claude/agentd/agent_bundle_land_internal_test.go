package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// Native history is budgeted by effective context window, not model name.
func TestLandsOnSmallerWindow(t *testing.T) {
	claude, err := harness.Resolve(harness.DefaultName)
	require.NoError(t, err)
	var std string
	for _, m := range claude.Models.Models() {
		if m != "" {
			std = m
			break
		}
	}
	require.NotEmpty(t, std)
	origin := func(m string) *agentbundle.Origin { return &agentbundle.Origin{Model: m} }
	assert.True(t, landsOnSmallerWindow(claude, origin(std+"[1m]"), std), "1M onto 200k is the same model with a smaller window")
	assert.False(t, landsOnSmallerWindow(claude, origin(std), std+"[1m]"), "a larger window keeps native fidelity")
	assert.False(t, landsOnSmallerWindow(claude, origin(std), std), "an equal window keeps native fidelity")
	assert.False(t, landsOnSmallerWindow(claude, origin("claude-unlisted"), "claude-unlisted"), "the same model with an unknown window keeps native fidelity")
	assert.True(t, landsOnSmallerWindow(claude, origin("claude-unlisted"), std), "a different model from an unknown window is budgeted")
}
