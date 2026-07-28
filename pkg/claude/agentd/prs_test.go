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

	manyRepos := make([]string, 15)
	for i := range manyRepos {
		manyRepos[i] = fmt.Sprintf("tofutools/repository-%02d", i)
	}
	query := recentlyMergedPRSearchQuery("octocat", manyRepos)
	assert.LessOrEqual(t, len(query), maxRecentlyMergedPRSearchQueryChars)
	assert.NotContains(t, query, "repo:",
		"an overlong 15-repo filter set falls back to the bounded global search")
}

func TestCachedGitHubLogin(t *testing.T) {
	prevResolver := githubLoginResolver
	invalidateCachedGitHubLogin()
	t.Cleanup(func() {
		githubLoginResolver = prevResolver
		invalidateCachedGitHubLogin()
	})

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
	assert.Equal(t, 2, calls, "a failed search may invalidate and re-resolve the login")
}

func TestLiveRecentlyMergedPRsResolverBuildsQueryAndParsesSearchIssues(t *testing.T) {
	prevLoginResolver := githubLoginResolver
	prevSearch := recentlyMergedPRSearch
	invalidateCachedGitHubLogin()
	t.Cleanup(func() {
		githubLoginResolver = prevLoginResolver
		recentlyMergedPRSearch = prevSearch
		invalidateCachedGitHubLogin()
	})
	githubLoginResolver = func() (string, bool) { return "octocat", true }

	var gotArgs []string
	recentlyMergedPRSearch = func(args ...string) string {
		gotArgs = append([]string(nil), args...)
		return `{"items":[{"number":42,"html_url":"https://github.com/acme/widgets/pull/42"}]}`
	}

	prs, ok := liveRecentlyMergedPRsResolver([]string{"acme/gadgets", "acme/widgets"}, 37)
	require.True(t, ok)
	require.Len(t, prs, 1)
	assert.Equal(t, 42, prs[0].Number)
	assert.Equal(t, "https://github.com/acme/widgets/pull/42", prs[0].URL)
	assert.Equal(t, "merged", prs[0].State)
	assert.Equal(t, []string{
		"api", "--method", "GET", "search/issues",
		"-F", "advanced_search=true",
		"-f", "q=author:octocat is:merged type:pr repo:acme/gadgets repo:acme/widgets",
		"-f", "sort=updated",
		"-f", "order=desc",
		"-F", "per_page=37",
	}, gotArgs)
}
