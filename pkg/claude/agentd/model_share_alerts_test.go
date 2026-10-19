package agentd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestShareAlertsFollowLevelsAndEnds(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	ends := now.Add(time.Hour)
	share := modelShare{Peer: "inst_desk", PeerName: "desk", Gateway: "team-key", Scope: "http_proxy=team-key", MaxUSD: 20, SpentUSD: 17, Since: now.Add(-time.Hour), ExpiresAt: &ends}
	var st shareAlerts
	require.Empty(t, st.observe("given", []modelShare{share}, nil, now), "the first sight only records where a share stands")
	require.Empty(t, st.observe("given", []modelShare{share}, nil, now))
	share.SpentUSD = 20
	got := st.observe("given", []modelShare{share}, nil, now)
	require.Len(t, got, 1)
	require.Equal(t, "Usage share used up: desk on gateway team-key", got[0].subject)
	require.Empty(t, st.observe("given", []modelShare{share}, nil, now), "each level alerts once")
	require.Empty(t, st.observe("given", nil, nil, now), "revoked before its time: the grantor knows")

	share.SpentUSD = 1
	require.Empty(t, st.observe("given", []modelShare{share}, nil, now))
	got = st.observe("given", nil, nil, ends)
	require.Len(t, got, 1)
	require.Equal(t, "Usage share ended: desk on gateway team-key", got[0].subject)

	received := modelShare{Peer: "inst_forge", PeerName: "forge", Gateway: "team-key", MaxUSD: 10, SpentUSD: 1, Since: now}
	require.Empty(t, st.observe("received", []modelShare{received}, nil, now))
	received.SpentUSD = 8.5
	got = st.observe("received", []modelShare{received}, nil, now)
	require.Len(t, got, 1)
	require.Equal(t, "forge's usage share is 80% used", got[0].subject)
	require.Empty(t, st.observe("received", nil, map[string]bool{"inst_forge": false}, now), "a peer that did not answer keeps its shares")
	got = st.observe("received", nil, map[string]bool{"inst_forge": true}, now)
	require.Len(t, got, 1)
	require.Equal(t, "forge's usage share ended", got[0].subject)

	// One cap on every gateway shows per gateway, and alerts once.
	all := modelShare{Peer: "inst_desk", PeerName: "desk", MaxUSD: 10, SpentUSD: 1, Since: now}
	a, b := all, all
	a.Gateway, b.Gateway = "team-key", "openai-key"
	require.Empty(t, st.observe("given", []modelShare{a, b}, nil, now))
	a.SpentUSD, b.SpentUSD = 9, 9
	got = st.observe("given", []modelShare{a, b}, nil, now)
	require.Len(t, got, 1)
	require.Equal(t, "Usage share at 80%: desk on every gateway", got[0].subject)
	// Another grant becoming the binding one restarts the level quietly.
	a.Scope, a.Since, a.SpentUSD = "http_proxy=team-key", now.Add(time.Minute), 9.9
	require.Empty(t, st.observe("given", []modelShare{a, b}, nil, now))
}
