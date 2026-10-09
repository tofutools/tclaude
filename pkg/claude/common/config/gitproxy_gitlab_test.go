package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// gitproxy_gitlab_test.go covers the GitLab additions to the git-proxy policy:
// the host-to-base map that both recognises a GitLab instance and says where
// its API lives, and the one default it carries.

func TestResolvedGitProxyGitLabHosts(t *testing.T) {
	cfg := &Config{Agent: &AgentConfig{GitProxy: &GitProxyConfig{
		AllowedRemotes: []string{"GitLab.Example.com/acme"},
		GitLabHosts: map[string]string{
			" GitLab.Example.com ": " https://gitlab.example.com:6080/ ",
			"plain.example.com":    "http://plain.example.com", // not https: dropped, never a token destination
			"":                     "https://nowhere.example.com",
		},
		GitLabTokenFile: " ~/gitlab-token ",
	}}}
	policy := cfg.ResolvedGitProxy()

	assert.Equal(t, map[string]string{"gitlab.example.com": "https://gitlab.example.com:6080"}, policy.GitLabHosts)
	assert.Equal(t, "~/gitlab-token", policy.GitLabTokenFile)

	assert.Equal(t, "https://gitlab.example.com:6080", policy.GitLabBase("GITLAB.example.com"),
		"the host is matched case-insensitively, as the allow-list spells it")
	assert.Equal(t, "", policy.GitLabBase("plain.example.com"), "an http base was dropped")
	assert.Equal(t, "https://gitlab.com", policy.GitLabBase("gitlab.com"), "gitlab.com needs no entry")
	assert.Equal(t, "", policy.GitLabBase("github.com"))
	assert.Equal(t, "", policy.GitLabBase(""))
}

func TestResolvedGitProxyGitLabHostsAbsent(t *testing.T) {
	var nilCfg *Config
	assert.Nil(t, nilCfg.ResolvedGitProxy().GitLabHosts)
	assert.Equal(t, "https://gitlab.com", nilCfg.ResolvedGitProxy().GitLabBase("gitlab.com"),
		"the default survives an absent policy, so a scope-only posture can still reach gitlab.com")
	assert.Equal(t, "", GitProxyConfig{}.GitLabBase("gitlab.example.com"))
}
