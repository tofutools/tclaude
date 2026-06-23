package agentd_test

import (
	"net/http"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Scenario: a human spawns an agent with a working directory that
// doesn't exist — a typo, or a "~/…" path the shell never expanded.
//
// Before the daemon validated the cwd, the bad path sailed straight
// into a detached `tclaude session new` subprocess that failed
// silently; the caller then waited out the full 30s conv-id poll and
// got a confusing gateway-timeout. The daemon now stat-checks the cwd
// up front and returns an immediate, actionable 400 — which the
// dashboard surfaces verbatim in the spawn modal's error line.
func TestSpawn_InvalidCwdReportsError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)
		f.HaveGroup("alpha")

		resp := f.AsHuman().SpawnWith("alpha", map[string]any{
			"name": "worker",
			"cwd":  "/no/such/directory/anywhere",
		})

		assert.Equalf(t, http.StatusBadRequest, resp.Code,
			"spawn with a bad cwd should 400; body=%s", resp.Raw)
		assert.Containsf(t, string(resp.Raw), "does not exist",
			"the error should explain the cwd is missing; got %s", resp.Raw)
	})
}

// Scenario: a human spawns an agent with "~" as the working directory.
// The daemon expands "~" to the human's home before launching, so
// dashboard inputs like "~/git/myproject" work without the caller
// having to pre-expand them.
func TestSpawn_TildeCwdExpands(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)
		f.HaveGroup("alpha")

		resp := f.AsHuman().SpawnWith("alpha", map[string]any{
			"name": "worker",
			"cwd":  "~",
		})

		require.Equalf(t, http.StatusOK, resp.Code,
			"spawn with ~ cwd should succeed; body=%s", resp.Raw)

		// The spawned session row should carry the expanded home, not "~".
		sess, err := db.LoadSession(resp.Label)
		require.NoError(t, err)
		require.NotNil(t, sess)
		assert.Equal(t, f.World.HomeDir, sess.Cwd,
			"spawned cwd should be the expanded home directory")
	})
}
