package agentd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestDecodeAuthoredOpenPRGraphQLSortsAttentionAndCachesChecks(t *testing.T) {
	data := []byte(`{"data":{"search":{"issueCount":3,"nodes":[
        {"number":3,"title":"Passing","url":"https://github.com/acme/app/pull/3","updatedAt":"2026-08-13T08:00:00Z","commits":{"nodes":[{"commit":{"statusCheckRollup":{"contexts":{"nodes":[{"__typename":"CheckRun","name":"test","status":"COMPLETED","conclusion":"SUCCESS"}]}}}}]}},
        {"number":1,"title":"Failing","url":"https://github.com/acme/app/pull/1","updatedAt":"2026-08-13T07:00:00Z","commits":{"nodes":[{"commit":{"statusCheckRollup":{"contexts":{"nodes":[{"__typename":"CheckRun","name":"lint","status":"COMPLETED","conclusion":"FAILURE"}]}}}}]}},
        {"number":2,"title":"No checks","url":"https://github.com/acme/other/pull/2","updatedAt":"2026-08-13T09:00:00Z","commits":{"nodes":[]}},
        {"number":9,"title":"Reject issue URL","url":"https://github.com/acme/app/issues/9"}
      ]}}}`)
	view, checks, err := decodeAuthoredOpenPRGraphQL(data, "octocat")
	require.NoError(t, err)
	require.Len(t, view.Items, 3)
	assert.Equal(t, []int{1, 2, 3}, []int{view.Items[0].Number, view.Items[1].Number, view.Items[2].Number})
	assert.Equal(t, "failing", view.Items[0].Checks.State)
	assert.Equal(t, "passing", view.Items[2].Checks.State)
	assert.Equal(t, "acme/other", view.Items[1].Repository)
	assert.Contains(t, view.SearchURL, "author%3Aoctocat")
	assert.Len(t, checks, 2)
}

func TestPollAndAssociateAuthoredOpenPRs(t *testing.T) {
	setupTestDB(t)
	previous := authoredOpenPRResolver
	t.Cleanup(func() { authoredOpenPRResolver = previous })
	authoredOpenPRResolver = func() (dashboardAuthoredOpenPRs, error) {
		return dashboardAuthoredOpenPRs{
			Login: "octocat", Total: 1,
			Items: []dashboardAuthoredOpenPR{{
				Number: 12, URL: "https://github.com/acme/app/pull/12", Title: "Ship it",
			}},
		}, nil
	}
	require.NoError(t, pollAuthoredOpenPRs())
	view := loadAuthoredOpenPRsSnapshot()
	assert.True(t, view.Available)
	assert.NotEmpty(t, view.UpdatedAt)
	require.Len(t, view.Items, 1)

	view = associateAuthoredOpenPRs(view, []dashboardAgent{{
		AgentID: "agt_1", ConvID: "conv", Title: "builder",
		repoLinksView: repoLinksView{BranchPRURL: "https://github.com/ACME/App/pull/12/files"},
	}})
	assert.Equal(t, "agt_1", view.Items[0].AgentID)
	assert.Equal(t, "builder", view.Items[0].AgentTitle)

	row, err := db.LoadGitCache(authoredOpenPRCacheKey)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.WithinDuration(t, time.Now(), row.FetchedAt, time.Second)
}

func TestAuthoredOpenPRRetryDelayCaps(t *testing.T) {
	assert.Equal(t, 30*time.Second, authoredOpenPRRetryDelay(0))
	assert.Equal(t, time.Minute, authoredOpenPRRetryDelay(1))
	assert.Equal(t, 5*time.Minute, authoredOpenPRRetryDelay(4))
	assert.Equal(t, 5*time.Minute, authoredOpenPRRetryDelay(20))
}
