package agentd

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunPowerOnBoundsConcurrentResumes(t *testing.T) {
	targets := make([]string, powerOnConcurrency*3)
	for i := range targets {
		targets[i] = fmt.Sprintf("bulk-resume-%02d", i)
	}

	// This test measures concurrency, not liveness: the fake resume below never
	// launches anything, so the real online check would burn the whole grace per
	// target and report every one failed.
	previousConfirm := confirmConvOnline
	confirmConvOnline = func(string, time.Duration) bool { return true }
	t.Cleanup(func() { confirmConvOnline = previousConfirm })

	entered := make(chan struct{}, len(targets))
	release := make(chan struct{})
	var mu sync.Mutex
	active := 0
	maxActive := 0
	resume := func(convID string) memberOpResult {
		mu.Lock()
		active++
		maxActive = max(maxActive, active)
		mu.Unlock()
		entered <- struct{}{}
		<-release
		mu.Lock()
		active--
		mu.Unlock()
		return memberOpResult{ConvID: convID, Action: "resumed"}
	}

	done := make(chan []powerAgentOutcome, 1)
	go func() { done <- runPowerOnWithResume(targets, resume) }()
	for range powerOnConcurrency {
		<-entered
	}
	close(release)
	outcomes := <-done

	require.Len(t, outcomes, len(targets))
	assert.Equal(t, powerOnConcurrency, maxActive)
	for i, outcome := range outcomes {
		assert.Equal(t, targets[i], outcome.ConvID)
		assert.Equal(t, powerOnResumed, outcome.Outcome)
	}
}
