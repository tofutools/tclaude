package harness

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveCodexAppServerIsDefaultOffAndCapabilityGated(t *testing.T) {
	codex := MustGet(CodexName)
	claude := MustGet(DefaultName)
	on := true
	off := false

	selected, err := ResolveCodexAppServer(codex, nil)
	require.NoError(t, err)
	assert.False(t, selected, "unset must preserve the established send-keys drive")

	selected, err = ResolveCodexAppServer(codex, &on)
	require.NoError(t, err)
	assert.True(t, selected)

	selected, err = ResolveCodexAppServer(claude, &off)
	require.NoError(t, err)
	assert.False(t, selected, "an explicit false is harmless on a sparse cross-harness profile")

	selected, err = ResolveCodexAppServer(claude, &on)
	assert.Error(t, err)
	assert.False(t, selected)
}
