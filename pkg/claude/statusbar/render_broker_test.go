package statusbar

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/usageapi"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// withTempCacheDir points the pane-local caches somewhere a test can
// write. Production uses the literal /tmp on purpose — see renderCacheDir.
func withTempCacheDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := renderCacheDir
	renderCacheDir = dir
	t.Cleanup(func() { renderCacheDir = prev })
	return dir
}

// The status line and the hooks must reach the SAME verdict about whether
// the database is reachable. A launch that brokered its hooks but wrote
// its status line directly would lose the context snapshot into a
// read-only mount — silently, since the warning goes to a log under the
// hidden root — and take the pre-compact guard down with it.
func TestBrokerRenders_AgreesWithTheHookBroker(t *testing.T) {
	// No HOME → no resolvable database path → the probe cannot fire, so
	// the marker alone is under test here.
	t.Setenv("HOME", "")

	t.Run("marker set routes to the broker", func(t *testing.T) {
		t.Setenv(session.HookBrokerEnvVar, session.HookBrokerAgentd)
		assert.True(t, brokerRenders())
	})
	t.Run("no marker keeps the direct path", func(t *testing.T) {
		t.Setenv(session.HookBrokerEnvVar, "")
		assert.False(t, brokerRenders())
	})
	t.Run("an unrecognised value is not a broker instruction", func(t *testing.T) {
		t.Setenv(session.HookBrokerEnvVar, "yes")
		assert.False(t, brokerRenders(),
			"only the exact %q value routes", session.HookBrokerAgentd)
	})
	t.Run("it is the hook broker's own predicate", func(t *testing.T) {
		// Pinning the shared predicate rather than a duplicated marker
		// test: the defect this guards against is the two drifting, which
		// a pair of independent marker tests would not catch.
		for _, marker := range []string{"", session.HookBrokerAgentd, "yes"} {
			t.Setenv(session.HookBrokerEnvVar, marker)
			assert.Equal(t, session.BrokerHostWrites(), brokerRenders(),
				"marker %q: the two halves must not disagree about the wall", marker)
		}
	})
}

// The change gate is what keeps a several-times-a-second render off the
// socket. It must key on everything the writes derive from and on nothing
// that merely ticks.
func TestRenderDigest_ChangesWithEverythingRecorded(t *testing.T) {
	base := renderRequest{
		RenderConvID:    "conv-1",
		EnvPinnedWindow: "450k",
		Payload:         []byte(`{"model":{"display_name":"Opus 5"}}`),
		Git:             &GitSnapshot{Branch: "main", RepoURL: "https://x/y"},
	}
	unchanged := renderDigest(base)

	t.Run("an identical render digests identically", func(t *testing.T) {
		assert.Equal(t, unchanged, renderDigest(base),
			"an unchanged render must record nothing, or the gate is pointless")
	})

	for name, mutate := range map[string]func(*renderRequest){
		"payload": func(r *renderRequest) { r.Payload = []byte(`{"model":{"display_name":"Haiku 4.5"}}`) },
		"conv":    func(r *renderRequest) { r.RenderConvID = "conv-2" },
		"pin":     func(r *renderRequest) { r.EnvPinnedWindow = "200k" },
		"branch":  func(r *renderRequest) { r.Git = &GitSnapshot{Branch: "feature", RepoURL: "https://x/y"} },
		"no git":  func(r *renderRequest) { r.Git = nil },
		"pr state": func(r *renderRequest) {
			r.Git = &GitSnapshot{Branch: "main", RepoURL: "https://x/y", PRState: "merged"}
		},
	} {
		t.Run(name+" changes the digest", func(t *testing.T) {
			mutated := base
			mutate(&mutated)
			assert.NotEqual(t, unchanged, renderDigest(mutated),
				"a changed %s changes what would be recorded, so it must go out", name)
		})
	}

	// FetchedAt ticks every time the 15-second git cache refreshes, without
	// changing a single recorded field. Digesting it would defeat the gate
	// on a schedule.
	t.Run("a git cache refresh alone does not", func(t *testing.T) {
		refreshed := base
		refreshed.Git = &GitSnapshot{
			Branch: "main", RepoURL: "https://x/y",
			FetchedAt: time.Now().Add(time.Hour),
		}
		assert.Equal(t, unchanged, renderDigest(refreshed),
			"re-fetching identical git data records nothing new")
	})
}

