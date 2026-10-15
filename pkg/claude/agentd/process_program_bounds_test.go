package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// The confinement rule for an agent-scoped program step: a program narrows to
// whatever OS sandbox confines the agent, and only an agent with none at all
// runs its program steps unconfined.
func TestAgentProgramUnconfinedReason(t *testing.T) {
	for _, tc := range []struct {
		name           string
		implementation sandboxpolicy.Implementation
		harness, mode  string
		osState        string
		unconfined     bool
	}{
		{"tclaude layer", sandboxpolicy.ImplementationTclaudeLayer, harness.CodexName, harness.SandboxDangerFull, "", false},
		{"stacked", sandboxpolicy.ImplementationStacked, harness.DefaultName, "", "on", false},
		{"sandbox-impl off", sandboxpolicy.ImplementationOff, harness.DefaultName, "", "", true},
		{"resource-only", sandboxpolicy.ImplementationResourceOnly, harness.DefaultName, "", "", true},
		{"codex workspace-write", sandboxpolicy.ImplementationHarnessBuiltin, harness.CodexName, harness.SandboxWorkspaceWrite, "", false},
		{"codex danger-full-access", sandboxpolicy.ImplementationHarnessBuiltin, harness.CodexName, harness.SandboxDangerFull, "", true},
		{"claude sandbox on", sandboxpolicy.ImplementationHarnessBuiltin, harness.DefaultName, "", "on", false},
		{"claude sandbox off", sandboxpolicy.ImplementationHarnessBuiltin, harness.DefaultName, "", "off", true},
		{"claude sandbox unconfigured", sandboxpolicy.ImplementationHarnessBuiltin, harness.DefaultName, "", "unconfigured", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason := agentProgramUnconfinedReason(tc.implementation, tc.harness, tc.mode, tc.osState)
			assert.Equal(t, tc.unconfined, reason != "", "reason=%q", reason)
		})
	}
}
