package sandboxpolicy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
)

func TestNormalizeAccessRules(t *testing.T) {
	t.Run("network canonical", func(t *testing.T) {
		got, _, err := NormalizeForPersistence(Profile{
			Name: "p",
			Network: &NetworkRules{Mode: AccessModeList, Allow: []NetworkAllowEntry{
				{Domain: "API.Example.COM", IncludeSubdomains: true, Ports: []int{443, 80, 443}},
				{CIDR: "10.1.2.3/8"},
				{Host: "GitHub.com"},
			}},
		})
		require.NoError(t, err)
		require.NotNil(t, got.Network)
		assert.Equal(t, []NetworkAllowEntry{
			{CIDR: "10.0.0.0/8"},
			{Domain: "api.example.com", IncludeSubdomains: true, Ports: []int{80, 443}},
			{Host: "github.com"},
		}, got.Network.Allow)
	})

	for _, tc := range []struct {
		name string
		rule NetworkRules
		want string
	}{
		{"bad mode", NetworkRules{Mode: "filtered"}, `network.mode "filtered" is invalid`},
		{"allow outside list", NetworkRules{Mode: AccessModeOpen, Allow: []NetworkAllowEntry{{Host: "example.com"}}}, `network.allow is only valid with mode "list"`},
		{"multiple selectors", NetworkRules{Mode: AccessModeList, Allow: []NetworkAllowEntry{{Host: "a.test", Domain: "b.test"}}}, "must set exactly one"},
		{"wildcard", NetworkRules{Mode: AccessModeList, Allow: []NetworkAllowEntry{{Host: "*.example.com"}}}, "without scheme, path, port, or wildcard"},
		{"loopback cidr", NetworkRules{Mode: AccessModeList, Allow: []NetworkAllowEntry{{CIDR: "127.1.2.3/16"}}}, `use {"loopback": true} instead`},
		{"mapped loopback cidr", NetworkRules{Mode: AccessModeList, Allow: []NetworkAllowEntry{{CIDR: "::ffff:127.0.0.1/128"}}}, `use {"loopback": true} instead`},
		{"mapped IPv4 space includes loopback", NetworkRules{Mode: AccessModeList, Allow: []NetworkAllowEntry{{CIDR: "::ffff:0:0/96"}}}, `use {"loopback": true} instead`},
		{"loopback host literal", NetworkRules{Mode: AccessModeList, Allow: []NetworkAllowEntry{{Host: "127.0.0.1"}}}, `must use {"loopback": true}`},
		{"domain IP literal", NetworkRules{Mode: AccessModeList, Allow: []NetworkAllowEntry{{Domain: "192.0.2.1"}}}, "IP literals must use cidr"},
		{"bad port", NetworkRules{Mode: AccessModeList, Allow: []NetworkAllowEntry{{Host: "example.com", Ports: []int{0}}}}, "want 1..65535"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := NormalizeForPersistence(Profile{Name: "p", Network: &tc.rule})
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestReplacingLaunchDegradationsCannotWidenNewIntent(t *testing.T) {
	stale := AccessNotice{
		Class: AccessNoticeClassDegradation, Axis: "network",
		Reason: "no_mechanism", Effect: AccessNoticeEffectNotEnforced,
		Detail: "old target widened a network list",
	}
	composition := compositionNotice("network", []string{"base", "worker"})
	current := AccessNotice{
		Class: AccessNoticeClassDegradation, Axis: "unix_sockets",
		Reason: "no_mechanism", Effect: AccessNoticeEffectNotEnforced,
		Detail: "current target widens a socket list",
	}
	replaced := ReplaceAccessDegradationNotices(
		[]AccessNotice{stale, composition}, current,
	)
	assert.Equal(t, []AccessNotice{composition, current}, replaced)

	planned, err := PlannedEffectiveAccessAxes(EffectiveProfile{
		Network:       &NetworkRules{Mode: AccessModeClosed},
		AccessNotices: []AccessNotice{stale},
	})
	require.NoError(t, err)
	assert.Equal(t, AccessModeClosed, planned.Network.Mode,
		"a stale list degradation must not override newly closed intent")
}

func TestNetworkAccessCompatibilityDerivation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile Profile
		network AccessMode
		sockets AccessMode
	}{
		{"unset", Profile{}, AccessModeUnset, AccessModeUnset},
		{"internet", Profile{NetworkAccess: NetworkAccessInternet}, AccessModeOpen, AccessModeUnset},
		{"none", Profile{NetworkAccess: NetworkAccessNone}, AccessModeClosed, AccessModeClosed},
		{"new fields", Profile{
			NetworkAccess: NetworkAccessNone,
			Network:       &NetworkRules{Mode: AccessModeClosed},
			UnixSockets:   &UnixSocketRules{Mode: AccessModeOpen},
		}, AccessModeClosed, AccessModeOpen},
	} {
		t.Run(tc.name, func(t *testing.T) {
			axes, err := DeriveAccessAxes(tc.profile)
			require.NoError(t, err)
			assert.Equal(t, tc.network, axes.Network.Mode)
			assert.Equal(t, tc.sockets, axes.UnixSockets.Mode)
		})
	}
	_, err := DeriveAccessAxes(Profile{
		NetworkAccess: NetworkAccessNone,
		Network:       &NetworkRules{Mode: AccessModeOpen},
	})
	require.ErrorContains(t, err, `network_access "none" conflicts with network.mode "open"`)
}

func TestNormalizeUnixSocketRulesProtectsRootsAndRetainsMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	protected := filepath.Join(home, ".tclaude", "data")
	require.NoError(t, os.MkdirAll(protected, 0o755))

	for _, entry := range []SocketAllowEntry{
		{Path: filepath.Join(protected, "private.sock")},
		{PathGlob: filepath.Join(protected, "*.sock")},
		{PathGlob: filepath.Join(home, ".tclaude", "*", "private.sock")},
	} {
		_, _, err := NormalizeForPersistence(Profile{
			Name: "p", UnixSockets: &UnixSocketRules{
				Mode: AccessModeList, Allow: []SocketAllowEntry{entry},
			},
		})
		require.ErrorContains(t, err, "intersects protected directory")
	}

	missing := filepath.Join(home, "service.sock")
	got, missingPaths, err := NormalizeForPersistence(Profile{
		Name: "p", UnixSockets: &UnixSocketRules{
			Mode: AccessModeList, Allow: []SocketAllowEntry{{Path: missing}},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{missing}, missingPaths)
	assert.Equal(t, []SocketAllowEntry{{Path: missing}}, got.UnixSockets.Allow)
}

func TestAccessIntersectionAndCompositionNotices(t *testing.T) {
	left := NetworkRules{Mode: AccessModeList, Allow: []NetworkAllowEntry{
		{Domain: "example.com", IncludeSubdomains: true, Ports: []int{80, 443}},
	}}
	right := NetworkRules{Mode: AccessModeList, Allow: []NetworkAllowEntry{
		{Host: "api.example.com", Ports: []int{443, 8443}},
	}}
	assert.Equal(t, NetworkRules{Mode: AccessModeList, Allow: []NetworkAllowEntry{
		{Host: "api.example.com", Ports: []int{443}},
	}}, intersectNetworkRules(left, right))

	registry := map[string]*Profile{
		"github": {
			Name: "github", Network: &NetworkRules{
				Mode: AccessModeList, Allow: []NetworkAllowEntry{{Host: "github.com"}},
			},
		},
	}
	flattened, notices, err := FlattenWithNotices(Profile{
		Name: "npm", Includes: []string{"github"},
		Network: &NetworkRules{
			Mode: AccessModeList, Allow: []NetworkAllowEntry{{Host: "registry.npmjs.org"}},
		},
	}, registryLookup(registry))
	require.NoError(t, err)
	require.NotNil(t, flattened.Network)
	assert.Empty(t, flattened.Network.Allow)
	require.Len(t, notices, 1)
	assert.Equal(t, AccessNoticeClassComposition, notices[0].Class)
	assert.Equal(t, []string{"github", "npm"}, notices[0].Tiers)
	assert.Contains(t, notices[0].Detail, "github ∩ npm")

	effective, err := Resolve(Scopes{
		Global: &Profile{Name: "base", Network: &NetworkRules{
			Mode: AccessModeList, Allow: []NetworkAllowEntry{{Host: "github.com"}},
		}},
		Group: &Profile{Name: "team", Network: &NetworkRules{
			Mode: AccessModeList, Allow: []NetworkAllowEntry{{Host: "registry.npmjs.org"}},
		}},
	})
	require.NoError(t, err)
	require.Len(t, effective.AccessNotices, 1)
	assert.Equal(t, []string{`global "base"`, `group "team"`}, effective.AccessNotices[0].Tiers)

	mixedRegistry := map[string]*Profile{
		"legacy-offline": {
			Name: "legacy-offline", NetworkAccess: NetworkAccessNone,
		},
	}
	mixed, _, err := FlattenWithNotices(Profile{
		Name: "new-network", Includes: []string{"legacy-offline"},
		Network: &NetworkRules{Mode: AccessModeOpen},
	}, registryLookup(mixedRegistry))
	require.NoError(t, err)
	require.NotNil(t, mixed.Network)
	require.NotNil(t, mixed.UnixSockets,
		"materializing a new network axis must not erase legacy socket closure")
	mixedAxes, err := DeriveAccessAxes(mixed)
	require.NoError(t, err)
	assert.Equal(t, AccessModeClosed, mixedAxes.Network.Mode)
	assert.Equal(t, AccessModeClosed, mixedAxes.UnixSockets.Mode)

	mixedEffective, err := Resolve(Scopes{
		Global: &Profile{Name: "legacy-offline", NetworkAccess: NetworkAccessNone},
		Group:  &Profile{Name: "new-network", Network: &NetworkRules{Mode: AccessModeOpen}},
	})
	require.NoError(t, err)
	require.NotNil(t, mixedEffective.UnixSockets)
	mixedEffectiveAxes, err := EffectiveAccessAxes(mixedEffective)
	require.NoError(t, err)
	assert.Equal(t, AccessModeClosed, mixedEffectiveAxes.UnixSockets.Mode)
}

func TestAgentdSocketFloorSurvivesEveryAccessModeCombinationAndRawJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	floor := []string{
		agentipc.CanonicalSocketPath(),
		agentipc.LegacyHomeSocketPath(),
		agentipc.LegacySocketPath(),
	}
	require.NotContains(t, floor, "")

	modes := []AccessMode{AccessModeOpen, AccessModeClosed, AccessModeList}
	for _, networkMode := range modes {
		for _, socketMode := range modes {
			name := string(networkMode) + "-" + string(socketMode)
			t.Run(name, func(t *testing.T) {
				var profile Profile
				raw := `{"name":"p","network":{"mode":"` + string(networkMode) +
					`"},"unix_sockets":{"mode":"` + string(socketMode) +
					`","agentd_socket_floor":[],"allow":[]}}`
				require.NoError(t, json.Unmarshal([]byte(raw), &profile))
				normalized, _, err := NormalizeForPersistence(profile)
				require.NoError(t, err)
				axes, err := DeriveAccessAxes(normalized)
				require.NoError(t, err)
				access := ResolveUnixSocketAccess(axes.UnixSockets)
				for _, socket := range floor {
					assert.Contains(t, access.AllowedPaths, socket,
						"raw JSON and %s/%s modes must not remove the agentd floor",
						networkMode, socketMode)
				}
			})
		}
	}
}

func TestAccessAxisSnapshotContainmentPreservesSelectorsPortsAndAmbientBoundary(t *testing.T) {
	parent := NetworkRules{Mode: AccessModeList, Allow: []NetworkAllowEntry{{
		Domain: "example.com", IncludeSubdomains: true, Ports: []int{80, 443},
	}}}
	assert.True(t, networkRulesContained(parent, NetworkRules{
		Mode: AccessModeList, Allow: []NetworkAllowEntry{{
			Host: "api.example.com", Ports: []int{443},
		}},
	}))
	assert.False(t, networkRulesContained(parent, NetworkRules{
		Mode: AccessModeList, Allow: []NetworkAllowEntry{{
			Host: "api.example.com",
		}},
	}), "dropping a parent's port restriction is a widening")
	assert.False(t, networkRulesContained(parent, NetworkRules{}),
		"an omitted ambient child axis cannot escape a parent allow list")
	assert.True(t, networkRulesContained(NetworkRules{Mode: AccessModeOpen}, NetworkRules{}))
	assert.True(t, networkRulesContained(
		NetworkRules{Mode: AccessModeClosed},
		NetworkRules{Mode: AccessModeList},
	), "an empty network list carries no more authority than closed")

	socketParent := UnixSocketRules{Mode: AccessModeList, Allow: []SocketAllowEntry{{
		PathGlob: "/tmp/service-*.sock",
	}}}
	assert.True(t, unixSocketRulesContained(socketParent, UnixSocketRules{
		Mode: AccessModeList, Allow: []SocketAllowEntry{{Path: "/tmp/service-api.sock"}},
	}))
	assert.False(t, unixSocketRulesContained(socketParent, UnixSocketRules{}),
		"an omitted ambient child axis cannot escape a parent socket list")
	assert.True(t, unixSocketRulesContained(
		UnixSocketRules{Mode: AccessModeClosed},
		UnixSocketRules{Mode: AccessModeList},
	), "an empty socket list carries no more authority than closed")
}
