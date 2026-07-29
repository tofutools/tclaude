package sandboxpolicy

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc/agentipctest"
)

func TestMaterializeUnixSocketPathsExpandsOnlyLiveSockets(t *testing.T) {
	root := agentipctest.ShortSocketDir(t)
	first := filepath.Join(root, "service-a.sock")
	second := filepath.Join(root, "service-b.sock")
	for _, path := range []string{first, second} {
		listener, err := net.Listen("unix", path)
		require.NoError(t, err)
		t.Cleanup(func() { _ = listener.Close() })
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "service-not-a-socket"), []byte("secret"), 0o600))
	alias := filepath.Join(root, "alias.sock")
	require.NoError(t, os.Symlink(first, alias))

	got, err := MaterializeUnixSocketPaths(UnixSocketRules{
		Mode: AccessModeList,
		Allow: []SocketAllowEntry{
			{PathGlob: filepath.Join(root, "service-*")},
			{Path: alias},
			{Path: filepath.Join(root, "future.sock")},
		},
	})
	require.NoError(t, err)
	canonicalFirst, err := filepath.EvalSymlinks(first)
	require.NoError(t, err)
	canonicalSecond, err := filepath.EvalSymlinks(second)
	require.NoError(t, err)
	assert.Equal(t, []string{canonicalFirst, canonicalSecond}, got)
}

func TestUnixSocketLaunchNoticeNamesEveryUnmaterializedEntry(t *testing.T) {
	root := agentipctest.ShortSocketDir(t)
	regular := filepath.Join(root, "regular")
	require.NoError(t, os.WriteFile(regular, []byte("not a socket"), 0o600))
	missing := filepath.Join(root, "missing.sock")
	unmatched := filepath.Join(root, "future-*.sock")
	rules := UnixSocketRules{
		Mode: AccessModeList,
		Allow: []SocketAllowEntry{
			{Path: missing},
			{Path: regular},
			{PathGlob: unmatched},
		},
	}

	materialized, err := MaterializeUnixSocketList(rules)
	require.NoError(t, err)
	assert.Empty(t, materialized.Paths)
	assert.Equal(t, []string{missing, regular, unmatched}, materialized.Unmaterialized)
	assert.Equal(t, []int{0, 1, 2}, materialized.Entries)

	prepared, err := PrepareUnixSocketLaunch(rules)
	require.NoError(t, err)
	notice := UnixSocketLaunchNotice(prepared)
	require.NotNil(t, notice)
	assert.Equal(t, AccessNoticeClassLaunch, notice.Class)
	assert.Equal(t, AccessNoticeReasonUnmaterializedEntries, notice.Reason)
	assert.Equal(t, AccessNoticeEffectNotMaterialized, notice.Effect)
	assert.Equal(t, []int{0, 1, 2}, notice.Entries)
	for _, selector := range []string{missing, regular, unmatched} {
		assert.Contains(t, notice.Detail, selector)
	}
}

func TestReplacingLaunchNoticesClearsStaleMaterializationDisclosure(t *testing.T) {
	composition := compositionNotice("unix_sockets", []string{"global", "worker"})
	stale := AccessNotice{
		Class: AccessNoticeClassLaunch, Axis: "unix_sockets",
		Reason: AccessNoticeReasonUnmaterializedEntries,
		Effect: AccessNoticeEffectNotMaterialized,
		Detail: "socket was absent on the previous launch",
	}

	assert.Equal(t, []AccessNotice{composition},
		ReplaceAccessLaunchNotices([]AccessNotice{composition, stale}))
}

func TestSetUnixSocketLaunchMaterializationKeepsSurfaceAndNoticeTogether(t *testing.T) {
	snapshot := NewSnapshot(EffectiveProfile{AccessNotices: []AccessNotice{{
		Class: AccessNoticeClassLaunch, Axis: "unix_sockets",
		Reason: AccessNoticeReasonUnmaterializedEntries,
		Effect: AccessNoticeEffectNotMaterialized,
		Detail: "stale",
	}}}, nil)
	result := &UnixSocketMaterialization{
		Paths:          []string{"/tmp/live.sock"},
		Unmaterialized: []string{"/tmp/missing.sock"},
		Entries:        []int{1},
	}

	SetUnixSocketLaunchMaterialization(&snapshot, result)
	require.Equal(t, result, snapshot.UnixSocketMaterialization)
	require.Len(t, snapshot.Effective.AccessNotices, 1)
	assert.Contains(t, snapshot.Effective.AccessNotices[0].Detail, "/tmp/missing.sock")

	SetUnixSocketLaunchMaterialization(&snapshot, nil)
	assert.Nil(t, snapshot.UnixSocketMaterialization)
	assert.Empty(t, snapshot.Effective.AccessNotices)
}

