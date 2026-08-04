package agentd_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// githubproxy_flow_test.go drives the GitHub proxy through the real daemon mux
// with the same stubbed subprocess boundary the git-proxy tests use.
//
// The assertions concentrate on the three things that are specific to `gh` and
// would fail silently: that the repository is DERIVED (never named by the
// agent), that gh runs outside the agent's repository so .git/config cannot
// re-aim it, and that free text travels by file rather than by argv.

// ghCall returns the single gh invocation the recorder saw.
func ghCall(t *testing.T, rec *gitProxyRecorder) agentd.ProxyCommand {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var found []agentd.ProxyCommand
	for _, c := range rec.calls {
		if c.Tool == "gh" {
			found = append(found, c)
		}
	}
	require.Len(t, found, 1, "expected exactly one gh invocation")
	return found[0]
}

func ghCallCount(rec *gitProxyRecorder) int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	n := 0
	for _, c := range rec.calls {
		if c.Tool == "gh" {
			n++
		}
	}
	return n
}

// TestGHProxy_WriteRequiresItsOwnSlug — reading PRs must not confer the ability
// to publish under the operator's GitHub identity.
func TestGHProxy_WriteRequiresItsOwnSlug(t *testing.T) {
	t.Run("github.read does not confer github.write", func(t *testing.T) {
		f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
		require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

		res := gitProxyPost(t, f, "/v1/github/pr/create",
			map[string]any{"title": "Add a thing", "body": "why"})
		assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
		assert.False(t, rec.sawAnySubprocess(), "a denied caller runs nothing at all")
	})

	t.Run("granted", func(t *testing.T) {
		f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
		rec.gh = agentd.ProxyResult{Stdout: `{"number":42,"url":"https://github.com/tofutools/tclaude/pull/42"}`}
		require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubWrite, "test"))

		res := gitProxyPost(t, f, "/v1/github/pr/create",
			map[string]any{"title": "Add a thing", "body": "why"})
		require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
		assert.Equal(t, 1, ghCallCount(rec))
	})
}

// TestGHProxy_RepositoryIsDerivedNotNamed is the invariant that stops an agent
// opening a pull request against somebody else's repository: there is no
// --repo parameter, and the slug the daemon passes to gh comes from the
// agent's own allow-listed remote.
func TestGHProxy_RepositoryIsDerivedNotNamed(t *testing.T) {
	f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
	rec.gh = agentd.ProxyResult{Stdout: "[]"}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/list", map[string]any{
		"repo": "attacker/exfil", "owner": "attacker", "state": "open",
	})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	call := ghCall(t, rec)
	i := slices.Index(call.Args, "--repo")
	require.GreaterOrEqual(t, i, 0, "gh must always be given an explicit --repo")
	assert.Equal(t, "tofutools/tclaude", call.Args[i+1],
		"the repo comes from the agent's own remote, never from the request")
	assert.NotContains(t, strings.Join(call.Args, " "), "attacker")
}

// TestGHProxy_RunsOutsideTheAgentRepository — gh discovers a repository by
// reading .git/config, a file the agent can write. Running it in a neutral
// directory is what makes the explicit --repo authoritative.
func TestGHProxy_RunsOutsideTheAgentRepository(t *testing.T) {
	f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
	rec.gh = agentd.ProxyResult{Stdout: "[]"}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/issue/list", map[string]any{})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	call := ghCall(t, rec)
	assert.NotEqual(t, rec.repoRoot, call.Dir,
		"gh must not run inside the agent's repository, where .git/config could re-aim it")
	assert.Equal(t, "/usr/bin/gh", call.Path, "the binary is pinned to an absolute path")
}

