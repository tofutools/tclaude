package session

import (
	"database/sql"
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/common"
)

// A tclaude-layer launch creates its private attachment root and its session
// row before tmux runs. When tmux then refuses the session, both belong to a
// launch that never committed and must be undone. This drives the real runNew
// failure path, which is what launchUndo unwinds.
func TestRunNewTclaudeLayerFailureUndoesRowAndPrivateAttachmentRoot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	oldRoot := resourceCgroupRoot
	resourceCgroupRoot = t.TempDir()
	t.Cleanup(func() { resourceCgroupRoot = oldRoot })
	t.Setenv(ResourceDelegationDirEnv, "")
	t.Setenv("TMUX", "")
	prevCheck := ClaudeAncestorCheck
	ClaudeAncestorCheck = func() bool { return false }
	t.Cleanup(func() { ClaudeAncestorCheck = prevCheck })
	stubTmuxOnPath(t)
	stubTclaudeLayerTooling(t)
	rec := &paneScriptCapturingTmux{launchRecordingTmux: &launchRecordingTmux{failNewSession: true}}
	swapTmux(t, rec)

	const label = "spwn-layerundo"
	privateRoot := common.SpawnAttachmentsPrivateDir(label)
	err := runNew(&NewParams{
		Label:       label,
		Dir:         t.TempDir(),
		Detached:    true,
		SandboxImpl: string(sandboxpolicy.ImplementationTclaudeLayer),
	})
	require.Error(t, err, "the fake tmux refuses new-session")
	require.Contains(t, rec.script, privateRoot,
		"the launch must reach tmux with its private attachment root bound")

	row, lerr := db.LoadSession(label)
	if lerr != nil {
		require.True(t, errors.Is(lerr, sql.ErrNoRows), "unexpected LoadSession error: %v", lerr)
	}
	assert.Nil(t, row, "the failed launch must not leave its session row behind")
	_, statErr := os.Stat(privateRoot)
	assert.True(t, os.IsNotExist(statErr),
		"the failed launch must remove its private attachment root, stat err = %v", statErr)
}
