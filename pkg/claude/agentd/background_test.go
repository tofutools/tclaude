package agentd

import (
	"sync"
	"testing"
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
