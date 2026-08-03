package agentd_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// gitproxy_flow_test.go drives the Git-remote proxy through the real daemon
// mux. Only the subprocess boundary is swapped (agentd.SetProxyExecForTest),
// which is the point: these tests assert on the EXACT argv and environment the
// daemon builds, because that is where the hardening lives. A dropped
// `core.hooksPath` pin or a missing permission gate changes no observable
// behaviour until it is exploited, so the argv is the contract.

const (
	gitProxyTestConv   = "gitp-aaaa-bbbb-cccc-000000000001"
	gitProxyTestRemote = "git@github.com:tofutools/tclaude.git"
)

// gitProxyRecorder is the stubbed subprocess boundary. It answers the local
// probes the daemon makes (repo root, remote URLs, current branch) and records
// every command so a test can assert on what would have been executed.
type gitProxyRecorder struct {
	mu       sync.Mutex
	calls    []agentd.ProxyCommand
	repoRoot string
	remotes  map[string]string // remote name -> URL
	branch   string
	// rewriteTo, when set for a URL, makes `ls-remote --get-url <url>` answer
	// with something else — simulating a repo-local url.*.insteadOf rewrite.
	rewriteTo map[string]string
	// network is the canned result for the actual fetch/push/ls-remote call.
	network agentd.ProxyResult
	// gh is the canned result for a `gh` invocation.
	gh agentd.ProxyResult
}

func newGitProxyRecorder(repoRoot string) *gitProxyRecorder {
	return &gitProxyRecorder{
		repoRoot:  repoRoot,
		remotes:   map[string]string{"origin": gitProxyTestRemote},
		branch:    "feat/thing",
		rewriteTo: map[string]string{},
	}
}

// subcommand strips the daemon's pinned `-c key=value` prefix (and --no-pager)
// so a test can dispatch on what git was actually asked to do.
func subcommand(args []string) []string {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-c" || args[i] == "-C":
			i++
		case args[i] == "--no-pager":
		case strings.HasPrefix(args[i], "-"):
		default:
			return args[i:]
		}
	}
	return nil
}

func (r *gitProxyRecorder) exec(_ context.Context, cmd agentd.ProxyCommand) (agentd.ProxyResult, error) {
	r.mu.Lock()
	r.calls = append(r.calls, cmd)
	r.mu.Unlock()

	if cmd.Tool == "gh" {
		return r.gh, nil
	}
	sub := subcommand(cmd.Args)
	if len(sub) == 0 {
		return agentd.ProxyResult{}, nil
	}
	miss := agentd.ProxyResult{ExitCode: 1, Stderr: "stub: no answer"}
	switch sub[0] {
	case "rev-parse":
		if slices.Contains(sub, "--show-toplevel") {
			return agentd.ProxyResult{Stdout: r.repoRoot + "\n"}, nil
		}
		if slices.Contains(sub, "--abbrev-ref") {
			return agentd.ProxyResult{Stdout: r.branch + "\n"}, nil
		}
		return miss, nil
	case "config":
		// No operator credential helpers configured in the test world.
		return miss, nil
	case "remote":
		if len(sub) == 1 {
			names := make([]string, 0, len(r.remotes))
			for name := range r.remotes {
				names = append(names, name)
			}
			slices.Sort(names)
			return agentd.ProxyResult{Stdout: strings.Join(names, "\n") + "\n"}, nil
		}
		if sub[1] == "get-url" {
			name := sub[len(sub)-1]
			url, ok := r.remotes[name]
			if !ok {
				return miss, nil
			}
			return agentd.ProxyResult{Stdout: url + "\n"}, nil
		}
		return miss, nil
	case "ls-remote":
		if len(sub) > 1 && sub[1] == "--get-url" {
			in := sub[len(sub)-1]
			if out, ok := r.rewriteTo[in]; ok {
				return agentd.ProxyResult{Stdout: out + "\n"}, nil
			}
			return agentd.ProxyResult{Stdout: in + "\n"}, nil
		}
		return r.network, nil
	case "fetch", "push":
		return r.network, nil
	}
	return miss, nil
}