// The cache is what remembers the gate's state between two short-lived
// render processes, so a round trip has to survive the process boundary.
func TestRenderCache_RoundTripsThroughThePanesTmp(t *testing.T) {
	withTempCacheDir(t)
	const sessionID = "spwn-cache"

	assert.Nil(t, loadRenderCache(sessionID), "nothing cached yet")

	want := renderCache{
		Digest:  "abc123",
		ReadsAt: time.Now().Truncate(time.Second),
		Reads:   BrokeredRenderResponse{Owned: true, PinnedWindow: 450000, SandboxOff: true},
	}
	saveRenderCache(sessionID, want)

	got := loadRenderCache(sessionID)
	require.NotNil(t, got, "the next render must see what this one cached")
	assert.Equal(t, want.Digest, got.Digest)
	assert.True(t, got.ReadsAt.Equal(want.ReadsAt))
	assert.Equal(t, want.Reads, got.Reads)
}

// Two agents must never read each other's cache. Inside the layer /tmp is
// already private per pane, but the keying is what makes that true
// anywhere else the path is reached.
func TestRenderCache_IsPerSession(t *testing.T) {
	withTempCacheDir(t)
	saveRenderCache("spwn-a", renderCache{Digest: "a-digest", ReadsAt: time.Now()})
	saveRenderCache("spwn-b", renderCache{Digest: "b-digest", ReadsAt: time.Now()})

	a, b := loadRenderCache("spwn-a"), loadRenderCache("spwn-b")
	require.NotNil(t, a)
	require.NotNil(t, b)
	assert.Equal(t, "a-digest", a.Digest)
	assert.Equal(t, "b-digest", b.Digest,
		"one agent's render state must never be read as another's")
}

// A clock that jumped backwards would otherwise make a cache look
// indefinitely fresh.
func TestRenderCache_RejectsAFutureStamp(t *testing.T) {
	withTempCacheDir(t)
	saveRenderCache("spwn-clock", renderCache{
		Digest:  "d",
		ReadsAt: time.Now().Add(time.Hour),
	})
	assert.Nil(t, loadRenderCache("spwn-clock"),
		"a cache stamped in the future is a clock change, not a fresh read")
}

// The one read that decides write authority is resolved daemon-side on
// every request and must never be answered from the cache. This pins the
// wire contract that makes that possible: the response carries a verdict,
// not a row the pane could reuse.
func TestBrokeredFacts_CarryTheDaemonsVerdictNotAStoredIdentity(t *testing.T) {
	req := renderRequest{EnvSessionID: "spwn-me", RenderConvID: "conv-me"}

	owned := factsFromBroker(req, BrokeredRenderResponse{Owned: true})
	assert.Equal(t, "spwn-me", owned.Owned)
	assert.Equal(t, "conv-me", owned.WorkspaceConv)

	refused := factsFromBroker(req, BrokeredRenderResponse{Owned: false})
	assert.Empty(t, refused.Owned,
		"a refused render must carry no write target, cached or otherwise")
	assert.Empty(t, refused.WorkspaceConv)
}

// The bar's own limits verdict and the one the writes use must agree, or
// a subscription session's cost lands in the real-cost column instead of
// the WHAT-IF one.
func TestUsageHasLimits_MatchesWhatTheBarRenders(t *testing.T) {
	assert.False(t, usageHasLimits(nil))
	assert.False(t, usageHasLimits(&usageapi.CachedUsage{}), "an empty cache renders no buckets")
	assert.True(t, usageHasLimits(&usageapi.CachedUsage{FiveHour: &usageapi.CachedBucket{Pct: 10}}))
	assert.True(t, usageHasLimits(&usageapi.CachedUsage{SevenDay: &usageapi.CachedBucket{Pct: 10}}))
	assert.False(t, usageHasLimits(&usageapi.CachedUsage{SevenDaySonnet: &usageapi.CachedBucket{Pct: 0}}),
		"a zero sonnet bucket is not rendered, so it must not count as a limit")
	assert.True(t, usageHasLimits(&usageapi.CachedUsage{SevenDaySonnet: &usageapi.CachedBucket{Pct: 3}}))
}
