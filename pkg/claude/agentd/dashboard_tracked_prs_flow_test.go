package agentd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type trackedPRsSnap struct {
	TrackedPRAuthors []struct {
		Login     string `json:"login"`
		Available bool   `json:"available"`
		Total     int    `json:"total"`
		Error     string `json:"error"`
		SearchURL string `json:"search_url"`
		Items     []struct {
			Number int    `json:"number"`
			Title  string `json:"title"`
			Draft  bool   `json:"draft"`
			Checks *struct {
				State string `json:"state"`
			} `json:"checks"`
		} `json:"items"`
		Recent []struct {
			Number int    `json:"number"`
			State  string `json:"state"`
		} `json:"recent"`
	} `json:"tracked_pr_authors"`
}

func fetchTrackedPRs(t *testing.T, mux http.Handler) trackedPRsSnap {
	t.Helper()
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var snap trackedPRsSnap
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snap))
	return snap
}

func postTrackedAuthor(t *testing.T, mux http.Handler, action, login string) (int, string) {
	t.Helper()
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost, "/api/open-prs/tracked-authors",
		map[string]string{"action": action, "login": login}))
	return rec.Code, rec.Body.String()
}

// Scenario: the operator adds two colleagues in the footer PR popover. One
// GraphQL request per poll covers both; each gets a tab with their open and
// recently closed PRs, and a colleague GitHub refuses to search shows the
// refusal on that tab alone. Removing a colleague drops the tab.
func TestDashboardTrackedPRAuthors_TabsFromOneSearch(t *testing.T) {
	_ = newFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	var calls [][]string
	t.Cleanup(agentd.SetTrackedPRSourcesForTest("me",
		func(login string) bool { return login != "ghost" },
		func(args []string) ([]byte, error) {
			calls = append(calls, args)
			return []byte(`{"data":{
				"u0":{"issueCount":2,"nodes":[
					{"number":812,"title":"infra: bump provider","url":"https://github.com/acme/infra/pull/812","isDraft":false,
					 "updatedAt":"2026-10-20T08:00:00Z","reviewDecision":"APPROVED","repository":{"nameWithOwner":"acme/infra"},
					 "commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"SUCCESS","contexts":{"totalCount":1,"nodes":[
					   {"__typename":"CheckRun","name":"ci","status":"COMPLETED","conclusion":"SUCCESS"}]}}}}]}},
					{"number":90,"title":"docs: runbooks","url":"https://github.com/acme/handbook/pull/90","isDraft":true,
					 "updatedAt":"2026-10-19T08:00:00Z","repository":{"nameWithOwner":"acme/handbook"},"commits":{"nodes":[]}}]},
				"r0":{"issueCount":1,"nodes":[
					{"number":700,"title":"old","url":"https://github.com/acme/infra/pull/700","state":"MERGED",
					 "mergedAt":"` + recentRFC3339() + `","closedAt":"` + recentRFC3339() + `","repository":{"nameWithOwner":"acme/infra"}}]},
				"u1":null,"r1":null},
				"errors":[{"type":"INVALID","path":["u1"],"message":"The listed users cannot be searched"}]}`), assert.AnError
		}))
	mux := agentd.BuildDashboardHandlerForTest()

	code, body := postTrackedAuthor(t, mux, "add", "ghost")
	assert.Equal(t, http.StatusBadRequest, code, "a login GitHub does not know is refused at once: %s", body)
	code, _ = postTrackedAuthor(t, mux, "add", "not a login")
	assert.Equal(t, http.StatusBadRequest, code)
	for _, login := range []string{"alice", "@bob", "Alice"} {
		code, body = postTrackedAuthor(t, mux, "add", login)
		require.Equalf(t, http.StatusOK, code, "add %s: %s", login, body)
	}
	assert.Contains(t, body, `["alice","bob"]`, "an @ is dropped and a re-add in other case is a no-op")

	snap := fetchTrackedPRs(t, mux)
	require.Len(t, snap.TrackedPRAuthors, 2, "tabs show before the first search lands")
	assert.False(t, snap.TrackedPRAuthors[0].Available)

	require.NoError(t, agentd.PollTrackedPRAuthorsForTest())
	require.Len(t, calls, 1, "one GitHub request covers every tracked author")
	argv := strings.Join(calls[0], "\n")
	assert.Contains(t, argv, "q0=author:alice is:open")
	assert.Contains(t, argv, "q1=author:bob is:open")
	assert.Contains(t, argv, "u1: search(query:$q1,")

	snap = fetchTrackedPRs(t, mux)
	require.Len(t, snap.TrackedPRAuthors, 2)
	alice := snap.TrackedPRAuthors[0]
	assert.Equal(t, "alice", alice.Login)
	assert.True(t, alice.Available)
	assert.Equal(t, 2, alice.Total)
	require.Len(t, alice.Items, 2)
	assert.Equal(t, 90, alice.Items[0].Number, "ordered by repository like the operator's own list")
	assert.True(t, alice.Items[0].Draft)
	assert.Equal(t, 812, alice.Items[1].Number)
	require.NotNil(t, alice.Items[1].Checks)
	assert.Equal(t, "passing", alice.Items[1].Checks.State)
	require.Len(t, alice.Recent, 1)
	assert.Equal(t, "merged", alice.Recent[0].State)
	assert.Contains(t, alice.SearchURL, "author%3Aalice")
	bob := snap.TrackedPRAuthors[1]
	assert.Equal(t, "bob", bob.Login)
	assert.False(t, bob.Available)
	assert.Contains(t, bob.Error, "cannot be searched")

	code, body = postTrackedAuthor(t, mux, "remove", "BOB")
	require.Equal(t, http.StatusOK, code, body)
	snap = fetchTrackedPRs(t, mux)
	require.Len(t, snap.TrackedPRAuthors, 1)
	assert.Equal(t, "alice", snap.TrackedPRAuthors[0].Login)
}

func recentRFC3339() string {
	return time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
}
