package sandboxpolicy

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveComposesScopesWithProvenance(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	a := filepath.Join(home, "a")
	b := filepath.Join(home, "b")
	c := filepath.Join(home, "c")
	for _, path := range []string{a, b, c} {
		require.NoError(t, os.Mkdir(path, 0o755))
	}
	canonicalA, err := filepath.EvalSymlinks(a)
	require.NoError(t, err)
	canonicalB, err := filepath.EvalSymlinks(b)
	require.NoError(t, err)
	canonicalC, err := filepath.EvalSymlinks(c)
	require.NoError(t, err)

	global := &Profile{
		Name:          " global ",
		NetworkAccess: NetworkAccessInternet,
		Filesystem: []FilesystemGrant{
			{Path: a, Access: AccessRead},
			{Path: b, Access: AccessWrite},
		},
		Environment: []EnvironmentEntry{{Name: "SHARED", Value: "global"}, {Name: "GLOBAL_ONLY", Value: "yes"}},
	}
	group := &Profile{
		Name: "group",
		Filesystem: []FilesystemGrant{
			{Path: a, Access: AccessWrite},
			{Path: c, Access: AccessRead},
		},
		Environment: []EnvironmentEntry{{Name: "SHARED", Value: "group"}, {Name: "GROUP_ONLY", Value: "yes"}},
	}
	explicit := &Profile{
		Name:          "explicit",
		Filesystem:    []FilesystemGrant{{Path: b, Access: AccessRead}},
		Environment:   []EnvironmentEntry{{Name: "SHARED", Value: "explicit"}},
		NetworkAccess: NetworkAccessNone,
	}

	got, err := Resolve(Scopes{Global: global, Group: group, Explicit: explicit})
	require.NoError(t, err)
	assert.Equal(t, []FilesystemGrant{
		{Path: canonicalA, Access: AccessWrite},
		{Path: canonicalB, Access: AccessWrite},
		{Path: canonicalC, Access: AccessRead},
	}, got.Filesystem)
	assert.Equal(t, []EnvironmentEntry{
		{Name: "GLOBAL_ONLY", Value: "yes"},
		{Name: "GROUP_ONLY", Value: "yes"},
		{Name: "SHARED", Value: "explicit"},
	}, got.Environment)
	assert.Equal(t, []ProfileSource{
		{Scope: ScopeGlobal, Profile: "global"},
		{Scope: ScopeGroup, Profile: "group"},
		{Scope: ScopeExplicit, Profile: "explicit"},
	}, got.Provenance.Applied)
	assert.Equal(t, []ProfileSource{
		{Scope: ScopeGlobal, Profile: "global"},
		{Scope: ScopeGroup, Profile: "group"},
	}, got.Provenance.Filesystem[canonicalA])
	assert.Equal(t, []ProfileSource{
		{Scope: ScopeGlobal, Profile: "global"},
		{Scope: ScopeExplicit, Profile: "explicit"},
	}, got.Provenance.Filesystem[canonicalB], "later read does not weaken an earlier write")
	assert.Equal(t, ProfileSource{Scope: ScopeExplicit, Profile: "explicit"}, got.Provenance.Environment["SHARED"])
	assert.Equal(t, ProfileSource{Scope: ScopeGlobal, Profile: "global"}, got.Provenance.Environment["GLOBAL_ONLY"])
	assert.Equal(t, NetworkAccessNone, got.NetworkAccess)
	require.NotNil(t, got.Provenance.Network)
	assert.Equal(t, ProfileSource{Scope: ScopeExplicit, Profile: "explicit"}, *got.Provenance.Network)
	assert.Equal(t, " global ", global.Name, "resolution does not mutate inputs")
}

func TestResolveEmptyScopesReturnsNonNilCollections(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	got, err := Resolve(Scopes{})
	require.NoError(t, err)
	assert.NotNil(t, got.Filesystem)
	assert.NotNil(t, got.Environment)
	assert.NotNil(t, got.Provenance.Applied)
	assert.NotNil(t, got.Provenance.Filesystem)
	assert.NotNil(t, got.Provenance.Environment)
}

