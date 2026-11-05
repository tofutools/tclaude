package session

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestResolveLaunchRouteHelperRefusesWhatItCannotHonour(t *testing.T) {
	helper, err := resolveLaunchRouteHelper(&NewParams{}, harness.DefaultName, true)
	require.NoError(t, err)
	assert.Nil(t, helper, "no route-helper fields asks for no helper")

	requested := &NewParams{RouteHelperAgentID: "agt_1"}
	_, err = resolveLaunchRouteHelper(requested, harness.DefaultName, false)
	require.ErrorContains(t, err, "pane-authoritative launch", "a helper needs tclaude's own sandbox")
	_, err = resolveLaunchRouteHelper(requested, harness.OpenCodeName, true)
	require.ErrorContains(t, err, "pane-authoritative launch", "OpenCode's pane is not the boundary")

	_, err = resolveLaunchRouteHelper(requested, harness.DefaultName, true)
	if runtime.GOOS == "linux" {
		require.ErrorContains(t, err, "launch identity is incomplete")
	} else {
		require.ErrorContains(t, err, "requires Linux", "the full helper is Linux-only")
	}
}

func TestBaseLaunchEnvCarriesTheLaunchKeysAndBrokersWalledHooks(t *testing.T) {
	claude := harness.MustGet(harness.DefaultName)
	env, err := baseLaunchEnv(&NewParams{}, claude, "", nil, false, "spwn-env", "gen-1", t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, "spwn-env", env["TCLAUDE_SESSION_ID"])
	assert.Equal(t, "gen-1", env["TCLAUDE_EXIT_GENERATION"])
	assert.NotContains(t, env, HookBrokerEnvVar, "hooks outside any wall write their own state")

	env, err = baseLaunchEnv(&NewParams{}, claude, "", nil, true, "spwn-env", "gen-1", t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, HookBrokerAgentd, env[HookBrokerEnvVar], "an outer-layer launch routes hooks through agentd")
}

func TestResolveOpenCodeLaunchServerValidatesTheInternalAuthority(t *testing.T) {
	setServer := func(t *testing.T, url, password, transport string) {
		t.Setenv("TCLAUDE_OPENCODE_SERVER_URL", url)
		t.Setenv("OPENCODE_SERVER_PASSWORD", password)
		t.Setenv(clcommon.OpenCodeTransportEnv, transport)
	}

	t.Run("missing server", func(t *testing.T) {
		setServer(t, "", "", "")
		_, err := resolveOpenCodeLaunchServer()
		require.ErrorContains(t, err, "authenticated agentd-managed server")
	})
	t.Run("missing password", func(t *testing.T) {
		setServer(t, "http://127.0.0.1:4096", "", "")
		_, err := resolveOpenCodeLaunchServer()
		require.ErrorContains(t, err, "authenticated agentd-managed server")
	})
	t.Run("non-loopback url", func(t *testing.T) {
		setServer(t, "http://example.com:4096", "pw", "")
		_, err := resolveOpenCodeLaunchServer()
		require.ErrorContains(t, err, "invalid internal OpenCode server URL")
	})
	t.Run("loopback defaults to tcp", func(t *testing.T) {
		setServer(t, " http://127.0.0.1:4096 ", "pw", "")
		srv, err := resolveOpenCodeLaunchServer()
		require.NoError(t, err)
		assert.Equal(t, openCodeLaunchServer{url: "http://127.0.0.1:4096", transport: db.OpenCodeTransportLoopbackTCP}, srv)
	})
	t.Run("unknown transport", func(t *testing.T) {
		setServer(t, "http://127.0.0.1:4096", "pw", "carrier-pigeon")
		_, err := resolveOpenCodeLaunchServer()
		require.ErrorContains(t, err, "invalid internal OpenCode transport")
	})
	t.Run("unix relay needs its socket identity", func(t *testing.T) {
		setServer(t, "http://127.0.0.1:4096", "pw", db.OpenCodeTransportUnixRelay)
		t.Setenv(clcommon.OpenCodeControlSocketDeviceEnv, "not-a-number")
		_, err := resolveOpenCodeLaunchServer()
		require.ErrorContains(t, err, "invalid internal OpenCode control socket device")
	})
}