// network returns the commands that actually reached the wire — everything
// except the local probes.
func (r *gitProxyRecorder) networkCalls() []agentd.ProxyCommand {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []agentd.ProxyCommand
	for _, c := range r.calls {
		sub := subcommand(c.Args)
		if len(sub) == 0 {
			continue
		}
		switch sub[0] {
		case "fetch", "push":
			out = append(out, c)
		case "ls-remote":
			if len(sub) > 1 && sub[1] == "--get-url" {
				continue // a local URL-rewrite probe, not a network call
			}
			out = append(out, c)
		}
		if c.Tool == "gh" {
			out = append(out, c)
		}
	}
	return out
}

func (r *gitProxyRecorder) sawAnySubprocess() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls) > 0
}

// gitProxyWorld sets up an agent whose recorded launch directory is a real
// directory, an operator allow-list, and the stubbed subprocess boundary.
func gitProxyWorld(t *testing.T, allowed []string) (*testharness.Flow, *gitProxyRecorder) {
	t.Helper()
	f := newFlow(t)

	repoRoot := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.MkdirAll(repoRoot, 0o755))
	// Resolve as the daemon will, so an assertion on cmd.Dir compares like
	// for like on a platform whose temp dir is itself a symlink.
	resolvedRoot, err := filepath.EvalSymlinks(repoRoot)
	require.NoError(t, err)

	f.HaveConvWithTitle(gitProxyTestConv, "pusher")
	f.HaveAliveSession(gitProxyTestConv, "lbl-gitp", "tclaude-gitp", resolvedRoot)
	f.HaveEnrolledAgent(gitProxyTestConv)

	if allowed == nil {
		require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{}}))
	} else {
		writeGitProxyConfig(t, allowed)
	}

	rec := newGitProxyRecorder(resolvedRoot)
	t.Cleanup(agentd.SetProxyExecForTest(rec.exec))
	t.Cleanup(agentd.SetProxyBinariesForTest("/usr/bin/git", "/usr/bin/gh"))
	return f, rec
}

// gitProxyConfigPatch is the mutable view a test uses to vary one field of the
// operator policy without restating the rest.
type gitProxyConfigPatch = config.GitProxyConfig

// writeGitProxyConfig saves an operator policy with the given allow-list,
// optionally tweaked. Tests that change policy mid-scenario call it again.
func writeGitProxyConfig(t *testing.T, allowed []string, tweak ...func(*gitProxyConfigPatch)) {
	t.Helper()
	proxy := &config.GitProxyConfig{AllowedRemotes: allowed}
	for _, fn := range tweak {
		fn(proxy)
	}
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{GitProxy: proxy}}))
}

// serveAsProxyAgent issues a request against the real daemon mux as the test
// agent — the same identity path a `tclaude agent git …` call takes over the
// Unix socket.
func serveAsProxyAgent(t *testing.T, f *testharness.Flow, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, method, path, body), gitProxyTestConv))
}

func gitProxyPost(t *testing.T, f *testharness.Flow, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return serveAsProxyAgent(t, f, http.MethodPost, path, body)
}

// --- permission gating ---

// TestGitProxy_PushRequiresItsOwnSlug is the core authorization contract: the
// read slug is not enough to write to a remote, and a refusal must happen
// BEFORE any subprocess runs.
func TestGitProxy_PushRequiresItsOwnSlug(t *testing.T) {
	t.Run("ungranted", func(t *testing.T) {
		f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
		res := gitProxyPost(t, f, "/v1/git/push", map[string]any{})
		assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
		assert.False(t, rec.sawAnySubprocess(),
			"a denied caller must not cause git to run at all — not even a probe")
	})

	t.Run("git.read does not confer git.push", func(t *testing.T) {
		f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
		require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitRead, "test"))
		res := gitProxyPost(t, f, "/v1/git/push", map[string]any{})
		assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
		assert.False(t, rec.sawAnySubprocess(), "still no subprocess")
	})

	t.Run("granted", func(t *testing.T) {
		f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
		require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitPush, "test"))
		res := gitProxyPost(t, f, "/v1/git/push", map[string]any{})
		require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
		require.Len(t, rec.networkCalls(), 1)
	})
}

