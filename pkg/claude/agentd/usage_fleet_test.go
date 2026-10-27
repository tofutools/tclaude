package agentd

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/usagerules"
)

// A peer's fleet hard stop stops each covered agent once when it fires; the
// renewals that follow stop it again only if it runs again.
func TestUsageFleetHardStopStopsOnceThenStands(t *testing.T) {
	f := newUsageRulesFixture(t)
	resetUsageFleetControls()
	t.Cleanup(resetUsageFleetControls)
	// Production lists sessions once per request: a stop does not change the
	// listing already taken.
	usageRulesLiveSessions = func() (map[string]struct{}, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := map[string]struct{}{}
		for k := range f.alive {
			out[k] = struct{}{}
		}
		return out, nil
	}
	f.agent("c1", 1)
	const peer = "inst_testpeer0001"
	require.NoError(t, db.TrustFederationPeer(db.FederationPeer{InstanceID: peer, PubKey: make([]byte, 32), Label: "lead"}))
	require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: peer, Slug: PermUsageRulesEnforce}))
	req := usageFleetEnforceRequest{RuleKey: "fleet:1", RuleName: "cap", Fleet: "all nodes", PeriodKey: "2026-10|b=100",
		Metric: usagerules.MetricTotalUSD, Period: usagerules.PeriodMonth, Pct: 130, Stop: true, LeaseSeconds: 180,
		Events: []usageFleetEvent{{Key: "hard_stop@120", Action: usagerules.ActionHardStop}}}

	reply, err := applyUsageFleetEnforce("lead", peer, true, req, time.Now())
	require.NoError(t, err)
	assert.Equal(t, 1, reply.Stopped)
	assert.Equal(t, []string{"c1"}, f.stopped, "stopped once, not again by the standing stop")
	for _, s := range f.operatorSubjects() {
		assert.False(t, strings.Contains(s, "again"), s)
	}

	// The agent is resumed; the next renewal stops it again.
	f.mu.Lock()
	f.alive["tmux-c1"] = struct{}{}
	f.mu.Unlock()
	req.Events = nil
	_, err = applyUsageFleetEnforce("lead", peer, true, req, time.Now())
	require.NoError(t, err)
	assert.Equal(t, []string{"c1", "c1"}, f.stopped)
}
