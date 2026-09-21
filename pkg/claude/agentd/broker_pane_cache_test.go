package agentd

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseBrokerPaneList(t *testing.T) {
	out := "spwn-a|%1|75501|0|gen-a\n" +
		"spwn-dead|%2|100|1|gen-dead\n" +
		"multi|%3|200|0|gen-m\n" +
		"multi|%4|201|0|gen-m\n" +
		"garbage\n" +
		"|%5|1|0|x\n"
	table := parseBrokerPaneList(out)
	require.Len(t, table, 3)
	assert.Equal(t, brokerPaneEntry{paneID: "%1", panePID: 75501, generation: "gen-a", panes: 1}, table["spwn-a"])
	assert.True(t, table["spwn-dead"].dead)
	assert.Equal(t, 2, table["multi"].panes, "a second pane for the same session is counted, not overwritten")
	assert.Equal(t, 200, table["multi"].panePID)
}

func feedBrokerPaneCache(t *testing.T, table map[string]brokerPaneEntry) *atomic.Int32 {
	t.Helper()
	prev := brokerPaneListFn
	t.Cleanup(func() { brokerPaneListFn = prev; resetBrokerPaneCache() })
	resetBrokerPaneCache()
	var reads atomic.Int32
	brokerPaneListFn = func() (map[string]brokerPaneEntry, error) {
		reads.Add(1)
		return table, nil
	}
	return &reads
}

// Only a pane the proof could trust as-is is served from the cache; every
// other state is a miss so the live probe decides. This is what keeps a
// stale table safe: it can only ever confirm facts fixed for a pane's
// lifetime, never overrule a live answer about a pane in flux.
func TestCachedBrokerPane_HitAndMissConditions(t *testing.T) {
	feedBrokerPaneCache(t, map[string]brokerPaneEntry{
		"live":    {paneID: "%1", panePID: 7104, generation: "gen", panes: 1},
		"dead":    {paneID: "%2", panePID: 7105, generation: "gen", panes: 1, dead: true},
		"multi":   {paneID: "%3", panePID: 7106, generation: "gen", panes: 2},
		"nopid":   {paneID: "%4", panePID: 0, generation: "gen", panes: 1},
		"badpane": {paneID: "x", panePID: 7107, generation: "gen", panes: 1},
	})

	pane, ok := cachedBrokerPane("live", "gen")
	require.True(t, ok)
	assert.Equal(t, lifecyclePaneProbe{state: paneProbeLive, paneID: "%1", panePID: 7104, generation: "gen"}, pane)

	for _, tc := range []struct{ session, gen, why string }{
		{"live", "other-gen", "generation mismatch: the pane was relaunched, probe it"},
		{"live", "", "no expected generation: nothing to match against"},
		{"absent", "gen", "unknown session: spawned inside the TTL, probe it"},
		{"dead", "gen", "a dead pane is never confirmed from cache"},
		{"multi", "gen", "several panes are ambiguous; the probe targets the active one"},
		{"nopid", "gen", "no usable pid"},
		{"badpane", "gen", "malformed pane id"},
		{"", "gen", "empty session"},
	} {
		_, ok := cachedBrokerPane(tc.session, tc.gen)
		assert.False(t, ok, tc.why)
	}
}

// brokerPaneFacts is the seam the proof calls: cache first, probe on miss,
// and it says which answered.
func TestBrokerPaneFacts_CacheFirstProbeOnMiss(t *testing.T) {
	feedBrokerPaneCache(t, map[string]brokerPaneEntry{
		"cached": {paneID: "%1", panePID: 7104, generation: "gen", panes: 1},
	})
	prevProbe := brokerLivePaneProbe
	t.Cleanup(func() { brokerLivePaneProbe = prevProbe })
	probed := 0
	brokerLivePaneProbe = func(tmux string) (lifecyclePaneProbe, error) {
		probed++
		if tmux == "fresh" {
			return lifecyclePaneProbe{state: paneProbeLive, paneID: "%9", panePID: 9000, generation: "gen"}, nil
		}
		return lifecyclePaneProbe{state: paneProbeUnknown}, errors.New("no such session")
	}

	pane, source, err := brokerPaneFacts("cached", "gen")
	require.NoError(t, err)
	assert.Equal(t, "cache", source)
	assert.Equal(t, 7104, pane.panePID)
	assert.Zero(t, probed, "a cache hit costs no tmux round trip")

	pane, source, err = brokerPaneFacts("fresh", "gen")
	require.NoError(t, err)
	assert.Equal(t, "probe", source)
	assert.Equal(t, 9000, pane.panePID)
	assert.Equal(t, 1, probed)

	_, source, err = brokerPaneFacts("cached", "relaunched-gen")
	assert.Error(t, err, "a generation the cache does not confirm falls through to the probe, which decides")
	assert.Equal(t, "probe", source)
}

// The one tmux call the cache costs must not scale with the agent count:
// concurrent requests inside the TTL share ONE list-panes, and a failed
// read is a miss for that caller without poisoning the table.
func TestCachedBrokerPanes_SingleFlightAndFailureIsAMiss(t *testing.T) {
	prev := brokerPaneListFn
	t.Cleanup(func() { brokerPaneListFn = prev; resetBrokerPaneCache() })
	resetBrokerPaneCache()

	var reads atomic.Int32
	release := make(chan struct{})
	brokerPaneListFn = func() (map[string]brokerPaneEntry, error) {
		reads.Add(1)
		<-release
		return map[string]brokerPaneEntry{"s": {paneID: "%1", panePID: 5, generation: "g", panes: 1}}, nil
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ok := cachedBrokerPane("s", "g")
			assert.True(t, ok)
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	assert.EqualValues(t, 1, reads.Load(), "twenty concurrent proofs share one list-panes")
	_, ok := cachedBrokerPane("s", "g")
	assert.True(t, ok)
	assert.EqualValues(t, 1, reads.Load(), "inside the TTL nothing is re-read")

	// Expire and make the read fail: this caller misses, the old table is kept.
	brokerPaneCache.mu.Lock()
	brokerPaneCache.taken = time.Time{}
	brokerPaneCache.mu.Unlock()
	brokerPaneListFn = func() (map[string]brokerPaneEntry, error) { return nil, errors.New("tmux wedged") }
	_, ok = cachedBrokerPane("s", "g")
	assert.False(t, ok, "a failed read is a miss, so the probe decides")
	brokerPaneCache.mu.Lock()
	assert.NotNil(t, brokerPaneCache.table, "the previous table survives a failed refresh")
	brokerPaneCache.mu.Unlock()
}
