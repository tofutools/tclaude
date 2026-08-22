package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSanitizeInitialSpawnRequestRedactsContentAndAuthority(t *testing.T) {
	request, redactions, err := sanitizeInitialSpawnRequest(`{
		"harness":"codex","cwd":"/work/repo","initial_message":"secret task",
		"write_proof_token":"never-export-this","sandbox_profile":"dev"}`)
	require.NoError(t, err)
	assert.Equal(t, "codex", request["harness"])
	assert.Equal(t, "/work/repo", request["cwd"])
	assert.Equal(t, "dev", request["sandbox_profile"])
	assert.Equal(t, "<redacted: 11 bytes>", request["initial_message"])
	assert.NotContains(t, request, "write_proof_token")
	assert.ElementsMatch(t, []string{
		"requested_at_spawn.parameters.initial_message",
		"requested_at_spawn.parameters.write_proof_token",
	}, redactions)
}
