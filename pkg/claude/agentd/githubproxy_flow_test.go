package agentd_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// githubproxy_flow_test.go drives the GitHub proxy through the real daemon mux
// with only the outbound-HTTP boundary stubbed.
//
// The assertions concentrate on the things that would fail SILENTLY: that the
// repository is DERIVED from the agent's own allow-listed remote and never
// named by the agent, that no caller-supplied value reaches a URL path or a
// GraphQL document body, and that the operator's token travels in a header and
// nowhere else.
//
// Recording the request rather than an argv is what makes this possible now
// that the daemon calls GitHub directly. It is also strictly more than the
// argv assertions could see: a test can prove a value landed in `variables`
// rather than in the query text, which is the GraphQL equivalent of the
// injection gate the old suite checked one flag at a time.

// ---------------------------------------------------------------------------
// The GitHub transport recorder
// ---------------------------------------------------------------------------

// ghCanned is one scripted response.
type ghCanned struct {
	Status int
	Body   string
	Header http.Header
	// Err makes this call fail as a TRANSPORT error rather than as a refusal.
	// The two are different outcomes and the multi-call verbs have to render
	// both honestly.
	Err error
}

// ghRecorder is the stubbed outbound edge: it remembers every request the
// daemon built and answers from a script.
type ghRecorder struct {
	mu     sync.Mutex
	calls  []agentd.GitHubRequestForTest
	tokens []string
	// budgets is the remaining context deadline seen by each call, same index.
	// It is the only visible trace of the timeout a handler chose, and a
	// handler that picks the wrong one fails in production rather than here.
	budgets []time.Duration

	// route answers by request, and is consulted first. It is how a verb that
	// makes several DIFFERENT calls is scripted without depending on the order
	// the handler happens to make them in.
	route func(req agentd.GitHubRequestForTest) (ghCanned, bool)
	// seq answers successive calls in order, falling back to `def` once
	// exhausted.
	seq []ghCanned
	def ghCanned

	// streams records every bulk transfer. zips answers them by path, and
	// zipAny is the fallback for a fixture that does not care which artifact
	// was asked for.
	streams []agentd.GitHubRequestForTest
	zips    map[string][]byte
	zipAny  []byte
	// streamErrs fails one path's transfer. streamErr fails every one. The
	// per-path form exists because a run whose log ARCHIVE has expired must
	// still be able to fall back to its per-job logs, and both travel through
	// this seam.
	streamErrs map[string]error
	streamErr  error
}

func newGHRecorder() *ghRecorder {
	return &ghRecorder{
		def:        ghCanned{Status: 200, Body: "{}"},
		zips:       map[string][]byte{},
		streamErrs: map[string]error{},
	}
}

func (r *ghRecorder) do(ctx context.Context, token string, req agentd.GitHubRequestForTest) (int, []byte, http.Header, error) {
	r.mu.Lock()
	r.calls = append(r.calls, req)
	r.tokens = append(r.tokens, token)
	budget := time.Duration(0)
	if deadline, ok := ctx.Deadline(); ok {
		budget = time.Until(deadline)
	}
	r.budgets = append(r.budgets, budget)

	answer := r.def
	switch {
	case r.route != nil:
		if got, ok := r.route(req); ok {
			answer = got
		}
	}
	if len(r.seq) > 0 {
		answer = r.seq[0]
		r.seq = r.seq[1:]
	}
	r.mu.Unlock()

	if answer.Err != nil {
		return 0, nil, nil, answer.Err
	}
	header := answer.Header
	if header == nil {
		header = http.Header{}
	}
	return answer.Status, []byte(answer.Body), header, nil
}

func (r *ghRecorder) stream(ctx context.Context, _ string, req agentd.GitHubRequestForTest, dst func([]byte) error) (int, error) {
	r.mu.Lock()
	r.streams = append(r.streams, req)
	zip, ok := r.zips[req.Path]
	if !ok && r.zipAny != nil {
		zip, ok = r.zipAny, true
	}
	streamErr := r.streamErr
	if perPath, found := r.streamErrs[req.Path]; found {
		streamErr = perPath
	}
	r.mu.Unlock()

	if streamErr != nil {
		return 0, streamErr
	}
	if !ok {
		return http.StatusNotFound, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := dst(zip); err != nil {
		return 0, err
	}
	return http.StatusOK, nil
}

func (r *ghRecorder) requests() []agentd.GitHubRequestForTest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]agentd.GitHubRequestForTest(nil), r.calls...)
}

// streamed returns every bulk transfer the recorder saw.
func (r *ghRecorder) streamed() []agentd.GitHubRequestForTest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]agentd.GitHubRequestForTest(nil), r.streams...)
}

func (r *ghRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// only returns the single request the recorder saw.
func (r *ghRecorder) only(t *testing.T) agentd.GitHubRequestForTest {
	t.Helper()
	calls := r.requests()
	require.Len(t, calls, 1, "expected exactly one GitHub call")
	return calls[0]
}

// graphqlVars decodes the variables of a recorded GraphQL request.
func graphqlVars(t *testing.T, req agentd.GitHubRequestForTest) (string, map[string]any) {
	t.Helper()
	require.Equal(t, "graphql", req.Path, "not a GraphQL request")
	var payload struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	require.NoError(t, json.Unmarshal(req.Body, &payload))
	return payload.Query, payload.Variables
}

// jsonBody decodes the JSON body of a recorded REST request.
func jsonBody(t *testing.T, req agentd.GitHubRequestForTest) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(req.Body, &out))
	return out
}

// ghWorld is gitProxyWorld plus a stubbed GitHub transport and a token for the
// daemon to spend.
//
// The token comes from `gh auth token` rather than from a config file, because
// that is the path an operator who configured nothing takes — and it leaves the
// file free for the tests that exercise precedence.
func ghWorld(t *testing.T, allowed []string) (*testharness.Flow, *gitProxyRecorder, *ghRecorder) {
	t.Helper()
	f, git := gitProxyWorld(t, allowed)
	gh := newGHRecorder()
	t.Cleanup(agentd.SetGitHubTransportForTestJSON(gh.do))
	t.Cleanup(agentd.SetGitHubStreamForTestBytes(gh.stream))
	t.Cleanup(agentd.SetGHTokenCommandForTest(func(context.Context) (string, error) {
		return ghTestToken + "\n", nil
	}))
	return f, git, gh
}

const ghTestToken = "ghp_test_token_from_gh_auth_token"

// ---------------------------------------------------------------------------
// Permission gating
// ---------------------------------------------------------------------------

// TestGHProxy_WriteRequiresItsOwnSlug — reading PRs must not confer the ability
// to publish under the operator's GitHub identity.
func TestGHProxy_WriteRequiresItsOwnSlug(t *testing.T) {
	t.Run("proxy.github.read does not confer proxy.github.write", func(t *testing.T) {
		f, git, gh := ghWorld(t, []string{"github.com/tofutools"})
		require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

		res := gitProxyPost(t, f, "/v1/github/pr/create",
			map[string]any{"title": "Add a thing", "body": "why"})
		assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
		assert.Equal(t, 0, gh.count(), "a denied caller spends no credential")
		assert.False(t, git.sawAnySubprocess(), "and runs nothing at all")
	})

	t.Run("granted", func(t *testing.T) {
		f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
		gh.def = ghCanned{Status: 201, Body: `{"number":42,"html_url":"https://github.com/tofutools/tclaude/pull/42"}`}
		require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubWrite, "test"))

		res := gitProxyPost(t, f, "/v1/github/pr/create",
			map[string]any{"title": "Add a thing", "body": "why", "base": "main"})
		require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
		assert.Equal(t, 1, gh.count())
	})
}

func TestGHProxy_RemoteScopedRead(t *testing.T) {
	t.Run("matching remote passes", func(t *testing.T) {
		f, _, gh := ghWorld(t, []string{"github.com"})
		gh.def = ghCanned{Status: 200, Body: `{"data":{"repository":{"pullRequests":{"nodes":[]}}}}`}
		require.NoError(t, db.GrantAgentPermissionWithScope(gitProxyTestConv, agentd.PermGitHubRead,
			`{"remote":["github.com/tofutools/*"]}`, "test"))

		res := gitProxyPost(t, f, "/v1/github/pr/list", map[string]any{})
		require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
		assert.Equal(t, 1, gh.count())
	})

	t.Run("another allow-listed repository is refused", func(t *testing.T) {
		f, git, gh := ghWorld(t, []string{"github.com"})
		git.remotes["origin"] = "https://github.com/other/repo.git"
		require.NoError(t, db.GrantAgentPermissionWithScope(gitProxyTestConv, agentd.PermGitHubRead,
			`{"remote":["github.com/tofutools/*"]}`, "test"))

		res := gitProxyPost(t, f, "/v1/github/pr/list", map[string]any{})
		assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
		assert.Equal(t, 0, gh.count(), "a scope refusal must precede the call")
	})
}

// TestGHProxy_InheritsTheRemoteAllowList — holding proxy.github.read is not enough:
// the underlying remote still has to be one the operator allow-listed.
func TestGHProxy_InheritsTheRemoteAllowList(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/someone-else"})
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/list", map[string]any{})
	assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, 0, gh.count())
}

// ---------------------------------------------------------------------------
// Repository derivation
// ---------------------------------------------------------------------------

