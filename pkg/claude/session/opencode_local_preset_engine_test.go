package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestOpenCodeSelectedModelStillRequiresAnExplicitProvider(t *testing.T) {
	_, cwd := isolateModelTransportLaunch(t)
	openCode := harness.MustGet(harness.OpenCodeName)

	_, err := ResolveTclaudeLayerModelTransport(openCode,
		ModelTransportLaunchContext{Model: "sonnet", Cwd: cwd})
	require.Error(t, err)
	assert.Contains(t, err.Error(),
		"requires an explicit provider/model launch model")

	_, err = ResolveTclaudeLayerModelTransport(openCode,
		ModelTransportLaunchContext{Model: "corp/model", Cwd: cwd})
	require.Error(t, err,
		"an explicit provider/model still needs the inline explicit-provider config")
}