func TestResolveMaterializesNetworkPacksBeforeSnapshotHandoff(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	effective, err := Resolve(Scopes{Explicit: &Profile{
		Name: "packed",
		Network: &NetworkRules{
			Baseline:  NetworkBaselineDeny,
			Packs:     []string{"net-local", "net-anthropic"},
			DenyPacks: []string{"net-npm"},
			Deny:      []NetworkAllowEntry{{Host: "blocked.example"}},
		},
	}})
	require.NoError(t, err)
	require.NotNil(t, effective.Network)
	assert.Equal(t, AccessModeList, effective.Network.Mode)
	assert.Empty(t, effective.Network.Baseline)
	assert.Empty(t, effective.Network.Packs)
	assert.Equal(t, []NetworkAllowEntry{
		{Domain: "api.anthropic.com", Ports: []int{443}},
		{Loopback: true},
	}, effective.Network.Allow)
	assert.Equal(t, []NetworkAllowEntry{
		{Domain: "registry.npmjs.org"},
		{Host: "blocked.example"},
	}, effective.Network.Deny)

	snapshot := NewSnapshot(effective, nil)
	require.NotNil(t, snapshot.Effective.Network)
	assert.Equal(t, effective.Network, snapshot.Effective.Network,
		"the immutable launch snapshot freezes expanded authority, not pack references")

	revalidated, err := RevalidateSnapshot(snapshot)
	require.NoError(t, err)
	assert.Equal(t, snapshot.Effective.Network, revalidated.Effective.Network)

	path, digest, err := WriteSnapshotFile(t.TempDir(), snapshot)
	require.NoError(t, err)
	handedOff, err := ReadSnapshotFile(path, digest)
	require.NoError(t, err)
	assert.Equal(t, snapshot.Effective.Network, handedOff.Effective.Network)
}

func TestResolveEffectiveDenyAggregateRevalidates(t *testing.T) {
	profile := func(name string, count int) *Profile {
		denies := make([]NetworkAllowEntry, count)
		for i := range count {
			denies[i] = NetworkAllowEntry{
				Host: fmt.Sprintf("%s-%03d.example", name, i),
			}
		}
		return &Profile{
			Name: name,
			Network: &NetworkRules{
				Baseline: NetworkBaselineAllow,
				Deny:     denies,
			},
		}
	}
	effective, err := Resolve(Scopes{
		Global: profile("global", 100),
		Group:  profile("group", 100),
	})
	require.NoError(t, err)
	require.NotNil(t, effective.Network)
	assert.Len(t, effective.Network.Deny, 200)

	revalidated, err := RevalidateSnapshot(NewSnapshot(effective, nil))
	require.NoError(t, err)
	assert.Equal(t, effective.Network, revalidated.Effective.Network)

	overflow := NewSnapshot(effective, nil)
	overflow.Effective.Network.Deny =
		profile("overflow", MaxEffectiveNetworkDenyEntries+1).Network.Deny
	_, err = RevalidateSnapshot(overflow)
	require.ErrorContains(t, err,
		"effective network.deny has too many entries")
}

func TestResolveRetainsCanonicalMissingFilesystemRule(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	parent := filepath.Join(home, "shared")
	require.NoError(t, os.Mkdir(parent, 0o755))
	canonicalParent, err := filepath.EvalSymlinks(parent)
	require.NoError(t, err)
	missing := filepath.Join(parent, "future", "cache")

	got, err := Resolve(Scopes{Explicit: &Profile{
		Name: "future", Filesystem: []FilesystemGrant{{Path: missing, Access: AccessWrite}},
	}})
	require.NoError(t, err)
	canonicalMissing := filepath.Join(canonicalParent, "future", "cache")
	assert.Equal(t, []FilesystemGrant{{Path: canonicalMissing, Access: AccessWrite}}, got.Filesystem)
	assert.Equal(t, []ProfileSource{{Scope: ScopeExplicit, Profile: "future"}}, got.Provenance.Filesystem[canonicalMissing])
}

func TestResolveExplicitDenyDominatesAmbientGrant(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "shared")
	require.NoError(t, os.Mkdir(dir, 0o755))
	canonical, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)

	got, err := Resolve(Scopes{
		Global:   &Profile{Name: "global", Filesystem: []FilesystemGrant{{Path: dir, Access: AccessWrite}}},
		Explicit: &Profile{Name: "restricted-agent", Filesystem: []FilesystemGrant{{Path: dir, Access: AccessDeny}}},
	})
	require.NoError(t, err)
	assert.Equal(t, []FilesystemGrant{{Path: canonical, Access: AccessDeny}}, got.Filesystem)
	assert.Equal(t, []ProfileSource{
		{Scope: ScopeGlobal, Profile: "global"},
		{Scope: ScopeExplicit, Profile: "restricted-agent"},
	}, got.Provenance.Filesystem[canonical])
}

