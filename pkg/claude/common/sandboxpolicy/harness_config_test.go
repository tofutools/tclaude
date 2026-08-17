package sandboxpolicy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHarnessConfigFloorAppliesUnlessExplicitWrite(t *testing.T) {
	assert.True(t, HarnessConfigFloorApplies(HarnessConfigAccessDefault),
		"omitted means the floor applies, not 'no opinion'")
	assert.True(t, HarnessConfigFloorApplies(HarnessConfigAccessRead))
	assert.False(t, HarnessConfigFloorApplies(HarnessConfigAccessWrite))
}

func TestNormalizeHarnessConfigAccessRejectsUnknown(t *testing.T) {
	for _, valid := range []HarnessConfigAccess{
		HarnessConfigAccessDefault, HarnessConfigAccessRead, HarnessConfigAccessWrite,
	} {
		got, err := NormalizeHarnessConfigAccess(valid)
		require.NoError(t, err)
		assert.Equal(t, valid, got)
	}
	_, err := NormalizeHarnessConfigAccess("deny")
	assert.Error(t, err)
}

// Strictest-wins across scopes: unlike environment (last-scope-wins), an
// explicit profile must not be able to opt out of a floor a broader scope
// pinned.
func TestResolveHarnessConfigComposesStrictestWins(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		global, group, explicit HarnessConfigAccess
		want                    HarnessConfigAccess
		wantFloor               bool
	}{
		{name: "all omitted", want: HarnessConfigAccessDefault, wantFloor: true},
		{name: "explicit opt-out", explicit: HarnessConfigAccessWrite,
			want: HarnessConfigAccessWrite, wantFloor: false},
		{name: "global opt-out", global: HarnessConfigAccessWrite,
			want: HarnessConfigAccessWrite, wantFloor: false},
		{name: "global pin beats explicit opt-out",
			global: HarnessConfigAccessRead, explicit: HarnessConfigAccessWrite,
			want: HarnessConfigAccessRead, wantFloor: true},
		{name: "group pin beats explicit opt-out",
			group: HarnessConfigAccessRead, explicit: HarnessConfigAccessWrite,
			want: HarnessConfigAccessRead, wantFloor: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scopes := Scopes{}
			if tc.global != "" {
				scopes.Global = &Profile{Name: "global", HarnessConfig: tc.global}
			}
			if tc.group != "" {
				scopes.Group = &Profile{Name: "group", HarnessConfig: tc.group}
			}
			if tc.explicit != "" {
				scopes.Explicit = &Profile{Name: "explicit", HarnessConfig: tc.explicit}
			}
			effective, err := Resolve(scopes)
			require.NoError(t, err)
			assert.Equal(t, tc.want, effective.HarnessConfig)
			assert.Equal(t, tc.wantFloor, HarnessConfigFloorApplies(effective.HarnessConfig))
		})
	}
}

func TestResolveHarnessConfigRecordsProvenance(t *testing.T) {
	effective, err := Resolve(Scopes{
		Global:   &Profile{Name: "base", HarnessConfig: HarnessConfigAccessWrite},
		Explicit: &Profile{Name: "strict", HarnessConfig: HarnessConfigAccessRead},
	})
	require.NoError(t, err)
	require.NotNil(t, effective.Provenance.HarnessConfig)
	assert.Equal(t, "strict", effective.Provenance.HarnessConfig.Profile)
	assert.Equal(t, ScopeExplicit, effective.Provenance.HarnessConfig.Scope)
}

// A floored parent must not be able to mint an unfloored child, the same
// widening rule every other axis has.
func TestRequireContainedRefusesHarnessConfigWidening(t *testing.T) {
	parent := EmptySnapshot()
	child := EmptySnapshot()
	child.Effective.HarnessConfig = HarnessConfigAccessWrite
	err := RequireContained(parent, child)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "harness config write access")

	// Narrowing is always fine.
	narrowing := EmptySnapshot()
	narrowing.Effective.HarnessConfig = HarnessConfigAccessRead
	require.NoError(t, RequireContained(parent, narrowing))

	// And an unfloored parent may still spawn an unfloored child.
	openParent := EmptySnapshot()
	openParent.Effective.HarnessConfig = HarnessConfigAccessWrite
	require.NoError(t, RequireContained(openParent, child))
}
