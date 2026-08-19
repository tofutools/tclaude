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
// must never wait on the `opencode models` subprocess. Effort validation
// refuses launches and must still be authoritative, while model validation
// treats discovery as suggestions. These pin both halves by counting execs.

const fakeOpenCodeModels = "openai/gpt-5\n" +
	`  "variants": {` + "\n" +
	`    "high": {` + "\n"

// resetOpenCodeModelCacheForTest clears every scrap of cached state, including
// the refresh rate floor, so each test starts from a cold daemon.
func resetOpenCodeModelCacheForTest() {
	openCodeModelCache.Lock()
	defer openCodeModelCache.Unlock()
	openCodeModelCache.value = openCodeCatalog{}
	openCodeModelCache.lastRefreshStart = time.Time{}
}

func backgroundRefreshIdle() bool {
	return openCodeRefreshesInFlight.Load() == 0
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
		// Drain first: calls is incremented BEFORE the stub body runs, so a
		// test that returns as soon as the counter moves can leave a refresh
		// goroutine between its exec and its store. Resetting under it would
		// let that store land in the NEXT test's cache.
		require.Eventually(t, backgroundRefreshIdle, 5*time.Second, 5*time.Millisecond,
			"a refresh goroutine outlived its test")
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

func TestOpenCodeModels_ModelValidationAcceptsFreeTextWithoutCatalogRead(t *testing.T) {
	calls := stubOpenCodeModels(t, func(string) ([]byte, error) {
		return nil, assert.AnError
	})

	for _, test := range []struct {
		input string
		want  string
	}{
		{input: "  custom/testmodel  ", want: "custom/testmodel"},
		{input: "local/llama", want: "local/llama"},
		{input: "", want: ""},
	} {
		value, err := openCodeModels{}.ValidateModel(test.input)
		require.NoError(t, err)
		assert.Equal(t, test.want, value)
	}
	assert.Zero(t, calls.Load(), "model validation must not consult the suggestion catalog")

	for _, malformed := range []string{"testmodel", "/model", "provider/"} {
		_, err := openCodeModels{}.ValidateModel(malformed)
		assert.Error(t, err, "model %q cannot be represented by OpenCode's prompt API", malformed)
	}
}

// Effort validation refuses launches, so it may not run on a catalog the
// disclosure path has not loaded yet — it loads synchronously.
func TestOpenCodeModels_EffortValidationLoadsSynchronously(t *testing.T) {
	calls := stubOpenCodeModels(t, func(string) ([]byte, error) {
		return []byte(fakeOpenCodeModels), nil
	})

	value, err := openCodeModels{}.ValidateEffort("high")
	require.NoError(t, err)
	assert.Equal(t, "high", value)
	assert.Equal(t, int64(1), calls.Load(),
		"effort validation executes rather than accepting an absent catalog")

	_, err = openCodeModels{}.ValidateEffort("not-an-effort")
	assert.Error(t, err, "an unknown effort is still refused")
}

// A burst of polls arriving on a cold cache must start ONE subprocess, not one
// per poll.
//
// The rate floor is deliberately defeated here — pushed back past its interval
// while a refresh is genuinely wedged — because otherwise the floor alone would
// produce this result and the test would silently stop covering the in-flight
// bound it is named for.
func TestOpenCodeModels_ConcurrentPollsSingleFlight(t *testing.T) {
	release := make(chan struct{})
	calls := stubOpenCodeModels(t, func(string) ([]byte, error) {
		<-release
		return []byte(fakeOpenCodeModels), nil
	})

	_ = openCodeModels{}.Models() // starts the one wedged refresh
	require.Eventually(t, func() bool { return calls.Load() == 1 },
		2*time.Second, 10*time.Millisecond)

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each poll finds the floor already expired, so only the in-flight
			// bound can stop it forking.
			openCodeModelCache.Lock()
			openCodeModelCache.lastRefreshStart =
				time.Now().Add(-2 * openCodeModelRefreshMinInterval)
			openCodeModelCache.Unlock()
			_ = openCodeModels{}.Models()
		}()
	}
	wg.Wait()
	assert.Equal(t, int64(1), calls.Load(),
		"50 polls against a refresh already in flight must not fork again")

	close(release)
	require.Eventually(t, func() bool {
		return len(openCodeModels{}.Models()) > 0
	}, 2*time.Second, 10*time.Millisecond)
}

// The flight group — not the background flag — is what bounds subprocesses on
// the effort-refusal path, where concurrent spawns need an authoritative answer.
func TestOpenCodeModels_ConcurrentEffortValidationsForkOnce(t *testing.T) {
	release := make(chan struct{})
	calls := stubOpenCodeModels(t, func(string) ([]byte, error) {
		<-release
		return []byte(fakeOpenCodeModels), nil
	})

	var wg sync.WaitGroup
	results := make([]error, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = openCodeModels{}.ValidateEffort("high")
		}()
	}
	time.Sleep(50 * time.Millisecond) // let them all reach the exec
	close(release)
	wg.Wait()

	assert.Equal(t, int64(1), calls.Load(),
		"8 concurrent spawns validating effort must fork opencode once")
	for i, err := range results {
		assert.NoError(t, err, "validator %d must still get an authoritative answer", i)
	}
}

// A slow refresh must not fork a second subprocess while it runs, and must not
// wedge the catalog once it finishes. Together with the exec's deadline and
// WaitDelay — which are what stop a refresh running forever — this is the
// liveness the design actually provides.
func TestOpenCodeModels_RecoversAfterASlowRefresh(t *testing.T) {
	release := make(chan struct{})
	calls := stubOpenCodeModels(t, func(string) ([]byte, error) {
		select {
		case <-release:
		case <-time.After(5 * time.Second): // stand-in for the exec deadline
		}
		return []byte(fakeOpenCodeModels), nil
	})

	_ = openCodeModels{}.Models()
	require.Eventually(t, func() bool { return calls.Load() == 1 },
		2*time.Second, 10*time.Millisecond)

	// While it is still running, polls past the floor must not fork again.
	openCodeModelCache.Lock()
	openCodeModelCache.lastRefreshStart =
		time.Now().Add(-2 * openCodeModelRefreshMinInterval)
	openCodeModelCache.Unlock()
	_ = openCodeModels{}.Models()
	assert.Equal(t, int64(1), calls.Load(), "one subprocess at a time")

	close(release)
	require.Eventually(t, func() bool {
		return len(openCodeModels{}.Models()) > 0
	}, 2*time.Second, 10*time.Millisecond, "the slow refresh still lands")

	// And the machinery is not wedged: age it out and a further refresh runs.
	openCodeModelCache.Lock()
	openCodeModelCache.value.at = time.Now().Add(-2 * openCodeModelCacheTTL)
	openCodeModelCache.lastRefreshStart =
		time.Now().Add(-2 * openCodeModelRefreshMinInterval)
	openCodeModelCache.Unlock()
	_ = openCodeModels{}.Models()
	require.Eventually(t, func() bool { return calls.Load() == 2 },
		2*time.Second, 10*time.Millisecond,
		"a completed slow refresh must not block later ones")
}

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

	require.NoError(t, func() error { _, err := openCodeModels{}.ValidateEffort("high"); return err }())
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
