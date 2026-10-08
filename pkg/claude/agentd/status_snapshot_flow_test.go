package agentd_test

import (
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestSharedStatusSnapshotConcurrentConsumers(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	fh := newFedHarness(t)
	f := fh.f
	const conv = "status-cache-agent"
	f.HaveConvWithTitle(conv, "shared agent")
	f.HaveGroup("team")
	f.HaveMember("team", conv)
	f.HaveAliveSession(conv, "status-cache-session", "tclaude-status-cache", f.TestCwd("work"))
	_, err := config.Update(func(c *config.Config, e error) error {
		if e != nil {
			return e
		}
		c.StatusSnapshot = &config.StatusSnapshotConfig{FreshnessMS: 60000}
		return nil
	})
	require.NoError(t, err)
	g, err := db.GetAgentGroupByName("team")
	require.NoError(t, err)
	require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: fh.peer.id.ID(), Slug: agentd.PermAgentsStatusRead, Scope: db.FederationGroupScope(g.ID)}))
	agentd.ResetStatusSnapshotForTest()
	var gathers atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	restore := agentd.SetStatusGatherHookForTest(func() {
		if gathers.Add(1) == 1 {
			close(entered)
		}
		<-release
	})
	defer restore()
	dash := agentd.BuildDashboardHandlerForTest()
	const n = 20
	var start sync.WaitGroup
	start.Add(n)
	results := make(chan int, n)
	for i := 0; i < n; i++ {
		go func(kind int) {
			start.Done()
			start.Wait()
			switch kind % 4 {
			case 0:
				r := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil))
				results <- r.Code
			case 1:
				r := fedHuman(t, f, http.MethodGet, "/v1/peers", nil)
				results <- r.Code
			case 2:
				r := fedHuman(t, f, http.MethodGet, "/v1/groups/team/context", nil)
				results <- r.Code
			default:
				c, e := agentd.FederationCatalogForStatusTest(fh.peer.id.ID())
				if e != nil || c == nil {
					results <- 500
				} else {
					results <- 200
				}
			}
		}(i)
	}
	<-entered
	close(release)
	for i := 0; i < n; i++ {
		require.Equal(t, 200, <-results)
	}
	require.EqualValues(t, 1, gathers.Load(), "dashboard, CLI, context tools and peer catalog share one gather")
	r := fedHuman(t, f, http.MethodGet, "/v1/peers", nil)
	require.Equal(t, 200, r.Code)
	require.EqualValues(t, 1, gathers.Load(), "warm reads reuse the common snapshot")
}
