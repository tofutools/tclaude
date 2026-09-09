package model

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestToolGovernanceHasExactConfigurationAndOverrideIdentity(t *testing.T) {
	base := DesiredConfiguration{Harness: "opencode", ToolGovernance: ToolGovernanceDeny}
	changed := base
	changed.ToolGovernance = ToolGovernanceAllow
	require.False(t, base.Equal(changed))
	require.False(t, (DesiredConfiguration{ToolGovernance: ToolGovernanceDeny}).Equal(DesiredConfiguration{}))
	harness := "copilot"
	overrides := TeamProfileOverrides{Harness: &harness}
	require.Empty(t, overrides.Apply(base).ToolGovernance)
	clear := ToolGovernance("")
	overrides = TeamProfileOverrides{ToolGovernance: &clear}
	require.Empty(t, overrides.Apply(base).ToolGovernance)
	require.Equal(t, ToolGovernanceDeny, ((*TeamProfileOverrides)(nil)).Apply(base).ToolGovernance)
	require.Error(t, ToolGovernance("unknown").Validate())
}
