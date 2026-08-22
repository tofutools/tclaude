package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func TestCallerIdentityRequiresExportVersion16(t *testing.T) {
	envelope := sandboxProfileExportEnvelope{
		Format: sandboxProfileExportFormat, FormatVersion: 15,
		Profiles: []sandboxProfileJSON{{
			Name: "caller-id",
			Network: &sandboxpolicy.NetworkRules{
				Baseline:               sandboxpolicy.NetworkBaselineDeny,
				PreserveCallerIdentity: true,
			},
		}},
	}
	failure := validateSandboxProfileExportVersionContent(envelope)
	require.NotNil(t, failure)
	assert.Equal(t, "invalid_format", failure.Kind)
	assert.Contains(t, failure.Msg, "version 16")

	envelope.FormatVersion = 16
	assert.Nil(t, validateSandboxProfileExportVersionContent(envelope))
}