func TestValidateMaterializedUnixSocketPathsRejectsUnselectedAuthority(t *testing.T) {
	root := agentipctest.ShortSocketDir(t)
	allowed := filepath.Join(root, "allowed.sock")
	unselected := filepath.Join(root, "unselected.sock")
	for _, path := range []string{allowed, unselected} {
		listener, err := net.Listen("unix", path)
		require.NoError(t, err)
		t.Cleanup(func() { _ = listener.Close() })
	}
	rules := UnixSocketRules{
		Mode: AccessModeList,
		Allow: []SocketAllowEntry{{
			Path: allowed,
		}},
	}
	canonicalAllowed, err := filepath.EvalSymlinks(allowed)
	require.NoError(t, err)
	canonicalUnselected, err := filepath.EvalSymlinks(unselected)
	require.NoError(t, err)

	require.NoError(t, ValidateMaterializedUnixSocketPaths(
		rules, []string{canonicalAllowed}))
	require.ErrorContains(t, ValidateMaterializedUnixSocketPaths(
		rules, []string{canonicalUnselected}), "selected by the authored allowlist")
}

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

func TestNetworkPackReferencesNormalizeAndMaterialize(t *testing.T) {
	normalized, _, err := NormalizeForPersistence(Profile{
		Name: "packed",
		Network: &NetworkRules{
			Baseline: NetworkBaselineDeny,
			Packs:    []string{"net-openai-codex", "net-local", "net-local"},
			Allow: []NetworkAllowEntry{{
				Domain: "example.com", Ports: []int{443},
			}},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, normalized.Network)
	assert.Equal(t, []string{"net-local", "net-openai-codex"}, normalized.Network.Packs)
	assert.Equal(t, NetworkBaselineDeny, normalized.Network.Baseline)
	assert.Empty(t, normalized.Network.Mode)

	axes, err := DeriveAccessAxes(normalized)
	require.NoError(t, err)
	assert.Equal(t, AccessModeList, axes.Network.Mode)
	assert.Empty(t, axes.Network.Baseline)
	assert.Empty(t, axes.Network.Packs)
	assert.Equal(t, []NetworkAllowEntry{
		{Domain: "api.openai.com", Ports: []int{443}},
		{Domain: "example.com", Ports: []int{443}},
		{Loopback: true},
	}, axes.Network.Allow)

	closed, err := MaterializeNetworkRules(NetworkRules{Baseline: NetworkBaselineDeny})
	require.NoError(t, err)
	assert.Equal(t, NetworkRules{Mode: AccessModeClosed}, closed)

	redundantAllow, _, err := NormalizeForPersistence(Profile{
		Name: "redundant-allow",
		Network: &NetworkRules{
			Baseline: NetworkBaselineAllow, Packs: []string{"net-local"},
			Allow: []NetworkAllowEntry{{Domain: "already-open.example"}},
		},
	})
	require.NoError(t, err)
	redundantAxes, err := DeriveAccessAxes(redundantAllow)
	require.NoError(t, err)
	assert.Equal(t, NetworkRules{Mode: AccessModeOpen}, redundantAxes.Network,
		"allow-mode rows remain authorable under Allow all but add no effective authority")

	for _, tc := range []struct {
		name  string
		rules NetworkRules
		match string
	}{
		{
			name: "unknown pack",
			rules: NetworkRules{
				Baseline: NetworkBaselineDeny, Packs: []string{"net-missing"},
			},
			match: "unknown pack",
		},
		{
			name: "legacy mode with packs",
			rules: NetworkRules{
				Mode: AccessModeList, Packs: []string{"net-local"},
			},
			match: "requires the compositional baseline",
		},
		{
			name: "baseline and mode",
			rules: NetworkRules{
				Mode: AccessModeClosed, Baseline: NetworkBaselineDeny,
			},
			match: "not both",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := NormalizeForPersistence(Profile{Name: tc.name, Network: &tc.rules})
			require.ErrorContains(t, err, tc.match)
		})
	}
}

func TestNetworkDenyAuthoringNormalizesWithoutReachingMaterialization(t *testing.T) {
	input := Profile{
		Name: "deny-authoring",
		Network: &NetworkRules{
			Baseline:  NetworkBaselineAllow,
			Packs:     []string{"net-local"},
			DenyPacks: []string{"net-npm", "net-github", "net-github"},
			Allow: []NetworkAllowEntry{
				{Domain: "same.example", Ports: []int{443}},
			},
			Deny: []NetworkAllowEntry{
				{Domain: "same.example", Ports: []int{443}},
				{CIDR: "192.0.2.9/24", Ports: []int{443, 80, 443}},
			},
		},
	}
	normalized, _, err := NormalizeForPersistence(input)
	require.NoError(t, err)
	require.NotNil(t, normalized.Network)
	assert.Equal(t, []string{"net-github", "net-npm"}, normalized.Network.DenyPacks)
	assert.Equal(t, []NetworkAllowEntry{
		{CIDR: "192.0.2.0/24", Ports: []int{80, 443}},
		{Domain: "same.example", Ports: []int{443}},
	}, normalized.Network.Deny)
	assert.Equal(t, normalized.Network.Allow[0], normalized.Network.Deny[1],
		"the same selector is valid on both sides because deny wins")

	axes, err := DeriveAccessAxes(normalized)
	require.NoError(t, err)
	assert.Equal(t, NetworkRules{Mode: AccessModeOpen}, axes.Network,
		"frontend-first deny state must not reach the existing applier seam")

	denyBaseline := *normalized.Network
	denyBaseline.Baseline = NetworkBaselineDeny
	materialized, err := MaterializeNetworkRules(denyBaseline)
	require.NoError(t, err)
	assert.Equal(t, AccessModeList, materialized.Mode)
	assert.Equal(t, []NetworkAllowEntry{
		{Domain: "same.example", Ports: []int{443}},
		{Loopback: true},
	}, materialized.Allow,
		"deny authoring does not change today's allow materialization")
	assert.Empty(t, materialized.Deny)
	assert.Empty(t, materialized.DenyPacks)
}

func TestNetworkDenyAuthoringRejectsAmbiguousOrUnsupportedShapes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rules NetworkRules
		match string
	}{
		{
			name: "pack in both modes",
			rules: NetworkRules{
				Baseline: NetworkBaselineDeny,
				Packs:    []string{"net-local"}, DenyPacks: []string{"net-local"},
			},
			match: `network pack capability "net-local" is authored in both`,
		},
		{
			name: "entries under inherit",
			rules: NetworkRules{
				Baseline: NetworkBaselineInherit,
				Deny:     []NetworkAllowEntry{{Domain: "blocked.example"}},
			},
			match: `not valid with baseline "inherit"`,
		},
		{
			name: "deny under legacy mode",
			rules: NetworkRules{
				Mode: AccessModeList,
				Deny: []NetworkAllowEntry{{Domain: "blocked.example"}},
			},
			match: "network.deny requires the compositional baseline",
		},
		{
			name: "unknown deny pack",
			rules: NetworkRules{
				Baseline:  NetworkBaselineAllow,
				DenyPacks: []string{"net-missing"},
			},
			match: `network.deny_packs[0] references unknown pack "net-missing"`,
		},
		{
			name: "invalid deny selector",
			rules: NetworkRules{
				Baseline: NetworkBaselineAllow,
				Deny:     []NetworkAllowEntry{{Host: "a.example", Domain: "b.example"}},
			},
			match: "network.deny[0] must set exactly one",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := NormalizeForPersistence(Profile{Name: tc.name, Network: &tc.rules})
			require.ErrorContains(t, err, tc.match)
		})
	}
}

