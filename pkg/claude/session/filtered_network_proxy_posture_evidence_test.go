package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// proxyPostureEvidenceRules is a policy that genuinely deploys the proxy
// engine: a discriminating allow list with `engine: proxy` selected. Every case
// below asserts ProxyEngineFloorApplies on it first, so a refusal recorded here
// is a refusal recorded for a PROXY-POSTURE launch rather than for whatever the
// engine field happened to resolve to.
func proxyPostureEvidenceRules() sandboxpolicy.NetworkRules {
	return sandboxpolicy.NetworkRules{
		Mode:   sandboxpolicy.AccessModeList,
		Engine: sandboxpolicy.NetworkEngineProxy,
		Allow: []sandboxpolicy.NetworkAllowEntry{
			{Host: "api.anthropic.com", Ports: []int{443}},
		},
	}
}

// TestProxyPostureForeignProxyVariableStillRefuses is §7.3, the defense-in-depth
// half of the M2c split: under `engine: proxy` tclaude sets the proxy variables
// itself, and a foreign one is STILL refused — including one whose value is
// shaped exactly like tclaude's own.
//
// The loopback-shaped cases are the ones worth executing. The refusal survives
// because the resolver never allowlists by value, so an attacker who can set
// launch environment cannot dress a foreign proxy up as ours; a resolver that
// recognized "our" values instead of relying on the injection ORDER would pass
// exactly these cases while passing the attacker too.
func TestProxyPostureForeignProxyVariableStillRefuses(t *testing.T) {
	require.True(t, ProxyEngineFloorApplies(proxyPostureEvidenceRules()),
		"the fixture must be a launch whose floor is the proxy engine's")

	for _, foreign := range []struct {
		name  string
		value string
	}{
		{"HTTPS_PROXY", "http://proxy.example:8443"},
		{"HTTP_PROXY", "http://127.0.0.1:39217"},
		{"https_proxy", "http://[::1]:39217"},
		{"ALL_PROXY", "socks5h://127.0.0.1:39217"},
		{"all_proxy", "socks5h://localhost:39217"},
	} {
		t.Run(foreign.name+"="+foreign.value, func(t *testing.T) {
			home, cwd := isolateModelTransportLaunch(t)
			_, err := ResolveTclaudeLayerModelTransport(
				harness.MustGet(harness.DefaultName),
				ModelTransportLaunchContext{
					Model: "sonnet",
					Cwd:   cwd,
					Environment: []sandboxpolicy.EnvironmentEntry{
						{Name: "HOME", Value: home},
						{Name: foreign.name, Value: foreign.value},
					},
				})
			require.Error(t, err,
				"a foreign %s must refuse even under engine: proxy, and even loopback-shaped",
				foreign.name)
			assert.Contains(t, err.Error(), foreign.name)
			assert.Contains(t, err.Error(),
				"behind a proxy this seam does not resolve")
		})
	}
}

// TestProxyPostureClaudeSettingsProxyVariableStillRefuses is §7.5. Claude
// re-reads settings.json env while a session runs, so a settings-authored proxy
// can re-point mid-session and a one-time preflight cannot freeze it. tclaude's
// own injection is process env rather than settings, so this refusal costs the
// proxy posture nothing — and it has to stay in place there, which is what this
// case executes.
func TestProxyPostureClaudeSettingsProxyVariableStillRefuses(t *testing.T) {
	require.True(t, ProxyEngineFloorApplies(proxyPostureEvidenceRules()),
		"the fixture must be a launch whose floor is the proxy engine's")

	for _, variable := range []string{
		"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy",
		"ALL_PROXY", "all_proxy",
	} {
		t.Run(variable, func(t *testing.T) {
			home, cwd := isolateModelTransportLaunch(t)
			settings := filepath.Join(home, ".claude", "settings.json")
			require.NoError(t, os.MkdirAll(filepath.Dir(settings), 0o700))
			// A loopback value again: the live-reload hazard is about WHERE the
			// value can change from, not about where it points.
			require.NoError(t, os.WriteFile(settings, []byte(
				`{"env":{"`+variable+`":"http://127.0.0.1:39217"}}`), 0o600))

			_, err := ResolveTclaudeLayerModelTransport(
				harness.MustGet(harness.DefaultName),
				ModelTransportLaunchContext{
					Model: "sonnet",
					Cwd:   cwd,
					Environment: []sandboxpolicy.EnvironmentEntry{
						{Name: "HOME", Value: home},
					},
				})
			require.Error(t, err,
				"a settings-authored %s must keep refusing under proxy posture",
				variable)
			assert.Contains(t, err.Error(), variable)
			assert.Contains(t, err.Error(), settings)
		})
	}
}

