package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// An OpenCode launch says in its echo that sub-agents are off, so an agent
// that never delegates does not leave the operator wondering why. Other
// harnesses carry no such note.
func TestSpawnPreview_OpenCodeSaysSubagentsAreOff(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	h := agentd.BuildDashboardHandlerForTest()

	code, got, raw := spawnPreview(t, h, "crew", map[string]any{"name": "oc", "harness": harness.OpenCodeName})
	require.Equal(t, http.StatusOK, code, raw)
	assert.Contains(t, got.Notes, harness.OpenCodeSubagentsOffNote)

	code, got, raw = spawnPreview(t, h, "crew", map[string]any{"name": "cc", "harness": harness.DefaultName})
	require.Equal(t, http.StatusOK, code, raw)
	assert.NotContains(t, got.Notes, harness.OpenCodeSubagentsOffNote)
}

// --tools all is echoed with its own note instead of the sub-agents-off one,
// keeps its per-tool entries in canonical form, and is refused, naming the
// fix, where OpenCode's sub-agents could not inherit the policy.
func TestSpawnPreview_OpenCodeToolsAll(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	h := agentd.BuildDashboardHandlerForTest()

	code, got, raw := spawnPreview(t, h, "crew", map[string]any{
		"name": "oc", "harness": harness.OpenCodeName, "sandbox_implementation": "tclaude-layer",
		"approval": harness.OpenCodeApprovalAllowTools, "tools": "all,plan=deny,question=deny",
	})
	require.Equal(t, http.StatusOK, code, raw)
	assert.Contains(t, got.Notes, harness.OpenCodeAllToolsNote)
	assert.NotContains(t, got.Notes, harness.OpenCodeSubagentsOffNote)

	code, _, raw = spawnPreview(t, h, "crew", map[string]any{
		"name": "oc", "harness": harness.OpenCodeName, "sandbox_implementation": "tclaude-layer",
		"approval": harness.OpenCodeApprovalAsk, "tools": "all",
	})
	assert.Equal(t, http.StatusUnprocessableEntity, code, raw)
	assert.Contains(t, raw, "do not inherit ask rules")

	code, _, raw = spawnPreview(t, h, "crew", map[string]any{
		"name": "oc", "harness": harness.OpenCodeName, "tools": "all",
	})
	assert.Equal(t, http.StatusUnprocessableEntity, code, raw)
	assert.Contains(t, raw, "tclaude-layer")
}
