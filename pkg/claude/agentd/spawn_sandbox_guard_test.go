package agentd

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestSandboxProfileCapabilityFailureRejectsClaudeOffWithDeny(t *testing.T) {
	snapshot := &sandboxpolicy.Snapshot{Effective: sandboxpolicy.EffectiveProfile{
		Filesystem: []sandboxpolicy.FilesystemGrant{{Path: "/tmp/secret", Access: sandboxpolicy.AccessDeny}},
	}}

	failure := sandboxProfileCapabilityFailure(harness.DefaultName, harness.ClaudeSandboxOff, snapshot)
	require.NotNil(t, failure)
	require.Contains(t, failure.Msg, "require the OS sandbox")
	require.Nil(t, sandboxProfileCapabilityFailure(harness.DefaultName, harness.ClaudeSandboxOn, snapshot))
	require.Nil(t, sandboxProfileCapabilityFailure(harness.DefaultName, harness.ClaudeSandboxInherit, snapshot))
}