func TestResolveRevalidatesPersistedCanonicalPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	mount := filepath.Join(home, "mount")
	require.NoError(t, os.Mkdir(mount, 0o755))

	// Simulate the canonical profile value persisted when mount was a safe
	// directory. Replace that directory with a symlink into protected state
	// before resolution; Normalize must run again and fail closed.
	persisted, err := Normalize(Profile{Name: "saved", Filesystem: []FilesystemGrant{{Path: mount, Access: AccessWrite}}})
	require.NoError(t, err)
	require.NoError(t, os.Rename(mount, filepath.Join(home, "old-mount")))
	protected := filepath.Join(home, ".claude", "sessions")
	require.NoError(t, os.MkdirAll(protected, 0o755))
	require.NoError(t, os.Symlink(protected, mount))

	_, err = Resolve(Scopes{Global: &persisted})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at resolution time")
	assert.Contains(t, err.Error(), "intersects protected")
}

func TestResolveRecanonicalizesPathChangedSincePersistence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	mount := filepath.Join(home, "mount")
	oldTarget := filepath.Join(home, "old-target")
	newTarget := filepath.Join(home, "new-target")
	require.NoError(t, os.Mkdir(mount, 0o755))
	require.NoError(t, os.Mkdir(newTarget, 0o755))
	persisted, err := Normalize(Profile{Name: "saved", Filesystem: []FilesystemGrant{{Path: mount, Access: AccessRead}}})
	require.NoError(t, err)
	require.NoError(t, os.Rename(mount, oldTarget))
	require.NoError(t, os.Symlink(newTarget, mount))
	canonicalNew, err := filepath.EvalSymlinks(newTarget)
	require.NoError(t, err)

	got, err := Resolve(Scopes{Global: &persisted})
	require.NoError(t, err)
	assert.Equal(t, []FilesystemGrant{{Path: canonicalNew, Access: AccessRead}}, got.Filesystem)
	assert.Equal(t, []ProfileSource{{Scope: ScopeGlobal, Profile: "saved"}}, got.Provenance.Filesystem[canonicalNew])
}

func TestResolveCarriesObservableMountAliases(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, root string) (spelled, link, target string)
	}{
		{
			name: "ancestor symlink",
			setup: func(t *testing.T, root string) (string, string, string) {
				target := filepath.Join(root, "real")
				link := filepath.Join(root, "alias")
				require.NoError(t, os.MkdirAll(filepath.Join(target, "nested"), 0o755))
				require.NoError(t, os.Symlink(target, link))
				return filepath.Join(link, "nested"), link, target
			},
		},
		{
			name: "symlink resolving through a second symlink",
			setup: func(t *testing.T, root string) (string, string, string) {
				target := filepath.Join(root, "real")
				second := filepath.Join(root, "second")
				link := filepath.Join(root, "first")
				require.NoError(t, os.MkdirAll(filepath.Join(target, "nested"), 0o755))
				require.NoError(t, os.Symlink(target, second))
				require.NoError(t, os.Symlink(second, link))
				return filepath.Join(link, "nested"), link, target
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			require.NoError(t, err)
			t.Setenv("HOME", root)
			spelled, link, target := tc.setup(t, root)
			probe := filepath.Join(target, "nested", "probe")
			require.NoError(t, os.WriteFile(probe, []byte("alias-ok"), 0o600))

			effective, err := Resolve(Scopes{Explicit: &Profile{
				Name:       "aliases",
				Filesystem: []FilesystemGrant{{Path: spelled, Access: AccessRead}},
			}})
			require.NoError(t, err)
			require.Equal(t, []MountAlias{{Link: link, Target: target}}, effective.MountAliases)

			plan, err := RenderMountPlan(effective)
			require.NoError(t, err)
			require.Equal(t, effective.MountAliases, plan.Aliases)
			require.Equal(t, []MountEntry{{
				Path: filepath.Join(target, "nested"),
				Mode: MountRO,
			}}, plan.Entries)

			relative, err := filepath.Rel(link, spelled)
			require.NoError(t, err)
			mapped := filepath.Join(plan.Aliases[0].Target, relative, "probe")
			assert.Equal(t, probe, mapped,
				"recreating the highest alias must map an open through the spelling to the bound target")
			content, err := os.ReadFile(mapped)
			require.NoError(t, err)
			assert.Equal(t, "alias-ok", string(content))
		})
	}
}

