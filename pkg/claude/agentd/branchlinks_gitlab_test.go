package agentd

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// branchlinks_gitlab_test.go: the Branch column for a project on a configured
// GitLab instance. Real git against a throwaway repository, as the GitHub
// tests here do, with the GitLab API stubbed at the transport seam.

// gitlabBranchLinkFixture is branchLinkFixture with a GitLab instance configured
// under gitlab_hosts and a token on disk.
func gitlabBranchLinkFixture(t *testing.T, allowedRemotes ...string) (string, string) {
	t.Helper()
	gitPath := gitAvailable(t)
	setupTestDB(t)
	tokenPath := filepath.Join(t.TempDir(), "gitlab-token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("glpat-branch-links\n"), 0o600))
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{
		GitProxy: &config.GitProxyConfig{
			AllowedRemotes:  allowedRemotes,
			GitLabHosts:     map[string]string{"gitlab.yamzz.test": "https://gitlab.yamzz.test:6080"},
			GitLabTokenFile: tokenPath,
		},
	}}))
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return gitPath, root
}

func TestBranchLinkRepoResolvesAConfiguredGitLabInstance(t *testing.T) {
	gitPath, root := gitlabBranchLinkFixture(t, "gitlab.yamzz.test/yamzz")
	repoDir := branchLinkGitRepo(t, gitPath, filepath.Join(root, "work"),
		"git@gitlab.yamzz.test:yamzz/sub/previewplayback.git")

	repo, ok := resolveBranchLinkRepo(context.Background(), branchLinkPolicy(t), repoDir)
	require.True(t, ok)
	assert.Equal(t, "gitlab", repo.Forge)
	assert.Equal(t, "https://gitlab.yamzz.test:6080", repo.Base, "the web root is the configured base, port included")
	assert.Equal(t, "https://gitlab.yamzz.test:6080/yamzz/sub/previewplayback", repo.RepoURL)
	assert.Equal(t, "yamzz/sub/previewplayback", repo.OwnerRepo, "the project is the full nested path")
	assert.Equal(t, "main", repo.DefaultBranch)

	t.Run("web links use the GitLab grammar", func(t *testing.T) {
		assert.Equal(t, "https://gitlab.yamzz.test:6080/yamzz/sub/previewplayback/-/compare/main...feat",
			branchWebURL(repo.RepoURL, "main", "feat"))
		assert.Equal(t, "https://gitlab.yamzz.test:6080/yamzz/sub/previewplayback/-/tree/main",
			branchWebURL(repo.RepoURL, "main", "main"))
		assert.Equal(t, "https://github.com/tofutools/tclaude/compare/main...feat",
			branchWebURL("https://github.com/tofutools/tclaude", "main", "feat"), "GitHub keeps its own")
	})
}

// TestBranchLinkRepoReadsAnHTTPSRemoteOnTheInstancesPort — the https form of
// the same project, on the configured instance's own port, gives the same
// Branch-column repository as the SSH form. Another port gives none.
func TestBranchLinkRepoReadsAnHTTPSRemoteOnTheInstancesPort(t *testing.T) {
	gitPath, root := gitlabBranchLinkFixture(t, "gitlab.yamzz.test/yamzz")
	repoDir := branchLinkGitRepo(t, gitPath, filepath.Join(root, "work"),
		"https://gitlab.yamzz.test:6080/yamzz/sub/previewplayback.git")

	repo, ok := resolveBranchLinkRepo(context.Background(), branchLinkPolicy(t), repoDir)
	require.True(t, ok)
	assert.Equal(t, "gitlab", repo.Forge)
	assert.Equal(t, "https://gitlab.yamzz.test:6080", repo.Base)
	assert.Equal(t, "https://gitlab.yamzz.test:6080/yamzz/sub/previewplayback", repo.RepoURL)
	assert.Equal(t, "yamzz/sub/previewplayback", repo.OwnerRepo)

	t.Run("another port on the same host", func(t *testing.T) {
		otherDir := branchLinkGitRepo(t, gitPath, filepath.Join(root, "other"),
			"https://gitlab.yamzz.test:9999/yamzz/sub/previewplayback.git")
		_, ok := resolveBranchLinkRepo(context.Background(), branchLinkPolicy(t), otherDir)
		assert.False(t, ok, "a port the operator never configured links nothing")
	})
}

