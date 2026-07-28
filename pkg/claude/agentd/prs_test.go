package agentd

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGitHubPRRefFromURL(t *testing.T) {
	tests := []struct {
		name   string
		rawURL string
		repo   string
		number int
		ok     bool
	}{
		{
			name:   "canonical",
			rawURL: "https://github.com/tofutools/tclaude/pull/800",
			repo:   "tofutools/tclaude",
			number: 800,
			ok:     true,
		},
		{
			name:   "trailing path is still the same PR",
			rawURL: "https://github.com/TofuTools/TClaude/pull/800/files",
			repo:   "TofuTools/TClaude",
			number: 800,
			ok:     true,
		},
		{
			name:   "non GitHub host",
			rawURL: "https://gitlab.com/tofutools/tclaude/pull/800",
		},
		{
			name:   "not a PR path",
			rawURL: "https://github.com/tofutools/tclaude/issues/800",
		},
		{
			name:   "zero PR number",
			rawURL: "https://github.com/tofutools/tclaude/pull/0",
		},
		{
			name:   "owner query injection",
			rawURL: "https://github.com/tofutools%20OR%20is%3Aopen/tclaude/pull/800",
		},
		{
			name:   "repo query injection",
			rawURL: "https://github.com/tofutools/tclaude%20OR%20is%3Aopen/pull/800",
		},
		{
			name:   "invalid owner slug",
			rawURL: "https://github.com/-tofutools/tclaude/pull/800",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := githubPRRefFromURL(tt.rawURL)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.repo, got.repo)
			assert.Equal(t, tt.number, got.number)
		})
	}
}

func TestRecentlyMergedPRRetryDelay(t *testing.T) {
	assert.Equal(t, 10*time.Second, recentlyMergedPRRetryDelay(0))
	assert.Equal(t, 20*time.Second, recentlyMergedPRRetryDelay(1))
	assert.Equal(t, 40*time.Second, recentlyMergedPRRetryDelay(2))
	assert.Equal(t, 5*time.Minute, recentlyMergedPRRetryDelay(5))
	assert.Equal(t, 5*time.Minute, recentlyMergedPRRetryDelay(50))
}

func TestRecentlyMergedPRPollBackoffLifecycle(t *testing.T) {
	var backoff recentlyMergedPRPollBackoff
	failure := assert.AnError

	delay, warn, recovered, previous := backoff.next(true, failure)
	assert.Equal(t, 20*time.Second, delay)
	assert.True(t, warn)
	assert.False(t, recovered)
	assert.Zero(t, previous)

	for _, step := range []struct {
		failures int
		delay    time.Duration
	}{
		{2, 40 * time.Second},
		{3, 80 * time.Second},
		{4, 160 * time.Second},
		{5, 5 * time.Minute},
	} {
		for backoff.failures < step.failures {
			delay, warn, recovered, _ = backoff.next(true, failure)
		}
		assert.Equal(t, step.delay, delay)
		assert.True(t, warn, "each growing delay emits one warning")
		assert.False(t, recovered)
	}

	delay, warn, recovered, _ = backoff.next(true, failure)
	assert.Equal(t, 5*time.Minute, delay)
	assert.False(t, warn, "warnings stop once the capped delay stops growing")
	assert.False(t, recovered)

	delay, warn, recovered, previous = backoff.next(true, nil)
	assert.Equal(t, 10*time.Second, delay)
	assert.False(t, warn)
	assert.True(t, recovered)
	assert.Equal(t, 6, previous)
	assert.Zero(t, backoff.failures)

	_, _, _, _ = backoff.next(true, failure)
	delay, warn, recovered, previous = backoff.next(false, nil)
	assert.Equal(t, 10*time.Second, delay)
	assert.False(t, warn)
	assert.False(t, recovered, "no-target reset does not claim GitHub recovery")
	assert.Equal(t, 1, previous)
	assert.Zero(t, backoff.failures)
}

func TestRecentlyMergedPRResultLimit(t *testing.T) {
	assert.Equal(t, 20, recentlyMergedPRResultLimit(0))
	assert.Equal(t, 20, recentlyMergedPRResultLimit(20))
	assert.Equal(t, 21, recentlyMergedPRResultLimit(21))
	assert.Equal(t, 100, recentlyMergedPRResultLimit(100))
	assert.Equal(t, 100, recentlyMergedPRResultLimit(101))
}

func TestBoundedRecentlyMergedPRSearchRepos(t *testing.T) {
	repos := []string{"tofutools/one", "tofutools/two"}
	assert.Equal(t, repos, boundedRecentlyMergedPRSearchRepos("octocat", repos))
	assert.Equal(t,
		"author:octocat is:merged type:pr (repo:tofutools/one OR repo:tofutools/two)",
		recentlyMergedPRSearchQuery("octocat", repos))

	manyRepos := make([]string, 15)
	for i := range manyRepos {
		manyRepos[i] = fmt.Sprintf("tofutools/repository-%02d", i)
	}
	query := recentlyMergedPRSearchQuery("octocat", manyRepos)
	assert.LessOrEqual(t, len(query), maxRecentlyMergedPRSearchQueryChars)
	assert.NotContains(t, query, "repo:",
		"a 15-repo set exceeds the boolean/query budget and falls back to global search")
}

