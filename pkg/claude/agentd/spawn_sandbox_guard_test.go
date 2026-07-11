package agentd

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestSandboxProfileCapabilityFailureRequiresClaudeOnWithDeny(t *testing.T) {
	snapshot := &sandboxpolicy.Snapshot{Effective: sandboxpolicy.EffectiveProfile{
		Filesystem: []sandboxpolicy.FilesystemGrant{{Path: "/tmp/secret", Access: sandboxpolicy.AccessDeny}},
	}}

	require.Nil(t, sandboxProfileCapabilityFailure(harness.DefaultName, harness.ClaudeSandboxOn, snapshot))
	for _, mode := range []string{harness.ClaudeSandboxOff, harness.ClaudeSandboxInherit, ""} {
		failure := sandboxProfileCapabilityFailure(harness.DefaultName, mode, snapshot)
		require.NotNil(t, failure)
		require.Contains(t, failure.Msg, `require sandbox "on"`)
	}
}
