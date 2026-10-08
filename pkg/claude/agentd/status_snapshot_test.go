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
