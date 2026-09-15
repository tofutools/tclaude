package sandboxpolicy

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeResourceLimitsCarriesThePIDCeiling(t *testing.T) {
	t.Parallel()
	pids := uint64(512)
	got, err := NormalizeResourceLimits(ResourceLimits{PIDs: &pids})
	require.NoError(t, err)
	require.NotNil(t, got.PIDs)
	assert.Equal(t, uint64(512), *got.PIDs)
	assert.True(t, got.Enabled(), "a PID ceiling alone is an authored ceiling and needs the cgroup")

	zero := uint64(0)
	_, err = NormalizeResourceLimits(ResourceLimits{PIDs: &zero})
	assert.ErrorContains(t, err, "at least 1 process",
		"pids.max 0 would refuse the workload its own first process")
}

func TestPIDCeilingSurvivesCompositionAndSnapshots(t *testing.T) {
	included, own := uint64(64), uint64(256)
	base := &Profile{Name: "base", ResourceLimits: ResourceLimits{PIDs: &included}}
	flat, err := Flatten(Profile{Name: "local", Includes: []string{"base"}},
		registryLookup(map[string]*Profile{"base": base}))
	require.NoError(t, err)
	require.NotNil(t, flat.ResourceLimits.PIDs)
	assert.Equal(t, uint64(64), *flat.ResourceLimits.PIDs, "an include with no local value still applies")

	flat.ResourceLimits.PIDs = &own
	effective, err := Resolve(Scopes{Global: &flat, Explicit: &Profile{Name: "explicit"}})
	require.NoError(t, err)
	require.NotNil(t, effective.ResourceLimits.PIDs)
	assert.Equal(t, uint64(256), *effective.ResourceLimits.PIDs)
	require.NotNil(t, effective.Provenance.ResourcePIDs)
	assert.Equal(t, "local", effective.Provenance.ResourcePIDs.Profile,
		"the axis reports its own source, not whichever scope authored memory or CPU")

	snapshot := NewSnapshot(effective, nil)
	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)
	var decoded Snapshot
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	require.NotNil(t, decoded.Effective.ResourceLimits.PIDs)
	assert.Equal(t, uint64(256), *decoded.Effective.ResourceLimits.PIDs)
	validated, err := RevalidateSnapshot(decoded)
	require.NoError(t, err)
	require.NotNil(t, validated.Effective.ResourceLimits.PIDs)
	assert.Equal(t, uint64(256), *validated.Effective.ResourceLimits.PIDs)
}

func TestPIDCeilingCannotBeWeakenedByAChildSnapshot(t *testing.T) {
	parentPIDs := uint64(128)
	parent := NewSnapshot(EffectiveProfile{
		Filesystem: []FilesystemGrant{}, Environment: []EnvironmentEntry{}, AgentDirectories: []string{},
		ResourceLimits: ResourceLimits{PIDs: &parentPIDs},
	}, nil)

	tighter := uint64(64)
	child := parent
	child.Effective.ResourceLimits = ResourceLimits{PIDs: &tighter}
	require.NoError(t, RequireContained(parent, child), "lowering the ceiling is a restriction")

	looser := uint64(1024)
	child.Effective.ResourceLimits = ResourceLimits{PIDs: &looser}
	assert.ErrorContains(t, RequireContained(parent, child), "child PID resource limit is weaker")

	child.Effective.ResourceLimits = ResourceLimits{}
	assert.ErrorContains(t, RequireContained(parent, child), "not preserved",
		"dropping the axis entirely is the widest weakening there is")
}

func TestPIDCeilingAloneRequiresTheCgroupAndRefusesOffAndDarwin(t *testing.T) {
	t.Parallel()
	pids := uint64(512)
	limits := ResourceLimits{PIDs: &pids}
	assert.True(t, ResourceCgroupRequired(limits, ImplementationHarnessBuiltin))
	require.NoError(t, ValidateResourceLimitTarget(limits, ImplementationHarnessBuiltin, "linux"))
	assert.ErrorContains(t, ValidateResourceLimitTarget(limits, ImplementationHarnessBuiltin, "darwin"), "Linux only")
	assert.ErrorContains(t, ValidateResourceLimitTarget(limits, ImplementationOff, "linux"), "implementation off")
}
