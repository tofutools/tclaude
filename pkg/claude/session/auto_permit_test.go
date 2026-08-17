package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The accept table is the whole of what auto-permit will answer. Pin its shape:
// a slug per entry, compile-time keys, and no catch-all.
func TestAutoPermitAccepts_Shape(t *testing.T) {
	accept, ok := AutoPermitAcceptForTool("EnterWorktree")
	require.True(t, ok, "the EnterWorktree safety check is the prompt this exists for")
	assert.Equal(t, PermAutoPermitEnterWorktree, accept.Slug)
	assert.Equal(t, []string{"Enter"}, accept.Keys)

	for tool, a := range autoPermitAccepts {
		assert.NotEmptyf(t, a.Slug, "%s: needs a consenting slug", tool)
		assert.NotEmptyf(t, a.Keys, "%s: needs accept keys", tool)
	}
	_, ok = AutoPermitAcceptForTool("")
	assert.False(t, ok, "there is no catch-all entry")
	_, ok = AutoPermitAcceptForTool("Bash")
	assert.False(t, ok, "consent is per named prompt, never a blanket accept")
}

// Which events an ordinary (unbrokered) launch hands to the daemon. Only a
// permission prompt auto-permit knows how to answer — everything else keeps
// applying in-process exactly as before.
func TestAutoPermitNeedsDaemon(t *testing.T) {
	assert.True(t, autoPermitNeedsDaemon(HookCallbackInput{
		HookEventName: "PermissionRequest", ToolName: "EnterWorktree"}))

	assert.False(t, autoPermitNeedsDaemon(HookCallbackInput{
		HookEventName: "PermissionRequest", ToolName: "Bash"}),
		"a prompt no condition names is not the daemon's business")
	assert.False(t, autoPermitNeedsDaemon(HookCallbackInput{
		HookEventName: "PreToolUse", ToolName: "EnterWorktree"}),
		"only the prompt event, not every mention of the tool")
	assert.False(t, autoPermitNeedsDaemon(HookCallbackInput{HookEventName: "Stop"}))
}
