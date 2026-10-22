package agentd

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func resetGitHubBackoffForTest(t *testing.T) {
	t.Helper()
	reset := func() {
		ghBackoff.Lock()
		ghBackoff.until, ghBackoff.kind, ghBackoff.streak = time.Time{}, "", 0
		ghBackoff.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

func TestGitHubBackoff(t *testing.T) {
	resetGitHubBackoffForTest(t)
	now := time.Now()
	secondary := errors.New("gh: You have exceeded a secondary rate limit. Please wait a few minutes before you try again.")

	// Other failures never pause.
	noteGitHubResultAt(errors.New("gh: HTTP 502"), now)
	remaining, _ := gitHubBackoffRemaining(now)
	assert.Zero(t, remaining)

	noteGitHubResultAt(secondary, now)
	remaining, kind := gitHubBackoffRemaining(now)
	assert.Equal(t, ghBackoffMin, remaining)
	assert.Equal(t, ghErrSecondaryRateLimit, kind)

	// While paused, calls that reach GitHub are refused; local ones are not.
	err := gitHubBackoffGate([]string{"api", "graphql"})
	assert.ErrorIs(t, err, errGitHubBackoff)
	assert.Equal(t, ghErrBackoff, classifyGHError(err))
	assert.NoError(t, gitHubBackoffGate([]string{"auth", "token"}))

	// A call already in flight when the pause began does not extend it.
	noteGitHubResultAt(secondary, now.Add(time.Minute))
	remaining, _ = gitHubBackoffRemaining(now)
	assert.Equal(t, ghBackoffMin, remaining)

	// Refused again right after the pause: it doubles, up to the cap.
	after := now.Add(ghBackoffMin)
	noteGitHubResultAt(errors.New("gh: API rate limit exceeded for user ID 1"), after)
	remaining, kind = gitHubBackoffRemaining(after)
	assert.Equal(t, 2*ghBackoffMin, remaining)
	assert.Equal(t, ghErrRateLimit, kind)
	for i := 0; i < 5; i++ {
		after = after.Add(ghBackoffMax)
		noteGitHubResultAt(secondary, after)
	}
	remaining, _ = gitHubBackoffRemaining(after)
	assert.Equal(t, ghBackoffMax, remaining)

	// A success resets the doubling.
	after = after.Add(ghBackoffMax)
	noteGitHubResultAt(nil, after)
	noteGitHubResultAt(secondary, after)
	remaining, _ = gitHubBackoffRemaining(after)
	assert.Equal(t, ghBackoffMin, remaining)
}

func TestRunInDirWithTimeoutHonoursGitHubBackoff(t *testing.T) {
	resetGitHubBackoffForTest(t)
	noteGitHubResultAt(errors.New("gh: You have exceeded a secondary rate limit"), time.Now())
	_, err := runInDirWithTimeout("", time.Second, "gh", "api", "user")
	assert.ErrorIs(t, err, errGitHubBackoff, "gh is not even started while paused")
	_, err = liveTrackedPRGraphQL([]string{"api", "graphql"})
	assert.ErrorIs(t, err, errGitHubBackoff)
}
