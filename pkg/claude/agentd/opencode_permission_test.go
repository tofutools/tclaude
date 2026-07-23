package agentd

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestOpenCodePermissionJSONForLaunchResolvesBlankCwd(t *testing.T) {
	encoded, err := openCodePermissionJSONForLaunch("",
		harness.OpenCodeSandboxAccessControl, harness.OpenCodeApprovalDeny, nil)
	require.NoError(t, err)

	var rules []harness.OpenCodePermissionRule
	require.NoError(t, json.Unmarshal([]byte(encoded), &rules))
	require.NotEmpty(t, rules)

	foundReadableLaunchDir := false
	for _, rule := range rules {
		if rule.Permission == "read" && rule.Action == "allow" {
			foundReadableLaunchDir = true
			break
		}
	}
	assert.True(t, foundReadableLaunchDir,
		"the inherited daemon cwd must be compiled into the access-control rules")
}