func TestBranchLinkRepoRefusesAGitLabHostThatIsNotConfigured(t *testing.T) {
	gitPath, root := gitlabBranchLinkFixture(t, "gitlab.other.test")
	repoDir := branchLinkGitRepo(t, gitPath, filepath.Join(root, "work"),
		"git@gitlab.other.test:yamzz/sub/previewplayback.git")
	// Allow-listed, but no gitlab_hosts entry: the host is neither GitHub nor a
	// known GitLab instance, and a nested path fails the GitHub rule anyway.
	_, ok := resolveBranchLinkRepo(context.Background(), branchLinkPolicy(t), repoDir)
	assert.False(t, ok)
}

func TestHardenedGitInfoReadsTheMergeRequestFromGitLab(t *testing.T) {
	gitPath, root := gitlabBranchLinkFixture(t, "gitlab.yamzz.test/yamzz")
	repoDir := branchLinkGitRepo(t, gitPath, filepath.Join(root, "work"),
		"git@gitlab.yamzz.test:yamzz/sub/previewplayback.git")

	var mu sync.Mutex
	var calls []glAPIRequest
	var bases []string
	t.Cleanup(SetGitLabTransportForTest(func(_ context.Context, base, token string, req glAPIRequest) (glAPIResult, error) {
		mu.Lock()
		calls = append(calls, req)
		bases = append(bases, base)
		mu.Unlock()
		assert.Equal(t, "glpat-branch-links", token)
		switch req.Path {
		case "projects/yamzz%2Fsub%2Fpreviewplayback/merge_requests":
			return glAPIResult{Status: http.StatusOK, Header: http.Header{}, Body: []byte(`[
				{"iid":3,"state":"merged","draft":false,"web_url":"https://gitlab.yamzz.test:6080/yamzz/sub/previewplayback/-/merge_requests/3"},
				{"iid":7,"state":"opened","draft":true,"web_url":"https://gitlab.yamzz.test:6080/yamzz/sub/previewplayback/-/merge_requests/7"}]`)}, nil
		case "projects/yamzz%2Fsub%2Fpreviewplayback/merge_requests/7":
			return glAPIResult{Status: http.StatusOK, Header: http.Header{}, Body: []byte(`{"iid":7,"state":"opened","draft":true,
				"web_url":"https://gitlab.yamzz.test:6080/yamzz/sub/previewplayback/-/merge_requests/7",
				"head_pipeline":{"id":99,"status":"running","web_url":"https://gitlab.yamzz.test:6080/p/99"}}`)}, nil
		case "projects/yamzz%2Fsub%2Fpreviewplayback/pipelines/99/jobs":
			return glAPIResult{Status: http.StatusOK, Header: http.Header{}, Body: []byte(`[
				{"id":1,"name":"build","stage":"build","status":"success"},
				{"id":2,"name":"test","stage":"test","status":"running"}]`)}, nil
		}
		return glAPIResult{Status: http.StatusNotFound, Header: http.Header{}, Body: []byte(`{"message":"404 Not Found"}`)}, nil
	}))

	info, ok := hardenedGitInfo(branchLinkPolicy(t), repoDir, "feat")
	require.True(t, ok)
	assert.Equal(t, "https://gitlab.yamzz.test:6080/yamzz/sub/previewplayback", info.RepoURL)
	assert.Equal(t, 7, info.PRNumber, "the open merge request wins over the merged one")
	assert.Equal(t, "draft", info.PRState)
	assert.Equal(t, "https://gitlab.yamzz.test:6080/yamzz/sub/previewplayback/-/merge_requests/7", info.PRURL)
	require.NotNil(t, info.Checks)
	assert.Equal(t, "pending", info.Checks.Summary.State)
	assert.Equal(t, 2, info.Checks.Summary.Total)

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, calls)
	assert.Equal(t, "feat", calls[0].Query.Get("source_branch"), "the branch is a query value, never a path")
	for _, base := range bases {
		assert.Equal(t, "https://gitlab.yamzz.test:6080", base)
	}

	t.Run("the default branch asks GitLab nothing", func(t *testing.T) {
		before := len(calls)
		mu.Unlock()
		info, ok := hardenedGitInfo(branchLinkPolicy(t), repoDir, "main")
		mu.Lock()
		require.True(t, ok)
		assert.Equal(t, 0, info.PRNumber)
		assert.Equal(t, before, len(calls))
	})
}

