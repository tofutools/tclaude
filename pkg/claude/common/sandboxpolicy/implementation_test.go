package sandboxpolicy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeImplementation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   string
		want    Implementation
		wantErr string
	}{
		{name: "empty defaults to legacy behavior", want: ImplementationHarnessBuiltin},
		{name: "harness builtin", value: " harness-builtin ", want: ImplementationHarnessBuiltin},
		{name: "tclaude layer", value: "tclaude-layer", want: ImplementationTclaudeLayer},
		{name: "stacked", value: "stacked", want: ImplementationStacked},
		{name: "off", value: "off", want: ImplementationOff},
		{name: "resource only", value: " resource-only ", want: ImplementationResourceOnly},
		{name: "invalid", value: "automatic", wantErr: "invalid sandbox implementation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeImplementation(tc.value)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// resource-only must never be mistaken for a confinement tier. It is the one
// implementation that enforces something (a cgroup) while confining nothing,
// so the two predicates have to disagree about it in exactly one direction.
func TestResourceOnlyEnforcesLimitsWithoutClaimingConfinement(t *testing.T) {
	assert.True(t, ImplementationResourceOnly.OmitsOSConfinement(),
		"a cgroup bounds consumption, not access; every access gate must read "+
			"resource-only exactly as it reads off")
	assert.False(t, ImplementationResourceOnly.UsesTclaudeLayer(),
		"resource-only must not take any bubblewrap or namespace path")
	assert.False(t, ImplementationResourceOnly.UsesNestedHarnessSandbox())
	assert.False(t, SupportsMountPaths(ImplementationResourceOnly, "linux"),
		"remapping a host path needs a mount namespace, which a cgroup is not")

	assert.True(t, ImplementationOff.OmitsOSConfinement())
	for _, implementation := range []Implementation{
		ImplementationHarnessBuiltin, ImplementationTclaudeLayer, ImplementationStacked,
	} {
		assert.Falsef(t, implementation.OmitsOSConfinement(),
			"%s owns an access boundary", implementation)
	}
}

// The one axis on which resource-only and off part company.
func TestResourceOnlyIsTheOnlyUnconfinedImplementationThatCarriesLimits(t *testing.T) {
	limits := ResourceLimits{Memory: "8GiB"}
	require.NoError(t, ValidateResourceLimitTarget(limits, ImplementationResourceOnly, "linux"))

	err := ValidateResourceLimitTarget(limits, ImplementationOff, "linux")
	require.Error(t, err)
	assert.Contains(t, err.Error(), string(ImplementationResourceOnly),
		"refusing off must name the implementation that does carry limits, or the "+
			"operator is told no without being told where to go")

	assert.ErrorContains(t,
		ValidateResourceLimitTarget(limits, ImplementationResourceOnly, "darwin"),
		"Linux only", "the cgroup is a Linux mechanism wherever it is requested from")
}
