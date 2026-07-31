//go:build linux

package session

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// TestProxyPostureInjectedVariablesNeverReachTheInspectedSet executes §7's
// central property: the M2c split is ORDERING, not recognition.
//
// It is asserted rather than assumed in one step that matters — the variables
// the launcher injects are fed BACK into the model-transport gate, and the gate
// refuses them. That refusal is the point: tclaude's own values are not
// special, are not recognized, and would refuse the launch exactly like a
// foreign proxy if they were ever present before the gate ran. The only thing
// keeping the launch alive is that the injection happens after it.
//
// Linux-tagged because proxyNetworkSandboxEnv is the Linux launcher's exec
// seam. The platform-neutral halves of §7.6 live beside this file and run
// everywhere.
func TestProxyPostureInjectedVariablesNeverReachTheInspectedSet(t *testing.T) {
	require.True(t, ProxyEngineFloorApplies(proxyPostureEvidenceRules()),
		"the fixture must be a launch whose floor is the proxy engine's")
	home, cwd := isolateModelTransportLaunch(t)
	entries := []sandboxpolicy.EnvironmentEntry{{Name: "HOME", Value: home}}

	// 1. The pre-injection environment the resolver inspects carries none of
	//    the launcher's variables, so the gate passes.
	// The ROUTING variables only: an inherited NO_PROXY may legitimately be
	// present here and is overridden rather than refused over (§7.4), which is
	// why the exemption variables are not part of the gate's refusal set.
	inspected := launchModelEnvironment(entries)
	for _, name := range proxyNetworkRoutingVariables {
		assert.Emptyf(t, strings.TrimSpace(inspected[name]),
			"%s must not be in the pre-injection environment the gate inspects",
			name)
	}
	require.Empty(t, modelTransportProxyVariable(inspected))
	resolved, err := ResolveTclaudeLayerModelTransport(
		harness.MustGet(harness.DefaultName),
		ModelTransportLaunchContext{Model: "sonnet", Cwd: cwd, Environment: entries})
	require.NoError(t, err,
		"a proxy-posture launch resolves its provider from the pre-injection environment")
	require.True(t, resolved.ProviderResolved)

	// 2. The launcher then owns all eight variables inside the namespace.
	const port = 39217
	injected := proxyPostureEnvironmentMap(
		proxyNetworkSandboxEnv(os.Environ(), port))
	for name, want := range map[string]string{
		"HTTP_PROXY":   "http://127.0.0.1:39217",
		"http_proxy":   "http://127.0.0.1:39217",
		"HTTPS_PROXY":  "http://127.0.0.1:39217",
		"https_proxy":  "http://127.0.0.1:39217",
		"ALL_PROXY":    "socks5h://127.0.0.1:39217",
		"all_proxy":    "socks5h://127.0.0.1:39217",
		// Empty rather than absent: empty exempts nothing, while absent lets a
		// harness fall back to its own default exemption list.
		"NO_PROXY": "",
		"no_proxy": "",
	} {
		value, present := injected[name]
		require.Truef(t, present, "%s must be set inside the namespace", name)
		assert.Equalf(t, want, value, "%s", name)
	}

	// 3. The property under test: run the gate against the POST-injection
	//    environment. It refuses — the injected values are indistinguishable
	//    from a foreign proxy to the resolver, which is why ordering is the
	//    whole mechanism.
	postInjection := make([]sandboxpolicy.EnvironmentEntry, 0, len(entries)+2)
	postInjection = append(postInjection, entries...)
	for _, name := range proxyNetworkRoutingVariables {
		postInjection = append(postInjection, sandboxpolicy.EnvironmentEntry{
			Name: name, Value: injected[name],
		})
	}
	refusing := modelTransportProxyVariable(launchModelEnvironment(postInjection))
	require.NotEmpty(t, refusing,
		"tclaude's own injected values must be refusing values, not recognized ones")
	_, err = ResolveTclaudeLayerModelTransport(
		harness.MustGet(harness.DefaultName),
		ModelTransportLaunchContext{
			Model: "sonnet", Cwd: cwd, Environment: postInjection,
		})
	require.Error(t, err,
		"the gate must refuse its own launcher's values when they precede it")
	assert.Contains(t, err.Error(), refusing)
}

func proxyPostureEnvironmentMap(environ []string) map[string]string {
	out := make(map[string]string, len(environ))
	for _, pair := range environ {
		name, value, ok := strings.Cut(pair, "=")
		if ok {
			out[name] = value
		}
	}
	return out
}