// TestGHProxy_BodyTravelsByFileNotArgv — a PR body is prose that may contain
// anything, and argv is world-readable through /proc for the life of the
// process. So the body must never appear there.
func TestGHProxy_BodyTravelsByFileNotArgv(t *testing.T) {
	f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
	rec.gh = agentd.ProxyResult{Stdout: "https://github.com/tofutools/tclaude/pull/7"}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubWrite, "test"))

	const secretish = "deploy key rotation notes, do not put me in /proc"
	res := gitProxyPost(t, f, "/v1/github/pr/create", map[string]any{
		"title": "Rotate the thing", "body": secretish,
	})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	call := ghCall(t, rec)
	joined := strings.Join(call.Args, " ")
	assert.NotContains(t, joined, secretish, "the body must not reach argv")
	i := slices.Index(call.Args, "--body-file")
	require.GreaterOrEqual(t, i, 0, "the body must be passed as a file")
	// The staged file is removed once gh has run, so the handler leaves nothing
	// behind on the daemon's disk.
	_, err := os.Stat(call.Args[i+1])
	assert.True(t, os.IsNotExist(err), "the staged body file must be cleaned up, got err=%v", err)
}

// TestGHProxy_TokenNeverReachesArgv — when the operator configures a token
// file, the token goes into the child's environment, never its command line.
func TestGHProxy_TokenNeverReachesArgv(t *testing.T) {
	f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
	rec.gh = agentd.ProxyResult{Stdout: "[]"}

	tokenPath := t.TempDir() + "/token"
	require.NoError(t, os.WriteFile(tokenPath, []byte("ghp_supersecret\n"), 0o600))
	writeGitProxyConfig(t, []string{"github.com/tofutools"}, func(c *gitProxyConfigPatch) {
		c.GitHubTokenFile = tokenPath
	})
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/list", map[string]any{})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	call := ghCall(t, rec)
	assert.NotContains(t, strings.Join(call.Args, " "), "ghp_supersecret",
		"/proc/<pid>/cmdline is readable by any same-uid process")
	assert.Contains(t, call.Env, "GH_TOKEN=ghp_supersecret")
}

// TestGHProxy_RefusesNonGitHubRemote — the gh half only speaks to github.com,
// and says so rather than issuing a call that would fail confusingly.
func TestGHProxy_RefusesNonGitHubRemote(t *testing.T) {
	f, rec := gitProxyWorld(t, []string{"gitlab.com/tofutools"})
	rec.remotes["origin"] = "git@gitlab.com:tofutools/tclaude.git"
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/list", map[string]any{})
	assert.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "not GitHub")
	assert.Equal(t, 0, ghCallCount(rec))
}

// TestGHProxy_InheritsTheRemoteAllowList — holding github.read is not enough:
// the underlying remote still has to be one the operator allow-listed.
func TestGHProxy_InheritsTheRemoteAllowList(t *testing.T) {
	f, rec := gitProxyWorld(t, []string{"github.com/someone-else"})
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/list", map[string]any{})
	assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, 0, ghCallCount(rec))
}

// TestGHProxy_RefusesInjectionShapedParameters — every scalar that reaches gh's
// argv is validated first.
func TestGHProxy_RefusesInjectionShapedParameters(t *testing.T) {
	f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubWrite, "test"))
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	cases := []struct {
		name string
		path string
		body map[string]any
	}{
		{"title beginning with a dash", "/v1/github/pr/create",
			map[string]any{"title": "--repo=attacker/exfil", "body": "x"}},
		{"title carrying a control character", "/v1/github/pr/create",
			map[string]any{"title": "hello\nworld", "body": "x"}},
		{"base branch that is a flag", "/v1/github/pr/create",
			map[string]any{"title": "ok", "body": "x", "base": "--exec=id"}},
		{"non-positive number", "/v1/github/pr/view",
			map[string]any{"number": 0}},
		{"negative number", "/v1/github/issue/view",
			map[string]any{"number": -1}},
		{"unknown state", "/v1/github/pr/list",
			map[string]any{"state": "; rm -rf /"}},
		{"out-of-range limit", "/v1/github/pr/list",
			map[string]any{"limit": 100000}},
		{"empty comment", "/v1/github/pr/comment",
			map[string]any{"number": 1, "body": "   "}},
		// A right-to-left override reorders how the title RENDERS on a PR
		// published under the operator's account, without changing what it
		// stores. A reader has no way to tell.
		{"title carrying a bidi override", "/v1/github/pr/create",
			map[string]any{"title": "Fix typo \u202egnp.exe", "body": "x"}},
		{"title longer than the character limit", "/v1/github/pr/create",
			map[string]any{"title": strings.Repeat("a", 257), "body": "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := gitProxyPost(t, f, tc.path, tc.body)
			assert.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
		})
	}
	assert.Equal(t, 0, ghCallCount(rec), "no invalid request may reach gh")
}

