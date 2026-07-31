package harness

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The disclosure path (Models/EffortLevels, rebuilt on every 2s dashboard poll)
// must never wait on the `opencode models` subprocess. The validation path
// (ValidateModel/ValidateEffort) refuses launches and must still be
// authoritative. These pin both halves by counting execs.

const fakeOpenCodeModels = "openai/gpt-5\n" +
	`  "variants": {` + "\n" +
	`    "high": {` + "\n"

// resetOpenCodeModelCacheForTest clears every scrap of cached state, including
// the refresh rate floor, so each test starts from a cold daemon.
func resetOpenCodeModelCacheForTest() {
	openCodeModelCache.Lock()
	defer openCodeModelCache.Unlock()
	openCodeModelCache.value = openCodeCatalog{}
	openCodeModelCache.refreshing = false
	openCodeModelCache.lastRefreshStart = time.Time{}
}

// stubOpenCodeModels swaps the subprocess and clears the cache, restoring both.
func stubOpenCodeModels(t *testing.T, run func(string) ([]byte, error)) *atomic.Int64 {
	t.Helper()
	var calls atomic.Int64
	previousRun := runOpenCodeModels
	previousExe := resolveOpenCodeExecutable
	runOpenCodeModels = func(path string) ([]byte, error) {
		calls.Add(1)
		return run(path)
	}
	resolveOpenCodeExecutable = func() (string, error) { return "/fake/opencode", nil }
	resetOpenCodeModelCacheForTest()
	t.Cleanup(func() {
		runOpenCodeModels = previousRun
		resolveOpenCodeExecutable = previousExe
		resetOpenCodeModelCacheForTest()
	})
	return &calls
}

func TestOpenCodeModels_DisclosureNeverBlocksOnTheSubprocess(t *testing.T) {
	release := make(chan struct{})
	calls := stubOpenCodeModels(t, func(string) ([]byte, error) {
		<-release // a subprocess that has not returned yet
		return []byte(fakeOpenCodeModels), nil
	})

	// The poll must come back immediately even though the exec is wedged.
	done := make(chan []string, 1)
	go func() { done <- openCodeModels{}.Models() }()
	select {
	case models := <-done:
		assert.Empty(t, models,
			"nothing is known yet, and saying so beats blocking the poll")
	case <-time.After(2 * time.Second):
		t.Fatal("Models() blocked on the opencode subprocess")
	}

	close(release)
	require.Eventually(t, func() bool {
		return len(openCodeModels{}.Models()) > 0
	}, 2*time.Second, 10*time.Millisecond,
		"the background refresh populates the catalog for the next poll")
	assert.Equal(t, []string{"openai/gpt-5"}, openCodeModels{}.Models())
	assert.Positive(t, calls.Load())
}

// Validation refuses launches, so it may not run on a catalog the disclosure
// path has not loaded yet — it loads synchronously.
func TestOpenCodeModels_ValidationLoadsSynchronously(t *testing.T) {
	calls := stubOpenCodeModels(t, func(string) ([]byte, error) {
		return []byte(fakeOpenCodeModels), nil
	})

	value, err := openCodeModels{}.ValidateModel("openai/gpt-5")
	require.NoError(t, err)
	assert.Equal(t, "openai/gpt-5", value)
	assert.Equal(t, int64(1), calls.Load(),
		"validation executes rather than accepting an absent catalog")

	_, err = openCodeModels{}.ValidateModel("openai/not-a-model")
	assert.Error(t, err, "an unknown model is still refused")
}

// A burst of polls arriving on a cold cache must start ONE subprocess, not one
// per poll — the property the old mutex-across-exec provided by serializing
// everyone behind it, which is what made a slow exec stall the whole poll.
func TestOpenCodeModels_ConcurrentPollsSingleFlight(t *testing.T) {
	release := make(chan struct{})
	calls := stubOpenCodeModels(t, func(string) ([]byte, error) {
		<-release
		return []byte(fakeOpenCodeModels), nil
	})

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = openCodeModels{}.Models() }()
	}
	wg.Wait()
	close(release)

	require.Eventually(t, func() bool {
		return len(openCodeModels{}.Models()) > 0
	}, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, int64(1), calls.Load(),
		"50 concurrent polls must fork once, not 50 times")
}

// Guava's refreshAfterWrite shape: once something is known, a stale entry is
// served immediately while the refresh happens behind it. The poll never waits,
// and never regresses to empty just because the value aged out.
func TestOpenCodeModels_ServesStaleWhileRefreshing(t *testing.T) {
	release := make(chan struct{})
	var second atomic.Bool
	calls := stubOpenCodeModels(t, func(string) ([]byte, error) {
		if second.Load() {
			<-release
			return []byte("openai/gpt-6\n"), nil
		}
		return []byte(fakeOpenCodeModels), nil
	})

	require.NoError(t, func() error { _, err := openCodeModels{}.ValidateModel("openai/gpt-5"); return err }())
	require.Equal(t, []string{"openai/gpt-5"}, openCodeModels{}.Models())

	// Age the entry past its TTL and wedge the next exec.
	second.Store(true)
	openCodeModelCache.Lock()
	openCodeModelCache.value.at = time.Now().Add(-2 * openCodeModelCacheTTL)
	openCodeModelCache.Unlock()

	assert.Equal(t, []string{"openai/gpt-5"}, openCodeModels{}.Models(),
		"a stale catalog is served rather than blocking or blanking the poll")

	close(release)
	require.Eventually(t, func() bool {
		models := openCodeModels{}.Models()
		return len(models) == 1 && models[0] == "openai/gpt-6"
	}, 2*time.Second, 10*time.Millisecond, "the refresh lands for the next poll")
	assert.Equal(t, int64(2), calls.Load())
}

// A permanently failing (or permanently slow) `opencode models` must not turn
// into back-to-back forks, each starting the moment the last gave up. The
// single-flight flag bounds CONCURRENCY to one; this bounds the RATE.
func TestOpenCodeModels_RefreshRateIsFloored(t *testing.T) {
	calls := stubOpenCodeModels(t, func(string) ([]byte, error) {
		return nil, assert.AnError // never succeeds, so the catalog stays stale
	})

	// First poll starts the one allowed refresh.
	assert.Empty(t, openCodeModels{}.Models())
	require.Eventually(t, func() bool { return calls.Load() == 1 },
		2*time.Second, 10*time.Millisecond)

	// A failed refresh stamps the entry, so the TTL alone would hold it off —
	// force it stale to prove the RATE floor is what stops the next fork.
	openCodeModelCache.Lock()
	openCodeModelCache.value.at = time.Now().Add(-2 * openCodeModelCacheTTL)
	openCodeModelCache.Unlock()

	for range 100 {
		_ = openCodeModels{}.Models()
	}
	assert.Equal(t, int64(1), calls.Load(),
		"100 polls inside the floor must not start a second subprocess")

	// Past the floor, one — and only one — further refresh is allowed.
	openCodeModelCache.Lock()
	openCodeModelCache.lastRefreshStart =
		time.Now().Add(-2 * openCodeModelRefreshMinInterval)
	openCodeModelCache.Unlock()

	for range 100 {
		_ = openCodeModels{}.Models()
	}
	require.Eventually(t, func() bool { return calls.Load() == 2 },
		2*time.Second, 10*time.Millisecond)
	assert.Equal(t, int64(2), calls.Load(), "still one at a time")
}