// TestGitProxy_DisabledWithoutAllowList pins the fail-closed default: an
// operator who has configured nothing gets a proxy that does nothing, with a
// message naming the config to add.
func TestGitProxy_DisabledWithoutAllowList(t *testing.T) {
	f, rec := gitProxyWorld(t, nil)
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitRead, "test"))

	res := gitProxyPost(t, f, "/v1/git/fetch", map[string]any{})
	assert.Equal(t, http.StatusServiceUnavailable, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "allowed_remotes",
		"the refusal must name the config the operator has to set")
	assert.False(t, rec.sawAnySubprocess(), "a disabled proxy runs nothing")
}

// --- the argv contract ---

// TestGitProxy_PushArgvIsHardened is the regression guard with the least
// visible failure mode. Every assertion here is a way a repository an agent
// can write would otherwise run code as the operator.
func TestGitProxy_PushArgvIsHardened(t *testing.T) {
	f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitPush, "test"))

	res := gitProxyPost(t, f, "/v1/git/push", map[string]any{"branch": "feat/thing", "set_upstream": true})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	calls := rec.networkCalls()
	require.Len(t, calls, 1)
	push := calls[0]

	assert.Equal(t, "/usr/bin/git", push.Path, "the binary is pinned to an absolute path")
	assert.Equal(t, rec.repoRoot, push.Dir, "git runs in the agent's own repository root")

	joined := strings.Join(push.Args, " ")
	assert.Contains(t, joined, "-c core.hooksPath=",
		"hooks MUST be redirected — .git/hooks/pre-push is agent-writable")
	assert.Contains(t, joined, "-c protocol.allow=never")
	assert.Contains(t, joined, "-c remote.origin.receivepack=git-receive-pack",
		"receivepack names a command run on the SERVER; it must be pinned")
	assert.Contains(t, joined, "-c core.sshCommand=ssh -o BatchMode=yes")

	// The refspec is fully qualified and constructed by the daemon, so
	// push.default and any repo-local refspec configuration are irrelevant.
	assert.Contains(t, push.Args, "refs/heads/feat/thing:refs/heads/feat/thing")
	assert.Contains(t, push.Args, "--set-upstream")
	assert.NotContains(t, push.Args, "--force")
	assert.NotContains(t, push.Args, "--force-with-lease")

	env := strings.Join(push.Env, "\n")
	assert.Contains(t, env, "GIT_TERMINAL_PROMPT=0")
	assert.NotContains(t, env, "GIT_CONFIG_COUNT=")
}

// TestGitProxy_FetchNeverTouchesTheWorkingTree records WHY the daemon only
// does the network half: a working-tree update would run .gitattributes filter
// programs named by a file the agent controls.
func TestGitProxy_FetchNeverTouchesTheWorkingTree(t *testing.T) {
	f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitRead, "test"))

	res := gitProxyPost(t, f, "/v1/git/fetch", map[string]any{"branch": "feat/thing", "prune": true})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	calls := rec.networkCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, "fetch", subcommand(calls[0].Args)[0])
	assert.Contains(t, calls[0].Args, "--prune")
	assert.Contains(t, calls[0].Args, "refs/heads/feat/thing:refs/remotes/origin/feat/thing")

	for _, c := range rec.calls {
		sub := subcommand(c.Args)
		require.NotEmpty(t, sub)
		assert.NotContains(t, []string{"merge", "checkout", "pull", "reset", "restore"}, sub[0],
			"the daemon must never update the work tree: %v", sub)
	}
}

// --- the remote gate ---

// TestGitProxy_RefusesRemoteOutsideAllowList proves the allow-list binds even
// when the caller holds the slug.
func TestGitProxy_RefusesRemoteOutsideAllowList(t *testing.T) {
	f, rec := gitProxyWorld(t, []string{"github.com/someone-else"})
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitPush, "test"))

	res := gitProxyPost(t, f, "/v1/git/push", map[string]any{"branch": "feat/thing"})
	assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "allow-list")
	assert.Empty(t, rec.networkCalls(), "nothing may reach the network")
}

// TestGitProxy_RefusesCommandExecutingRemote is the end-to-end form of the
// unit test: a repository whose origin is an `ext::` URL must be refused
// before git is asked to talk to it.
func TestGitProxy_RefusesCommandExecutingRemote(t *testing.T) {
	f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
	rec.remotes["origin"] = "ext::sh -c 'touch /tmp/pwned'"
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitPush, "test"))

	res := gitProxyPost(t, f, "/v1/git/push", map[string]any{"branch": "feat/thing"})
	assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "execute", "the refusal must name the hazard")
	assert.Empty(t, rec.networkCalls())
}

