package session

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// The wrapper hands the token to the workload through fd 3: the workload
// reads it from the pipe, and only the fd number is in its environment.
func TestClaudeSubscriptionWrapperHandsTokenThroughAPipe(t *testing.T) {
	dir := testutil.CanonicalTempDir(t)
	t.Setenv("HOME", dir)
	t.Chdir(dir)
	t.Setenv("ANTHROPIC_API_KEY", "would-bill-this-node")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "would-also-win")
	t.Setenv("HTTPS_PROXY", "http://corp-proxy.invalid:3128")
	const token = "sk-ant-oat01-wrapper-test"
	fetches := 0
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "launch-row", r.Header.Get(agentipc.SessionClaimHeader))
		switch r.URL.Path {
		case "/v1/http/environment":
			_, _ = io.WriteString(w, `{"names":[]}`)
		case "/v1/models/claude-subscription":
			fetches++
			_, _ = io.WriteString(w, `{"token":"`+token+`"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer daemon.Close()
	target, err := url.Parse(daemon.URL)
	require.NoError(t, err)
	previous := newHTTPProxyDaemonClient
	t.Cleanup(func() { newHTTPProxyDaemonClient = previous })
	newHTTPProxyDaemonClient = func() *http.Client {
		return &http.Client{Transport: httpProxyTestTransport(func(r *http.Request) (*http.Response, error) {
			clone := r.Clone(r.Context())
			copyURL := *r.URL
			clone.URL = &copyURL
			clone.URL.Scheme, clone.URL.Host = target.Scheme, target.Host
			return daemon.Client().Transport.RoundTrip(clone)
		})}
	}
	out := filepath.Join(dir, "out")
	workload := `cat <&"$CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR" > ` + out + `.token; env > ` + out + `.env`
	code, err := runHTTPProxyExecWithOptions("launch-row", workload, false, nil, true)
	require.NoError(t, err)
	require.Zero(t, code)
	require.Equal(t, 1, fetches)
	got, err := os.ReadFile(out + ".token")
	require.NoError(t, err)
	require.Equal(t, token+"\n", string(got))
	env, err := os.ReadFile(out + ".env")
	require.NoError(t, err)
	require.NotContains(t, string(env), token, "the token is never in the environment")
	require.Contains(t, string(env), "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR=3\n")
	require.NotContains(t, string(env), "ANTHROPIC_API_KEY=")
	require.NotContains(t, string(env), "CLAUDE_CODE_OAUTH_TOKEN=")
	require.Contains(t, string(env), "HTTPS_PROXY=http://corp-proxy.invalid:3128", "network proxies stay")

	// Without the token the launch is refused, never run on this node's login.
	newHTTPProxyDaemonClient = func() *http.Client {
		return &http.Client{Transport: httpProxyTestTransport(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/v1/http/environment" {
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"names":[]}`))}, nil
			}
			return &http.Response{StatusCode: 404, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":"no Claude subscription token is held for this launch"}`))}, nil
		})}
	}
	_, err = runHTTPProxyExecWithOptions("launch-row", "true", false, nil, true)
	require.ErrorContains(t, err, "no Claude subscription token is held")
}

func TestClaudeSubscriptionLaunchIsAlwaysWrapped(t *testing.T) {
	t.Setenv("HOME", testutil.CanonicalTempDir(t))
	wrapped := HTTPProxySpawnCommand("launch-row", harness.MustGet(harness.DefaultName), harness.SpawnSpec{ExecutablePath: "/bin/claude", ClaudeSubscription: true, ShellEnvironment: map[string]string{"ANTHROPIC_API_KEY": "x", "KEEP": "y"}})
	require.Contains(t, wrapped, " session http-proxy-exec --claude-subscription ")
	require.NotContains(t, wrapped, "ANTHROPIC_API_KEY")
	plain := HTTPProxySpawnCommand("launch-row", harness.MustGet(harness.DefaultName), harness.SpawnSpec{ExecutablePath: "/bin/claude"})
	require.NotContains(t, plain, "--claude-subscription")
}

func TestClaudeSubscriptionVersionGate(t *testing.T) {
	dir := testutil.CanonicalTempDir(t)
	fake := func(version string) string {
		path := filepath.Join(dir, "claude-"+version)
		require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\necho '"+version+" (Claude Code)'\n"), 0o755))
		return path
	}
	require.NoError(t, validateClaudeSubscriptionVersion(fake("2.1.303")))
	require.NoError(t, validateClaudeSubscriptionVersion(fake("2.2.0")))
	require.ErrorContains(t, validateClaudeSubscriptionVersion(fake("2.1.250")), "2.1.303 or newer")
}
