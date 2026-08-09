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

func TestProxyPermissionSlugsFollowProxyVisibility(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	proxySlugs := []string{
		agentd.PermGitRead,
		agentd.PermGitPush,
		agentd.PermGitHubRead,
		agentd.PermGitHubWrite,
		agentd.PermLinearRead,
		agentd.PermLinearWrite,
	}

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
			assert.Contains(t, got, agentd.PermGroupsSpawn)
			for _, slug := range proxySlugs {
				assert.NotContains(t, got, slug)
			}
		}
	})

	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{
		GitProxy: &config.GitProxyConfig{AllowedRemotes: []string{"github.com/acme"}},
	}}))

	t.Run("enabled", func(t *testing.T) {
		for _, got := range [][]string{readRegistry(t), readDashboard(t)} {
			for _, slug := range proxySlugs {
				assert.Contains(t, got, slug)
			}
		}
	})
}
