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
			" GitLab.Example.com ": " https://gitlab.example.com:8929/ ",
			"plain.example.com":    "http://plain.example.com",           // not https: dropped, never a token destination
			"mounted.example.com":  "https://mounted.example.com/gitlab", // a path: dropped, not half-served
			"":                     "https://nowhere.example.com",
		},
		GitLabTokenFile: " ~/gitlab-token ",
	}}}
	policy := cfg.ResolvedGitProxy()

	assert.Equal(t, map[string]string{"gitlab.example.com": "https://gitlab.example.com:8929"}, policy.GitLabHosts)
	assert.Equal(t, "~/gitlab-token", policy.GitLabTokenFile)

	assert.Equal(t, "https://gitlab.example.com:8929", policy.GitLabBase("GITLAB.example.com"),
		"the host is matched case-insensitively, as the allow-list spells it")
	assert.Equal(t, "", policy.GitLabBase("plain.example.com"), "an http base was dropped")
	assert.Equal(t, "", policy.GitLabBase("mounted.example.com"), "a path-mounted base was dropped")
	assert.Equal(t, "", policy.GitLabBase("gitlab.com"),
		"with an instance listed, gitlab.com is not recognised (and gets no token) until it is listed too")
	explicit := GitProxyConfig{GitLabHosts: map[string]string{
		"gitlab.example.com": "https://gitlab.example.com:8929", "gitlab.com": "https://gitlab.com",
	}}
	assert.Equal(t, "https://gitlab.com", explicit.GitLabBase("gitlab.com"), "listed explicitly, it is")
	assert.Equal(t, []string{"https://gitlab.com", "https://gitlab.example.com:8929"}, explicit.GitLabBases())
	assert.Equal(t, "", policy.GitLabBase("github.com"))
	assert.Equal(t, "", policy.GitLabBase(""))
}

func TestResolvedGitProxyGitLabHostsAbsent(t *testing.T) {
	var nilCfg *Config
	assert.Nil(t, nilCfg.ResolvedGitProxy().GitLabHosts)
	assert.Equal(t, "https://gitlab.com", nilCfg.ResolvedGitProxy().GitLabBase("gitlab.com"),
		"the default survives an absent policy, so a scope-only posture can still reach gitlab.com")
	assert.Equal(t, []string{"https://gitlab.com"}, GitProxyConfig{}.GitLabBases(),
		"with no instance listed, the token is for gitlab.com")
	assert.Equal(t, "", GitProxyConfig{}.GitLabBase("gitlab.example.com"))
}

func TestResolvedGitProxyGitLabHostsAllInvalidNeverFallBackToGitLabCom(t *testing.T) {
	cfg := &Config{Agent: &AgentConfig{GitProxy: &GitProxyConfig{
		GitLabHosts:     map[string]string{"gitlab.corp": "http://gitlab.corp", "mounted.corp": "https://mounted.corp/gitlab"},
		GitLabTokenFile: "/token",
	}}}
	policy := cfg.ResolvedGitProxy()
	assert.Empty(t, policy.GitLabHosts, "every entry was invalid and dropped")
	assert.Equal(t, "", policy.GitLabBase("gitlab.com"),
		"the operator listed instances, so the token is not gitlab.com's, even with none left")
	assert.Empty(t, policy.GitLabBases())
}

func TestResolvedGitProxyGitLabHostsConflictingDuplicatesAreDropped(t *testing.T) {
	cfg := &Config{Agent: &AgentConfig{GitProxy: &GitProxyConfig{GitLabHosts: map[string]string{
		" GitLab.Corp ": "https://gitlab.corp:8929",
		"gitlab.corp":   "https://gitlab.corp:7080",
		"same.corp":     "https://same.corp",
		"SAME.corp":     "https://same.corp/",
		"other.corp":    "https://other.corp",
	}}}}
	policy := cfg.ResolvedGitProxy()
	assert.Equal(t, "", policy.GitLabBase("gitlab.corp"), "two bases for one host: neither is trusted")
	assert.Equal(t, "https://same.corp", policy.GitLabBase("same.corp"), "duplicates that agree are fine")
	assert.Equal(t, "https://other.corp", policy.GitLabBase("other.corp"))
}