// TestHardenedGitInfoPicksTheProjectsOwnOpenMergeRequest: the listing is by
// source branch name, so a fork's merge request into this project can share
// it; and an open one must be found even behind newer merged ones.
func TestHardenedGitInfoPicksTheProjectsOwnOpenMergeRequest(t *testing.T) {
	gitPath, root := gitlabBranchLinkFixture(t, "gitlab.yamzz.test/yamzz")
	repoDir := branchLinkGitRepo(t, gitPath, filepath.Join(root, "work"),
		"git@gitlab.yamzz.test:yamzz/sub/previewplayback.git")
	const list = "projects/yamzz%2Fsub%2Fpreviewplayback/merge_requests"
	answer := func(opened, all string) {
		t.Cleanup(SetGitLabTransportForTest(func(_ context.Context, _, _ string, req glAPIRequest) (glAPIResult, error) {
			if req.Path == list {
				body := all
				if req.Query.Get("state") == "opened" {
					body = opened
				}
				return glAPIResult{Status: http.StatusOK, Header: http.Header{}, Body: []byte(body)}, nil
			}
			if strings.HasPrefix(req.Path, list+"/") {
				return glAPIResult{Status: http.StatusOK, Header: http.Header{}, Body: []byte(`{"iid":1,"state":"opened"}`)}, nil
			}
			return glAPIResult{Status: http.StatusNotFound, Header: http.Header{}, Body: []byte(`{}`)}, nil
		}))
	}

	t.Run("a fork's merge request with the same branch name is skipped", func(t *testing.T) {
		answer(`[
			{"iid":9,"state":"opened","title":"From a fork","source_project_id":5,"target_project_id":1,
			 "web_url":"https://gitlab.yamzz.test:6080/yamzz/sub/previewplayback/-/merge_requests/9"},
			{"iid":8,"state":"opened","title":"Our own","source_project_id":1,"target_project_id":1,
			 "web_url":"https://gitlab.yamzz.test:6080/yamzz/sub/previewplayback/-/merge_requests/8"}]`, `[]`)
		info, ok := hardenedGitInfo(branchLinkPolicy(t), repoDir, "feat")
		require.True(t, ok)
		assert.Equal(t, 8, info.PRNumber)
		assert.Equal(t, "Our own", info.PRTitle, "the hover shows the merge request's title")
	})

	t.Run("no open one: the newest of the project's own in any state", func(t *testing.T) {
		answer(`[]`, `[
			{"iid":4,"state":"closed","title":"Fork","source_project_id":5,"target_project_id":1,
			 "web_url":"https://gitlab.yamzz.test:6080/yamzz/sub/previewplayback/-/merge_requests/4"},
			{"iid":3,"state":"merged","title":"Merged","source_project_id":1,"target_project_id":1,
			 "web_url":"https://gitlab.yamzz.test:6080/yamzz/sub/previewplayback/-/merge_requests/3"}]`)
		info, ok := hardenedGitInfo(branchLinkPolicy(t), repoDir, "feat")
		require.True(t, ok)
		assert.Equal(t, 3, info.PRNumber)
		assert.Equal(t, "merged", info.PRState)
	})
}

