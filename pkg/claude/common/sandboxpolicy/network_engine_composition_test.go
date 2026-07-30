package sandboxpolicy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// engineProfile is a minimal profile whose only interesting content is its
// network axis, so composition tests read as engine tests.
func engineProfile(name string, engine NetworkEngine, hosts ...string) *Profile {
	allow := make([]NetworkAllowEntry, 0, len(hosts))
	for _, host := range hosts {
		allow = append(allow, NetworkAllowEntry{Host: host, Ports: []int{443}})
	}
	return &Profile{
		Name: name,
		Network: &NetworkRules{
			Baseline: NetworkBaselineDeny,
			Allow:    allow,
			Engine:   engine,
		},
	}
}

func networkEngineNotice(t *testing.T, effective EffectiveProfile) (AccessNotice, bool) {
	t.Helper()
	for _, notice := range effective.AccessNotices {
		if notice.Reason == AccessNoticeReasonNetworkEngine {
			return notice, true
		}
	}
	return AccessNotice{}, false
}

// TestResolveNetworkEngineMostExplicitWinsAndSaysSo covers decision (b) end to
// end at the composition seam: the winning engine reaches the effective policy,
// and the disclosure names the winner, the profile it came from, and the lower
// layer that asked for something else and lost.
func TestResolveNetworkEngineMostExplicitWinsAndSaysSo(t *testing.T) {
	effective, err := Resolve(Scopes{
		Global:   engineProfile("company", NetworkEnginePacket, "example.com"),
		Group:    engineProfile("frontend-team", NetworkEngineProxy, "example.com"),
		Explicit: engineProfile("session", NetworkEngineUnset, "example.com"),
	})
	require.NoError(t, err)
	require.NotNil(t, effective.Network)

	// The group layer wins: the session profile expressed no opinion and is
	// absorbed rather than counting as a selection of the default.
	assert.Equal(t, NetworkEngineProxy, effective.Network.Engine)

	notice, ok := networkEngineNotice(t, effective)
	require.True(t, ok, "a composed engine must be disclosed")
	assert.Equal(t, AccessNoticeClassComposition, notice.Class)
	assert.Equal(t, "network", notice.Axis)
	assert.Equal(t, AccessNoticeEffectMechanismSelected, notice.Effect)
	assert.Contains(t, notice.Detail, "Proxy filter")
	assert.Contains(t, notice.Detail, `group profile "frontend-team"`)
	// The overridden layer is named explicitly, with the engine it asked for,
	// so an operator reading their global policy is not surprised by a launch
	// that quietly enforces differently.
	assert.Contains(t, notice.Detail, `global profile "company"`)
	assert.Contains(t, notice.Detail, "Packet filter")
	assert.Contains(t, notice.Detail, "overridden")
	assert.Equal(t, []string{"group", "global"}, notice.Tiers)
}

// TestResolveNetworkEngineAgreementIsNotAnOverride keeps the disclosure honest
// in the other direction: two layers that named the SAME engine have no
// override to report, and rendering one would teach operators to ignore the
// sentence that matters.
func TestResolveNetworkEngineAgreementIsNotAnOverride(t *testing.T) {
	effective, err := Resolve(Scopes{
		Global:   engineProfile("company", NetworkEngineProxy, "example.com"),
		Explicit: engineProfile("session", NetworkEngineProxy, "example.com"),
	})
	require.NoError(t, err)
	assert.Equal(t, NetworkEngineProxy, effective.Network.Engine)
	notice, ok := networkEngineNotice(t, effective)
	require.True(t, ok)
	assert.NotContains(t, notice.Detail, "overridden")
	assert.Equal(t, []string{"session"}, notice.Tiers)
}

// TestResolveNetworkEngineUnsetChangesNothing is the parity assertion the
// milestone turns on: a composition in which no layer names an engine must
// produce exactly what it produced before the field existed — no engine on the
// effective policy, and no notice mentioning one.
func TestResolveNetworkEngineUnsetChangesNothing(t *testing.T) {
	effective, err := Resolve(Scopes{
		Global:   engineProfile("company", NetworkEngineUnset, "example.com"),
		Group:    engineProfile("frontend-team", NetworkEngineUnset, "example.com"),
		Explicit: engineProfile("session", NetworkEngineUnset, "example.com"),
	})
	require.NoError(t, err)
	require.NotNil(t, effective.Network)
	assert.Equal(t, NetworkEngineUnset, effective.Network.Engine)
	_, ok := networkEngineNotice(t, effective)
	assert.False(t, ok, "an unset engine must disclose nothing at all")
}

