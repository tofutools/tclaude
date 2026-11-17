package agentd

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rollupCheckRun builds one raw GraphQL CheckRun context, the shape the
// open-PR search and its page reads return.
func rollupCheckRun(name, workflow, conclusion, startedAt string) json.RawMessage {
	node := map[string]any{
		"__typename": "CheckRun", "name": name, "status": "COMPLETED", "conclusion": conclusion,
		"startedAt": startedAt, "completedAt": startedAt,
		"detailsUrl": "https://github.com/acme/app/actions/runs/1/job/2",
		"checkSuite": map[string]any{"workflowRun": map[string]any{"workflow": map[string]any{"name": workflow}}},
	}
	raw, _ := json.Marshal(node)
	return raw
}

// openPRSearchWithContexts wraps a first page of contexts in a one-PR search
// response, with GitHub's aggregate state and totalCount.
func openPRSearchWithContexts(t *testing.T, state string, total int, first []json.RawMessage, hasNext bool) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"data": map[string]any{"search": map[string]any{
		"issueCount": 1,
		"nodes": []any{map[string]any{
			"number": 7, "title": "Big", "url": "https://github.com/acme/app/pull/7",
			"commits": map[string]any{"nodes": []any{map[string]any{"commit": map[string]any{
				"oid": "abc123",
				"statusCheckRollup": map[string]any{"state": state, "contexts": map[string]any{
					"totalCount": total,
					"pageInfo":   map[string]any{"hasNextPage": hasNext, "endCursor": "c0"},
					"nodes":      first,
				}},
			}}}},
		}},
	}}})
	require.NoError(t, err)
	return data
}

// pagedRollupFetcher serves the contexts after the first page, 100 at a time,
// recording each cursor it was asked for.
func pagedRollupFetcher(t *testing.T, rest []json.RawMessage, calls *[]string) {
	t.Helper()
	previous := authoredPRRollupPageFetcher
	t.Cleanup(func() { authoredPRRollupPageFetcher = previous })
	authoredPRRollupPageFetcher = func(repo, oid, cursor string) ([]json.RawMessage, rollupContextsPage, error) {
		*calls = append(*calls, cursor)
		assert.Equal(t, "acme/app", repo)
		assert.Equal(t, "abc123", oid)
		var page int
		_, err := fmt.Sscanf(cursor, "c%d", &page)
		require.NoError(t, err)
		lo := page * 100
		hi := min(lo+100, len(rest))
		return rest[lo:hi], rollupContextsPage{HasNextPage: hi < len(rest), EndCursor: fmt.Sprintf("c%d", page+1)}, nil
	}
}

// TestOpenPRChecksMatchGHForSupersededRunPastFirstPage reproduces the
// operator's report: 107 runs on the head commit, 46 passed, 60 skipped and
// one cancelled whose re-run passed under the same name. GitHub's aggregate
// is FAILURE because it counts the cancelled run; `gh pr checks` reports 46
// passed and 60 skipped, and so must the badge.
func TestOpenPRChecksMatchGHForSupersededRunPastFirstPage(t *testing.T) {
	var contexts []json.RawMessage
	contexts = append(contexts, rollupCheckRun("Test (macOS, agentd)", "CI", "SUCCESS", "2026-11-17T10:30:00Z"))
	for i := range 45 {
		contexts = append(contexts, rollupCheckRun(fmt.Sprintf("pass-%d", i), "CI", "SUCCESS", "2026-11-17T10:00:00Z"))
	}
	for i := range 60 {
		contexts = append(contexts, rollupCheckRun(fmt.Sprintf("skip-%d", i), "CI", "SKIPPED", "2026-11-17T10:00:00Z"))
	}
	// The superseded run sits on the second page, after its re-run.
	contexts = append(contexts, rollupCheckRun("Test (macOS, agentd)", "CI", "CANCELLED", "2026-11-17T10:00:00Z"))
	require.Len(t, contexts, 107)

	var calls []string
	pagedRollupFetcher(t, contexts[100:], &calls)
	data := openPRSearchWithContexts(t, "FAILURE", 107, contexts[:100], true)

	view, checks, err := decodeAuthoredOpenPRGraphQL(data, "octocat")
	require.NoError(t, err)
	assert.Equal(t, []string{"c0"}, calls, "one extra page, and only for the PR that has one")
	require.Len(t, view.Items, 1)
	summary := view.Items[0].Checks
	require.NotNil(t, summary)
	assert.Equal(t, "passing", summary.State)
	assert.Equal(t, 46, summary.Passed)
	assert.Equal(t, 60, summary.Skipped)
	assert.Zero(t, summary.Failed)
	assert.Equal(t, 106, summary.Total)
	assert.False(t, summary.Incomplete)
	assert.Len(t, checks["https://github.com/acme/app/pull/7"].Checks, 106)
}