// TestProxyPostureNoProxyOverrideIsDisclosed is §7.4. The override is merged
// behavior; what this pins is that it is DISCLOSED, and only when a non-empty
// host value was actually discarded.
func TestProxyPostureNoProxyOverrideIsDisclosed(t *testing.T) {
	rules := proxyPostureEvidenceRules()
	require.True(t, ProxyEngineFloorApplies(rules))

	t.Run("inherited-value-discloses", func(t *testing.T) {
		isolateProxyNoProxyEnvironment(t)
		notice := ProxyEngineNoProxyOverrideNotice(rules,
			[]sandboxpolicy.EnvironmentEntry{
				{Name: "NO_PROXY", Value: "internal.example,10.0.0.0/8"},
			})
		require.NotNil(t, notice, "a discarded NO_PROXY must be disclosed")
		assert.Equal(t, sandboxpolicy.AccessNoticeClassDegradation, notice.Class)
		assert.Equal(t, "network", notice.Axis)
		assert.Equal(t,
			sandboxpolicy.AccessNoticeReasonProxyEngineNoProxyOverride,
			notice.Reason)
		assert.Equal(t,
			sandboxpolicy.AccessNoticeEffectEnvironmentOverridden, notice.Effect)
		assert.Contains(t, notice.Detail, "NO_PROXY")
		assert.NotContains(t, notice.Detail, "no_proxy",
			"only the variable that carried a value is named")
		assert.Contains(t, notice.Detail, "fails closed")
	})

	t.Run("host-value-discloses", func(t *testing.T) {
		isolateProxyNoProxyEnvironment(t)
		t.Setenv("no_proxy", "internal.example")
		notice := ProxyEngineNoProxyOverrideNotice(rules, nil)
		require.NotNil(t, notice,
			"the host environment is inspected, not only authored overrides")
		assert.Contains(t, notice.Detail, "no_proxy")
	})

	t.Run("both-spellings-named", func(t *testing.T) {
		isolateProxyNoProxyEnvironment(t)
		t.Setenv("NO_PROXY", "a.example")
		t.Setenv("no_proxy", "b.example")
		notice := ProxyEngineNoProxyOverrideNotice(rules, nil)
		require.NotNil(t, notice)
		assert.Contains(t, notice.Detail, "NO_PROXY and no_proxy")
	})

	t.Run("empty-and-whitespace-stay-silent", func(t *testing.T) {
		isolateProxyNoProxyEnvironment(t)
		t.Setenv("NO_PROXY", "   ")
		assert.Nil(t, ProxyEngineNoProxyOverrideNotice(rules,
			[]sandboxpolicy.EnvironmentEntry{{Name: "no_proxy", Value: ""}}),
			"an override that discarded nothing is not a disclosure")
	})

	t.Run("authored-override-wins-over-host", func(t *testing.T) {
		isolateProxyNoProxyEnvironment(t)
		t.Setenv("NO_PROXY", "host.example")
		assert.Nil(t, ProxyEngineNoProxyOverrideNotice(rules,
			[]sandboxpolicy.EnvironmentEntry{{Name: "NO_PROXY", Value: ""}}),
			"the inspected value is the composed pre-injection one the launch carries")
	})

	t.Run("packet-engine-stays-silent", func(t *testing.T) {
		isolateProxyNoProxyEnvironment(t)
		t.Setenv("NO_PROXY", "internal.example")
		packet := proxyPostureEvidenceRules()
		packet.Engine = sandboxpolicy.NetworkEnginePacket
		require.False(t, ProxyEngineFloorApplies(packet))
		assert.Nil(t, ProxyEngineNoProxyOverrideNotice(packet, nil),
			"the packet gateway performs no override, so it discloses none")
	})

	t.Run("non-discriminating-policy-stays-silent", func(t *testing.T) {
		isolateProxyNoProxyEnvironment(t)
		t.Setenv("NO_PROXY", "internal.example")
		// Selected but latent: nothing is filtered, so no proxy is deployed
		// and no proxy environment is injected to override anything.
		latent := sandboxpolicy.NetworkRules{
			Mode:   sandboxpolicy.AccessModeOpen,
			Engine: sandboxpolicy.NetworkEngineProxy,
		}
		require.False(t, ProxyEngineFloorApplies(latent))
		assert.Nil(t, ProxyEngineNoProxyOverrideNotice(latent, nil))
	})
}

// isolateProxyNoProxyEnvironment clears the exemption variables the runner's
// own environment may carry. Without it a CI host that exports NO_PROXY would
// make the silent cases pass or fail for a reason the test never authored.
func isolateProxyNoProxyEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range proxyNetworkExemptionVariables {
		t.Setenv(name, "")
	}
}
