package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func TestNetworkNamespaceRequiresExportVersion13(t *testing.T) {
	envelope := sandboxProfileExportEnvelope{
		Format: sandboxProfileExportFormat, FormatVersion: 12,
		Profiles: []sandboxProfileJSON{{
			Name: "private",
			Network: &sandboxpolicy.NetworkRules{
				Baseline:  sandboxpolicy.NetworkBaselineAllow,
				Namespace: sandboxpolicy.NetworkNamespacePrivate,
			},
		}},
	}
	failure := validateSandboxProfileExportVersionContent(envelope)
	require.NotNil(t, failure)
	assert.Equal(t, "invalid_format", failure.Kind)
	assert.Contains(t, failure.Msg, "version 13")

	envelope.FormatVersion = 13
	assert.Nil(t, validateSandboxProfileExportVersionContent(envelope))
}
