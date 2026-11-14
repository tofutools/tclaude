package agentd

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// useHarnessCatalogCacheForTest enables the cache with a fake builder and an
// empty slot, restoring the previous state when the test ends.
func useHarnessCatalogCacheForTest(t *testing.T, build func() []dashboardHarness) {
	t.Helper()
	c := &harnessCatalogCache
	c.Lock()
	prevEnabled, prevValue, prevAt, prevBuild := c.enabled, c.value, c.at, c.build
	c.enabled, c.value, c.at, c.refreshing, c.build = true, nil, time.Time{}, false, build
	c.Unlock()
	t.Cleanup(func() {
		require.Eventually(t, func() bool {
			c.Lock()
			defer c.Unlock()
			return !c.refreshing
		}, 5*time.Second, 5*time.Millisecond, "background harness catalog refresh never finished")
		c.Lock()
		defer c.Unlock()
		c.enabled, c.value, c.at, c.build = prevEnabled, prevValue, prevAt, prevBuild
	})
}

func ageHarnessCatalogForTest() {
	harnessCatalogCache.Lock()
	defer harnessCatalogCache.Unlock()
	harnessCatalogCache.at = time.Now().Add(-2 * harnessCatalogRefreshAfter)
}

func catalogNamed(name string) []dashboardHarness {
	return []dashboardHarness{{Name: name}}
}

// A stale catalog is served as-is while the rebuild runs in the background:
// a slow build (pi's `--list-models`) never stalls the read that noticed it.
func TestHarnessCatalogStaleReadDoesNotWaitForRebuild(t *testing.T) {
	var builds atomic.Int32
	release := make(chan struct{})
	useHarnessCatalogCacheForTest(t, func() []dashboardHarness {
		if builds.Add(1) == 1 {
			return catalogNamed("first")
		}
		<-release
		return catalogNamed("second")
	})

	require.Equal(t, "first", cachedHarnessCatalog()[0].Name, "first read builds inline")
	ageHarnessCatalogForTest()

	done := make(chan []dashboardHarness, 1)
	go func() { done <- cachedHarnessCatalog() }()
	select {
	case got := <-done:
		require.Equal(t, "first", got[0].Name, "stale read serves the cached catalog")
	case <-time.After(5 * time.Second):
		t.Fatal("stale read blocked on the background rebuild")
	}
	// Further reads while the rebuild is in flight do not start another.
	require.Equal(t, "first", cachedHarnessCatalog()[0].Name)
	close(release)

	require.Eventually(t, func() bool {
		return cachedHarnessCatalog()[0].Name == "second"
	}, 5*time.Second, 5*time.Millisecond, "rebuilt catalog was never published")
	require.Equal(t, int32(2), builds.Load(), "one inline build plus one background rebuild")
}

// A fresh catalog is served without rebuilding.
func TestHarnessCatalogFreshReadDoesNotRebuild(t *testing.T) {
	var builds atomic.Int32
	useHarnessCatalogCacheForTest(t, func() []dashboardHarness {
		builds.Add(1)
		return catalogNamed("only")
	})
	for range 5 {
		require.Equal(t, "only", cachedHarnessCatalog()[0].Name)
	}
	require.Equal(t, int32(1), builds.Load())
}

// A rebuild that panics keeps the previous catalog and does not take the
// daemon down.
func TestHarnessCatalogRefreshPanicKeepsPreviousCatalog(t *testing.T) {
	var builds atomic.Int32
	useHarnessCatalogCacheForTest(t, func() []dashboardHarness {
		if builds.Add(1) == 1 {
			return catalogNamed("good")
		}
		panic("descriptor exploded")
	})
	require.Equal(t, "good", cachedHarnessCatalog()[0].Name)
	ageHarnessCatalogForTest()
	require.Equal(t, "good", cachedHarnessCatalog()[0].Name)
	require.Eventually(t, func() bool {
		harnessCatalogCache.Lock()
		defer harnessCatalogCache.Unlock()
		return !harnessCatalogCache.refreshing
	}, 5*time.Second, 5*time.Millisecond)
	require.Equal(t, int32(2), builds.Load())
	require.Equal(t, "good", cachedHarnessCatalog()[0].Name)
}