// TestGHProxy_TitleLimitCountsCharactersNotBytes — the limit and the refusal
// message are both stated in characters, and so is GitHub's own. Counting
// bytes would refuse a perfectly ordinary non-ASCII title at roughly a third of
// the advertised length.
func TestGHProxy_TitleLimitCountsCharactersNotBytes(t *testing.T) {
	f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubWrite, "test"))
	rec.gh = agentd.ProxyResult{Stdout: "https://github.com/tofutools/tclaude/pull/1\n"}

	// 200 CJK characters: 600 bytes, comfortably under the 256-character limit.
	res := gitProxyPost(t, f, "/v1/github/pr/create",
		map[string]any{"title": strings.Repeat("修", 200), "body": "x"})
	assert.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
}

// TestGHProxy_PassesJSONThroughUnmodelled — the daemon deliberately does not
// model gh's schemas, so a new GitHub field reaches the agent without a
// release on either side.
func TestGHProxy_PassesJSONThroughUnmodelled(t *testing.T) {
	f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
	rec.gh = agentd.ProxyResult{
		Stdout: `[{"number":7,"title":"a pr","someFieldAddedLater":{"nested":true}}]`,
	}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/list", map[string]any{})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	var out struct {
		Repo     string          `json:"repo"`
		ExitCode int             `json:"exit_code"`
		JSON     json.RawMessage `json:"json"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	assert.Equal(t, "tofutools/tclaude", out.Repo)
	assert.Equal(t, 0, out.ExitCode)
	assert.JSONEq(t, `[{"number":7,"title":"a pr","someFieldAddedLater":{"nested":true}}]`, string(out.JSON))
}

// TestGHProxy_AuditsWithoutRecordingContent — the audit row must name the repo
// and the operation, and must NOT carry the PR title or body. A PR body is
// free text an agent authored; the audit log is not the place for it.
func TestGHProxy_AuditsWithoutRecordingContent(t *testing.T) {
	f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
	rec.gh = agentd.ProxyResult{Stdout: "https://github.com/tofutools/tclaude/pull/9"}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubWrite, "test"))

	const title = "TITLE-SHOULD-NOT-BE-AUDITED"
	const body = "BODY-SHOULD-NOT-BE-AUDITED"
	res := gitProxyPost(t, f, "/v1/github/pr/create",
		map[string]any{"title": title, "body": body})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	row := auditRowByVerb(t, "github.pr.create")
	assert.Contains(t, row.Detail, "tofutools/tclaude")
	assert.Contains(t, row.Detail, "exit=0")
	assert.NotContains(t, row.Detail, title)
	assert.NotContains(t, row.Detail, body)
}

// TestGHProxy_FailureIsAnAnswer mirrors the git side: HTTP 200 means the daemon
// ran gh; gh's own message is what tells the agent what went wrong.
func TestGHProxy_FailureIsAnAnswer(t *testing.T) {
	f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
	rec.gh = agentd.ProxyResult{
		ExitCode: 1,
		Stderr:   "pull request create failed: GraphQL: No commits between main and feat/thing",
	}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubWrite, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/create",
		map[string]any{"title": "ok", "body": "x"})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	var out struct {
		ExitCode int    `json:"exit_code"`
		Stderr   string `json:"stderr"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	assert.Equal(t, 1, out.ExitCode)
	assert.Contains(t, out.Stderr, "No commits between",
		"gh's own diagnosis is the actionable part")
}

