package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// The launch echo's disclosure notes, in order. A default profile pinned to
// another harness is skipped field by field, and each skip is disclosed; the
// echo lists those notes in one fixed order, which is what an operator reads
// top to bottom. This pins that order while the echo's assembly is reshaped.
func TestSpawnPreview_DisclosureNotesKeepTheirOrder(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	on := true
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{
		Name: "codex-kit", Harness: harness.CodexName,
		Model: "gpt-5.2", Effort: "high", Sandbox: "workspace-write", Approval: "never",
		ToolGovernance: "full", AskUserQuestionTimeout: "5m", AutoCompactWindow: "200k",
		ContextWindowMax: 100_000, CopilotAPI: &on, CodexAppServer: &on, FastMode: &on,
		AutoReview: &on, TrustDir: &on, AutoMemory: &on, PeerMessaging: &on, SSHWorkaround: &on,
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, setGroupProfile(t, f, "crew", "codex-kit").Code)
	h := agentd.BuildDashboardHandlerForTest()

	code, got, raw := spawnPreview(t, h, "crew", map[string]any{"name": "w", "harness": harness.DefaultName})
	require.Equal(t, http.StatusOK, code, raw)
	skip := func(field string) string {
		return `group default profile "codex-kit" ` + field + " ignored (not valid for claude)"
	}
	pinned := func(field string) string {
		return `group default profile "codex-kit" ` + field + " ignored (profile targets codex, launch is claude)"
	}
	assert.Equal(t, []string{
		skip("sandbox"),
		skip("approval"),
		skip("tools"),
		pinned("context_window_max"),
		skip("copilot_api"),
		skip("codex_app_server"),
		skip("fast_mode"),
		skip("auto_review"),
		skip("trust_dir"),
		skip("auto_memory"),
		skip("peer_messaging"),
		skip("ssh_workaround"),
		"SSH workaround disabled because this launch does not use the Codex managed sandbox or tclaude's sandbox",
	}, got.Notes)
	// Model and effort carry their note on the echoed field instead.
	assert.Equal(t, pinned("model"), got.Model.Note)
	assert.Equal(t, pinned("effort"), got.Effort.Note)
}
