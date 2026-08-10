package agentd_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// linearproxy_workspaces_flow_test.go covers the multi-key half of the Linear
// proxy: agent.linear_proxy.workspaces, which exists because a Linear personal
// API key is scoped to ONE workspace.
//
// The failure this file guards against is quiet. A team answered with the wrong
// workspace's key does not produce an error — that workspace has simply never
// heard of the issue, so Linear says "entity not found" and the agent reads it
// as a typo. Every assertion here is therefore about WHICH KEY was spent, which
// the recorder captures per call, rather than only about the answer.

// linearKeyFile writes a key to a temp file and returns its path.
func linearKeyFile(t *testing.T, name, key string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, []byte(key+"\n"), 0o600))
	return path
}

// twoWorkspaceWorld is the shape this whole file exercises: TCL in the
// workspace the operator's default key belongs to, ACM in a second workspace
// with a key of its own.
func twoWorkspaceWorld(t *testing.T) (*testharness.Flow, *linearRecorder, string) {
	t.Helper()
	acmeKey := linearKeyFile(t, "acme.key", "lin_api_acme")
	f, rec := linearWorld(t, []string{"TCL", "ACM"}, func(c *config.LinearProxyConfig) {
		c.AllowWrite = true
		c.Workspaces = []config.LinearWorkspaceConfig{
			{Name: "acme", APIKeyFile: acmeKey, Teams: []string{"ACM"}},
		}
	})
	require.NoError(t, db.GrantAgentPermission(linearProxyTestConv, agentd.PermLinearRead, "test"))
	return f, rec, "lin_api_acme"
}

// TestLinearProxy_PerTeamKeyRouting is the core contract: the credential
// follows the team, not the request order or the config order.
func TestLinearProxy_PerTeamKeyRouting(t *testing.T) {
	f, rec, acmeKey := twoWorkspaceWorld(t)
	rec.response = func(call linearCall) (int, string) {
		if id, _ := call.Variables["id"].(string); strings.HasPrefix(id, "ACM") {
			return http.StatusOK, issueJSON("ACM-7", "ACM")
		}
		return http.StatusOK, issueJSON("TCL-1", "TCL")
	}

	res := linearPost(t, f, "/v1/linear/issue/view", map[string]any{"identifier": "TCL-1"})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	res = linearPost(t, f, "/v1/linear/issue/view", map[string]any{"identifier": "ACM-7"})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	calls := rec.snapshot()
	require.Len(t, calls, 2)
	assert.Equal(t, "lin_api_testkey", calls[0].Key,
		"a team no workspaces entry claims must use the default key")
	assert.Equal(t, acmeKey, calls[1].Key,
		"a team an entry claims must use THAT workspace's key")
}

// TestLinearProxy_WriteRoutesToTheClaimingWorkspace — a write spends a
// credential under the operator's name, so it matters even more than a read
// that it spends the right one. `issue create` is the one write that names a
// team rather than an issue, so it routes on a different path.
func TestLinearProxy_WriteRoutesToTheClaimingWorkspace(t *testing.T) {
	f, rec, acmeKey := twoWorkspaceWorld(t)
	require.NoError(t, db.GrantAgentPermission(linearProxyTestConv, agentd.PermLinearWrite, "test"))
	rec.response = func(call linearCall) (int, string) {
		if strings.Contains(call.Query, "query TeamMeta") {
			return http.StatusOK, `{"data":{"teams":{"nodes":[{"id":"acm-uuid","key":"ACM","name":"Acme",
				"states":{"nodes":[{"id":"s1","name":"Todo"}]}}]}}}`
		}
		return http.StatusOK, `{"data":{"issueCreate":{"success":true,
			"issue":{"identifier":"ACM-8","team":{"key":"ACM"}}}}}`
	}

	res := linearPost(t, f, "/v1/linear/issue/create",
		map[string]any{"team": "ACM", "title": "Something for Acme"})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	calls := rec.snapshot()
	require.Len(t, calls, 2, "expected the team lookup then the mutation")
	for i, call := range calls {
		assert.Equal(t, acmeKey, call.Key, "call %d must spend Acme's key", i)
	}
}

