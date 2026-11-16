package agentd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// The lineage guard reads the parent's posture from its current launch. Right
// after a resume, the stopped launch's row is marked exited after the new
// launch's row is written; were the guard to read the last updated row, a
// parent whose sandbox the operator tightened while it was offline would
// delegate with the looser posture it no longer has (fails open).
func TestSpawnLineageParentSandbox_ReadsTheCurrentLaunchRightAfterAResume(t *testing.T) {
	resetTestDB(t)
	const conv = "conv-lineage-resume"
	started := time.Now().Add(-time.Minute)
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "spwn-old", TmuxSession: "spwn-old", ConvID: conv, Cwd: t.TempDir(), Status: "running",
		Harness: "claude", HarnessBuiltinMode: "off", SandboxImplementation: string(sandboxpolicy.ImplementationOff),
		CreatedAt: started, UpdatedAt: started,
	}))
	old, err := db.LoadSession("spwn-old")
	require.NoError(t, err)

	relaunched := time.Now().Add(-30 * time.Second)
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "rsme-new", TmuxSession: "rsme-new", ConvID: conv, Cwd: t.TempDir(), Status: "running",
		Harness: "claude", HarnessBuiltinMode: "default", SandboxImplementation: string(sandboxpolicy.ImplementationHarnessBuiltin),
		CreatedAt: relaunched, UpdatedAt: relaunched,
	}))
	marked, err := db.MarkSessionExitedIfUnchanged("spwn-old", old.Status, old.UpdatedAt, "")
	require.NoError(t, err)
	require.True(t, marked, "the old launch is marked exited after the new launch's row")

	parent, err := spawnLineageParentSandbox(conv)
	require.NoError(t, err)
	require.Equal(t, sandboxpolicy.ImplementationHarnessBuiltin, parent.Implementation,
		"the parent's posture is its current launch's, not the dead launch's unsandboxed one")
	require.Equal(t, "default", parent.HarnessBuiltinMode)
}
