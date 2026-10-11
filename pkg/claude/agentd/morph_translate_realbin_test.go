package agentd_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// Opt-in smoke against real harness binaries (never in CI):
//
//	TCLAUDE_MORPH_REALBIN=1 scripts/test.sh ./pkg/claude/agentd/ -run TestMorphTranslateRealBinary -v
//
// It exercises only credential-free native install paths: OpenCode's own
// `import`/`export` validate the translated session against the real schema.
// A resume that prompts each real CLI spends model calls on the operator's
// accounts and is deliberately not part of this test.
func TestMorphTranslateRealBinary(t *testing.T) {
	if os.Getenv("TCLAUDE_MORPH_REALBIN") != "1" {
		t.Skip("set TCLAUDE_MORPH_REALBIN=1 to run against installed harness binaries")
	}
	t.Run("opencode-import-export", func(t *testing.T) {
		if _, err := exec.LookPath("opencode"); err != nil {
			t.Skip("opencode is not installed")
		}
		f := newFlow(t)
		// OpenCode keeps its store under XDG dirs; keep it in the test HOME.
		for _, k := range []string{"XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME", "XDG_CONFIG_HOME"} {
			t.Setenv(k, "")
		}
		const source = "0f0e0d0c-0b0a-4908-8706-050403020102"
		cwd := testutil.CanonicalTempDir(t)
		f.HaveAliveSession(source, "realbin-src", "realbin-pane", cwd)
		cc := f.World.CCs.GetByLabel("realbin-src")
		require.NoError(t, cc.AppendTurn(map[string]any{"type": "user", "uuid": "u1", "cwd": cwd, "timestamp": "2026-10-01T10:00:00.000Z", "message": map[string]any{"role": "user", "content": morphSecret}}))
		require.NoError(t, cc.AppendTurn(map[string]any{"type": "assistant", "uuid": "a1", "parentUuid": "u1", "cwd": cwd, "timestamp": "2026-10-01T10:00:01.000Z", "message": map[string]any{"id": "m1", "role": "assistant", "model": "claude-opus-5-5", "content": []any{
			map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": "ls"}}}}}))
		require.NoError(t, cc.AppendTurn(map[string]any{"type": "user", "uuid": "u2", "parentUuid": "a1", "cwd": cwd, "timestamp": "2026-10-01T10:00:02.000Z", "message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "README.md\nmain.go"},
			map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="}}}}}))
		require.NoError(t, cc.AppendTurn(map[string]any{"type": "assistant", "uuid": "a2", "parentUuid": "u2", "cwd": cwd, "timestamp": "2026-10-01T10:00:03.000Z", "message": map[string]any{"id": "m2", "role": "assistant", "model": "claude-opus-5-5", "content": []any{map[string]any{"type": "text", "text": "There are two files."}}}}))
		convops.ScanAndUpsertFile(cc.JsonlPath)

		claude, opencode := mustGetHarness(t, harness.DefaultName), mustGetHarness(t, harness.OpenCodeName)
		res, err := harness.TranslateHistory(claude, opencode, source, cwd, harness.TranslateOptions{})
		require.NoError(t, err, "the real `opencode import` accepts the translated session")
		t.Cleanup(res.Cleanup)
		text := historyText(t, opencode, res.ConvID, cwd)
		for _, want := range []string{morphSecret, "⟦tool call · Bash⟧", "README.md\nmain.go", "There are two files.", "carried over from claude"} {
			assert.Contains(t, text, want, "the real `opencode export` returns the carried turn")
		}
		assert.Contains(t, text, "image/png), 70 bytes]", "the image survives the real OpenCode store")

		// And back out of OpenCode: the session the real binary now owns
		// translates to another harness.
		codex := mustGetHarness(t, harness.CodexName)
		t.Setenv("CODEX_HOME", "")
		back, err := harness.TranslateHistory(opencode, codex, res.ConvID, cwd, harness.TranslateOptions{})
		require.NoError(t, err)
		t.Cleanup(back.Cleanup)
		assert.True(t, strings.Contains(historyText(t, codex, back.ConvID, cwd), morphSecret))
	})
}
