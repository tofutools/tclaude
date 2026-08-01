package sandboxpolicy

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseMemoryLimitBytes(t *testing.T) {
	t.Parallel()
	tests := map[string]uint64{
		"4GB":    4_000_000_000,
		"4g":     4_000_000_000,
		"4GiB":   4 * (1 << 30),
		"512M":   512_000_000,
		"512mIb": 512 * (1 << 20),
		"1.5GiB": 1_610_612_736,
		".5Ki":   512,
		"1.1B":   2,
	}
	for input, want := range tests {
		input, want := input, want
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			got, err := ParseMemoryLimitBytes(input)
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}
}

func TestParseMemoryLimitBytesRejectsInvalid(t *testing.T) {
	t.Parallel()
	for _, input := range []string{"", "4", "0GiB", "-1GiB", "NaN", "Inf", "1XB", "1 GiB", "1GiB!", "18446744073709551616B"} {
		_, err := ParseMemoryLimitBytes(input)
		assert.Error(t, err, input)
	}
}

func TestNormalizeResourceLimitsPreservesSpellingAndCarriesBytes(t *testing.T) {
	cpu := 0.25
	got, err := NormalizeResourceLimits(ResourceLimits{Memory: " 1.5GiB ", CPU: &cpu})
	require.NoError(t, err)
	assert.Equal(t, "1.5GiB", got.Memory)
	assert.Equal(t, uint64(1_610_612_736), got.MemoryBytes)
	assert.Equal(t, 0.25, *got.CPU)
}

func TestNormalizeResourceLimitsRejectsCPUBelowKernelMinimum(t *testing.T) {
	cpu := 0.009
	_, err := NormalizeResourceLimits(ResourceLimits{CPU: &cpu})
	assert.ErrorContains(t, err, "at least 0.01")
}

func TestCPUQuotaMicros(t *testing.T) {
	for cores, want := range map[float64]uint64{0.01: 1_000, 0.25: 25_000, 0.5: 50_000, 1: 100_000, 4.5: 450_000} {
		got, err := CPUQuotaMicros(cores)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
	for _, cores := range []float64{0, -1, 0.009, math.NaN(), math.Inf(1), math.Exp2(64)} {
		_, err := CPUQuotaMicros(cores)
		assert.Error(t, err)
	}
}

func TestResolveResourceLimitsUsesLastScopePerAxis(t *testing.T) {
	globalCPU, groupCPU := 1.0, 2.5
	effective, err := Resolve(Scopes{
		Global:   &Profile{Name: "global", ResourceLimits: ResourceLimits{Memory: "4GiB", CPU: &globalCPU}},
		Group:    &Profile{Name: "group", ResourceLimits: ResourceLimits{CPU: &groupCPU}},
		Explicit: &Profile{Name: "explicit", ResourceLimits: ResourceLimits{Memory: "2GB"}},
	})
	require.NoError(t, err)
	assert.Equal(t, "2GB", effective.ResourceLimits.Memory)
	assert.Equal(t, uint64(2_000_000_000), effective.ResourceLimits.MemoryBytes)
	assert.Equal(t, 2.5, *effective.ResourceLimits.CPU)
	assert.Equal(t, "explicit", effective.Provenance.ResourceMemory.Profile)
	assert.Equal(t, "group", effective.Provenance.ResourceCPU.Profile)
}

func TestValidateResourceLimitTarget(t *testing.T) {
	limits := ResourceLimits{Memory: "1GiB"}
	require.NoError(t, ValidateResourceLimitTarget(limits, ImplementationHarnessBuiltin, "linux"))
	assert.ErrorContains(t, ValidateResourceLimitTarget(limits, ImplementationHarnessBuiltin, "darwin"), "Linux only")
	assert.ErrorContains(t, ValidateResourceLimitTarget(limits, ImplementationOff, "linux"), "implementation off")
	require.NoError(t, ValidateResourceLimitTarget(ResourceLimits{}, ImplementationOff, "darwin"))
}