// TestGHProxy_RepositoryIsDerivedNotNamed is the invariant that stops an agent
// reaching somebody else's repository: there is no --repo parameter, and the
// slug every request is built from comes from the agent's own allow-listed
// remote.
func TestGHProxy_RepositoryIsDerivedNotNamed(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.def = ghCanned{Status: 200, Body: `{"data":{"repository":{"pullRequests":{"nodes":[]}}}}`}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/list", map[string]any{
		"repo": "attacker/exfil", "owner": "attacker", "state": "open",
	})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	call := gh.only(t)
	_, vars := graphqlVars(t, call)
	assert.Equal(t, "tofutools", vars["owner"])
	assert.Equal(t, "tclaude", vars["name"])
	assert.NotContains(t, string(call.Body), "attacker",
		"nothing the agent named may reach the request")
}

// TestGHProxy_RESTPathsCarryTheDerivedSlug — the REST half builds a URL rather
// than passing a flag, so the containment lives in the PATH. A request that
// took its owner or repository from the caller would be an allow-list escape
// with no other symptom.
func TestGHProxy_RESTPathsCarryTheDerivedSlug(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.def = ghCanned{Status: 201, Body: `{"html_url":"https://github.com/tofutools/tclaude/issues/7#issuecomment-1"}`}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubWrite, "test"))

	res := gitProxyPost(t, f, "/v1/github/issue/comment", map[string]any{
		"number": 7, "body": "noted", "repo": "attacker/exfil",
	})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	call := gh.only(t)
	assert.Equal(t, "repos/tofutools/tclaude/issues/7/comments", call.Path)
	assert.Equal(t, http.MethodPost, call.Method)
}

// TestGHProxy_RefusesNonGitHubRemote — this half only speaks to github.com, and
// says so rather than issuing a call that would fail confusingly.
func TestGHProxy_RefusesNonGitHubRemote(t *testing.T) {
	f, git, gh := ghWorld(t, []string{"gitlab.com/tofutools"})
	git.remotes["origin"] = "git@gitlab.com:tofutools/tclaude.git"
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/list", map[string]any{})
	assert.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "not GitHub")
	assert.Equal(t, 0, gh.count())
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
// The git half never noticed because GitHub 404s a four-segment path. This half
// re-derives the slug, which is where the two rules disagree.
func TestGHProxy_RefusesToDeriveARepoFromADeeperPath(t *testing.T) {
	f, git, gh := ghWorld(t, []string{"github.com/tofutools/tclaude"})
	git.remotes["origin"] = "https://github.com/tofutools/tclaude/private-secrets"
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/list", map[string]any{})
	assert.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "not a plain github owner/repo path")
	assert.Equal(t, 0, gh.count(), "no repository may be derived from it")
}

// ---------------------------------------------------------------------------
// Credentials
// ---------------------------------------------------------------------------

// TestGHProxy_TokenTravelsInAHeaderAndNowhereElse — the whole reason this proxy
// stopped shelling out for its API calls. A token in a child's environment is
// readable through /proc/<pid>/environ by any same-uid process for the life of
// the call; a token handed to the transport never leaves daemon memory.
func TestGHProxy_TokenTravelsInAHeaderAndNowhereElse(t *testing.T) {
	f, git, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.def = ghCanned{Status: 200, Body: `{"data":{"repository":{"pullRequests":{"nodes":[]}}}}`}

	tokenPath := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("ghp_supersecret\n"), 0o600))
	writeGitProxyConfig(t, []string{"github.com/tofutools"}, func(c *gitProxyConfigPatch) {
		c.GitHubTokenFile = tokenPath
	})
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/list", map[string]any{})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	gh.mu.Lock()
	tokens := append([]string(nil), gh.tokens...)
	gh.mu.Unlock()
	require.Len(t, tokens, 1)
	assert.Equal(t, "ghp_supersecret", tokens[0],
		"a configured token file is an explicit choice of identity and wins")

	call := gh.only(t)
	assert.NotContains(t, call.Path, "ghp_supersecret")
	assert.NotContains(t, string(call.Body), "ghp_supersecret")
	assert.NotContains(t, res.Body.String(), "ghp_supersecret",
		"a token must never be echoed back to the agent")
	// The git-side gates still run git, so "no subprocess at all" is not the
	// claim. The claim is that no subprocess ever SEES the token — which is
	// what /proc/<pid>/environ and /proc/<pid>/cmdline would otherwise expose
	// to any same-uid process for the life of the call.
	git.mu.Lock()
	defer git.mu.Unlock()
	for _, cmd := range git.calls {
		assert.NotContains(t, strings.Join(cmd.Args, " "), "ghp_supersecret")
		assert.NotContains(t, strings.Join(cmd.Env, " "), "ghp_supersecret")
	}
}

// TestGHProxy_TokenFileSkipsGHEntirely — configuring a token file is how an
// operator runs this proxy on a host with no `gh` installed, so a configured
// file must not merely WIN over gh: gh must not be consulted at all.
func TestGHProxy_TokenFileSkipsGHEntirely(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.def = ghCanned{Status: 200, Body: `{"data":{"repository":{"pullRequests":{"nodes":[]}}}}`}
	t.Cleanup(agentd.SetGHTokenCommandForTest(func(context.Context) (string, error) {
		return "", errors.New("gh must not be run when a token file is configured")
	}))

	tokenPath := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("ghp_from_the_file\n"), 0o600))
	writeGitProxyConfig(t, []string{"github.com/tofutools"}, func(c *gitProxyConfigPatch) {
		c.GitHubTokenFile = tokenPath
	})
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/list", map[string]any{})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
}

// TestGHProxy_TokenFileAcceptsATildePath — "~/github-token.txt" is how an
// operator writes a path in a JSON config file, and every other human-typed
// path in the daemon goes through expandTilde. Without it the operator gets
// `open ~/github-token.txt: no such file or directory`, which names a path that
// looks perfectly correct.
func TestGHProxy_TokenFileAcceptsATildePath(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.def = ghCanned{Status: 200, Body: `{"data":{"repository":{"pullRequests":{"nodes":[]}}}}`}

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

	gh.mu.Lock()
	defer gh.mu.Unlock()
	require.Len(t, gh.tokens, 1)
	assert.Equal(t, "ghp_from_a_tilde_path", gh.tokens[0])
}

// TestGHProxy_TokenFileExplainsAnUnexpandedShellVariable — a config file is not
// a shell, so "${HOME}/token.txt" arrives literally. That is the one path form
// the daemon deliberately does not expand, so the refusal has to say so rather
// than reporting a missing file whose path reads as correct.
//
// It must also NOT fall through to gh. A configured file is an explicit choice
// of identity, and quietly spending a different credential because the
// configured one could not be read is the worst of both answers.
func TestGHProxy_TokenFileExplainsAnUnexpandedShellVariable(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	t.Cleanup(agentd.SetGHTokenCommandForTest(func(context.Context) (string, error) {
		return "ghp_a_different_identity", nil
	}))
	writeGitProxyConfig(t, []string{"github.com/tofutools"}, func(c *gitProxyConfigPatch) {
		c.GitHubTokenFile = "${HOME}/github-token.txt"
	})
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/list", map[string]any{})
	require.Equal(t, http.StatusServiceUnavailable, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "not expanded",
		"the operator must be told why a path that looks right did not resolve")
	assert.Equal(t, 0, gh.count())
}

// TestGHProxy_DelegatesToGHWhenNoTokenFileIsConfigured — the ordinary posture.
// An operator who has run `gh auth login` and configured nothing keeps working,
// and the daemon asks gh rather than reimplementing its lookup.
func TestGHProxy_DelegatesToGHWhenNoTokenFileIsConfigured(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.def = ghCanned{Status: 200, Body: `{"data":{"repository":{"pullRequests":{"nodes":[]}}}}`}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/list", map[string]any{})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	gh.mu.Lock()
	defer gh.mu.Unlock()
	require.Len(t, gh.tokens, 1)
	assert.Equal(t, ghTestToken, gh.tokens[0])
}

// TestGHProxy_UnauthenticatedGHIsAnOperatorProblemNotAnAgentOne — the agent
// cannot fix this, so the refusal names what the OPERATOR has to do, carries
// gh's own diagnosis, and does not read as a permission denial.
func TestGHProxy_UnauthenticatedGHIsAnOperatorProblemNotAnAgentOne(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token string
		err   error
		want  string
	}{
		{"gh is not installed", "",
			errors.New("gh is not installed on the host running agentd: exec: \"gh\": executable file not found in $PATH"),
			"executable file not found"},
		{"gh is installed but not logged in", "",
			errors.New("exit status 1: gh: You are not logged into any GitHub hosts. To log in, run: gh auth login"),
			"not logged into any GitHub hosts"},
		{"gh answers with nothing at all", "", nil, "returned nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
			t.Cleanup(agentd.SetGHTokenCommandForTest(func(context.Context) (string, error) {
				return tc.token, tc.err
			}))
			require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

			res := gitProxyPost(t, f, "/v1/github/pr/list", map[string]any{})
			assert.Equal(t, http.StatusServiceUnavailable, res.Code, "body=%s", res.Body.String())
			body := res.Body.String()
			assert.Contains(t, body, tc.want, "gh's own diagnosis is the actionable part")
			assert.Contains(t, body, "github_token_file",
				"and the operator needs to know gh can be skipped entirely")
			assert.Equal(t, 0, gh.count(), "no credential means no call")
		})
	}
}

// TestGHProxy_RefusesATokenThatCannotBeAHeader — a stray newline inside a token
// file is an ordinary editor accident, and net/http rejects the header with a
// message that names the header rather than the cause.
func TestGHProxy_RefusesATokenThatCannotBeAHeader(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	tokenPath := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("ghp_first\nghp_second\n"), 0o600))
	writeGitProxyConfig(t, []string{"github.com/tofutools"}, func(c *gitProxyConfigPatch) {
		c.GitHubTokenFile = tokenPath
	})
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/list", map[string]any{})
	assert.Equal(t, http.StatusServiceUnavailable, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "control character")
	assert.Equal(t, 0, gh.count())
}

