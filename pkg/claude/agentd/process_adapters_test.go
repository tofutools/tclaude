package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	processexec "github.com/tofutools/tclaude/pkg/claude/process/exec"
	"github.com/tofutools/tclaude/pkg/claude/process/plan"
)

func TestProcessSpawnParams_ExplicitFalseBlocksGlobalTrue(t *testing.T) {
	setupTestDB(t)
	off := false
	on := true
	explicit := &db.SpawnProfile{
		Name: "process-off", Harness: "codex", AutoReview: &off, TrustDir: &off,
	}
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{
		Name: "global-on", Harness: "codex", AutoReview: &on, TrustDir: &on,
	})
	require.NoError(t, err)
	require.NoError(t, db.SetDashboardPref(dashboardDefaultProfilePrefKey, "global-on"))

	p, err := processSpawnParams(explicit, processexec.Request{
		Command: plan.Command{ID: "cmd_0123456789abcdef01234567"},
		Input:   processexec.Input{RunID: "run", NodeID: "node"},
	})
	require.NoError(t, err)
	require.True(t, p.AutoReviewSet)
	require.True(t, p.TrustDirSet)
	require.Nil(t, applyDefaultProfile(nil, &p))
	assert.False(t, p.AutoReview)
	assert.False(t, p.TrustDir)
}