// TestLinearProxy_ListFansOutOverEveryWorkspace — a listing with no --team
// spans the caller's whole effective set, which no single key can answer once
// the teams live in different workspaces.
func TestLinearProxy_ListFansOutOverEveryWorkspace(t *testing.T) {
	f, rec, acmeKey := twoWorkspaceWorld(t)
	rec.response = func(call linearCall) (int, string) {
		if call.Key == acmeKey {
			return http.StatusOK, issuesJSON(
				issueRow("ACM-2", "ACM", "2026-08-09T10:00:00Z"),
				issueRow("ACM-1", "ACM", "2026-08-01T10:00:00Z"))
		}
		return http.StatusOK, issuesJSON(
			issueRow("TCL-5", "TCL", "2026-08-10T10:00:00Z"),
			issueRow("TCL-4", "TCL", "2026-08-05T10:00:00Z"))
	}

	res := linearPost(t, f, "/v1/linear/issue/list", map[string]any{})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	calls := rec.snapshot()
	require.Len(t, calls, 2, "one call per workspace")
	assert.ElementsMatch(t, []string{"lin_api_testkey", acmeKey},
		[]string{calls[0].Key, calls[1].Key})

	// Each call may only ask about the teams ITS key can reach. A call that
	// carried the whole effective set would be asking one workspace about
	// another's teams — harmless in itself, but it would mean the filter no
	// longer describes what the credential can see.
	for _, call := range calls {
		teams := filterTeams(t, call)
		if call.Key == acmeKey {
			assert.Equal(t, []string{"acm"}, teams)
		} else {
			assert.Equal(t, []string{"tcl"}, teams)
		}
	}

	// Merged newest-first across workspaces, which is what orderBy: updatedAt
	// promises within one and what the merge has to restore across several.
	assert.Equal(t, []string{"TCL-5", "ACM-2", "TCL-4", "ACM-1"}, listedIdentifiers(t, res.Body.Bytes()))
}

// TestLinearProxy_ListHonoursTheLimitAcrossWorkspaces — each workspace is asked
// for `first: limit`, so N workspaces can return N*limit rows between them and
// the caller must still get the limit it asked for, made of the newest rows.
func TestLinearProxy_ListHonoursTheLimitAcrossWorkspaces(t *testing.T) {
	f, rec, acmeKey := twoWorkspaceWorld(t)
	rec.response = func(call linearCall) (int, string) {
		if call.Key == acmeKey {
			return http.StatusOK, issuesJSON(
				issueRow("ACM-2", "ACM", "2026-08-09T10:00:00Z"),
				issueRow("ACM-1", "ACM", "2026-08-08T10:00:00Z"))
		}
		return http.StatusOK, issuesJSON(
			issueRow("TCL-5", "TCL", "2026-08-10T10:00:00Z"),
			issueRow("TCL-4", "TCL", "2026-08-01T10:00:00Z"))
	}

	res := linearPost(t, f, "/v1/linear/issue/list", map[string]any{"limit": 2})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	for _, call := range rec.snapshot() {
		assert.EqualValues(t, 2, call.Variables["first"], "each workspace is asked for the caller's limit")
	}
	assert.Equal(t, []string{"TCL-5", "ACM-2"}, listedIdentifiers(t, res.Body.Bytes()),
		"the limit must bound the MERGED result, keeping the newest rows")
}

// TestLinearProxy_SearchTakesTurnsBetweenWorkspaces — relevance ranks from two
// responses are not comparable, so a bounded search result must not be filled
// by whichever workspace happens to be first.
func TestLinearProxy_SearchTakesTurnsBetweenWorkspaces(t *testing.T) {
	f, rec, acmeKey := twoWorkspaceWorld(t)
	rec.response = func(call linearCall) (int, string) {
		if call.Key == acmeKey {
			return http.StatusOK, searchJSON(
				issueRow("ACM-1", "ACM", "2026-08-01T10:00:00Z"),
				issueRow("ACM-2", "ACM", "2026-08-02T10:00:00Z"))
		}
		return http.StatusOK, searchJSON(
			issueRow("TCL-1", "TCL", "2026-08-03T10:00:00Z"),
			issueRow("TCL-2", "TCL", "2026-08-04T10:00:00Z"))
	}

	res := linearPost(t, f, "/v1/linear/issue/search",
		map[string]any{"term": "flaky", "limit": 3})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	assert.Equal(t, []string{"TCL-1", "ACM-1", "TCL-2"}, listedIdentifiers(t, res.Body.Bytes()),
		"each workspace's best result comes before either workspace's second")
}