// ---------------------------------------------------------------------------
// Pull requests
// ---------------------------------------------------------------------------

// TestGHProxy_PRCreateAlwaysSendsAHeadBranch is the regression for the headline
// verb being broken.
//
// The head branch is a property of the AGENT's checkout, and this proxy has no
// checkout of its own — so unless the daemon reads the branch from the git
// session and puts it in the request, `pr create` cannot work at all.
func TestGHProxy_PRCreateAlwaysSendsAHeadBranch(t *testing.T) {
	f, git, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.def = ghCanned{Status: 201, Body: `{"number":7,"html_url":"https://github.com/tofutools/tclaude/pull/7"}`}
	git.branch = "feat/the-thing"
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubWrite, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/create",
		map[string]any{"title": "Add the thing", "body": "why", "base": "main"})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	call := gh.only(t)
	assert.Equal(t, "repos/tofutools/tclaude/pulls", call.Path)
	body := jsonBody(t, call)
	assert.Equal(t, "feat/the-thing", body["head"])
	assert.Equal(t, "main", body["base"])

	// The URL is the answer a human wants, and it is text rather than JSON.
	var out struct {
		Stdout string `json:"stdout"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	assert.Equal(t, "https://github.com/tofutools/tclaude/pull/7\n", out.Stdout)
}

// TestGHProxy_PRCreateSendsDraftOnlyWhenAsked — a draft pull request does not
// notify reviewers and does not run some workflows, so getting this backwards
// in either direction is a visible mistake on a PR published as the operator.
func TestGHProxy_PRCreateSendsDraftOnlyWhenAsked(t *testing.T) {
	for _, draft := range []bool{true, false} {
		t.Run(fmt.Sprintf("draft=%t", draft), func(t *testing.T) {
			f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
			gh.def = ghCanned{Status: 201, Body: `{"number":7,"html_url":"https://x/7"}`}
			require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubWrite, "test"))

			res := gitProxyPost(t, f, "/v1/github/pr/create", map[string]any{
				"title": "Add the thing", "body": "why", "base": "main", "draft": draft,
			})
			require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

			body := jsonBody(t, gh.only(t))
			if draft {
				assert.Equal(t, true, body["draft"])
			} else {
				// Absent rather than false: an ordinary pull request is what
				// the endpoint does without being told, and sending the field
				// would be this proxy asserting a default it does not own.
				assert.NotContains(t, body, "draft")
			}
		})
	}
}

// TestGHProxy_CommentAnswersWithTheCommentsAddress — a comment an agent cannot
// link to is one it cannot refer to in a report or reply under.
func TestGHProxy_CommentAnswersWithTheCommentsAddress(t *testing.T) {
	for _, path := range []string{"/v1/github/pr/comment", "/v1/github/issue/comment"} {
		t.Run(path, func(t *testing.T) {
			f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
			const url = "https://github.com/tofutools/tclaude/pull/42#issuecomment-99"
			gh.def = ghCanned{Status: 201, Body: `{"html_url":"` + url + `"}`}
			require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubWrite, "test"))

			res := gitProxyPost(t, f, path, map[string]any{"number": 42, "body": "noted"})
			require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

			assert.Equal(t, url+"\n", ghStdout(t, res.Body.Bytes()))
			assert.Equal(t, "noted", jsonBody(t, gh.only(t))["body"])
		})
	}
}

// TestGHProxy_TimeoutSaysItMayHaveTakenEffect — the one transport failure that
// needs telling apart from the rest. "Could not connect" means nothing
// happened; a deadline means the write may well have been applied, and an
// agent that retries without looking opens a second pull request.
func TestGHProxy_TimeoutSaysItMayHaveTakenEffect(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.def = ghCanned{Err: fmt.Errorf("Post \"https://api.github.com/…\": %w", context.DeadlineExceeded)}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubWrite, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/create",
		map[string]any{"title": "Add the thing", "body": "why", "base": "main"})
	require.Equal(t, http.StatusOK, res.Code,
		"a 502 would tell the agent nothing about whether the PR was created")

	var out struct {
		ExitCode int    `json:"exit_code"`
		TimedOut bool   `json:"timed_out"`
		Stderr   string `json:"stderr"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	assert.NotEqual(t, 0, out.ExitCode)
	assert.True(t, out.TimedOut, "the CLI renders this as 'it may or may not have taken effect'")
	assert.Contains(t, out.Stderr, "did not answer within")
}

