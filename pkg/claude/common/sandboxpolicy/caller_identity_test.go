package sandboxpolicy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPreserveCallerIdentityIsExplicitAndComposesMonotonically(t *testing.T) {
	unset, err := Resolve(Scopes{Explicit: &Profile{
		Name: "unset", Network: &NetworkRules{Baseline: NetworkBaselineDeny},
	}})
	require.NoError(t, err)
	require.NotNil(t, unset.Network)
	assert.False(t, unset.Network.PreserveCallerIdentity)

	effective, err := Resolve(Scopes{
		Global: &Profile{Name: "caller", Network: &NetworkRules{
			Baseline: NetworkBaselineInherit, PreserveCallerIdentity: true,
		}},
		Explicit: &Profile{Name: "filtered", Network: &NetworkRules{
			Baseline: NetworkBaselineDeny,
			Packs:    []string{"net-github"},
		}},
	})
	require.NoError(t, err)
	require.NotNil(t, effective.Network)
	assert.True(t, effective.Network.PreserveCallerIdentity)

	plan, err := RenderMountPlan(effective)
	require.NoError(t, err)
	assert.Equal(t, NetworkFiltered, plan.NetworkPosture)
	assert.True(t, plan.PreserveCallerIdentity)
}
