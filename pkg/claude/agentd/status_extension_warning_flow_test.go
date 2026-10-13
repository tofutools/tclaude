package agentd_test

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestPiStatusExtensionMissingWarningClearsAfterHook(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	const conv = "019ec013-1111-2222-3333-444444444444"
	cwd := filepath.Join(f.World.HomeDir, "project")
	sim, err := testharness.NewPiSim(t, testharness.PiAgentDirFor(f.World.HomeDir), cwd, "pi --session-id "+conv)
	require.NoError(t, err)
	require.NoError(t, sim.Start()) // This launch never loads/fires the extension.
	f.World.Tmux.Register("silent-pi", cwd, sim)
	created := time.Now().Add(-31 * time.Second)
	require.NoError(t, db.SaveSession(&db.SessionRow{ID: "silent-pi", TmuxSession: "silent-pi", ConvID: conv, Cwd: cwd, Harness: harness.PiName, Status: "running", CreatedAt: created, UpdatedAt: created}))
	f.HaveMember("crew", conv)
	reaper := agentd.NewSessionReaperForTest(0, func(string, string) {})
	require.Zero(t, reaper.TickAt(created.Add(31*time.Second)))
	warning := func() string {
		rec := testharness.Serve(agentd.BuildDashboardHandlerForTest(), testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil))
		require.Equal(t, http.StatusOK, rec.Code)
		var snap struct {
			Groups []struct {
				Members []struct {
					ConvID string `json:"conv_id"`
					State  struct {
						Warning string `json:"status_warning"`
					} `json:"state"`
				} `json:"members"`
			} `json:"groups"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snap))
		for _, g := range snap.Groups {
			for _, m := range g.Members {
				if m.ConvID == conv {
					return m.State.Warning
				}
			}
		}
		t.Fatal("pi member missing from snapshot")
		return ""
	}
	require.Contains(t, warning(), "pi reported no status within 30s")
	require.NoError(t, db.UpdateSessionLastHook("silent-pi", created.Add(32*time.Second)))
	require.Zero(t, reaper.TickAt(created.Add(33*time.Second)))
	require.Empty(t, warning(), "a late valid hook clears the diagnosis without changing launch state")
}