// TestGHProxy_TokenFileAcceptsATildePath — "~/github-token.txt" is how an
// operator writes a path in a JSON config file, and every other human-typed
// path in the daemon goes through expandTilde. Without it the operator gets
// `open ~/github-token.txt: no such file or directory`, which names a path that
// looks perfectly correct.
func TestGHProxy_TokenFileAcceptsATildePath(t *testing.T) {
	f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
	rec.gh = agentd.ProxyResult{Stdout: "[]"}

	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "github-token.txt"),
		[]byte("ghp_from_a_tilde_path\n"), 0o600))
	writeGitProxyConfig(t, []string{"github.com/tofutools"}, func(c *gitProxyConfigPatch) {
		c.GitHubTokenFile = "~/github-token.txt"
	})
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/list", map[string]any{})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, ghCall(t, rec).Env, "GH_TOKEN=ghp_from_a_tilde_path")
}

// TestGHProxy_TokenFileExplainsAnUnexpandedShellVariable — a config file is not
// a shell, so "${HOME}/token.txt" arrives literally. That is the one path form
// the daemon deliberately does not expand, so the refusal has to say so rather
// than reporting a missing file whose path reads as correct.
func TestGHProxy_TokenFileExplainsAnUnexpandedShellVariable(t *testing.T) {
	f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
	writeGitProxyConfig(t, []string{"github.com/tofutools"}, func(c *gitProxyConfigPatch) {
		c.GitHubTokenFile = "${HOME}/github-token.txt"
	})
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/list", map[string]any{})
	require.Equal(t, http.StatusServiceUnavailable, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "not expanded",
		"the operator must be told why a path that looks right did not resolve")
	assert.Equal(t, 0, ghCallCount(rec))
}

// TestGHProxy_PRCreateAlwaysPassesAHeadBranch is the regression for the
// headline verb being broken.
//
// gh derives the head branch from the LOCAL repository, and this proxy runs gh
// in a neutral directory on purpose — so `pr create` without an explicit --head
// fails with "could not determine the current branch: ... not a git repository"
// (gh 2.97). The documented invocation passes only --title and --body-file, so
// the feature's headline operation could not succeed at all.
//
// The old test asserted a 200 and stopped there; the recorder returns canned
// success for any argv, so it passed while nothing worked. Asserting the ARGV
// is what makes this real.
func TestGHProxy_PRCreateAlwaysPassesAHeadBranch(t *testing.T) {
	f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
	rec.gh = agentd.ProxyResult{Stdout: "https://github.com/tofutools/tclaude/pull/7"}
	rec.branch = "feat/the-thing"
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubWrite, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/create",
		map[string]any{"title": "Add the thing", "body": "why"})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	call := ghCall(t, rec)
	i := slices.Index(call.Args, "--head")
	require.GreaterOrEqual(t, i, 0,
		"gh cannot determine a head branch in a neutral directory, so the daemon must supply one")
	assert.Equal(t, "feat/the-thing", call.Args[i+1])
}

// TestGHProxy_PRCreateRefusesDetachedHead — with no branch there is nothing to
// open a pull request from, and guessing would be worse than saying so.
func TestGHProxy_PRCreateRefusesDetachedHead(t *testing.T) {
	f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
	rec.branch = "HEAD" // what rev-parse --abbrev-ref reports when detached
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubWrite, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/create",
		map[string]any{"title": "Add the thing", "body": "why"})
	assert.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "detached HEAD")
	assert.Equal(t, 0, ghCallCount(rec))
}

// TestGHProxy_RefusesToDeriveARepoFromADeeperPath closes an allow-list escape.
//
// matchRemotePattern lets a pattern shorter than the target match as a prefix,
// which is deliberate for nested GitLab groups. OwnerRepo() is first+last. Put
// those together and an allow-list naming exactly one repository also admits
// any sibling repository in the same owner:
//
//	allow-list  github.com/tofutools/tclaude
//	remote      github.com/tofutools/tclaude/private-secrets  → tofutools/private-secrets
//
// The git half never noticed because GitHub 404s a four-segment path. The gh
// half re-derives the slug, which is where the two rules disagree.
func TestGHProxy_RefusesToDeriveARepoFromADeeperPath(t *testing.T) {
	f, rec := gitProxyWorld(t, []string{"github.com/tofutools/tclaude"})
	rec.remotes["origin"] = "https://github.com/tofutools/tclaude/private-secrets"
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/list", map[string]any{})
	assert.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "not a plain github owner/repo path")
	assert.Equal(t, 0, ghCallCount(rec), "no repository may be derived from it")
}