// TestResolveRefusesAnInvalidAuthoredEngine keeps an unknown spelling from
// composing into a launch as an unrecognized mechanism.
func TestResolveRefusesAnInvalidAuthoredEngine(t *testing.T) {
	_, err := Resolve(Scopes{
		Global: engineProfile("company", NetworkEngine("socks"), "example.com"),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "network.engine")
}

// TestNetworkEngineSurvivesMaterialization proves the engine is not lost by
// pack expansion. It is the failure that would make a launch run the default
// engine for a profile that authored one, with nothing in the preview to say
// so, because materialization happens between authoring and both consumers.
func TestNetworkEngineSurvivesMaterialization(t *testing.T) {
	for name, authored := range map[string]NetworkRules{
		"deny baseline with allow entries": {
			Baseline: NetworkBaselineDeny,
			Allow:    []NetworkAllowEntry{{Host: "example.com"}},
			Engine:   NetworkEngineProxy,
		},
		"deny baseline authorizing nothing": {
			Baseline: NetworkBaselineDeny,
			Engine:   NetworkEngineProxy,
		},
		"allow baseline with denies": {
			Baseline: NetworkBaselineAllow,
			Deny:     []NetworkAllowEntry{{Host: "blocked.example"}},
			Engine:   NetworkEngineProxy,
		},
		"inherit baseline": {
			Baseline: NetworkBaselineInherit,
			Engine:   NetworkEngineProxy,
		},
	} {
		t.Run(name, func(t *testing.T) {
			materialized, err := MaterializeNetworkRules(authored)
			require.NoError(t, err)
			assert.Equal(t, NetworkEngineProxy, materialized.Engine)
		})
	}
}

// TestDeployedNetworkEngineForRulesReadsTheComposedSelection proves the helper
// both consumers call answers from the policy's own selection rather than from
// a separately supplied one.
func TestDeployedNetworkEngineForRulesReadsTheComposedSelection(t *testing.T) {
	discriminating := NetworkRules{
		Mode:   AccessModeList,
		Allow:  []NetworkAllowEntry{{Host: "example.com"}},
		Engine: NetworkEngineProxy,
	}
	deployed, err := DeployedNetworkEngineForRules(discriminating)
	require.NoError(t, err)
	assert.Equal(t, NetworkEngineProxy, deployed)

	// Selecting an engine for a policy that asks for no distinction between
	// destinations is a latent choice, not a deployment (§1.3-4).
	latent := NetworkRules{
		Mode:   AccessModeList,
		Allow:  []NetworkAllowEntry{{Loopback: true, Ports: []int{11434}}},
		Engine: NetworkEngineProxy,
	}
	deployed, err = DeployedNetworkEngineForRules(latent)
	require.NoError(t, err)
	assert.Equal(t, NetworkEngineUnset, deployed)
}

// TestPlannedAxesKeepTheEngineThroughWidening covers the seam that would
// silently split preview from launch. A degradation notice widens an
// unenforceable list to open before either consumer reads the axes; if the
// engine did not survive that rewrite, the launch would deploy the pre-engine
// default for a profile whose preview named the authored engine.
func TestPlannedAxesKeepTheEngineThroughWidening(t *testing.T) {
	effective := EffectiveProfile{
		Network: &NetworkRules{
			Mode:   AccessModeList,
			Allow:  []NetworkAllowEntry{{Host: "example.com"}},
			Deny:   []NetworkAllowEntry{{Host: "blocked.example"}},
			Engine: NetworkEngineProxy,
		},
		AccessNotices: []AccessNotice{{
			Class:  AccessNoticeClassDegradation,
			Axis:   "network",
			Reason: "no_mechanism",
			Effect: AccessNoticeEffectNotEnforced,
			Detail: "the network allow list is not enforced",
		}},
	}
	axes, err := PlannedEffectiveAccessAxes(effective)
	require.NoError(t, err)
	require.Equal(t, AccessModeOpen, axes.Network.Mode, "the list must have widened")
	assert.Equal(t, NetworkEngineProxy, axes.Network.Engine,
		"widening drops destinations, never the authored mechanism")
}
