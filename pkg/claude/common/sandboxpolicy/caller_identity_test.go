package sandboxpolicy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPacketFilteringPreservesCallerIdentityByDefault(t *testing.T) {
	effective, err := Resolve(Scopes{
		Explicit: &Profile{Name: "filtered", Network: &NetworkRules{
			Baseline: NetworkBaselineDeny,
			Packs:    []string{"net-github"},
		}},
	})
	require.NoError(t, err)
	require.NotNil(t, effective.Network)

	plan, err := RenderMountPlan(effective)
	require.NoError(t, err)
	assert.Equal(t, NetworkFiltered, plan.NetworkPosture)
	assert.Equal(t, NetworkEnginePacket, plan.NetworkEngine)
}

func TestCallerIdentityDefaultSurvivesDisclosedAllowListWidening(t *testing.T) {
	effective := EffectiveProfile{
		Network: &NetworkRules{
			Mode:  AccessModeList,
			Allow: []NetworkAllowEntry{{Host: "unsupported.example"}},
			Deny:  []NetworkAllowEntry{{Host: "blocked.example"}},
		},
		AccessNotices: []AccessNotice{{
			Class:  AccessNoticeClassDegradation,
			Axis:   "network",
			Reason: "selector_unsupported",
			Effect: AccessNoticeEffectNotEnforced,
		}},
	}

	axes, err := PlannedEffectiveAccessAxes(effective)
	require.NoError(t, err)
	assert.Equal(t, AccessModeOpen, axes.Network.Mode)
	assert.NotEmpty(t, axes.Network.Deny, "the retained deny keeps packet filtering active")

	plan, err := RenderMountPlan(effective)
	require.NoError(t, err)
	assert.Equal(t, NetworkFiltered, plan.NetworkPosture)
	assert.Equal(t, NetworkEnginePacket, plan.NetworkEngine)
}
