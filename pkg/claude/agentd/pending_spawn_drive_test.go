package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestPendingSpawnFromParamsFreezesResolvedCodexDrive(t *testing.T) {
	group := &db.AgentGroup{ID: 42}
	for _, tc := range []struct {
		name     string
		harness  string
		selected bool
		source   string
		want     *bool
	}{
		{name: "explicit true", harness: harness.CodexName, selected: true, source: agent.ProvExplicit, want: boolPointer(true)},
		{name: "explicit false", harness: harness.CodexName, selected: false, source: agent.ProvExplicit, want: boolPointer(false)},
		{name: "profile true", harness: harness.CodexName, selected: true, source: `spawn profile "ab-opted-in"`, want: boolPointer(true)},
		{name: "non Codex remains unset", harness: harness.DefaultName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateRoot := "/host/codex-state"
			pending := pendingSpawnFromParams(group, spawnParams{
				Harness: tc.harness, CodexAppServer: tc.selected,
				CodexAppServerSource: tc.source,
				CodexStateRoot:       stateRoot, CodexStateRootSource: codexStateRootSourceCodexHome,
			}, "pending-drive")
			if tc.want == nil {
				assert.Nil(t, pending.CodexAppServer)
				assert.Empty(t, pending.CodexAppServerSource)
				assert.Empty(t, pending.CodexStateRoot)
				return
			}
			require.NotNil(t, pending.CodexAppServer)
			assert.Equal(t, *tc.want, *pending.CodexAppServer)
			assert.Equal(t, tc.source, pending.CodexAppServerSource)
			assert.Equal(t, stateRoot, pending.CodexStateRoot)
			assert.Equal(t, codexStateRootSourceCodexHome, pending.CodexStateRootSource)
		})
	}
}

func boolPointer(value bool) *bool { return &value }