// TestLinearProxy_NamedTeamSpendsOneKeyOnly — the fan-out is for the unnamed
// case alone. Spending every credential to answer a question about one team
// would put a read against workspaces that have no part in it into the
// operator's audit trail.
func TestLinearProxy_NamedTeamSpendsOneKeyOnly(t *testing.T) {
	f, rec, acmeKey := twoWorkspaceWorld(t)
	rec.response = func(linearCall) (int, string) {
		return http.StatusOK, issuesJSON(issueRow("ACM-1", "ACM", "2026-08-01T10:00:00Z"))
	}

	res := linearPost(t, f, "/v1/linear/issue/list", map[string]any{"team": "ACM"})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	call := rec.only(t)
	assert.Equal(t, acmeKey, call.Key)
	assert.Equal(t, []string{"ACM"}, filterTeams(t, call))
}

// TestLinearProxy_WorkspacesDoNotWidenTheAllowList — a workspaces entry says
// which key reaches a team, never whether the caller may. An operator who adds
// a workspace and forgets the allow-list must not have quietly granted it.
func TestLinearProxy_WorkspacesDoNotWidenTheAllowList(t *testing.T) {
	secretKey := linearKeyFile(t, "secret.key", "lin_api_secret")
	f, rec := linearWorld(t, []string{"TCL"}, func(c *config.LinearProxyConfig) {
		c.Workspaces = []config.LinearWorkspaceConfig{
			{Name: "secret", APIKeyFile: secretKey, Teams: []string{"SECRET"}},
		}
	})
	require.NoError(t, db.GrantAgentPermission(linearProxyTestConv, agentd.PermLinearRead, "test"))

	res := linearPost(t, f, "/v1/linear/issue/view", map[string]any{"identifier": "SECRET-1"})
	assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "team_not_allowed")
	assert.False(t, rec.sawAnyCall(), "a routed but unauthorized team must not reach Linear")

	// And the workspace's key is never spent on a listing either: SECRET is not
	// in the effective set, so no route covers it.
	rec.response = func(linearCall) (int, string) { return http.StatusOK, issuesJSON() }
	res = linearPost(t, f, "/v1/linear/issue/list", map[string]any{})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	call := rec.only(t)
	assert.Equal(t, "lin_api_testkey", call.Key)
	assert.Equal(t, []string{"tcl"}, filterTeams(t, call))
}

// TestLinearProxy_RefusesAnAmbiguousOrIncompletePolicy — a policy the daemon
// cannot route is refused whole, before any credential is spent. Picking a key
// for an ambiguously-claimed team would answer from the wrong workspace, and
// Linear reports that as a missing issue rather than as an error.
func TestLinearProxy_RefusesAnAmbiguousOrIncompletePolicy(t *testing.T) {
	keyPath := linearKeyFile(t, "other.key", "lin_api_other")

	cases := map[string]struct {
		workspaces []config.LinearWorkspaceConfig
		wants      string
	}{
		"two entries claim the same team": {
			workspaces: []config.LinearWorkspaceConfig{
				{Name: "one", APIKeyFile: keyPath, Teams: []string{"TCL"}},
				{Name: "two", APIKeyFile: keyPath, Teams: []string{"tcl"}},
			},
			wants: "claimed by two",
		},
		"an entry with no key file": {
			workspaces: []config.LinearWorkspaceConfig{{Name: "one", Teams: []string{"ACM"}}},
			wants:      "no api_key_file",
		},
		"an entry with no teams": {
			workspaces: []config.LinearWorkspaceConfig{{Name: "one", APIKeyFile: keyPath}},
			wants:      "lists no teams",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f, rec := linearWorld(t, []string{"TCL"}, func(c *config.LinearProxyConfig) {
				c.Workspaces = tc.workspaces
			})
			require.NoError(t, db.GrantAgentPermission(linearProxyTestConv, agentd.PermLinearRead, "test"))

			res := linearPost(t, f, "/v1/linear/issue/view", map[string]any{"identifier": "TCL-1"})
			assert.Equal(t, http.StatusServiceUnavailable, res.Code, "body=%s", res.Body.String())
			assert.Contains(t, res.Body.String(), "linear_proxy_misconfigured")
			assert.Contains(t, res.Body.String(), tc.wants)
			assert.False(t, rec.sawAnyCall(), "a policy that cannot be routed must spend nothing")
		})
	}
}