func TestResolveCarriesPersistedFilesystemSpellingsIntoMountPlan(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	target := filepath.Join(root, "real")
	link := filepath.Join(root, "alias")
	spelled := filepath.Join(link, "nested")
	require.NoError(t, os.MkdirAll(filepath.Join(target, "nested"), 0o755))
	require.NoError(t, os.Symlink(target, link))

	persisted, _, err := NormalizeForAuthoring(Profile{
		Name: "persisted-alias",
		Filesystem: []FilesystemGrant{{
			Path: spelled, Access: AccessRead,
		}},
	})
	require.NoError(t, err)
	require.NotNil(t, persisted.FilesystemSpellings)

	effective, err := Resolve(Scopes{Explicit: &persisted})
	require.NoError(t, err)
	assert.Equal(t, []FilesystemGrant{{
		Path: filepath.Join(target, "nested"), Access: AccessRead,
	}}, effective.Filesystem)
	assert.Equal(t, []MountAlias{{Link: link, Target: target}}, effective.MountAliases)
	plan, err := RenderMountPlan(effective)
	require.NoError(t, err)
	assert.Equal(t, effective.MountAliases, plan.Aliases)
}

func TestAliasDiscoveryValidatesTheTargetCapturedByTheSameWalk(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	original := filepath.Join(root, "original")
	current := filepath.Join(root, "current")
	link := filepath.Join(root, "alias")
	require.NoError(t, os.Mkdir(original, 0o755))
	require.NoError(t, os.Mkdir(current, 0o755))
	require.NoError(t, os.Symlink(current, link))

	aliases, discovered, err := mountAliasesForPath(link)
	require.NoError(t, err)
	require.Equal(t, []MountAlias{{Link: link, Target: current}}, aliases)
	require.Equal(t, current, discovered)

	// Restore the spelling before validation. A second filesystem walk would
	// now pass and publish the stale alias captured above; validating the
	// discovered target itself must still refuse it.
	require.NoError(t, os.Remove(link))
	require.NoError(t, os.Symlink(original, link))
	err = validateDiscoveredFilesystemSpellingTarget(
		"race", link, original, discovered,
	)
	require.Error(t, err)
	assert.ErrorContains(t, err, `sandbox profile "race"`)
	assert.ErrorContains(t, err, `retained spelling "`+link+`"`)
	assert.ErrorContains(t, err, `originally resolved to "`+original+`"`)
	assert.ErrorContains(t, err, `now resolves to "`+current+`"`)
}

func TestResolveEnforcesAggregateEnvironmentLimits(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	entries := func(prefix string, count int) []EnvironmentEntry {
		out := make([]EnvironmentEntry, count)
		for i := range out {
			out[i] = EnvironmentEntry{Name: fmt.Sprintf("%s_%03d", prefix, i), Value: "x"}
		}
		return out
	}
	global := &Profile{Name: "global", Environment: entries("GLOBAL", 65)}
	group := &Profile{Name: "group", Environment: entries("GROUP", 64)}
	_, err := Resolve(Scopes{Global: global, Group: group})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "effective environment")
	assert.Contains(t, err.Error(), "too many entries")
}

func TestResolveAgentDirectoriesUnionAndLiteralConflict(t *testing.T) {
	global := &Profile{Name: "global", AgentDirectories: []string{"GOCACHE", "GOLANGCI_LINT_CACHE"}}
	group := &Profile{Name: "group", AgentDirectories: []string{"GOCACHE"}}
	got, err := Resolve(Scopes{Global: global, Group: group})
	require.NoError(t, err)
	assert.Equal(t, []string{"GOCACHE", "GOLANGCI_LINT_CACHE"}, got.AgentDirectories)
	assert.Equal(t, []ProfileSource{
		{Scope: ScopeGlobal, Profile: "global"},
		{Scope: ScopeGroup, Profile: "group"},
	}, got.Provenance.AgentDirectories["GOCACHE"])

	_, err = Resolve(Scopes{
		Global:   global,
		Explicit: &Profile{Name: "explicit", Environment: []EnvironmentEntry{{Name: "GOCACHE", Value: "/literal"}}},
	})
	require.ErrorContains(t, err, `environment variable "GOCACHE" is both literal and agent-owned`)
}

func TestResolveWrapsScopeValidationErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	bad := &Profile{Name: "bad", Environment: []EnvironmentEntry{{Name: "PATH", Value: "nope"}}}
	_, err := Resolve(Scopes{Group: bad})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `normalize group sandbox profile "bad" at resolution time`)
	assert.Contains(t, err.Error(), "reserved")
}
