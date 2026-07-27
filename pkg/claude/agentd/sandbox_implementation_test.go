package agentd

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestSandboxImplementationHostFailureUsesServerProbeForOpenCode(t *testing.T) {
	previousInteractive := tclaudeLayerHostAvailability
	previousServer := tclaudeLayerServerHostAvailability
	t.Cleanup(func() {
		tclaudeLayerHostAvailability = previousInteractive
		tclaudeLayerServerHostAvailability = previousServer
	})
	tclaudeLayerHostAvailability = func() error {
		return errors.New("pidfd unavailable")
	}
	tclaudeLayerServerHostAvailability = func() error { return nil }

	require.Nil(t, sandboxImplementationHostFailure(
		harness.OpenCodeName,
		string(sandboxpolicy.ImplementationTclaudeLayer),
	))
	failure := sandboxImplementationHostFailure(
		harness.DefaultName,
		string(sandboxpolicy.ImplementationTclaudeLayer),
	)
	require.NotNil(t, failure)
	assert.Contains(t, failure.Msg, "pidfd unavailable")
}

func TestStringLaunchFieldSkipNoteQualifiesOpenCodeDarwinReason(t *testing.T) {
	note := stringLaunchFieldSkipNote(
		`group default profile "layered"`,
		sandboxImplementationField,
		harness.OpenCodeName,
		errors.New("tclaude-layer does not support OpenCode on macOS: server wrapping requires Linux"),
	)
	assert.Equal(t,
		`group default profile "layered" sandbox_implementation ignored (not supported for OpenCode on macOS)`,
		note,
	)
}