// TestLinearProxy_UnreadableWorkspaceKeyOnlyFailsThatWorkspace — keys are read
// per route, when a route is first used. A broken key for a workspace this
// request never touches must not fail the request.
func TestLinearProxy_UnreadableWorkspaceKeyOnlyFailsThatWorkspace(t *testing.T) {
	f, rec := linearWorld(t, []string{"TCL", "ACM"}, func(c *config.LinearProxyConfig) {
		c.Workspaces = []config.LinearWorkspaceConfig{
			{Name: "acme", APIKeyFile: filepath.Join(t.TempDir(), "absent.key"), Teams: []string{"ACM"}},
		}
	})
	require.NoError(t, db.GrantAgentPermission(linearProxyTestConv, agentd.PermLinearRead, "test"))
	rec.response = func(linearCall) (int, string) { return http.StatusOK, issueJSON("TCL-1", "TCL") }

	res := linearPost(t, f, "/v1/linear/issue/view", map[string]any{"identifier": "TCL-1"})
	require.Equal(t, http.StatusOK, res.Code,
		"a broken key in another workspace must not fail this request: %s", res.Body.String())

	res = linearPost(t, f, "/v1/linear/issue/view", map[string]any{"identifier": "ACM-1"})
	assert.Equal(t, http.StatusServiceUnavailable, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "key_unreadable")
	assert.Contains(t, res.Body.String(), "acme", "the refusal must name the workspace to fix")
	assert.Equal(t, 1, rec.count(), "the failed route must not have reached Linear")
}

