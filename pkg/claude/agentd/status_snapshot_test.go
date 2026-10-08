package agentd

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStatusSnapshotSingleFlightAndWindow(t *testing.T) {
	var c statusSnapshotCache
	var count atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	gather := func() *statusSnapshot {
		if count.Add(1) == 1 {
			close(entered)
		}
		<-release
		return &statusSnapshot{observedAt: time.Now()}
	}
	const consumers = 20
	results := make(chan *statusSnapshot, consumers)
	var start sync.WaitGroup
	start.Add(consumers)
	for i := 0; i < consumers; i++ {
		go func() { start.Done(); start.Wait(); results <- c.get("data-a", time.Minute, gather) }()
	}
	<-entered
	close(release)
	first := <-results
	for i := 1; i < consumers; i++ {
		require.Same(t, first, <-results)
	}
	require.EqualValues(t, 1, count.Load())
	require.Same(t, first, c.get("data-a", time.Minute, gather))
	require.EqualValues(t, 1, count.Load())
	next := c.get("data-b", time.Minute, gather)
	require.NotSame(t, first, next)
	require.EqualValues(t, 2, count.Load())
	c.cachedAt = time.Now().Add(-2 * time.Minute) // test-owned cache, no concurrent readers
	require.NotSame(t, next, c.get("data-b", time.Minute, gather))
	require.EqualValues(t, 3, count.Load())
}

func TestStatusSnapshotKnownWritesInvalidateWarmAndInflight(t *testing.T) {
	var version atomic.Uint64
	c := statusSnapshotCache{revision: version.Load}
	var gathers atomic.Int32
	gather := func() *statusSnapshot { gathers.Add(1); return &statusSnapshot{observedAt: time.Now()} }
	first := c.get("data", time.Hour, gather)
	require.Same(t, first, c.get("data", time.Hour, gather))
	version.Add(1)
	second := c.get("data", time.Hour, gather)
	require.NotSame(t, first, second)
	require.Same(t, second, c.get("data", time.Hour, gather))
	require.EqualValues(t, 2, gathers.Load())
	// A write during gathering must not be absorbed into the cached generation.
	version.Add(1)
	c.get("data", time.Hour, func() *statusSnapshot { version.Add(1); return gather() })
	c.get("data", time.Hour, gather)
	require.EqualValues(t, 4, gathers.Load())
}

func TestStatusSnapshotReaderAfterInflightWriteGetsNewGather(t *testing.T) {
	var version atomic.Uint64
	c := statusSnapshotCache{revision: version.Load}
	var count atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	gather := func() *statusSnapshot {
		n := count.Add(1)
		if n == 1 {
			close(entered)
			<-release
		}
		return &statusSnapshot{observedAt: time.Now()}
	}
	first, second := make(chan *statusSnapshot, 1), make(chan *statusSnapshot, 1)
	go func() { first <- c.get("data", time.Hour, gather) }()
	<-entered
	version.Add(1)
	go func() { second <- c.get("data", time.Hour, gather) }()
	close(release)
	a, b := <-first, <-second
	require.NotSame(t, a, b, "a reader after a known write cannot reuse the older gather")
	require.EqualValues(t, 2, count.Load())
}
