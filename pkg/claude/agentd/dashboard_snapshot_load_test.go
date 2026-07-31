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

func TestRunSnapshotNamedLoadsReportsQueueAndRun(t *testing.T) {
	phases := runSnapshotNamedLoads(snapshotNamedLoad{name: "load", run: func() {}})
	if assert.Len(t, phases, 1) && assert.Len(t, phases[0].Children, 2) {
		assert.Equal(t, "queue", phases[0].Children[0].Name)
		assert.Equal(t, "run", phases[0].Children[1].Name)
	}
}

func TestRunSnapshotNamedLoadsIncludesWaitForLaterWorkerWave(t *testing.T) {
	started := make(chan struct{}, snapshotLoadConcurrency)
	release := make(chan struct{})
	loads := make([]snapshotNamedLoad, snapshotLoadConcurrency+1)
	for i := range loads {
		loads[i] = snapshotNamedLoad{name: "load", run: func() {
			if i < snapshotLoadConcurrency {
				started <- struct{}{}
				<-release
			}
		}}
	}
	phasesCh := make(chan []perfPhase, 1)
	go func() { phasesCh <- runSnapshotNamedLoads(loads...) }()
	for range snapshotLoadConcurrency {
		<-started
	}
	time.Sleep(5 * time.Millisecond)
	close(release)
	phases := <-phasesCh
	if assert.Len(t, phases, snapshotLoadConcurrency+1) && assert.Len(t, phases[snapshotLoadConcurrency].Children, 2) {
		assert.GreaterOrEqual(t, phases[snapshotLoadConcurrency].Children[0].Ms, float64(4),
			"job after the worker limit must report its wait behind the first wave")
	}
}

func BenchmarkSnapshotPreloadFanoutOverhead(b *testing.B) {
	batchSizes := []int{8, 2, 5, 4}
	for b.Loop() {
		for _, size := range batchSizes {
			loads := make([]snapshotNamedLoad, size)
			for i := range loads {
				loads[i] = snapshotNamedLoad{name: "noop", run: func() {}}
			}
			runSnapshotNamedLoads(loads...)
		}
	}
}