// TestGHProxy_MultiCallWritesShareOneBudget — `pr create` without a base,
// `pr ready` and `pr merge` each make two calls. Two independent per-call
// bounds would let the daemon run to twice what the CLI waits on, which on a
// write is the worst place to leave "did it happen?" unanswered.
func TestGHProxy_MultiCallWritesShareOneBudget(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		perm string
		body map[string]any
		def  ghCanned
	}{
		{"pr create resolving a base", "/v1/github/pr/create", agentd.PermGitHubWrite,
			map[string]any{"title": "t", "body": "b"},
			ghCanned{Status: 200, Body: `{"default_branch":"main","number":7,"html_url":"https://x/7"}`}},
		{"pr ready", "/v1/github/pr/ready", agentd.PermGitHubWrite, map[string]any{"number": 42},
			ghCanned{Status: 200, Body: `{"data":{"repository":{"pullRequest":{"id":"PR_1","number":42,"url":"https://x/42","isDraft":true}}}}`}},
		// The merge answers both calls from one canned body: the first read
		// needs the pull request, the second needs `merged`, and neither
		// notices the other's fields.
		{"pr merge", "/v1/github/pr/merge", agentd.PermGitHubMerge, map[string]any{"number": 42},
			ghCanned{Status: 200, Body: `{"merged":true,"sha":"abc1234","data":{"repository":{"pullRequest":{
				"id":"PR_1","number":42,"url":"https://x/42","state":"OPEN","mergeable":"MERGEABLE","baseRefName":"main"}}}}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
			gh.def = tc.def
			require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, tc.perm, "test"))

			res := gitProxyPost(t, f, tc.path, tc.body)
			require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

			gh.mu.Lock()
			defer gh.mu.Unlock()
			require.Len(t, gh.budgets, 2, "this verb makes two calls")
			assert.Less(t, gh.budgets[1], gh.budgets[0],
				"the second call must inherit what is LEFT of the budget, not a fresh one")
		})
	}
}

// TestGHProxy_PRCreateResolvesTheDefaultBranch — `--base` is optional, and the
// REST API has no notion of "the obvious one". Without this the verb would fail
// on every invocation that did not name a base.
func TestGHProxy_PRCreateResolvesTheDefaultBranch(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.route = func(req agentd.GitHubRequestForTest) (ghCanned, bool) {
		switch req.Path {
		case "repos/tofutools/tclaude":
			return ghCanned{Status: 200, Body: `{"default_branch":"trunk"}`}, true
		case "repos/tofutools/tclaude/pulls":
			return ghCanned{Status: 201, Body: `{"number":8,"html_url":"https://x/8"}`}, true
		}
		return ghCanned{}, false
	}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubWrite, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/create",
		map[string]any{"title": "Add the thing", "body": "why"})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	calls := gh.requests()
	require.Len(t, calls, 2)
	assert.Equal(t, "trunk", jsonBody(t, calls[1])["base"])
}

// TestGHProxy_PRCreateRefusesDetachedHead — with no branch there is nothing to
// open a pull request from, and guessing would be worse than saying so.
func TestGHProxy_PRCreateRefusesDetachedHead(t *testing.T) {
	f, git, gh := ghWorld(t, []string{"github.com/tofutools"})
	git.branch = "HEAD" // what rev-parse --abbrev-ref reports when detached
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubWrite, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/create",
		map[string]any{"title": "Add the thing", "body": "why"})
	assert.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "detached HEAD")
	assert.Equal(t, 0, gh.count())
}

// TestGHProxy_BodyTravelsInTheRequestBody — a PR body is prose that may contain
// anything. It must not reach a URL, a query string, or a file on the daemon's
// disk; the request body is the only place it belongs.
//
// The old proxy staged it in a 0600 temp file because a child process had to
// read it from somewhere. Nothing does now, so the file — and the window in
// which it existed — is gone.
func TestGHProxy_BodyTravelsInTheRequestBody(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.def = ghCanned{Status: 201, Body: `{"number":7,"html_url":"https://x/7"}`}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubWrite, "test"))

	const secretish = "deploy key rotation notes, do not put me in /proc"
	res := gitProxyPost(t, f, "/v1/github/pr/create", map[string]any{
		"title": "Rotate the thing", "body": secretish, "base": "main",
	})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	call := gh.only(t)
	assert.NotContains(t, call.Path, secretish)
	assert.NotContains(t, call.Query.Encode(), url.QueryEscape(secretish))
	assert.Equal(t, secretish, jsonBody(t, call)["body"])
}

// TestGHProxy_PRListMapsTheStateVocabulary — `--state merged` is not a REST
// filter at all, and folding merged into closed would make two options that
// callers expect to partition overlap instead.
func TestGHProxy_PRListMapsTheStateVocabulary(t *testing.T) {
	for state, want := range map[string]any{
		"open":   []any{"OPEN"},
		"closed": []any{"CLOSED"},
		"merged": []any{"MERGED"},
		"all":    nil,
	} {
		t.Run(state, func(t *testing.T) {
			f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
			gh.def = ghCanned{Status: 200, Body: `{"data":{"repository":{"pullRequests":{"nodes":[]}}}}`}
			require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

			res := gitProxyPost(t, f, "/v1/github/pr/list", map[string]any{"state": state})
			require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

			_, vars := graphqlVars(t, gh.only(t))
			assert.Equal(t, want, vars["states"])
		})
	}
}

// TestGHProxy_PRViewRendersTheDocumentedFieldSet pins the answer's shape.
//
// The field vocabulary is a contract: the bundled skill and every agent written
// against this proxy read `state`, `mergeable` and `reviewDecision` by name and
// in GitHub's own SHOUTED enum spelling. Four of these fields have no REST
// equivalent, which is why this read goes through GraphQL at all — so a
// regression to REST would show up here as changed values rather than as an
// error.
func TestGHProxy_PRViewRendersTheDocumentedFieldSet(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.def = ghCanned{Status: 200, Body: `{"data":{"repository":{"pullRequest":{
		"number": 42, "title": "Add the thing", "state": "MERGED", "isDraft": false,
		"headRefName": "feat/thing", "baseRefName": "main",
		"url": "https://github.com/tofutools/tclaude/pull/42", "body": "why",
		"createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-02T00:00:00Z",
		"mergeable": "MERGEABLE", "reviewDecision": "APPROVED",
		"author": {"__typename": "User", "login": "mikes", "id": "U_1", "name": "Mikael"}
	}}}}`}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/view", map[string]any{"number": 42})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	var out struct {
		Repo string          `json:"repo"`
		JSON json.RawMessage `json:"json"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	assert.Equal(t, "tofutools/tclaude", out.Repo)
	assert.JSONEq(t, `{
		"number": 42, "title": "Add the thing", "state": "MERGED", "isDraft": false,
		"headRefName": "feat/thing", "baseRefName": "main",
		"url": "https://github.com/tofutools/tclaude/pull/42", "body": "why",
		"createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-02T00:00:00Z",
		"author": {"id": "U_1", "is_bot": false, "login": "mikes", "name": "Mikael"},
		"mergeable": "MERGEABLE", "reviewDecision": "APPROVED"
	}`, string(out.JSON))
}

// TestGHProxy_BotAuthorsAreLabelled — "was this reviewed by a human?" is a
// question an agent acts on, and a bot's login is spelled `app/<name>`
// everywhere else GitHub shows it.
func TestGHProxy_BotAuthorsAreLabelled(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.def = ghCanned{Status: 200, Body: `{"data":{"repository":{"pullRequests":{"nodes":[
		{"number": 1, "title": "bot pr", "state": "OPEN",
		 "author": {"__typename": "Bot", "login": "coderabbitai"}}
	]}}}}`}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/list", map[string]any{})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	var out struct {
		JSON json.RawMessage `json:"json"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	assert.Contains(t, string(out.JSON), `"is_bot":true`)
	assert.Contains(t, string(out.JSON), `"login":"app/coderabbitai"`)
}

// TestGHProxy_MissingPullRequestIsAnAnswerNotAZeroValue — GraphQL answers an
// unknown number with `data.repository.pullRequest: null` and NO error, so a
// handler that just unmarshalled would render a pull request with number 0 and
// an empty title as though it were real.
func TestGHProxy_MissingPullRequestIsAnAnswerNotAZeroValue(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.def = ghCanned{Status: 200, Body: `{"data":{"repository":{"pullRequest":null}}}`}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/view", map[string]any{"number": 999999})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	var out struct {
		ExitCode int    `json:"exit_code"`
		Stderr   string `json:"stderr"`
		JSON     any    `json:"json"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	assert.NotEqual(t, 0, out.ExitCode)
	assert.Contains(t, out.Stderr, "no pull request #999999")
	assert.Nil(t, out.JSON)
}

// TestGHProxy_PRChecksFlattensTheRollup — the rollup arrives nested three
// levels deep inside the head commit, and the flat array is what every caller
// of this verb reads.
func TestGHProxy_PRChecksFlattensTheRollup(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.def = ghCanned{Status: 200, Body: `{"data":{"repository":{"pullRequest":{
		"number": 42, "url": "https://github.com/tofutools/tclaude/pull/42",
		"commits": {"nodes": [{"commit": {"statusCheckRollup": {"contexts": {"nodes": [
			{"__typename": "CheckRun", "name": "build", "status": "COMPLETED",
			 "conclusion": "FAILURE", "detailsUrl": "https://github.com/x/actions/runs/9/job/1",
			 "checkSuite": {"workflowRun": {"workflow": {"name": "CI"}}}},
			{"__typename": "StatusContext", "context": "codecov/patch",
			 "state": "SUCCESS", "targetUrl": "https://codecov.io/x"}
		]}}}}]}
	}}}}`}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/checks", map[string]any{"number": 42})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	var out struct {
		JSON struct {
			Number int `json:"number"`
			Rollup []struct {
				TypeName     string `json:"__typename"`
				Name         string `json:"name"`
				WorkflowName string `json:"workflowName"`
				Conclusion   string `json:"conclusion"`
				DetailsURL   string `json:"detailsUrl"`
				Context      string `json:"context"`
				State        string `json:"state"`
			} `json:"statusCheckRollup"`
		} `json:"json"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	assert.Equal(t, 42, out.JSON.Number)
	require.Len(t, out.JSON.Rollup, 2)
	assert.Equal(t, "CheckRun", out.JSON.Rollup[0].TypeName)
	assert.Equal(t, "build", out.JSON.Rollup[0].Name)
	assert.Equal(t, "CI", out.JSON.Rollup[0].WorkflowName)
	// The run id an agent walks to `run log-failed` with lives in this URL, so
	// losing it would break the documented route from "CI is red" to "why".
	assert.Contains(t, out.JSON.Rollup[0].DetailsURL, "/actions/runs/9/")
	assert.Equal(t, "StatusContext", out.JSON.Rollup[1].TypeName)
	assert.Equal(t, "codecov/patch", out.JSON.Rollup[1].Context)
}

// TestGHProxy_PRChecksDistinguishesNoRollupFromAnEmptyOne — `null` means the
// head commit has no rollup at all (nothing has reported yet); `[]` means it
// has one with nothing in it. An agent waiting for CI acts on the difference.
func TestGHProxy_PRChecksDistinguishesNoRollupFromAnEmptyOne(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.def = ghCanned{Status: 200, Body: `{"data":{"repository":{"pullRequest":{
		"number": 42, "url": "https://x/42", "commits": {"nodes": [{"commit": {"statusCheckRollup": null}}]}
	}}}}`}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/checks", map[string]any{"number": 42})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), `"statusCheckRollup":null`)
}

// TestGHProxy_PREditSendsTitleAndBody — editing a description is a write under
// the operator's identity, so it sits behind proxy.github.write, and the repository
// is derived rather than named.
func TestGHProxy_PREditSendsTitleAndBody(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.def = ghCanned{Status: 200, Body: `{"html_url":"https://github.com/tofutools/tclaude/pull/1925"}`}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubWrite, "test"))

	const prose = "a rewritten description with `backticks` and\nnewlines"
	res := gitProxyPost(t, f, "/v1/github/pr/edit",
		map[string]any{"number": 1925, "title": "New title", "body": prose})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	call := gh.only(t)
	assert.Equal(t, http.MethodPatch, call.Method)
	assert.Equal(t, "repos/tofutools/tclaude/pulls/1925", call.Path)
	body := jsonBody(t, call)
	assert.Equal(t, "New title", body["title"])
	assert.Equal(t, prose, body["body"])
	// Narrow on purpose: base, reviewers and labels are not this verb's to move.
	assert.Len(t, body, 2)
}

// TestGHProxy_PREditRequiresWriteAndSomethingToChange.
func TestGHProxy_PREditRequiresWriteAndSomethingToChange(t *testing.T) {
	t.Run("proxy.github.read does not confer it", func(t *testing.T) {
		f, git, gh := ghWorld(t, []string{"github.com/tofutools"})
		require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))
		res := gitProxyPost(t, f, "/v1/github/pr/edit", map[string]any{"number": 1, "body": "x"})
		assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
		assert.Equal(t, 0, gh.count())
		assert.False(t, git.sawAnySubprocess())
	})

	t.Run("an empty edit is refused rather than sent", func(t *testing.T) {
		f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
		require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubWrite, "test"))
		res := gitProxyPost(t, f, "/v1/github/pr/edit", map[string]any{"number": 1})
		assert.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
		assert.Contains(t, res.Body.String(), "nothing to edit")
		assert.Equal(t, 0, gh.count())
	})
}

// TestGHProxy_PRReadyUsesTheMutationRESTCannotExpress — REST will not clear a
// pull request's draft flag, so this is the one write that has to resolve a
// node id and call GraphQL. A PATCH with `draft: false` succeeds and does
// nothing, which is exactly the failure this pins against.
func TestGHProxy_PRReadyUsesTheMutationRESTCannotExpress(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.seq = []ghCanned{
		{Status: 200, Body: `{"data":{"repository":{"pullRequest":{"id":"PR_node1","number":42,"url":"https://x/42","isDraft":true}}}}`},
		{Status: 200, Body: `{"data":{"markPullRequestReadyForReview":{"pullRequest":{"number":42,"isDraft":false}}}}`},
	}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubWrite, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/ready", map[string]any{"number": 42})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	calls := gh.requests()
	require.Len(t, calls, 2)
	doc, vars := graphqlVars(t, calls[1])
	assert.Contains(t, doc, "markPullRequestReadyForReview")
	assert.Equal(t, "PR_node1", vars["id"], "the node id must come from the lookup, not from the caller")
}

// TestGHProxy_PRReadyOnANonDraftIsNotAFailure — the caller asked for a state
// the pull request is already in. Reporting that as an error would have an
// agent retrying, or backing out a change that is fine.
func TestGHProxy_PRReadyOnANonDraftIsNotAFailure(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.def = ghCanned{Status: 200, Body: `{"data":{"repository":{"pullRequest":{"id":"PR_1","number":42,"url":"https://x/42","isDraft":false}}}}`}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubWrite, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/ready", map[string]any{"number": 42})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	var out struct {
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	assert.Equal(t, 0, out.ExitCode)
	assert.Contains(t, out.Stdout, "already ready for review")
	assert.Equal(t, 1, gh.count(), "no mutation is sent")
}

// ---------------------------------------------------------------------------
// Merging
// ---------------------------------------------------------------------------

// ghMergeablePR is a pull request in the one state a merge may proceed from.
const ghMergeablePR = `{"data":{"repository":{"pullRequest":{
	"id":"PR_1","number":42,"url":"https://x/42","isDraft":false,
	"state":"OPEN","mergeable":"MERGEABLE","baseRefName":"main"}}}}`

// TestGHProxy_MergeNeedsItsOwnSlug is the whole reason proxy.github.merge
// exists. Opening a pull request proposes a change; merging one lands it on the
// base branch. An agent granted the write slug so it can write its own work up
// must not thereby be able to decide that work ships.
func TestGHProxy_MergeNeedsItsOwnSlug(t *testing.T) {
	for _, slug := range []string{agentd.PermGitHubRead, agentd.PermGitHubWrite} {
		t.Run(slug+" does not confer it", func(t *testing.T) {
			f, git, gh := ghWorld(t, []string{"github.com/tofutools"})
			require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, slug, "test"))

			res := gitProxyPost(t, f, "/v1/github/pr/merge", map[string]any{"number": 42})
			assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, 0, gh.count(), "a denied caller spends no credential")
			assert.False(t, git.sawAnySubprocess(), "and runs nothing at all")
		})
	}

	t.Run("granted", func(t *testing.T) {
		f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
		gh.seq = []ghCanned{
			{Status: 200, Body: ghMergeablePR},
			{Status: 200, Body: `{"sha":"abc1234","merged":true,"message":"Pull Request successfully merged"}`},
		}
		require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubMerge, "test"))

		res := gitProxyPost(t, f, "/v1/github/pr/merge", map[string]any{"number": 42})
		require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

		var out struct {
			ExitCode int    `json:"exit_code"`
			Stdout   string `json:"stdout"`
		}
		require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
		assert.Equal(t, 0, out.ExitCode)
		// The commit and the branch it landed on, because "merged" alone does
		// not say what happened or where to look for it.
		assert.Contains(t, out.Stdout, "merged #42 into main as abc1234")
		assert.Contains(t, out.Stdout, "https://x/42")

		// Merging under the operator's GitHub account is exactly the kind of
		// call an operator reviews later.
		row := auditRowByVerb(t, "github.pr.merge")
		assert.Contains(t, row.Detail, "tofutools/tclaude")
		assert.Contains(t, row.Detail, "exit=0")
	})
}

// TestGHProxy_MergeBuildsTheRequestFromDerivedAndGatedValues — the repository
// still comes from the agent's own allow-listed remote, the method from the
// daemon's allow-list, and the commit message travels in the request body.
func TestGHProxy_MergeBuildsTheRequestFromDerivedAndGatedValues(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.seq = []ghCanned{
		{Status: 200, Body: ghMergeablePR},
		{Status: 200, Body: `{"sha":"abc1234","merged":true}`},
	}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubMerge, "test"))

	const prose = "why this lands, with `backticks` and\nnewlines"
	res := gitProxyPost(t, f, "/v1/github/pr/merge", map[string]any{
		"number": 42, "method": "SQUASH", "subject": "Land the thing", "body": prose,
		"repo": "attacker/exfil", "owner": "attacker",
	})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	calls := gh.requests()
	require.Len(t, calls, 2, "the state read, then the merge")
	assert.Equal(t, http.MethodPut, calls[1].Method)
	assert.Equal(t, "repos/tofutools/tclaude/pulls/42/merge", calls[1].Path)
	body := jsonBody(t, calls[1])
	assert.Equal(t, "squash", body["merge_method"], "the gate lower-cases into its own literal")
	assert.Equal(t, "Land the thing", body["commit_title"])
	assert.Equal(t, prose, body["commit_message"])
	// Narrow on purpose: `sha` and deleting the head branch are not this verb's.
	assert.Len(t, body, 3)
	assert.NotContains(t, string(calls[1].Body), "attacker")
	assert.NotContains(t, calls[1].Path, "attacker")
}

// TestGHProxy_MergeDefaultsToAMergeCommit — an omitted method must resolve to
// the one that preserves the commits as they were reviewed, and it must be
// SENT: leaving merge_method out lets GitHub's own default decide instead.
func TestGHProxy_MergeDefaultsToAMergeCommit(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.seq = []ghCanned{
		{Status: 200, Body: ghMergeablePR},
		{Status: 200, Body: `{"sha":"abc1234","merged":true}`},
	}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubMerge, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/merge", map[string]any{"number": 42})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	calls := gh.requests()
	require.Len(t, calls, 2)
	body := jsonBody(t, calls[1])
	assert.Equal(t, "merge", body["merge_method"])
	// An empty commit_title is not "GitHub's default title", it is an empty
	// first line — so an unsupplied one is omitted rather than sent blank.
	assert.Len(t, body, 1)
}

// TestGHProxy_MergeRefusesAnUnknownMethod — the value that reaches GitHub is
// one of the daemon's literals or the call does not happen.
func TestGHProxy_MergeRefusesAnUnknownMethod(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubMerge, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/merge",
		map[string]any{"number": 42, "method": "fast-forward"})
	assert.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "merge method",
		"the refusal must name the parameter the caller got wrong")
	assert.Equal(t, 0, gh.count())
}

// TestGHProxy_MergePreflightTellsTheUnmergeableCasesApart — GitHub answers
// every one of these with the same 405 "Pull Request is not mergeable", which
// an agent cannot act on. Reading the state first is what makes each a distinct
// answer, and makes the already-merged case a success rather than a retry.
func TestGHProxy_MergePreflightTellsTheUnmergeableCasesApart(t *testing.T) {
	for _, tc := range []struct {
		name     string
		pr       string
		wantExit int
		wantText string
	}{
		{"already merged", `"state":"MERGED","mergeable":"UNKNOWN"`, 0, "already merged"},
		{"closed", `"state":"CLOSED","mergeable":"UNKNOWN"`, 1, "is closed"},
		{"still a draft", `"state":"OPEN","isDraft":true,"mergeable":"MERGEABLE"`, 1, "is a draft"},
		{"conflicting", `"state":"OPEN","mergeable":"CONFLICTING"`, 1, "conflicts with main"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
			gh.def = ghCanned{Status: 200, Body: `{"data":{"repository":{"pullRequest":{
				"id":"PR_1","number":42,"url":"https://x/42","baseRefName":"main",` + tc.pr + `}}}}`}
			require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubMerge, "test"))

			res := gitProxyPost(t, f, "/v1/github/pr/merge", map[string]any{"number": 42})
			require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

			var out struct {
				ExitCode int    `json:"exit_code"`
				Stdout   string `json:"stdout"`
				Stderr   string `json:"stderr"`
			}
			require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
			assert.Equal(t, tc.wantExit, out.ExitCode)
			assert.Contains(t, out.Stdout+out.Stderr, tc.wantText)
			assert.Equal(t, 1, gh.count(), "the merge itself is never sent")
		})
	}
}

// TestGHProxy_MergeProceedsWhileMergeabilityIsStillUnknown — GitHub computes
// `mergeable` asynchronously and reports UNKNOWN for a while after every push.
// Refusing on it would refuse perfectly mergeable pull requests depending only
// on how recently they were touched, so only a definite CONFLICTING stops the
// merge; everything else is GitHub's to judge at merge time.
func TestGHProxy_MergeProceedsWhileMergeabilityIsStillUnknown(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.seq = []ghCanned{
		{Status: 200, Body: `{"data":{"repository":{"pullRequest":{
			"id":"PR_1","number":42,"url":"https://x/42","isDraft":false,
			"state":"OPEN","mergeable":"UNKNOWN","baseRefName":"main"}}}}`},
		{Status: 200, Body: `{"sha":"abc1234","merged":true}`},
	}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubMerge, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/merge", map[string]any{"number": 42})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, 2, gh.count(), "the merge is attempted")
}

// TestGHProxy_MergeReportsGitHubsRefusalVerbatim — branch protection, a
// required review and a failing required check are all decided by GitHub
// against the operator's account. The proxy does not second-guess them; it
// relays the refusal, because "At least 1 approving review is required" is the
// actionable part.
func TestGHProxy_MergeReportsGitHubsRefusalVerbatim(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.seq = []ghCanned{
		{Status: 200, Body: ghMergeablePR},
		{Status: 405, Body: `{"message":"At least 1 approving review is required by reviewers with write access."}`},
	}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubMerge, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/merge", map[string]any{"number": 42})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	var out struct {
		ExitCode int    `json:"exit_code"`
		Stderr   string `json:"stderr"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	assert.NotEqual(t, 0, out.ExitCode)
	assert.Contains(t, out.Stderr, "At least 1 approving review is required")
}

// ---------------------------------------------------------------------------
// Issues
// ---------------------------------------------------------------------------

// TestGHProxy_IssueListDoesNotReturnPullRequests is the regression for the one
// bug a REST implementation of this verb would have.
//
// GitHub models a pull request as an issue with extra parts, and REST's
// /issues endpoint returns both. This verb goes through GraphQL's
// repository.issues, which does not — so the pin is on the document, because a
// listing full of pull requests looks perfectly healthy until someone notices
// the numbers do not match.
func TestGHProxy_IssueListDoesNotReturnPullRequests(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.def = ghCanned{Status: 200, Body: `{"data":{"repository":{"issues":{"nodes":[
		{"number": 7, "title": "a real issue", "state": "OPEN", "url": "https://x/7",
		 "author": {"__typename":"User","login":"mikes","id":"U_1","name":"Mikael"},
		 "labels": {"nodes": [{"id":"L_1","name":"bug","description":"","color":"d73a4a"}]}}
	]}}}}`}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/issue/list", map[string]any{"state": "open"})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	doc, vars := graphqlVars(t, gh.only(t))
	assert.Contains(t, doc, "issues(", "REST's /issues would include pull requests")
	assert.NotContains(t, doc, "pullRequests(")
	assert.Equal(t, []any{"OPEN"}, vars["states"])

	var out struct {
		JSON json.RawMessage `json:"json"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	assert.Contains(t, string(out.JSON), `"name":"bug"`)
}

// TestGHProxy_IssueViewRendersEmptyCollectionsAsArrays — an issue with no
// labels must render `[]`, not `null`. A caller iterating the field should not
// have to special-case the empty case.
func TestGHProxy_IssueViewRendersEmptyCollectionsAsArrays(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.def = ghCanned{Status: 200, Body: `{"data":{"repository":{"issue":{
		"number": 7, "title": "bare", "state": "OPEN", "url": "https://x/7", "body": ""
	}}}}`}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/issue/view", map[string]any{"number": 7})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), `"labels":[]`)
	assert.Contains(t, res.Body.String(), `"assignees":[]`)
}

// ---------------------------------------------------------------------------
// Pull-request comments
// ---------------------------------------------------------------------------

// ghCommentsWorld scripts the three reads `pr comments` makes.
func ghCommentsWorld(t *testing.T, comments, reviews, inline string) (*testharness.Flow, *ghRecorder) {
	t.Helper()
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.route = func(req agentd.GitHubRequestForTest) (ghCanned, bool) {
		switch req.Path {
		case "repos/tofutools/tclaude/issues/42/comments":
			return ghCanned{Status: 200, Body: comments}, true
		case "repos/tofutools/tclaude/pulls/42/reviews":
			return ghCanned{Status: 200, Body: reviews}, true
		case "repos/tofutools/tclaude/pulls/42/comments":
			return ghCanned{Status: 200, Body: inline}, true
		}
		return ghCanned{}, false
	}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))
	return f, gh
}

func ghStdout(t *testing.T, body []byte) string {
	t.Helper()
	var out struct {
		Stdout string `json:"stdout"`
	}
	require.NoError(t, json.Unmarshal(body, &out))
	return out.Stdout
}

// TestGHProxy_PRCommentsReadsEveryPlaceFeedbackLives is the regression for the
// verb answering only part of the question.
//
// GitHub keeps pull-request feedback in three stores. A review bot posts its
// summary as a review BODY and every actionable finding as an INLINE comment,
// so a `pr comments` that read only the conversation would report "reviewed, no
// findings" on a PR with thirty of them — a wrong answer that reads exactly
// like a right one.
func TestGHProxy_PRCommentsReadsEveryPlaceFeedbackLives(t *testing.T) {
	f, gh := ghCommentsWorld(t,
		`[{"user":{"login":"mikes"},"author_association":"OWNER","body":"a human said something",
		   "created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]`,
		`[{"user":{"login":"coderabbitai"},"author_association":"NONE","body":"summary of the review",
		   "state":"CHANGES_REQUESTED","submitted_at":"2026-01-02T00:00:00Z"}]`,
		`[{"path":"pkg/x/y.go","line":42,"user":{"login":"coderabbitai"},
		   "created_at":"2026-01-02T00:00:01Z","html_url":"https://x/#r1","body":"nit: rename this"}]`)

	res := gitProxyPost(t, f, "/v1/github/pr/comments", map[string]any{"number": 42})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	var paths []string
	for _, c := range gh.requests() {
		paths = append(paths, c.Path)
	}
	assert.ElementsMatch(t, []string{
		"repos/tofutools/tclaude/issues/42/comments",
		"repos/tofutools/tclaude/pulls/42/reviews",
		"repos/tofutools/tclaude/pulls/42/comments",
	}, paths, "every store has to be read")
	for _, c := range gh.requests() {
		assert.Equal(t, http.MethodGet, orGet(c.Method),
			"reading a thread must never issue a write with the operator's credential")
	}

	stdout := ghStdout(t, res.Body.Bytes())
	assert.Contains(t, stdout, "conversation")
	assert.Contains(t, stdout, "inline review comments")
	assert.Contains(t, stdout, "a human said something")
	assert.Contains(t, stdout, "summary of the review")
	assert.Contains(t, stdout, "changes requested")
	assert.Contains(t, stdout, "nit: rename this")
	assert.Contains(t, stdout, "pkg/x/y.go:42")
	// Chronological across the two conversation sources, so an argument reads
	// in the order it happened rather than issue-comments-then-reviews.
	assert.Less(t, strings.Index(stdout, "a human said something"),
		strings.Index(stdout, "summary of the review"))
}

func orGet(method string) string {
	if method == "" {
		return http.MethodGet
	}
	return method
}

// TestGHProxy_PRCommentsIsARead — reading the thread must sit behind
// proxy.github.read, not proxy.github.write. The route name is one character
// away from the verb that PUBLISHES a comment as the operator, so getting this
// backwards would either lock an agent out of reading or, worse, let a
// read-only agent through to the write path.
func TestGHProxy_PRCommentsIsARead(t *testing.T) {
	f, _ := ghCommentsWorld(t, `[]`, `[]`,
		`[{"path":"a.go","line":1,"user":{"login":"coderabbitai"},"body":"nit: rename this"}]`)

	res := gitProxyPost(t, f, "/v1/github/pr/comments", map[string]any{"number": 42})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	// The thread is prose, not JSON, so it must survive as stdout rather than
	// being filed as a document.
	assert.Contains(t, ghStdout(t, res.Body.Bytes()), "nit: rename this")
}

// TestGHProxy_PRCommentsReportsAMissingLinePositionHonestly — GitHub nulls
// `line` once the code a comment was written against has changed.
// `original_line` still says where it was, and "?" is the honest answer when
// even that is gone.
func TestGHProxy_PRCommentsReportsAMissingLinePositionHonestly(t *testing.T) {
	f, _ := ghCommentsWorld(t, `[]`, `[]`, `[
		{"path":"a.go","line":null,"original_line":17,"user":{"login":"x"},"body":"moved"},
		{"path":"b.go","line":null,"original_line":null,"user":{"login":"x"},"body":"gone"}
	]`)

	res := gitProxyPost(t, f, "/v1/github/pr/comments", map[string]any{"number": 42})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	stdout := ghStdout(t, res.Body.Bytes())
	assert.Contains(t, stdout, "a.go:17")
	assert.Contains(t, stdout, "b.go:?")
}

// TestGHProxy_PRCommentsStillReportsTheConversationWhenTheInlineReadFails —
// half an answer must not be served as a whole one. If the inline read fails,
// the agent has to learn that it is missing exactly the section a review bot
// files its findings in, rather than reading "no inline review comments".
func TestGHProxy_PRCommentsStillReportsTheConversationWhenTheInlineReadFails(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.route = func(req agentd.GitHubRequestForTest) (ghCanned, bool) {
		switch req.Path {
		case "repos/tofutools/tclaude/issues/42/comments":
			return ghCanned{Status: 200, Body: `[{"user":{"login":"mikes"},"body":"the human said something"}]`}, true
		case "repos/tofutools/tclaude/pulls/42/reviews":
			return ghCanned{Status: 200, Body: `[]`}, true
		case "repos/tofutools/tclaude/pulls/42/comments":
			return ghCanned{Status: 404, Body: `{"message":"Not Found"}`}, true
		}
		return ghCanned{}, false
	}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/comments", map[string]any{"number": 42})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	var out struct {
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		ExitCode int    `json:"exit_code"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	assert.Contains(t, out.Stdout, "the human said something", "the half that worked is still worth having")
	assert.Contains(t, out.Stdout, "could not be read")
	assert.NotContains(t, out.Stdout, "(no inline review comments)",
		"a failed read must never render as an empty one")
	assert.NotEqual(t, 0, out.ExitCode, "and the failure has to be visible in the exit code")
	assert.Contains(t, out.Stderr, "Not Found")
}

// TestGHProxy_PRCommentsSurvivesATransportFailureOnTheInlineRead — the same
// half-answer contract as the refusal case, but for the path that would
// otherwise become a bare 502 with no body at all, discarding a conversation
// the daemon had already read successfully.
func TestGHProxy_PRCommentsSurvivesATransportFailureOnTheInlineRead(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.route = func(req agentd.GitHubRequestForTest) (ghCanned, bool) {
		if req.Path == "repos/tofutools/tclaude/pulls/42/comments" {
			return ghCanned{Err: errors.New("dial tcp: network is unreachable")}, true
		}
		if req.Path == "repos/tofutools/tclaude/issues/42/comments" {
			return ghCanned{Status: 200, Body: `[{"user":{"login":"mikes"},"body":"the human said something"}]`}, true
		}
		return ghCanned{Status: 200, Body: `[]`}, true
	}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/comments", map[string]any{"number": 42})
	require.Equal(t, http.StatusOK, res.Code,
		"a 502 here would throw away a conversation that was read successfully")

	var out struct {
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		ExitCode int    `json:"exit_code"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	assert.Contains(t, out.Stdout, "the human said something")
	assert.Contains(t, out.Stdout, "could not be read")
	assert.NotEqual(t, 0, out.ExitCode)
	assert.Contains(t, out.Stderr, "network is unreachable")
}

// TestGHProxy_PRCommentsStopsWhenThePRCannotBeRead — no number, no access, no
// network. The remaining reads would fail identically and say so three times.
func TestGHProxy_PRCommentsStopsWhenThePRCannotBeRead(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.def = ghCanned{Status: 404, Body: `{"message":"Not Found"}`}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/comments", map[string]any{"number": 999999})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, 1, gh.count(), "a PR that cannot be read is not read three times")
}

// TestGHProxy_PRCommentsFollowsPagination — a long-running pull request runs
// past one page, and a conversation silently cut at 100 entries is the failure
// mode this verb exists to avoid.
func TestGHProxy_PRCommentsFollowsPagination(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	const next = "https://api.github.com/repositories/1/issues/42/comments?page=2"
	gh.route = func(req agentd.GitHubRequestForTest) (ghCanned, bool) {
		switch req.Path {
		case "repos/tofutools/tclaude/issues/42/comments":
			return ghCanned{
				Status: 200,
				Body:   `[{"user":{"login":"a"},"body":"page one"}]`,
				Header: http.Header{"Link": []string{`<` + next + `>; rel="next"`}},
			}, true
		case next:
			return ghCanned{Status: 200, Body: `[{"user":{"login":"b"},"body":"page two"}]`}, true
		}
		return ghCanned{Status: 200, Body: `[]`}, true
	}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/comments", map[string]any{"number": 42})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	stdout := ghStdout(t, res.Body.Bytes())
	assert.Contains(t, stdout, "page one")
	assert.Contains(t, stdout, "page two")
}

// TestGHProxy_PRCommentsBudgetsEveryCallTogether — several reads under ONE
// budget, so the daemon's worst case stays the number the CLI is waiting on
// rather than the sum of whatever the verb happens to do next.
func TestGHProxy_PRCommentsBudgetsEveryCallTogether(t *testing.T) {
	f, gh := ghCommentsWorld(t, `[]`, `[]`, `[]`)

	res := gitProxyPost(t, f, "/v1/github/pr/comments", map[string]any{"number": 42})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	gh.mu.Lock()
	defer gh.mu.Unlock()
	require.NotEmpty(t, gh.budgets)
	for i, budget := range gh.budgets {
		assert.LessOrEqual(t, budget, 90*time.Second,
			"call %d must run inside the verb's total budget, not its own fresh one", i)
		assert.Greater(t, budget, time.Second, "call %d has no usable budget at all", i)
	}
}

// ---------------------------------------------------------------------------
// Workflow runs
// ---------------------------------------------------------------------------

// TestGHProxy_RunListIsTheRouteToARunID — `run log-failed` takes an id derived
// from nothing, so an agent needs a way to find one. This is it, and the field
// that makes it usable is databaseId: without it the listing names runs it
// gives no way to open.
func TestGHProxy_RunListIsTheRouteToARunID(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	// 9007199254740993 is 2^53+1, the smallest integer float64 cannot hold. A
	// real run id (~3x10^10) round-trips through float64 exactly, so a fixture
	// using one would pass against the very implementation this is meant to
	// exclude — an unmarshal-into-any-then-remarshal anywhere on the path,
	// which would silently hand the agent ...992 and send it looking at a run
	// that does not exist.
	gh.def = ghCanned{Status: 200, Body: `{"workflow_runs":[
		{"id":9007199254740993,"conclusion":"failure","status":"completed","run_attempt":2,
		 "head_sha":"abc","name":"CI","display_title":"fix it","head_branch":"feat/thing",
		 "event":"push","created_at":"2026-01-01T00:00:00Z","html_url":"https://x/9"}
	]}`}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/run/list", map[string]any{
		"branch": "feat/thing", "status": "failure", "limit": 5,
	})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	call := gh.only(t)
	assert.Equal(t, "repos/tofutools/tclaude/actions/runs", call.Path)
	assert.Equal(t, "feat/thing", call.Query.Get("branch"))
	assert.Equal(t, "failure", call.Query.Get("status"))
	assert.Equal(t, "5", call.Query.Get("per_page"))

	var out struct {
		JSON json.RawMessage `json:"json"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	assert.Contains(t, string(out.JSON), "9007199254740993",
		"a float64 round-trip would render this as 9007199254740992")
	assert.Contains(t, string(out.JSON), `"databaseId"`)
	assert.Contains(t, string(out.JSON), `"attempt":2`)
	assert.Contains(t, string(out.JSON), `"workflowName":"CI"`)
}

// TestGHProxy_RunListRefusesInjectionShapedFilters — the filters are
// caller-supplied and reach a query string, so both are gated. An unfiltered
// listing is a legitimate request; a listing filtered by "--exec=id" is not.
func TestGHProxy_RunListRefusesInjectionShapedFilters(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	for _, body := range []map[string]any{
		{"branch": "--exec=id"},
		{"status": "; rm -rf /"},
		{"status": "definitely-not-a-status"},
		{"limit": 100000},
	} {
		res := gitProxyPost(t, f, "/v1/github/run/list", body)
		assert.Equal(t, http.StatusBadRequest, res.Code, "body=%v got=%s", body, res.Body.String())
	}
	assert.Equal(t, 0, gh.count(), "no invalid filter may reach GitHub")
}

// TestGHProxy_RunListOmitsFiltersItWasNotGiven — an empty status must mean "no
// filter", not a default one. validateGHState (used by `pr ls`) treats empty as
// "take the first allowed value", and inheriting that here would silently show
// only queued runs while looking like a complete listing.
func TestGHProxy_RunListOmitsFiltersItWasNotGiven(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.def = ghCanned{Status: 200, Body: `{"workflow_runs":[]}`}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/run/list", map[string]any{})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	call := gh.only(t)
	assert.False(t, call.Query.Has("status"), "an unasked-for status filter hides runs")
	assert.False(t, call.Query.Has("branch"))
	assert.Equal(t, "20", call.Query.Get("per_page"), "the default limit")
	// An empty listing renders as an array, not as null.
	assert.Contains(t, res.Body.String(), `"json":[]`)
}

// TestGHProxy_RunLogFailedRefusesAnUnrepresentableRunID — a run id above 2^53
// did not survive JSON intact, so echoing it back would query a different run
// than the caller named.
func TestGHProxy_RunLogFailedRefusesAnUnrepresentableRunID(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	for _, id := range []any{0, -1, 1 << 60} {
		res := gitProxyPost(t, f, "/v1/github/run/log-failed", map[string]any{"run_id": id})
		assert.Equal(t, http.StatusBadRequest, res.Code, "run_id=%v body=%s", id, res.Body.String())
	}
	assert.Equal(t, 0, gh.count())
}

// TestGHProxy_RunLogFailedAcceptsARealRunID is the regression for bounding a
// run id with the PR-number validator. GitHub Actions run ids are global
// database ids already past 10^10, so a ceiling sized for per-repository PR
// numbers would refuse every run that exists.
//
// It also pins the containment: a run id is the one scalar the agent supplies
// freely, so every request it produces has to be built under the DERIVED slug.
// GitHub 404s a run id belonging to another repository.
func TestGHProxy_RunLogFailedAcceptsARealRunID(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.route = ghRunLogRoute()
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/run/log-failed", map[string]any{
		"run_id": 18234567890, "repo": "attacker/exfil",
	})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	for _, call := range gh.requests() {
		assert.True(t, strings.HasPrefix(call.Path, "repos/tofutools/tclaude/"),
			"every request must be built under the derived slug, got %q", call.Path)
	}
}

// ghRunLogRoute scripts a completed run with one failed job. Its log archive
// and per-job logs are bulk transfers, so they are registered on the stream
// seam (gh.zips) rather than here.
func ghRunLogRoute() func(agentd.GitHubRequestForTest) (ghCanned, bool) {
	return func(req agentd.GitHubRequestForTest) (ghCanned, bool) {
		switch req.Path {
		case "repos/tofutools/tclaude/actions/runs/18234567890":
			return ghCanned{Status: 200, Body: `{"status":"completed","conclusion":"failure"}`}, true
		case "repos/tofutools/tclaude/actions/runs/18234567890/jobs":
			return ghCanned{Status: 200, Body: `{"jobs":[{"id":501,"name":"build","status":"completed",
				"conclusion":"failure","steps":[{"name":"Test","number":3,"conclusion":"failure"}]}]}`}, true
		}
		return ghCanned{Status: 200, Body: `{}`}, true
	}
}

// TestGHProxy_RunLogFailedWaitsForTheRunToComplete — a run still in progress
// has no complete log archive, and "nothing failed" is what an empty answer
// reads as. The two must not be confusable.
func TestGHProxy_RunLogFailedWaitsForTheRunToComplete(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.def = ghCanned{Status: 200, Body: `{"status":"in_progress"}`}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/run/log-failed", map[string]any{"run_id": 18234567890})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	var out struct {
		ExitCode int    `json:"exit_code"`
		Stderr   string `json:"stderr"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	assert.NotEqual(t, 0, out.ExitCode)
	assert.Contains(t, out.Stderr, "in progress")
	assert.Equal(t, 1, gh.count(), "a run that is not finished is not fetched")
}

// TestGHProxy_RunLogFailedOnAGreenRunIsSilent — no failed steps prints nothing
// and succeeds. Silence means the run is green, not that the read failed, and
// the docs promise exactly that.
func TestGHProxy_RunLogFailedOnAGreenRunIsSilent(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.route = func(req agentd.GitHubRequestForTest) (ghCanned, bool) {
		switch req.Path {
		case "repos/tofutools/tclaude/actions/runs/18234567890":
			return ghCanned{Status: 200, Body: `{"status":"completed","conclusion":"success"}`}, true
		case "repos/tofutools/tclaude/actions/runs/18234567890/jobs":
			return ghCanned{Status: 200, Body: `{"jobs":[{"id":1,"name":"build","conclusion":"success",
				"steps":[{"name":"Test","number":1,"conclusion":"success"}]}]}`}, true
		}
		return ghCanned{}, false
	}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/run/log-failed", map[string]any{"run_id": 18234567890})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	var out struct {
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	assert.Equal(t, 0, out.ExitCode)
	assert.Empty(t, out.Stdout)
	assert.Empty(t, gh.streamed(), "a green run costs no log-archive transfer")
}

// TestGHProxy_RunLogFailedLabelsEveryLine — a matrix build interleaves output
// from several jobs, and a log with no job or step column is unattributable.
func TestGHProxy_RunLogFailedLabelsEveryLine(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.route = ghRunLogRoute()
	gh.zips["repos/tofutools/tclaude/actions/runs/18234567890/logs"] = zipOf(t, map[string]string{
		"build/3_Test.txt": "--- FAIL: TestThing\n    thing_test.go:12: boom\n",
	})
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/run/log-failed", map[string]any{"run_id": 18234567890})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	stdout := ghStdout(t, res.Body.Bytes())
	assert.Equal(t,
		"build\tTest\t--- FAIL: TestThing\nbuild\tTest\t    thing_test.go:12: boom\n",
		stdout)
}

// TestGHProxy_RunLogFailedFallsBackToTheJobLog — GitHub does not always put a
// job's steps in the run archive. Reporting a red job with no explanation is
// the one outcome worse than reporting more log than was asked for.
func TestGHProxy_RunLogFailedFallsBackToTheJobLog(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.route = ghRunLogRoute()
	// An archive with nothing for this job in it.
	gh.zips["repos/tofutools/tclaude/actions/runs/18234567890/logs"] = zipOf(t, map[string]string{
		"other-job/1_Checkout.txt": "irrelevant\n",
	})
	gh.zips["repos/tofutools/tclaude/actions/jobs/501/logs"] = []byte("the whole job log\n")
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/run/log-failed", map[string]any{"run_id": 18234567890})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, "build\tTest\tthe whole job log\n", ghStdout(t, res.Body.Bytes()))
}

// TestGHProxy_RunLogFailedSurvivesAnUnreadableArchive — an archive that has
// expired or was never assembled must not cost the answer entirely; the
// per-job read reaches the same text one request at a time.
func TestGHProxy_RunLogFailedSurvivesAnUnreadableArchive(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.route = ghRunLogRoute()
	gh.zips["repos/tofutools/tclaude/actions/jobs/501/logs"] = []byte("recovered from the job endpoint\n")
	gh.streamErrs["repos/tofutools/tclaude/actions/runs/18234567890/logs"] = errors.New("410 Gone")
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/run/log-failed", map[string]any{"run_id": 18234567890})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, ghStdout(t, res.Body.Bytes()), "recovered from the job endpoint")
}

// ---------------------------------------------------------------------------
// Cross-cutting
// ---------------------------------------------------------------------------

// TestGHProxy_RefusesInjectionShapedParameters — every scalar a caller supplies
// is validated before it can reach a request.
func TestGHProxy_RefusesInjectionShapedParameters(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
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
		{"base branch that would traverse the URL", "/v1/github/pr/create",
			map[string]any{"title": "ok", "body": "x", "base": "../../attacker/exfil/pulls"}},
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
	assert.Equal(t, 0, gh.count(), "no invalid request may reach GitHub")
}

// TestGHProxy_TitleLimitCountsCharactersNotBytes — the limit and the refusal
// message are both stated in characters, and so is GitHub's own. Counting
// bytes would refuse a perfectly ordinary non-ASCII title at roughly a third of
// the advertised length.
func TestGHProxy_TitleLimitCountsCharactersNotBytes(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.def = ghCanned{Status: 201, Body: `{"number":1,"html_url":"https://x/1"}`}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubWrite, "test"))

	// 200 CJK characters: 600 bytes, comfortably under the 256-character limit.
	res := gitProxyPost(t, f, "/v1/github/pr/create",
		map[string]any{"title": strings.Repeat("修", 200), "body": "x", "base": "main"})
	assert.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
}

// TestGHProxy_FailureIsAnAnswer mirrors the git side: HTTP 200 means the daemon
// REACHED GitHub; GitHub's own message is what tells the agent what went wrong.
func TestGHProxy_FailureIsAnAnswer(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.def = ghCanned{Status: 422, Body: `{"message":"Validation Failed","errors":[
		{"resource":"PullRequest","field":"base","code":"invalid",
		 "message":"No commits between main and feat/thing"}]}`}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubWrite, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/create",
		map[string]any{"title": "ok", "body": "x", "base": "main"})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	var out struct {
		ExitCode int    `json:"exit_code"`
		Stderr   string `json:"stderr"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	assert.Equal(t, 1, out.ExitCode)
	assert.Contains(t, out.Stderr, "Validation Failed")
	assert.Contains(t, out.Stderr, "No commits between",
		"GitHub's own field-level diagnosis is the actionable part")
}

// TestGHProxy_GraphQLErrorsAreAnAnswerToo — GraphQL reports application errors
// with HTTP 200 as often as with 4xx, so a handler that judged the status alone
// would render an error document as a successful empty result.
func TestGHProxy_GraphQLErrorsAreAnAnswerToo(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.def = ghCanned{Status: 200, Body: `{"data":null,"errors":[
		{"type":"FORBIDDEN","message":"Resource not accessible by integration"}]}`}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/list", map[string]any{})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	var out struct {
		ExitCode int    `json:"exit_code"`
		Stderr   string `json:"stderr"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	assert.Equal(t, 1, out.ExitCode)
	assert.Contains(t, out.Stderr, "Resource not accessible")
}

// TestGHProxy_RateLimitSaysWhenToComeBack — "API rate limit exceeded" alone
// does not say when, and an agent told only that retries immediately.
func TestGHProxy_RateLimitSaysWhenToComeBack(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.def = ghCanned{
		Status: 403,
		Body:   `{"message":"API rate limit exceeded for user ID 1."}`,
		Header: http.Header{
			"X-Ratelimit-Remaining": []string{"0"},
			"X-Ratelimit-Reset":     []string{fmt.Sprint(time.Now().Add(11 * time.Minute).Unix())},
		},
	}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/pr/list", map[string]any{})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	var out struct {
		ExitCode int    `json:"exit_code"`
		Stderr   string `json:"stderr"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	assert.Equal(t, 1, out.ExitCode)
	assert.Contains(t, out.Stderr, "rate limit")
	// The remaining time is computed against a wall clock, so it lands just
	// under the eleven minutes the fixture set. What matters is that a duration
	// is reported at all.
	assert.Regexp(t, `resets in 1[01]m`, out.Stderr, "an agent needs to know how long to wait")
}

// TestGHProxy_AuditsWithoutRecordingContent — the audit row must name the repo
// and the operation, and must NOT carry the PR title or body. A PR body is
// free text an agent authored; the audit log is not the place for it. Nor is it
// the place for the operator's token.
func TestGHProxy_AuditsWithoutRecordingContent(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.def = ghCanned{Status: 201, Body: `{"number":9,"html_url":"https://x/9"}`}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubWrite, "test"))

	const title = "TITLE-SHOULD-NOT-BE-AUDITED"
	const body = "BODY-SHOULD-NOT-BE-AUDITED"
	res := gitProxyPost(t, f, "/v1/github/pr/create",
		map[string]any{"title": title, "body": body, "base": "main"})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	row := auditRowByVerb(t, "github.pr.create")
	assert.Contains(t, row.Detail, "tofutools/tclaude")
	assert.Contains(t, row.Detail, "exit=0")
	assert.NotContains(t, row.Detail, title)
	assert.NotContains(t, row.Detail, body)
	assert.NotContains(t, row.Detail, ghTestToken)
}

// TestGHProxy_RunLogFailedNeverReportsAFailedJobAsSilence — every fallback is
// exhausted and the job's log still cannot be read. Contributing nothing for
// that job is indistinguishable from a run where nothing failed, which is the
// one conclusion that would make an agent stop looking.
func TestGHProxy_RunLogFailedNeverReportsAFailedJobAsSilence(t *testing.T) {
	f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
	gh.route = ghRunLogRoute()
	gh.streamErr = errors.New("502 Bad Gateway")
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/run/log-failed", map[string]any{"run_id": 18234567890})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	stdout := ghStdout(t, res.Body.Bytes())
	assert.NotEmpty(t, stdout, "a red job must never render as an empty answer")
	assert.Contains(t, stdout, "build\tTest\t")
	assert.Contains(t, stdout, "could not be read")
}