// TestOpenPRChecksPastTheCapAreIncompleteNotRed: 450 contexts, all read ones
// green, aggregate FAILURE. 400 are read; the rest are not judged.
func TestOpenPRChecksPastTheCapAreIncompleteNotRed(t *testing.T) {
	var contexts []json.RawMessage
	for i := range 450 {
		contexts = append(contexts, rollupCheckRun(fmt.Sprintf("job-%d", i), "CI", "SUCCESS", "2026-11-17T10:00:00Z"))
	}
	var calls []string
	pagedRollupFetcher(t, contexts[100:], &calls)
	data := openPRSearchWithContexts(t, "FAILURE", 450, contexts[:100], true)

	view, checks, err := decodeAuthoredOpenPRGraphQL(data, "octocat")
	require.NoError(t, err)
	assert.Equal(t, []string{"c0", "c1", "c2"}, calls, "reading stops at the cap")
	summary := view.Items[0].Checks
	require.NotNil(t, summary)
	assert.Equal(t, "pending", summary.State, "unread checks must not paint the badge red")
	assert.Zero(t, summary.Failed)
	assert.Equal(t, 400, summary.Passed)
	assert.True(t, summary.Incomplete)
	assert.Equal(t, 400, summary.Read)
	assert.Equal(t, 450, summary.Reported)
	assert.Len(t, checks["https://github.com/acme/app/pull/7"].Checks, 400)
}

// TestOpenPRChecksFailedPageReadIsIncomplete: a page read that fails leaves
// the first page, marked incomplete, never the aggregate's red.
func TestOpenPRChecksFailedPageReadIsIncomplete(t *testing.T) {
	var contexts []json.RawMessage
	for i := range 100 {
		contexts = append(contexts, rollupCheckRun(fmt.Sprintf("job-%d", i), "CI", "SUCCESS", "2026-11-17T10:00:00Z"))
	}
	previous := authoredPRRollupPageFetcher
	t.Cleanup(func() { authoredPRRollupPageFetcher = previous })
	authoredPRRollupPageFetcher = func(string, string, string) ([]json.RawMessage, rollupContextsPage, error) {
		return nil, rollupContextsPage{}, fmt.Errorf("rate limited")
	}
	view, _, err := decodeAuthoredOpenPRGraphQL(openPRSearchWithContexts(t, "FAILURE", 107, contexts, true), "octocat")
	require.NoError(t, err)
	summary := view.Items[0].Checks
	assert.Equal(t, "pending", summary.State)
	assert.True(t, summary.Incomplete)
	assert.Equal(t, 100, summary.Read)
	assert.Equal(t, 107, summary.Reported)
}

// TestParseStatusCheckRollupKeepsTheNewestRunPerCheck covers the `gh pr view`
// shape, which flattens the workflow into workflowName.
func TestParseStatusCheckRollupKeepsTheNewestRunPerCheck(t *testing.T) {
	raw := json.RawMessage(`[
	  {"__typename":"CheckRun","name":"build","workflowName":"CI","status":"COMPLETED","conclusion":"FAILURE","startedAt":"2026-11-17T10:00:00Z","completedAt":"2026-11-17T10:05:00Z"},
	  {"__typename":"CheckRun","name":"build","workflowName":"Release","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-11-17T09:00:00Z","completedAt":"2026-11-17T09:05:00Z"},
	  {"__typename":"CheckRun","name":"build","workflowName":"CI","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-11-17T10:10:00Z","completedAt":"2026-11-17T10:15:00Z"},
	  {"__typename":"CheckRun","name":"lint","workflowName":"CI","status":"COMPLETED","conclusion":"CANCELLED","startedAt":"2026-11-17T10:00:00Z","completedAt":"2026-11-17T10:01:00Z"},
	  {"__typename":"CheckRun","name":"lint","workflowName":"CI","status":"QUEUED"},
	  {"__typename":"StatusContext","context":"ci/external","state":"SUCCESS","createdAt":"2026-11-17T10:00:00Z"}
	]`)
	info := parseStatusCheckRollup(raw, time.Now())
	require.Len(t, info.Checks, 4)
	byKey := map[string]prCheckRun{}
	for _, c := range info.Checks {
		byKey[c.Name+"/"+c.Source] = c
	}
	assert.Equal(t, "pass", byKey["build/CI"].Bucket, "the re-run replaces the failed run")
	assert.Equal(t, "pass", byKey["build/Release"].Bucket, "a same-named job in another workflow is its own check")
	assert.Equal(t, "pending", byKey["lint/CI"].Bucket, "a queued re-run is newer than the run it replaces")
	assert.Equal(t, "build", info.Checks[0].Name, "the kept run holds the first slot")
	assert.Equal(t, "pending", info.Summary.State)
	assert.Equal(t, 3, info.Summary.Passed)
	assert.Equal(t, 1, info.Summary.Pending)
}

