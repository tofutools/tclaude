package agentd

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// TestGitLabPRDetails_AnInstanceWithoutTheDiffstatIsNotAskedAgain: once an
// instance has refused the diffstat read over a field it lacks, later cards
// on it skip that read until agentd restarts, as the reviewer and recently
// closed reads do.
func TestGitLabPRDetails_AnInstanceWithoutTheDiffstatIsNotAskedAgain(t *testing.T) {
	setupTestDB(t)
	tokenPath := filepath.Join(t.TempDir(), "gitlab-token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("glpat-details\n"), 0o600))
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{GitProxy: &config.GitProxyConfig{
		AllowedRemotes:  []string{"gitlab.example.test/acme"},
		GitLabHosts:     map[string]string{"gitlab.example.test": testMRBase},
		GitLabTokenFile: tokenPath,
	}}}))
	t.Cleanup(glDiffStatsMissing.Clear)
	graphql := 0
	t.Cleanup(SetGitLabTransportForTest(func(_ context.Context, _, _ string, req glAPIRequest) (glAPIResult, error) {
		if req.GraphQL {
			graphql++
			return glAPIResult{Status: http.StatusOK, Header: http.Header{}, Body: []byte(`{"errors":[{
				"message":"Field 'diffStatsSummary' doesn't exist on type 'MergeRequest'","extensions":{"code":"undefinedField"}}]}`)}, nil
		}
		return glAPIResult{Status: http.StatusOK, Header: http.Header{}, Body: []byte(`{"iid":7,"title":"Change","state":"opened",
			"web_url":"` + testMRBase + `/acme/sub/app/-/merge_requests/7"}`)}, nil
	}))
	url := testMRBase + "/acme/sub/app/-/merge_requests/7"
	for range 2 {
		details, ok := gitlabPRDetails(url)
		require.True(t, ok)
		assert.True(t, details.NoDiffstat)
		assert.Equal(t, "Change", details.Title)
	}
	assert.Equal(t, 1, graphql, "the missing fields are remembered")
}
