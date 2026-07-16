package agentd

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRunSnapshotLoadsBoundsConcurrency(t *testing.T) {
	var active atomic.Int32
	var maximum atomic.Int32
	var completed atomic.Int32
	loads := make([]func(), snapshotLoadConcurrency*20)
	for i := range loads {
		loads[i] = func() {
			current := active.Add(1)
			for observed := maximum.Load(); current > observed && !maximum.CompareAndSwap(observed, current); observed = maximum.Load() {
			}
			time.Sleep(time.Millisecond)
			active.Add(-1)
			completed.Add(1)
		}
	}
	runSnapshotLoads(loads...)
	assert.Equal(t, int32(len(loads)), completed.Load())
	assert.LessOrEqual(t, maximum.Load(), int32(snapshotLoadConcurrency))
}