// TestParseStatusCheckRollupPastTheCapIsIncomplete covers a flat list longer
// than the cap, as `gh pr view` can return.
func TestParseStatusCheckRollupPastTheCapIsIncomplete(t *testing.T) {
	var nodes []json.RawMessage
	for i := range prChecksMaxRuns + 5 {
		nodes = append(nodes, rollupCheckRun(fmt.Sprintf("job-%d", i), "CI", "SUCCESS", "2026-11-17T10:00:00Z"))
	}
	raw, err := json.Marshal(nodes)
	require.NoError(t, err)
	info := parseStatusCheckRollup(raw, time.Now())
	assert.Len(t, info.Checks, prChecksMaxRuns)
	assert.True(t, info.Summary.Incomplete)
	assert.Equal(t, "pending", info.Summary.State)
	assert.Equal(t, prChecksMaxRuns+5, info.Summary.Reported)
}

// TestParseStatusCheckRollupKeepsEachTriggerOfAJob: a workflow on both push and
// pull_request runs each job twice per commit. Those are two checks, so a
// failed push run must not be hidden by a later green pull_request run. With
// no event (`gh pr view`'s flat shape), the workflow run id tells them apart.
func TestParseStatusCheckRollupKeepsEachTriggerOfAJob(t *testing.T) {
	graphQL := json.RawMessage(`[
	  {"__typename":"CheckRun","name":"test","status":"COMPLETED","conclusion":"FAILURE","startedAt":"2026-11-17T10:00:00Z",
	   "checkSuite":{"workflowRun":{"event":"push","workflow":{"name":"CI"}}}},
	  {"__typename":"CheckRun","name":"test","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-11-17T10:00:05Z",
	   "checkSuite":{"workflowRun":{"event":"pull_request","workflow":{"name":"CI"}}}}
	]`)
	info := parseStatusCheckRollup(graphQL, time.Now())
	assert.Len(t, info.Checks, 2)
	assert.Equal(t, "failing", info.Summary.State)

	flat := json.RawMessage(`[
	  {"__typename":"CheckRun","name":"test","workflowName":"CI","status":"COMPLETED","conclusion":"FAILURE","startedAt":"2026-11-17T10:00:00Z",
	   "detailsUrl":"https://github.com/acme/app/actions/runs/11/job/1"},
	  {"__typename":"CheckRun","name":"test","workflowName":"CI","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-11-17T10:00:05Z",
	   "detailsUrl":"https://github.com/acme/app/actions/runs/12/job/2"},
	  {"__typename":"CheckRun","name":"lint","workflowName":"CI","status":"COMPLETED","conclusion":"CANCELLED","startedAt":"2026-11-17T10:00:00Z",
	   "detailsUrl":"https://github.com/acme/app/actions/runs/11/job/3"},
	  {"__typename":"CheckRun","name":"lint","workflowName":"CI","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-11-17T10:20:00Z",
	   "detailsUrl":"https://github.com/acme/app/actions/runs/11/job/4"}
	]`)
	info = parseStatusCheckRollup(flat, time.Now())
	assert.Len(t, info.Checks, 3, "two runs of test stay; lint's re-run (same run id) replaces its cancelled attempt")
	assert.Equal(t, "failing", info.Summary.State)
	assert.Equal(t, 2, info.Summary.Passed)
}
