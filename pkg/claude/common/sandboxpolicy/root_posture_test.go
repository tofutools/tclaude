package sandboxpolicy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TCL-798 splits the constructed root out of the network posture. This is the
// whole truth table of that split, stated once, so a later change that
// re-welds the two — in either direction — has to fail here.
func TestRootPostureSeparatesConstructedRootFromNetworkIsolation(t *testing.T) {
	socketTiers := []AccessMode{AccessModeUnset, AccessModeOpen}
	for _, sockets := range socketTiers {
		assert.Equalf(t, RootHostInherited, RootPostureFor(NetworkHostOpen, sockets),
			"a profile that does not restrict sockets (%q) must keep the inherited root", sockets)
	}
	for _, sockets := range []AccessMode{AccessModeClosed, AccessModeList} {
		assert.Equalf(t, RootConstructed, RootPostureFor(NetworkHostOpen, sockets),
			"an explicit %q socket tier is the new independent trigger", sockets)
	}
	// The other direction of the old coupling survives: network isolation still
	// implies a constructed root whatever the socket axis says.
	for _, posture := range []NetworkPosture{NetworkIsolatedWithAgentd, NetworkFiltered} {
		for _, sockets := range append(socketTiers, AccessModeClosed, AccessModeList) {
			assert.Equalf(t, RootConstructed, RootPostureFor(posture, sockets),
				"%v must construct its root regardless of the %q socket tier", posture, sockets)
		}
	}
}

func TestExplicitFilesystemRootComposesWithAxisMinimum(t *testing.T) {
	assert.Equal(t, RootConstructed, RootPostureForMode(
		NetworkHostOpen, AccessModeOpen, FilesystemRootSeparate))
	assert.Equal(t, RootHostInherited, RootPostureForMode(
		NetworkHostOpen, AccessModeOpen, FilesystemRootInherit))
	assert.Equal(t, RootConstructed, RootPostureForMode(
		NetworkFiltered, AccessModeOpen, FilesystemRootInherit),
		"inherit is a preference and cannot weaken a stricter network requirement")
	assert.Equal(t, RootConstructed, RootPostureForMode(
		NetworkHostOpen, AccessModeClosed, FilesystemRootInherit),
		"inherit is a preference and cannot weaken a stricter socket requirement")
}

// A plan literal written before this field existed names only a network
// posture. Reading its zero-valued root as "inherit the host root" would
// unbuild the root of exactly the postures whose purpose is building one, so
// the effective reading restates the surviving implication.
func TestEffectiveRootPostureFailsClosedForPreExistingPlanLiterals(t *testing.T) {
	assert.Equal(t, RootConstructed,
		MountPlan{NetworkPosture: NetworkIsolatedWithAgentd}.EffectiveRootPosture())
	assert.Equal(t, RootConstructed,
		MountPlan{NetworkPosture: NetworkFiltered}.EffectiveRootPosture())
	assert.Equal(t, RootHostInherited,
		MountPlan{NetworkPosture: NetworkHostOpen}.EffectiveRootPosture())
	assert.Equal(t, RootConstructed, MountPlan{
		NetworkPosture: NetworkHostOpen,
		RootPosture:    RootConstructed,
	}.EffectiveRootPosture())
}

func TestRenderMountPlanDerivesRootPostureFromBothAxes(t *testing.T) {
	socketRule := &UnixSocketRules{
		Mode:  AccessModeList,
		Allow: []SocketAllowEntry{{Path: "/tmp/service.sock"}},
	}
	for _, tc := range []struct {
		name    string
		network *NetworkRules
		sockets *UnixSocketRules
		want    RootPosture
	}{
		{
			name: "host open without a socket rule keeps the walking skeleton",
			want: RootHostInherited,
		},
		{
			name:    "an explicit socket list constructs the root under host networking",
			sockets: socketRule,
			want:    RootConstructed,
		},
		{
			name:    "an explicit closed socket tier does the same",
			sockets: &UnixSocketRules{Mode: AccessModeClosed},
			want:    RootConstructed,
		},
		{
			name:    "an explicit open socket tier does not",
			sockets: &UnixSocketRules{Mode: AccessModeOpen},
			want:    RootHostInherited,
		},
		{
			name:    "closed network still constructs the root on its own",
			network: &NetworkRules{Mode: AccessModeClosed},
			want:    RootConstructed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			effective := EffectiveProfile{Network: tc.network, UnixSockets: tc.sockets}
			if effective.Network == nil {
				effective.Network = &NetworkRules{Mode: AccessModeOpen}
			}
			plan, err := RenderMountPlan(effective)
			require.NoError(t, err)
			assert.Equal(t, tc.want, plan.RootPosture)
			assert.Equal(t, tc.want, plan.EffectiveRootPosture())
		})
	}
}

func TestRenderMountPlanHonorsExplicitSeparateRoot(t *testing.T) {
	plan, err := RenderMountPlan(EffectiveProfile{
		FilesystemRoot: FilesystemRootSeparate,
		Network:        &NetworkRules{Mode: AccessModeOpen},
	})
	require.NoError(t, err)
	assert.Equal(t, RootConstructed, plan.RootPosture)
}