func TestNetworkRulesWithoutDenyStateKeepTheirJSONBytes(t *testing.T) {
	rules := NetworkRules{
		Baseline: NetworkBaselineDeny,
		Packs:    []string{"net-local"},
		Allow: []NetworkAllowEntry{{
			Domain: "api.example.com", Ports: []int{443},
		}},
	}
	raw, err := json.Marshal(rules)
	require.NoError(t, err)
	assert.Equal(t,
		`{"baseline":"deny","packs":["net-local"],"allow":[{"domain":"api.example.com","ports":[443]}]}`,
		string(raw),
	)
	var decoded NetworkRules
	require.NoError(t, json.Unmarshal(raw, &decoded))
	roundTrip, err := json.Marshal(decoded)
	require.NoError(t, err)
	assert.Equal(t, raw, roundTrip)
}

func TestNetworkPackMaterializationMatchesManualRules(t *testing.T) {
	packed, err := MaterializeNetworkRules(NetworkRules{
		Baseline: NetworkBaselineDeny,
		Packs:    []string{"net-local", "net-anthropic", "net-openai-codex"},
	})
	require.NoError(t, err)
	manual, err := normalizeNetworkRules(&NetworkRules{
		Mode: AccessModeList,
		Allow: []NetworkAllowEntry{
			{Loopback: true},
			{Domain: "api.anthropic.com", Ports: []int{443}},
			{Domain: "api.openai.com", Ports: []int{443}},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, *manual, packed)
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
	alias := filepath.Join(home, "private-socket-alias")
	require.NoError(t, os.Symlink(protected, alias))

	for _, entry := range []SocketAllowEntry{
		{Path: filepath.Join(protected, "private.sock")},
		{PathGlob: filepath.Join(protected, "*.sock")},
		{PathGlob: filepath.Join(home, ".tclaude", "*", "private.sock")},
		{Path: filepath.Join(alias, "private.sock")},
		{PathGlob: filepath.Join(alias, "*.sock")},
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
