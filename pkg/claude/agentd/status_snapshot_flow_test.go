package agentd_test

import (
	"database/sql"
	"net/http"
	"os"
	"os/exec"
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

func TestSharedStatusSnapshotHistoricalContext(t *testing.T) {
	f := newFlow(t)
	const conv = "historical-plain-status"
	f.HaveConvWithTitle(conv, "Historical conversation")
	f.HaveAliveSession(conv, "historical-context-session", "tclaude-historical", f.TestCwd("historical"))
	f.MarkOffline("tclaude-historical")
	f.SetSessionStatus(conv, "exited")
	require.NoError(t, db.UpdateContextSnapshot("historical-context-session", 25, 100, 20, 1000))
	agentd.ResetStatusSnapshotForTest()
	for i := 0; i < 2; i++ {
		r := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/agent/"+conv+"/context", nil)))
		require.Equal(t, 200, r.Code, r.Body.String())
		var got ctxInfo
		testharness.DecodeJSON(t, r, &got)
		require.Equal(t, "historical-context-session", got.SessionID)
		require.EqualValues(t, 100, got.TokensInput)
		require.EqualValues(t, 1000, got.ContextWindowSize)
	}
}

func TestSharedStatusSnapshotKnownWritesAreImmediatelyVisible(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	const conv = "status-write-agent"
	f.HaveConvWithTitle(conv, "worker")
	f.HaveGroup("team")
	f.HaveMember("team", conv)
	f.HaveAliveSession(conv, "status-write-session", "tmux-status-write", f.TestCwd("work"))
	dash := agentd.BuildDashboardHandlerForTest()
	read := func() *dashMember {
		m := findDashMember(fetchDashSnapshot(t, dash), "team", conv)
		require.NotNil(t, m)
		return m
	}
	require.True(t, read().Online)
	f.SetSessionStatus(conv, "awaiting_input")
	require.Equal(t, "awaiting_input", read().State.Status)
	require.NoError(t, db.UpdateSessionModel("status-write-session", "model-after-write"))
	require.Equal(t, "model-after-write", read().State.Model)
	require.NoError(t, db.UpdateContextSnapshot("status-write-session", 25, 100, 20, 1000))
	require.EqualValues(t, 100, read().State.TokensInput)
	f.MarkOffline("tmux-status-write")
	require.False(t, read().Online)
}

func TestSharedStatusSnapshotExternalProcessWrite(t *testing.T) {
	if path := os.Getenv("TCLAUDE_TEST_STATUS_WRITE_DB"); path != "" {
		d, err := sql.Open("sqlite", path)
		require.NoError(t, err)
		defer d.Close()
		_, err = d.Exec("UPDATE sessions SET status='awaiting_input' WHERE id='external-status-session'")
		require.NoError(t, err)
		return
	}
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	const conv = "external-status-agent"
	f.HaveConvWithTitle(conv, "external worker")
	f.HaveGroup("team")
	f.HaveMember("team", conv)
	f.HaveAliveSession(conv, "external-status-session", "tmux-external-status", f.TestCwd("work"))
	dash := agentd.BuildDashboardHandlerForTest()
	require.NotEqual(t, "awaiting_input", findDashMember(fetchDashSnapshot(t, dash), "team", conv).State.Status)
	cmd := exec.Command(os.Args[0], "-test.run=^TestSharedStatusSnapshotExternalProcessWrite$")
	cmd.Env = append(os.Environ(), "TCLAUDE_TEST_STATUS_WRITE_DB="+db.DBPath())
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	got := findDashMember(fetchDashSnapshot(t, dash), "team", conv)
	require.NotNil(t, got)
	require.Equal(t, "awaiting_input", got.State.Status, "a separate callback process invalidates the daemon's warm snapshot")
}
