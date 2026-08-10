package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

func TestHarnessCatalogReportsNativeCodexRegistryReadiness(t *testing.T) {
	restore := SetCodexNativeRegistryReadinessForTest(func() error {
		return &session.CodexNativeRegistryError{
			Code: session.CodexNativeRegistryUnsafeMode, Detail: "managed target mode must be 0700",
		}
	})
	t.Cleanup(restore)
	for _, entry := range buildHarnessCatalog() {
		if entry.Name != harness.CodexName {
			continue
		}
		assert.False(t, entry.CodexNativeRegistryReady)
		assert.Contains(t, entry.CodexNativeRegistryReason, session.CodexNativeRegistryUnsafeMode)
		assert.Contains(t, entry.CodexNativeRegistryReason, session.CodexNativeRegistrySetupDoc)
		return
	}
	require.Fail(t, "Codex harness missing from catalog")
}
