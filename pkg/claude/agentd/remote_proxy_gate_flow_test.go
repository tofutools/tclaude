package agentd_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The git, GitHub and GitLab proxies open every request the same way: the
// permission, then the remote it is about, then the grant's scope judged
// against that remote, all before a credential is spent or anything reaches
// the network. These pins hold that for every route behind that prologue.

var remoteProxyRoutes = []string{
	"/v1/git/ls-remote", "/v1/git/fetch", "/v1/git/push",

	"/v1/github/pr/create", "/v1/github/pr/list", "/v1/github/pr/view", "/v1/github/pr/checks",
	"/v1/github/pr/comments", "/v1/github/run/list", "/v1/github/run/log-failed",
	"/v1/github/run/artifacts", "/v1/github/run/download", "/v1/github/pr/comment",
	"/v1/github/pr/edit", "/v1/github/pr/ready", "/v1/github/pr/merge",
	"/v1/github/issue/list", "/v1/github/issue/view", "/v1/github/issue/comment",

	"/v1/gitlab/mr/create", "/v1/gitlab/mr/list", "/v1/gitlab/mr/view", "/v1/gitlab/mr/checks",
	"/v1/gitlab/mr/comments", "/v1/gitlab/mr/comment", "/v1/gitlab/mr/edit",
	"/v1/gitlab/mr/ready", "/v1/gitlab/mr/merge", "/v1/gitlab/issue/list",
	"/v1/gitlab/issue/view", "/v1/gitlab/issue/comment", "/v1/gitlab/pipeline/list",
	"/v1/gitlab/job/log-failed", "/v1/gitlab/job/artifacts", "/v1/gitlab/job/download",
}

// remoteProxyWorld sets up the world a route's proxy needs and reports how
// many credentialed calls reached the network, plus the slugs its routes take.
func remoteProxyWorld(t *testing.T, path string) (send func() *http.Response, spent func() int, slugs []string) {
	t.Helper()
	allowed := []string{"github.com/tofutools", "gitlab.contoso.test/contoso"}
	switch {
	case strings.HasPrefix(path, "/v1/github/"):
		f, git, gh := ghWorld(t, allowed)
		return func() *http.Response { return gitProxyPost(t, f, path, map[string]any{}).Result() },
			func() int { return gh.count() + len(git.networkCalls()) },
			[]string{agentd.PermGitHubRead, agentd.PermGitHubWrite, agentd.PermGitHubMerge}
	case strings.HasPrefix(path, "/v1/gitlab/"):
		f, git, gl := glWorld(t, allowed)
		return func() *http.Response { return gitProxyPost(t, f, path, map[string]any{}).Result() },
			func() int { return gl.count() + len(git.networkCalls()) },
			[]string{agentd.PermGitLabRead, agentd.PermGitLabWrite, agentd.PermGitLabMerge}
	default:
		f, git := gitProxyWorld(t, allowed)
		return func() *http.Response { return gitProxyPost(t, f, path, map[string]any{}).Result() },
			func() int { return len(git.networkCalls()) },
			[]string{agentd.PermGitRead, agentd.PermGitPush}
	}
}

func TestRemoteProxies_RefuseAnUngrantedCallerBeforeTheNetwork(t *testing.T) {
	for _, path := range remoteProxyRoutes {
		t.Run(path, func(t *testing.T) {
			send, spent, _ := remoteProxyWorld(t, path)
			res := send()
			assert.Equal(t, http.StatusForbidden, res.StatusCode)
			assert.Zero(t, spent(), "no credential spent")
		})
	}
}

// A grant scoped to another remote is judged against the remote the request
// resolves to, before anything is spent.
func TestRemoteProxies_JudgeAScopedGrantAgainstTheResolvedRemote(t *testing.T) {
	for _, path := range remoteProxyRoutes {
		t.Run(path, func(t *testing.T) {
			send, spent, slugs := remoteProxyWorld(t, path)
			for _, slug := range slugs {
				require.NoError(t, db.GrantAgentPermissionWithScope(gitProxyTestConv, slug,
					`{"remote":["github.com/elsewhere/*"]}`, "test"))
			}
			res := send()
			assert.Equal(t, http.StatusForbidden, res.StatusCode)
			assert.Zero(t, spent(), "no credential spent")
		})
	}
}

// The control: the same grants scoped to the remote the request does resolve
// to get past the gate, so the refusals above are the scope's.
func TestRemoteProxies_PassAScopedGrantForTheResolvedRemote(t *testing.T) {
	for _, path := range remoteProxyRoutes {
		t.Run(path, func(t *testing.T) {
			send, _, slugs := remoteProxyWorld(t, path)
			for _, slug := range slugs {
				require.NoError(t, db.GrantAgentPermissionWithScope(gitProxyTestConv, slug,
					`{"remote":["github.com/tofutools/*","gitlab.contoso.test/contoso/*/*"]}`, "test"))
			}
			res := send()
			assert.NotEqual(t, http.StatusForbidden, res.StatusCode)
		})
	}
}