// TestGitProxy_RefusesInsteadOfRewrite covers the one dangerous git key a `-c`
// override cannot reset. The daemon does not try to disable url.*.insteadOf —
// it requires that the validated URL is a fixed point, and refuses when a
// repository would rewrite it somewhere unchecked.
func TestGitProxy_RefusesInsteadOfRewrite(t *testing.T) {
	f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
	rec.rewriteTo[gitProxyTestRemote] = "git@evil.example:tofutools/tclaude.git"
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitPush, "test"))

	res := gitProxyPost(t, f, "/v1/git/push", map[string]any{"branch": "feat/thing"})
	assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "insteadOf")
	assert.Empty(t, rec.networkCalls(), "a rewritten URL must never be dialled")
}

// TestGitProxy_RefusesUnvalidatedPushURL covers remote.<name>.pushurl: a repo
// can send pushes somewhere other than the fetch URL, so validating only the
// fetch URL would leave push aimed at an unchecked destination.
func TestGitProxy_RefusesUnvalidatedPushURL(t *testing.T) {
	f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitPush, "test"))
	// The recorder answers `remote get-url --push` from the same map, so
	// point the whole remote at an off-list host to model a diverging pushurl
	// that the fetch-side check would have let through.
	rec.remotes["origin"] = "git@evil.example:tofutools/tclaude.git"

	res := gitProxyPost(t, f, "/v1/git/push", map[string]any{"branch": "feat/thing"})
	assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Empty(t, rec.networkCalls())
}

// --- ref protection ---

// TestGitProxy_RefusesProtectedBranch pins the guard that keeps an agent off
// the trunk. It is checked after the remote gate but before any subprocess.
func TestGitProxy_RefusesProtectedBranch(t *testing.T) {
	f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitPush, "test"))

	res := gitProxyPost(t, f, "/v1/git/push", map[string]any{"branch": "main"})
	assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "protected")
	assert.Empty(t, rec.networkCalls())
}

// TestGitProxy_ForcePushIsOptIn — force-with-lease is refused unless the
// operator enabled it, and plain --force is never available at all.
func TestGitProxy_ForcePushIsOptIn(t *testing.T) {
	t.Run("disabled by default", func(t *testing.T) {
		f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
		require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitPush, "test"))

		res := gitProxyPost(t, f, "/v1/git/push",
			map[string]any{"branch": "feat/thing", "force_with_lease": true})
		assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
		assert.Contains(t, res.Body.String(), "allow_force_push")
		assert.Empty(t, rec.networkCalls())
	})

	t.Run("enabled by the operator", func(t *testing.T) {
		f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
		require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{
			GitProxy: &config.GitProxyConfig{
				AllowedRemotes: []string{"github.com/tofutools"},
				AllowForcePush: true,
			},
		}}))
		require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitPush, "test"))

		res := gitProxyPost(t, f, "/v1/git/push",
			map[string]any{"branch": "feat/thing", "force_with_lease": true})
		require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
		calls := rec.networkCalls()
		require.Len(t, calls, 1)
		assert.Contains(t, calls[0].Args, "--force-with-lease")
		assert.NotContains(t, calls[0].Args, "--force",
			"plain --force must not exist here: a lease is what makes this recoverable")
	})

	t.Run("force cannot override a protected branch", func(t *testing.T) {
		f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
		require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{
			GitProxy: &config.GitProxyConfig{
				AllowedRemotes: []string{"github.com/tofutools"},
				AllowForcePush: true,
			},
		}}))
		require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitPush, "test"))

		res := gitProxyPost(t, f, "/v1/git/push",
			map[string]any{"branch": "main", "force_with_lease": true})
		assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
		assert.Empty(t, rec.networkCalls())
	})
}

// --- parameter refusals ---

// TestGitProxy_RefusesArgumentInjection pins the leading-"-" rule at the HTTP
// boundary: a branch that would parse as a git flag never reaches argv.
func TestGitProxy_RefusesArgumentInjection(t *testing.T) {
	f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitPush, "test"))

	for _, branch := range []string{"--exec=id", "-o", "a b", "a..b", "x^y"} {
		res := gitProxyPost(t, f, "/v1/git/push", map[string]any{"branch": branch})
		assert.Equal(t, http.StatusBadRequest, res.Code, "branch %q body=%s", branch, res.Body.String())
	}
	assert.Empty(t, rec.networkCalls())
}

