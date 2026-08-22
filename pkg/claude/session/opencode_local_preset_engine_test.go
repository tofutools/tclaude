package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestOpenCodeSelectedModelMayLeaveProviderRouteUnknown(t *testing.T) {
	_, cwd := isolateModelTransportLaunch(t)
	openCode := harness.MustGet(harness.OpenCodeName)

	resolved, err := ResolveTclaudeLayerModelTransport(openCode,
		ModelTransportLaunchContext{Model: "sonnet", Cwd: cwd})
	require.NoError(t, err)
	assert.Equal(t, "sonnet", resolved.Model)
	assert.False(t, resolved.ProviderResolved)

	resolved, err = ResolveTclaudeLayerModelTransport(openCode,
		ModelTransportLaunchContext{Model: "corp/model", Cwd: cwd})
	require.NoError(t, err)
	assert.Equal(t, "corp/model", resolved.Model)
	assert.False(t, resolved.ProviderResolved)
}