func TestBranchWebURLEscapesTheBranchName(t *testing.T) {
	const repo = "https://gitlab.yamzz.test:6080/yamzz/app"
	assert.Equal(t, repo+"/-/compare/main...feat/%231%3Fx", branchWebURL(repo, "main", "feat/#1?x"),
		"a '#' or '?' stays in the path; slashes stay separators")
	assert.Equal(t, repo+"/-/tree/feat/thing", branchWebURL(repo, "", "feat/thing"))
}

func TestHardenedGitInfoKeepsTheCachedChecksWhenTheDetailReadFails(t *testing.T) {
	gitPath, root := gitlabBranchLinkFixture(t, "gitlab.yamzz.test/yamzz")
	repoDir := branchLinkGitRepo(t, gitPath, filepath.Join(root, "work"),
		"git@gitlab.yamzz.test:yamzz/sub/previewplayback.git")
	const list = "projects/yamzz%2Fsub%2Fpreviewplayback/merge_requests"
	t.Cleanup(SetGitLabTransportForTest(func(_ context.Context, _, _ string, req glAPIRequest) (glAPIResult, error) {
		if req.Path == list {
			return glAPIResult{Status: http.StatusOK, Header: http.Header{}, Body: []byte(`[{"iid":8,"state":"opened",
				"web_url":"https://gitlab.yamzz.test:6080/yamzz/sub/previewplayback/-/merge_requests/8"}]`)}, nil
		}
		return glAPIResult{Status: http.StatusBadGateway, Header: http.Header{}, Body: []byte(`{"message":"bad gateway"}`)}, nil
	}))
	info, ok := hardenedGitInfo(branchLinkPolicy(t), repoDir, "feat")
	require.True(t, ok)
	assert.Equal(t, 8, info.PRNumber)
	assert.Nil(t, info.Checks, "no checks, not zero checks, so the cached summary is kept")
}

func TestHardenedGitInfoLooksPastForksForTheProjectsOwnMergeRequest(t *testing.T) {
	gitPath, root := gitlabBranchLinkFixture(t, "gitlab.yamzz.test/yamzz")
	repoDir := branchLinkGitRepo(t, gitPath, filepath.Join(root, "work"),
		"git@gitlab.yamzz.test:yamzz/sub/previewplayback.git")
	const list = "projects/yamzz%2Fsub%2Fpreviewplayback/merge_requests"
	mrURL := func(n int) string {
		return "https://gitlab.yamzz.test:6080/yamzz/sub/previewplayback/-/merge_requests/" + strconv.Itoa(n)
	}
	perPage := ""
	t.Cleanup(SetGitLabTransportForTest(func(_ context.Context, _, _ string, req glAPIRequest) (glAPIResult, error) {
		if req.Path == list && req.Query.Get("state") == "opened" {
			perPage = req.Query.Get("per_page")
			var page []map[string]any
			for i := range 20 { // newer merge requests from forks, same branch name
				page = append(page, map[string]any{"iid": 100 + i, "state": "opened", "source_project_id": 50 + i,
					"target_project_id": 1, "web_url": mrURL(100 + i)})
			}
			page = append(page, map[string]any{"iid": 8, "state": "opened", "title": "Ours",
				"source_project_id": 1, "target_project_id": 1, "web_url": mrURL(8)})
			body, _ := json.Marshal(page)
			return glAPIResult{Status: http.StatusOK, Header: http.Header{}, Body: body}, nil
		}
		if strings.HasPrefix(req.Path, list+"/") {
			return glAPIResult{Status: http.StatusOK, Header: http.Header{}, Body: []byte(`{"iid":8,"state":"opened"}`)}, nil
		}
		return glAPIResult{Status: http.StatusOK, Header: http.Header{}, Body: []byte(`[]`)}, nil
	}))
	info, ok := hardenedGitInfo(branchLinkPolicy(t), repoDir, "feat")
	require.True(t, ok)
	assert.Equal(t, 8, info.PRNumber, "the project's own merge request, past newer ones from forks")
	assert.Equal(t, "100", perPage, "one generous page, so forks cannot push it off")
}
