package agentd

import (
	"sync"
	"testing"
	"time"
)

// A drain must tolerate work launched while another drain is returning;
// a sync.WaitGroup panics on that Add ("reused before previous Wait has
// returned").
func TestWaitBackgroundToleratesConcurrentLaunches(t *testing.T) {
	var drains sync.WaitGroup
	for i := 0; i < 5000; i++ {
		goBackground(func() {})
		drains.Add(2)
		go func() { defer drains.Done(); waitBackground() }()
		go func() { defer drains.Done(); goBackground(func() {}); waitBackground() }()
	}
	drains.Wait()
	waitBackground()
}

// A drain returns only once running background work has finished.
func TestWaitBackgroundWaitsForRunningWork(t *testing.T) {
	release := make(chan struct{})
	goBackground(func() { <-release })
	drained := make(chan struct{})
	go func() { waitBackground(); close(drained) }()
	select {
	case <-drained:
		t.Fatal("drain returned while work was running")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not return after work finished")
	}
}