func TestCachedGitHubLogin(t *testing.T) {
	prevResolver := githubLoginResolver
	prevConfiguredResolver := githubConfiguredLoginResolver
	invalidateCachedGitHubLogin()
	t.Cleanup(func() {
		githubLoginResolver = prevResolver
		githubConfiguredLoginResolver = prevConfiguredResolver
		invalidateCachedGitHubLogin()
	})
	githubConfiguredLoginResolver = func() (string, bool) { return "", false }

	calls := 0
	githubLoginResolver = func() (string, bool) {
		calls++
		return "octocat", true
	}

	login, ok := cachedGitHubLogin()
	require.True(t, ok)
	assert.Equal(t, "octocat", login)
	login, ok = cachedGitHubLogin()
	require.True(t, ok)
	assert.Equal(t, "octocat", login)
	assert.Equal(t, 1, calls, "the daemon reuses one resolved login")

	invalidateCachedGitHubLogin()
	_, ok = cachedGitHubLogin()
	require.True(t, ok)
	assert.Equal(t, 2, calls, "explicit invalidation re-resolves the login")
}

func TestCachedGitHubLoginFollowsAuthSwitch(t *testing.T) {
	prevResolver := githubLoginResolver
	prevConfiguredResolver := githubConfiguredLoginResolver
	invalidateCachedGitHubLogin()
	t.Cleanup(func() {
		githubLoginResolver = prevResolver
		githubConfiguredLoginResolver = prevConfiguredResolver
		invalidateCachedGitHubLogin()
	})
	githubLoginResolver = func() (string, bool) {
		t.Fatal("network login fallback should not run with a configured account")
		return "", false
	}
	active := "octocat"
	githubConfiguredLoginResolver = func() (string, bool) { return active, true }

	login, ok := cachedGitHubLogin()
	require.True(t, ok)
	assert.Equal(t, "octocat", login)

	active = "hubot"
	login, ok = cachedGitHubLogin()
	require.True(t, ok)
	assert.Equal(t, "hubot", login, "local gh auth switch is observed on the next poll")
}

func TestLiveRecentlyMergedPRsResolverBuildsQueryAndParsesSearchIssues(t *testing.T) {
	prevLoginResolver := githubLoginResolver
	prevConfiguredResolver := githubConfiguredLoginResolver
	prevSearch := recentlyMergedPRSearch
	invalidateCachedGitHubLogin()
	t.Cleanup(func() {
		githubLoginResolver = prevLoginResolver
		githubConfiguredLoginResolver = prevConfiguredResolver
		recentlyMergedPRSearch = prevSearch
		invalidateCachedGitHubLogin()
	})
	githubConfiguredLoginResolver = func() (string, bool) { return "", false }
	githubLoginResolver = func() (string, bool) { return "octocat", true }

	var gotArgs []string
	recentlyMergedPRSearch = func(args ...string) (string, error) {
		gotArgs = append([]string(nil), args...)
		return `{"items":[{"number":42,"html_url":"https://github.com/acme/widgets/pull/42"}]}`, nil
	}

	prs, err := liveRecentlyMergedPRsResolver([]string{"acme/gadgets", "acme/widgets"}, 37)
	require.NoError(t, err)
	require.Len(t, prs, 1)
	assert.Equal(t, 42, prs[0].Number)
	assert.Equal(t, "https://github.com/acme/widgets/pull/42", prs[0].URL)
	assert.Equal(t, "merged", prs[0].State)
	assert.Equal(t, []string{
		"api", "--method", "GET", "search/issues",
		"-F", "advanced_search=true",
		"-f", "q=author:octocat is:merged type:pr (repo:acme/gadgets OR repo:acme/widgets)",
		"-f", "sort=updated",
		"-f", "order=desc",
		"-F", "per_page=37",
	}, gotArgs)
}

func TestLiveRecentlyMergedPRsResolverUsesFullPageAfterRepoFallback(t *testing.T) {
	prevConfiguredResolver := githubConfiguredLoginResolver
	prevSearch := recentlyMergedPRSearch
	invalidateCachedGitHubLogin()
	t.Cleanup(func() {
		githubConfiguredLoginResolver = prevConfiguredResolver
		recentlyMergedPRSearch = prevSearch
		invalidateCachedGitHubLogin()
	})
	githubConfiguredLoginResolver = func() (string, bool) { return "octocat", true }

	repos := make([]string, 15)
	for i := range repos {
		repos[i] = fmt.Sprintf("tofutools/repository-%02d", i)
	}
	var gotArgs []string
	recentlyMergedPRSearch = func(args ...string) (string, error) {
		gotArgs = append([]string(nil), args...)
		return `{"items":[]}`, nil
	}

	_, err := liveRecentlyMergedPRsResolver(repos, 20)
	require.NoError(t, err)
	assert.Contains(t, gotArgs, "q=author:octocat is:merged type:pr")
	assert.Contains(t, gotArgs, "per_page=100")
}