// TestLinearProxy_WhoamiReportsEveryWorkspace — whoami is the verb an agent
// runs to explain a refusal to its human, so with several credentials it has to
// report each one, including the ones that failed.
func TestLinearProxy_WhoamiReportsEveryWorkspace(t *testing.T) {
	f, rec := linearWorld(t, []string{"TCL", "ACM"}, func(c *config.LinearProxyConfig) {
		c.Workspaces = []config.LinearWorkspaceConfig{
			{Name: "acme", APIKeyFile: filepath.Join(t.TempDir(), "absent.key"), Teams: []string{"ACM"}},
		}
	})
	require.NoError(t, db.GrantAgentPermission(linearProxyTestConv, agentd.PermLinearRead, "test"))
	rec.response = func(linearCall) (int, string) {
		return http.StatusOK, `{"data":{"viewer":{"name":"Op","displayName":"Op"},
			"teams":{"nodes":[{"key":"TCL","name":"Tclaude"}]}}}`
	}

	res := linearPost(t, f, "/v1/linear/whoami", map[string]any{})
	require.Equal(t, http.StatusOK, res.Code,
		"one working credential is still an answer: %s", res.Body.String())

	var out struct {
		JSON struct {
			Viewer     *struct{} `json:"viewer"`
			Workspaces []struct {
				Name   string   `json:"name"`
				Routes []string `json:"routes"`
				Error  string   `json:"error"`
			} `json:"workspaces"`
		} `json:"json"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	require.Len(t, out.JSON.Workspaces, 2)
	assert.Nil(t, out.JSON.Viewer,
		"with several credentials there is no single viewer to report")

	assert.Equal(t, "default", out.JSON.Workspaces[0].Name)
	assert.Equal(t, []string{"tcl"}, out.JSON.Workspaces[0].Routes)
	assert.Empty(t, out.JSON.Workspaces[0].Error)

	assert.Equal(t, "acme", out.JSON.Workspaces[1].Name)
	assert.Equal(t, []string{"acm"}, out.JSON.Workspaces[1].Routes)
	assert.Contains(t, out.JSON.Workspaces[1].Error, "key_unreadable",
		"the workspace that failed must say why, beside the one that worked")
}

// TestLinearProxy_WhoamiKeepsItsSingleWorkspaceShape — the overwhelmingly
// common configuration is one key, and its response must be exactly what it has
// always been.
func TestLinearProxy_WhoamiKeepsItsSingleWorkspaceShape(t *testing.T) {
	f, rec := linearWorld(t, []string{"TCL"})
	require.NoError(t, db.GrantAgentPermission(linearProxyTestConv, agentd.PermLinearRead, "test"))
	rec.response = func(linearCall) (int, string) {
		return http.StatusOK, `{"data":{"viewer":{"name":"Op","displayName":"Op"},
			"teams":{"nodes":[{"key":"TCL","name":"Tclaude"}]}}}`
	}

	res := linearPost(t, f, "/v1/linear/whoami", map[string]any{})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	var out struct {
		JSON struct {
			Viewer struct {
				DisplayName string `json:"displayName"`
			} `json:"viewer"`
			Teams []struct {
				Key     string `json:"key"`
				Allowed bool   `json:"allowed"`
			} `json:"teams"`
			Workspaces []struct {
				Name string `json:"name"`
			} `json:"workspaces"`
		} `json:"json"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	assert.Equal(t, "Op", out.JSON.Viewer.DisplayName)
	require.Len(t, out.JSON.Teams, 1)
	assert.True(t, out.JSON.Teams[0].Allowed)
	require.Len(t, out.JSON.Workspaces, 1, "the breakdown is present whatever the configuration")
	assert.Equal(t, "default", out.JSON.Workspaces[0].Name)
}

// --- helpers ---

// issueRow is one row of a scripted list or search response.
func issueRow(identifier, teamKey, updatedAt string) string {
	return `{"identifier":"` + identifier + `","title":"A thing","updatedAt":"` + updatedAt +
		`","team":{"key":"` + teamKey + `","name":"Team"}}`
}

func issuesJSON(rows ...string) string {
	return `{"data":{"issues":{"nodes":[` + strings.Join(rows, ",") + `]}}}`
}

func searchJSON(rows ...string) string {
	return `{"data":{"searchIssues":{"nodes":[` + strings.Join(rows, ",") + `]}}}`
}

// listedIdentifiers reads the identifiers out of a list or search response, in
// the order the daemon returned them — which is the merge's whole output.
func listedIdentifiers(t *testing.T, body []byte) []string {
	t.Helper()
	var out struct {
		JSON []struct {
			Identifier string `json:"identifier"`
		} `json:"json"`
	}
	require.NoError(t, json.Unmarshal(body, &out))
	ids := make([]string, 0, len(out.JSON))
	for _, row := range out.JSON {
		ids = append(ids, row.Identifier)
	}
	return ids
}

// filterTeams reads the team keys out of the filter one call carried, whichever
// shape the clause took: one team is a direct constraint, several are an `or`.
func filterTeams(t *testing.T, call linearCall) []string {
	t.Helper()
	filter, ok := call.Variables["filter"].(map[string]any)
	require.True(t, ok, "a list-shaped call must carry a filter")
	clauses, ok := filter["and"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, clauses)
	clause, ok := clauses[0].(map[string]any)
	require.True(t, ok)

	if alternatives, isOr := clause["or"].([]any); isOr {
		keys := make([]string, 0, len(alternatives))
		for _, alt := range alternatives {
			keys = append(keys, teamKeyOfClause(t, alt.(map[string]any)))
		}
		return keys
	}
	return []string{teamKeyOfClause(t, clause)}
}

func teamKeyOfClause(t *testing.T, clause map[string]any) string {
	t.Helper()
	team, ok := clause["team"].(map[string]any)
	require.True(t, ok, "every alternative must constrain a team")
	key, ok := team["key"].(map[string]any)["eqIgnoreCase"].(string)
	require.True(t, ok, "teams are matched case-insensitively")
	return key
}