// TestGitProxy_RefusesDetachedHeadPush — with no branch to name, the daemon
// refuses rather than guessing a ref.
func TestGitProxy_RefusesDetachedHeadPush(t *testing.T) {
	f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
	rec.branch = "HEAD"
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitPush, "test"))

	res := gitProxyPost(t, f, "/v1/git/push", map[string]any{})
	assert.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	assert.Empty(t, rec.networkCalls())
}

// --- the repo gate ---

// TestGitProxy_TakesNoRepoParameter is the invariant that keeps the proxy
// honest: the repository comes from daemon-recorded launch state, so there is
// no parameter through which an agent could aim the operator's credentials at
// another checkout. An unknown field in the body must change nothing.
func TestGitProxy_TakesNoRepoParameter(t *testing.T) {
	f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitRead, "test"))

	res := gitProxyPost(t, f, "/v1/git/fetch", map[string]any{
		"repo": "/etc", "dir": "/etc", "cwd": "/etc", "path": "/etc",
	})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	for _, c := range rec.calls {
		sub := subcommand(c.Args)
		require.NotEmpty(t, sub)
		if sub[0] == "config" {
			// The operator's global/system credential-helper read is the one
			// deliberate exception: it runs in a NEUTRAL directory precisely
			// so no repository-local config is in scope for it.
			assert.NotEqual(t, rec.repoRoot, c.Dir,
				"the global credential-helper probe must not see repo-local config")
			continue
		}
		assert.Equal(t, rec.repoRoot, c.Dir,
			"every repository operation must run in the recorded launch repository, got %v", sub)
	}
}

// --- the response contract ---

// TestGitProxy_GitFailureIsAnAnswerNotAnError pins the deliberate split: HTTP
// 200 means the daemon RAN git. A non-fast-forward is something the agent must
// read and act on, not a daemon fault, so git's own exit code and stderr are
// carried through.
func TestGitProxy_GitFailureIsAnAnswerNotAnError(t *testing.T) {
	f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
	rec.network = agentd.ProxyResult{
		ExitCode: 1,
		Stderr:   "! [rejected] feat/thing -> feat/thing (non-fast-forward)",
	}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitPush, "test"))

	res := gitProxyPost(t, f, "/v1/git/push", map[string]any{"branch": "feat/thing"})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	var out struct {
		ExitCode int    `json:"exit_code"`
		Stderr   string `json:"stderr"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	assert.Equal(t, 1, out.ExitCode)
	assert.Contains(t, out.Stderr, "non-fast-forward",
		"git's own diagnosis is what tells the agent what to do next")
}

// TestGitProxy_RemotesReportsRefusalReasons — the discovery command must
// explain a refusal, so an agent can tell its operator exactly what to add
// instead of guessing from a later 403.
func TestGitProxy_RemotesReportsRefusalReasons(t *testing.T) {
	f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
	rec.remotes["upstream"] = "git@github.com:someone-else/fork.git"
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitRead, "test"))

	res := serveAsProxyAgent(t, f, http.MethodGet, "/v1/git/remotes", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	var out struct {
		Remotes []struct {
			Name       string `json:"name"`
			Allowed    bool   `json:"allowed"`
			RefusedFor string `json:"refused_for"`
		} `json:"remotes"`
		AllowedRemotes []string `json:"allowed_remotes"`
		ProtectedRefs  []string `json:"protected_refs"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	require.Len(t, out.Remotes, 2)

	byName := map[string]bool{}
	reasons := map[string]string{}
	for _, r := range out.Remotes {
		byName[r.Name] = r.Allowed
		reasons[r.Name] = r.RefusedFor
	}
	assert.True(t, byName["origin"])
	assert.False(t, byName["upstream"])
	assert.Contains(t, reasons["upstream"], "allow-list")
	assert.Equal(t, []string{"github.com/tofutools"}, out.AllowedRemotes)
	assert.Equal(t, []string{"main", "master"}, out.ProtectedRefs,
		"the agent should be able to see which branches are off limits before trying")
}
