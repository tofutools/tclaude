package agentd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// TestGitProxyValidateRefusesOnlyWhatTheLoaderIgnores is the contract between
// the dashboard's save check (config.Validate) and the daemon's lenient load
// (ResolvedGitProxy): every agent.git_proxy value the save refuses is one the
// running daemon already drops or can never use. So a hand-edited file that
// blocks a Config save has not been doing anything either, and refusing it
// takes nothing away. A new refusal must say here what the loader does with
// the value.
func TestGitProxyValidateRefusesOnlyWhatTheLoaderIgnores(t *testing.T) {
	remote := func(host string, path ...string) remoteRef { return remoteRef{Scheme: "https", Host: host, Path: path} }
	cases := []struct {
		name  string
		gp    config.GitProxyConfig
		inert func(t *testing.T, loaded config.GitProxyConfig)
	}{
		{"a URL in the allow-list", config.GitProxyConfig{AllowedRemotes: []string{"https://github.com/acme/app"}},
			func(t *testing.T, p config.GitProxyConfig) {
				assert.False(t, remoteAllowed(remote("github.com", "acme", "app"), p.AllowedRemotes), "matches no remote")
			}},
		{"an scp remote in the allow-list", config.GitProxyConfig{AllowedRemotes: []string{"git@gitlab.example.com:team/app"}},
			func(t *testing.T, p config.GitProxyConfig) {
				assert.False(t, remoteAllowed(remote("gitlab.example.com", "team", "app"), p.AllowedRemotes))
			}},
		{"a port in the allow-list", config.GitProxyConfig{AllowedRemotes: []string{"gitlab.example.com:8929/team"}},
			func(t *testing.T, p config.GitProxyConfig) {
				assert.False(t, remoteAllowed(remote("gitlab.example.com", "team", "app"), p.AllowedRemotes),
					"a remote is matched by its host without a port")
			}},
		{"an empty segment", config.GitProxyConfig{AllowedRemotes: []string{"github.com//app"}},
			func(t *testing.T, p config.GitProxyConfig) {
				assert.False(t, remoteAllowed(remote("github.com", "acme", "app"), p.AllowedRemotes))
			}},
		{"a blank protected branch", config.GitProxyConfig{ProtectedRefs: &[]string{"main", " "}},
			func(t *testing.T, p config.GitProxyConfig) {
				assert.Equal(t, []string{"main"}, *p.ProtectedRefs, "dropped")
			}},
		{"an http base", config.GitProxyConfig{GitLabHosts: map[string]string{"gitlab.example.com": "http://gitlab.example.com"}},
			func(t *testing.T, p config.GitProxyConfig) {
				assert.Empty(t, p.GitLabBase("gitlab.example.com"), "dropped")
			}},
		{"a base with a path", config.GitProxyConfig{GitLabHosts: map[string]string{"gitlab.example.com": "https://example.com/gitlab"}},
			func(t *testing.T, p config.GitProxyConfig) { assert.Empty(t, p.GitLabBase("gitlab.example.com")) }},
		{"an empty base", config.GitProxyConfig{GitLabHosts: map[string]string{"gitlab.example.com": " "}},
			func(t *testing.T, p config.GitProxyConfig) { assert.Empty(t, p.GitLabBase("gitlab.example.com")) }},
		{"an empty host", config.GitProxyConfig{GitLabHosts: map[string]string{" ": "https://gitlab.example.com"}},
			func(t *testing.T, p config.GitProxyConfig) { assert.Empty(t, p.GitLabHosts, "dropped") }},
		{"a host with a port", config.GitProxyConfig{GitLabHosts: map[string]string{"gitlab.example.com:8929": "https://gitlab.example.com:8929"}},
			func(t *testing.T, p config.GitProxyConfig) {
				assert.Empty(t, p.GitLabBase("gitlab.example.com"), "a remote's host has no port, so this key is never looked up")
			}},
		{"a host listed twice", config.GitProxyConfig{GitLabHosts: map[string]string{
			"gitlab.example.com": "https://gitlab.example.com", " GitLab.example.com ": "https://other.example.com"}},
			func(t *testing.T, p config.GitProxyConfig) {
				assert.Empty(t, p.GitLabBase("gitlab.example.com"), "dropped")
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gp := tc.gp
			cfg := &config.Config{Agent: &config.AgentConfig{GitProxy: &gp}}
			errs := config.Validate(cfg)
			assert.NotEmpty(t, errs, "the save refuses it")
			assert.Contains(t, strings.Join(errs, "\n"), "agent.git_proxy.")
			tc.inert(t, cfg.ResolvedGitProxy())
		})
	}
}
