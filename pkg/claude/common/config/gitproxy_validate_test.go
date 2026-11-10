package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// gitproxy_validate_test.go: what the dashboard's Git & forges form may not
// save. Each bad value names its key, so the save dialog can say which row.

func gitProxyValidate(gp GitProxyConfig) []string {
	return Validate(&Config{Agent: &AgentConfig{GitProxy: &gp}})
}

func TestValidate_GitProxyAcceptsAGoodBlock(t *testing.T) {
	empty := []string{}
	assert.Empty(t, gitProxyValidate(GitProxyConfig{
		AllowedRemotes: []string{"github.com/acme", "gitlab.example.com/team/*", "github.com"},
		ProtectedRefs:  &[]string{"main", "release/*"},
		AllowForcePush: true,
		SSHKey:         "~/.ssh/id_ed25519",
		GitLabHosts: map[string]string{
			"gitlab.example.com": "https://gitlab.example.com:8929/",
			"gitlab.com":         "https://gitlab.com",
		},
		GitLabTokenFile: "~/.config/tclaude/gitlab-token",
	}))
	assert.Empty(t, gitProxyValidate(GitProxyConfig{ProtectedRefs: &empty}),
		"an empty protected_refs list turns the protection off, deliberately")
	assert.Empty(t, gitProxyValidate(GitProxyConfig{AllowedRemotes: []string{"  "}}),
		"a blank allow-list entry is dropped on load and harms nothing")
}

func TestValidate_GitProxyRejectsBadValues(t *testing.T) {
	cases := []struct {
		name string
		gp   GitProxyConfig
		want string
	}{
		{"a remote URL in the allow-list", GitProxyConfig{AllowedRemotes: []string{"https://github.com/acme/app"}},
			`allowed_remotes: "https://github.com/acme/app" must be a host/owner/repo pattern`},
		{"an scp-style remote in the allow-list", GitProxyConfig{AllowedRemotes: []string{"git@gitlab.example.com:team/app"}},
			"allowed_remotes"},
		{"a port in the allow-list", GitProxyConfig{AllowedRemotes: []string{"gitlab.example.com:8929/team"}},
			"allowed_remotes"},
		{"an empty segment", GitProxyConfig{AllowedRemotes: []string{"github.com//app"}},
			`allowed_remotes: "github.com//app" has an empty segment`},
		{"a blank protected branch", GitProxyConfig{ProtectedRefs: &[]string{"main", " "}},
			"protected_refs: an entry is blank"},
		{"an http base", GitProxyConfig{GitLabHosts: map[string]string{"gitlab.example.com": "http://gitlab.example.com"}},
			`gitlab_hosts["gitlab.example.com"]: "http://gitlab.example.com" must be an https origin`},
		{"a base with a path", GitProxyConfig{GitLabHosts: map[string]string{"gitlab.example.com": "https://example.com/gitlab"}},
			"with no path, query or user"},
		{"a base with a user", GitProxyConfig{GitLabHosts: map[string]string{"gitlab.example.com": "https://me@gitlab.example.com"}},
			"with no path, query or user"},
		{"an empty base", GitProxyConfig{GitLabHosts: map[string]string{"gitlab.example.com": " "}},
			"the base URL is empty"},
		{"an empty host", GitProxyConfig{GitLabHosts: map[string]string{" ": "https://gitlab.example.com"}},
			"gitlab_hosts: a host is empty"},
		{"a host with a port", GitProxyConfig{GitLabHosts: map[string]string{"gitlab.example.com:8929": "https://gitlab.example.com:8929"}},
			"must be a host name alone"},
		{"a host listed twice", GitProxyConfig{GitLabHosts: map[string]string{
			"gitlab.example.com": "https://gitlab.example.com", " GitLab.example.com ": "https://other.example.com"}},
			`"gitlab.example.com" is listed twice with different bases`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := gitProxyValidate(tc.gp)
			assert.Len(t, errs, 1, "%v", errs)
			assert.Contains(t, strings.Join(errs, "\n"), "agent.git_proxy.")
			assert.Contains(t, strings.Join(errs, "\n"), tc.want)
		})
	}
	t.Run("a host listed twice with the same base is fine", func(t *testing.T) {
		assert.Empty(t, gitProxyValidate(GitProxyConfig{GitLabHosts: map[string]string{
			"gitlab.example.com": "https://gitlab.example.com", "GitLab.example.com": "https://GitLab.example.com/"}}))
	})
}
