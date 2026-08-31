package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// TestProxyPermissionSlugsFollowProxyVisibility checks that the permission
// catalog advertises a proxy family only where that family could work.
//
// Each family answers for ITSELF. This test used to assert that configuring the
// git proxy revealed the Linear slugs too — a coupling that was wrong in both
// directions: a git-only host advertised slugs backed by no Linear key, and a
// Linear-only host hid slugs its operator needed in order to grant them. The
// families are configured independently, so they are advertised independently.
//
// Visibility is not enforcement. The full registry still backs validation and
// stored-grant resolution, so hiding a slug never withdraws a grant made under
// it, and revealing one never confers anything.
func TestProxyPermissionSlugsFollowProxyVisibility(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	// LINEAR_API_KEY is one of the sources that makes the Linear family
	// visible, so it has to be out of the way or this test would depend on
	// whatever the machine running it happens to export.
	t.Setenv("LINEAR_API_KEY", "")
	f := newFlow(t)

	gitSlugs := []string{
		agentd.PermGitRead,
		agentd.PermGitPush,
		agentd.PermGitHubRead,
		agentd.PermGitHubWrite,
		agentd.PermGitHubMerge,
	}
	linearSlugs := []string{
		agentd.PermLinearRead,
		agentd.PermLinearWrite,
	}
	proxySlugs := append(append([]string{}, gitSlugs...), linearSlugs...)

	readRegistry := func(t *testing.T) []string {
		t.Helper()
		rec := testharness.Serve(f.Mux,
			testharness.JSONRequest(t, http.MethodGet, "/v1/permissions/slugs", nil))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var entries []struct {
			Slug string `json:"slug"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &entries))
		out := make([]string, 0, len(entries))
		for _, entry := range entries {
			out = append(out, entry.Slug)
		}
		return out
	}
	readDashboard := func(t *testing.T) []string {
		t.Helper()
		rec := testharness.Serve(agentd.BuildDashboardHandlerForTest(),
			testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var snapshot struct {
			Slugs []struct {
				Slug string `json:"slug"`
			} `json:"slugs"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snapshot))
		out := make([]string, 0, len(snapshot.Slugs))
		for _, entry := range snapshot.Slugs {
			out = append(out, entry.Slug)
		}
		return out
	}

	t.Run("disabled", func(t *testing.T) {
		for _, got := range [][]string{readRegistry(t), readDashboard(t)} {
			assert.Contains(t, got, agentd.PermGroupsMembersSpawn)
			for _, slug := range proxySlugs {
				assert.NotContains(t, got, slug)
			}
		}
	})

	// The git proxy alone. Its own family appears; Linear's does not, because
	// nothing here names a Linear key or team.
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{
		GitProxy: &config.GitProxyConfig{AllowedRemotes: []string{"github.com/acme"}},
	}}))

	t.Run("git only", func(t *testing.T) {
		for _, got := range [][]string{readRegistry(t), readDashboard(t)} {
			for _, slug := range gitSlugs {
				assert.Contains(t, got, slug)
			}
			for _, slug := range linearSlugs {
				assert.NotContains(t, got, slug,
					"a git-only host has no Linear configuration to back this slug")
			}
		}
	})

	// Linear alone, with no git proxy at all: the case that used to hide both
	// Linear slugs and so left an operator unable to grant them.
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{
		LinearProxy: &config.LinearProxyConfig{AllowedTeams: []string{"TCL"}},
	}}))

	t.Run("linear only", func(t *testing.T) {
		for _, got := range [][]string{readRegistry(t), readDashboard(t)} {
			for _, slug := range linearSlugs {
				assert.Contains(t, got, slug,
					"a slug missing from the catalog is one nobody can grant")
			}
			for _, slug := range gitSlugs {
				assert.NotContains(t, got, slug)
			}
		}
	})

	// A Linear key with no allow-list — the scope-only posture, where the teams
	// come from each agent's own grant. The slug still has to be grantable.
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{
		LinearProxy: &config.LinearProxyConfig{APIKeyFile: "~/.tclaude/linear-key.txt"},
	}}))

	t.Run("linear configured with a key but no allow-list", func(t *testing.T) {
		for _, got := range [][]string{readRegistry(t), readDashboard(t)} {
			for _, slug := range linearSlugs {
				assert.Contains(t, got, slug)
			}
		}
	})

	// Both families configured: both advertised.
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{
		GitProxy:    &config.GitProxyConfig{AllowedRemotes: []string{"github.com/acme"}},
		LinearProxy: &config.LinearProxyConfig{AllowedTeams: []string{"TCL"}},
	}}))

	t.Run("both", func(t *testing.T) {
		for _, got := range [][]string{readRegistry(t), readDashboard(t)} {
			for _, slug := range proxySlugs {
				assert.Contains(t, got, slug)
			}
		}
	})
}
