package sandboxpolicy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// protectedHome installs an isolated $HOME containing the three protected
// roots so every protected-path test operates on temporary state. Production
// tclaude/harness state is never read or written by these tests.
func protectedHome(t *testing.T) (home, tclaudeData, claudeSessions, codexHome string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	tclaudeData = filepath.Join(home, ".tclaude", "data")
	claudeSessions = filepath.Join(home, ".claude", "sessions")
	codexHome = filepath.Join(home, ".codex")
	for _, path := range []string{tclaudeData, claudeSessions, codexHome} {
		require.NoError(t, os.MkdirAll(path, 0o755))
	}
	canonical, err := filepath.EvalSymlinks(home)
	require.NoError(t, err)
	// Callers compare against canonical output, and macOS /var → /private/var
	// makes the temp home a symlink.
	return canonical,
		filepath.Join(canonical, ".tclaude", "data"),
		filepath.Join(canonical, ".claude", "sessions"),
		filepath.Join(canonical, ".codex")
}

// TestOmittedBaselineIsByteIdenticalToLegacy pins the compatibility
// requirement: a profile that sets neither new field must normalize, resolve,
// and serialize exactly as it did before TCL-609.
func TestOmittedBaselineIsByteIdenticalToLegacy(t *testing.T) {
	home, _, _, _ := protectedHome(t)
	workspace := filepath.Join(home, "workspace")
	require.NoError(t, os.Mkdir(workspace, 0o755))

	profile, missing, err := NormalizeForPersistence(Profile{
		Name:       "legacy",
		Filesystem: []FilesystemGrant{{Path: workspace, Access: AccessWrite}},
	})
	require.NoError(t, err)
	assert.Empty(t, missing)
	assert.Equal(t, ReadBaselineDefault, profile.ReadBaseline)
	assert.Nil(t, profile.BreakGlassFilesystem)

	encoded, err := json.Marshal(profile)
	require.NoError(t, err)
	assert.JSONEq(t,
		`{"name":"legacy","filesystem":[{"path":`+mustJSON(t, workspace)+`,"access":"write"}]}`,
		string(encoded),
		"an untouched profile must not gain read_baseline or break_glass_filesystem keys")

	effective, err := Resolve(Scopes{Explicit: &profile})
	require.NoError(t, err)
	assert.Equal(t, ReadBaselineDefault, effective.ReadBaseline)
	assert.Empty(t, effective.BreakGlassFilesystem)
	assert.Nil(t, effective.Provenance.ReadBaseline)
	assert.Empty(t, effective.Provenance.BreakGlassFilesystem)

	encodedEffective, err := json.Marshal(effective)
	require.NoError(t, err)
	assert.NotContains(t, string(encodedEffective), "read_baseline")
	assert.NotContains(t, string(encodedEffective), "break_glass_filesystem")

	// A legacy snapshot must still revalidate and report no added capability.
	snapshot := NewSnapshot(effective, nil)
	revalidated, err := RevalidateSnapshot(snapshot)
	require.NoError(t, err)
	assert.Equal(t, ReadBaselineDefault, revalidated.Effective.ReadBaseline)
	assert.Nil(t, revalidated.Effective.BreakGlassFilesystem)
}

func mustJSON(t *testing.T, v string) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

func TestNormalizeReadBaseline(t *testing.T) {
	for _, tc := range []struct {
		in   ReadBaseline
		want ReadBaseline
		err  bool
	}{
		{in: ReadBaselineDefault, want: ReadBaselineDefault},
		{in: ReadBaselineMinimal, want: ReadBaselineMinimal},
		{in: "default", want: ReadBaselineDefault},
		{in: "strict", err: true},
		{in: "MINIMAL", err: true},
	} {
		got, err := NormalizeReadBaseline(tc.in)
		if tc.err {
			require.Error(t, err, "input %q", tc.in)
			continue
		}
		require.NoError(t, err, "input %q", tc.in)
		assert.Equal(t, tc.want, got, "input %q", tc.in)
	}
}

// The explicit "default" spelling is accepted from a UI selector but must
// never survive normalization, so persisted profiles have exactly one spelling.
func TestReadBaselineDefaultAliasNormalizesAway(t *testing.T) {
	protectedHome(t)
	profile, _, err := NormalizeForPersistence(Profile{Name: "p", ReadBaseline: "default"})
	require.NoError(t, err)
	assert.Equal(t, ReadBaselineDefault, profile.ReadBaseline)
	encoded, err := json.Marshal(profile)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "read_baseline")
}

func TestReadBaselineComposesStrictestWinsAcrossScopes(t *testing.T) {
	protectedHome(t)
	minimal := &Profile{Name: "minimal", ReadBaseline: ReadBaselineMinimal}
	broad := &Profile{Name: "broad"}

	// A later, broader scope cannot widen an earlier minimal one.
	got, err := Resolve(Scopes{Global: minimal, Group: broad, Explicit: broad})
	require.NoError(t, err)
	assert.Equal(t, ReadBaselineMinimal, got.ReadBaseline)
	require.NotNil(t, got.Provenance.ReadBaseline)
	assert.Equal(t, ProfileSource{Scope: ScopeGlobal, Profile: "minimal"}, *got.Provenance.ReadBaseline,
		"provenance must name the scope that imposed minimal")

	// An earlier broad scope does not stop a later scope from tightening.
	got, err = Resolve(Scopes{Global: broad, Explicit: minimal})
	require.NoError(t, err)
	assert.Equal(t, ReadBaselineMinimal, got.ReadBaseline)
	require.NotNil(t, got.Provenance.ReadBaseline)
	assert.Equal(t, ScopeExplicit, got.Provenance.ReadBaseline.Scope)
}

func TestReadBaselineComposesStrictestWinsAcrossIncludes(t *testing.T) {
	protectedHome(t)
	registry := map[string]*Profile{
		"strict-base": {Name: "strict-base", ReadBaseline: ReadBaselineMinimal},
	}
	// The including profile omits the field entirely; it must NOT widen the
	// included minimal back to the default baseline.
	got, err := Flatten(Profile{Name: "child", Includes: []string{"strict-base"}}, func(name string) (*Profile, error) {
		return registry[name], nil
	})
	require.NoError(t, err)
	assert.Equal(t, ReadBaselineMinimal, got.ReadBaseline)
}

func TestOrdinaryFilesystemStillRejectsProtectedPaths(t *testing.T) {
	_, tclaudeData, claudeSessions, codexHome := protectedHome(t)
	for _, path := range []string{tclaudeData, claudeSessions, codexHome} {
		for _, access := range []Access{AccessRead, AccessWrite} {
			_, _, err := NormalizeForPersistence(Profile{
				Name:       "p",
				Filesystem: []FilesystemGrant{{Path: path, Access: access}},
			})
			require.Error(t, err, "%s %s must stay rejected on the ordinary filesystem field", access, path)
			assert.Contains(t, err.Error(), "intersects protected directory")
		}
	}
}

func TestBreakGlassAcceptsProtectedPathsOnly(t *testing.T) {
	home, tclaudeData, _, _ := protectedHome(t)
	ordinary := filepath.Join(home, "workspace")
	require.NoError(t, os.Mkdir(ordinary, 0o755))

	profile, _, err := NormalizeForPersistence(Profile{
		Name:                 "debug-tclaude",
		BreakGlassFilesystem: []BreakGlassGrant{{Path: tclaudeData, Access: AccessRead}},
	})
	require.NoError(t, err)
	assert.Equal(t, []BreakGlassGrant{{Path: tclaudeData, Access: AccessRead}}, profile.BreakGlassFilesystem)
	assert.True(t, profile.HasBreakGlass())

	// An ordinary path in the dangerous field is a category error: it would
	// carry a danger marker and demand an acknowledgement for nothing.
	_, _, err = NormalizeForPersistence(Profile{
		Name:                 "p",
		BreakGlassFilesystem: []BreakGlassGrant{{Path: ordinary, Access: AccessRead}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not inside a protected directory")
}

// Break-glass must not become a whole-host grant wearing a danger label: a
// path that merely CONTAINS a protected root (home, or /) is rejected, so the
// hatch can only ever narrow to the protected trees themselves.
func TestBreakGlassRejectsAncestorsOfProtectedRoots(t *testing.T) {
	home, _, _, _ := protectedHome(t)
	for _, path := range []string{home, "/", filepath.Join(home, ".tclaude")} {
		_, _, err := NormalizeForPersistence(Profile{
			Name:                 "p",
			BreakGlassFilesystem: []BreakGlassGrant{{Path: path, Access: AccessRead}},
		})
		require.Error(t, err, "%q contains a protected root and must not qualify as break-glass", path)
		assert.Contains(t, err.Error(), "is not inside a protected directory")
	}
}

// The narrowest useful grant is a subdirectory of a protected root, which must
// stay representable.
func TestBreakGlassAcceptsSubdirectoryOfProtectedRoot(t *testing.T) {
	_, tclaudeData, _, _ := protectedHome(t)
	processes := filepath.Join(tclaudeData, "processes")
	require.NoError(t, os.MkdirAll(processes, 0o755))
	profile, _, err := NormalizeForPersistence(Profile{
		Name:                 "p",
		BreakGlassFilesystem: []BreakGlassGrant{{Path: processes, Access: AccessRead}},
	})
	require.NoError(t, err)
	assert.Equal(t, []BreakGlassGrant{{Path: processes, Access: AccessRead}}, profile.BreakGlassFilesystem)
}

func TestBreakGlassRejectsDenyAccess(t *testing.T) {
	_, tclaudeData, _, _ := protectedHome(t)
	_, _, err := NormalizeForPersistence(Profile{
		Name:                 "p",
		BreakGlassFilesystem: []BreakGlassGrant{{Path: tclaudeData, Access: AccessDeny}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "want read or write")
}

// Read must never imply write: the two accesses are distinct authority, and
// read-only inspection of the daemon database is materially less dangerous.
func TestBreakGlassReadDoesNotImplyWrite(t *testing.T) {
	_, tclaudeData, _, _ := protectedHome(t)
	profile, _, err := NormalizeForPersistence(Profile{
		Name:                 "p",
		BreakGlassFilesystem: []BreakGlassGrant{{Path: tclaudeData, Access: AccessRead}},
	})
	require.NoError(t, err)
	require.Len(t, profile.BreakGlassFilesystem, 1)
	assert.Equal(t, AccessRead, profile.BreakGlassFilesystem[0].Access)
}

func TestBreakGlassFoldsDuplicatePathsWriteDominating(t *testing.T) {
	_, tclaudeData, _, _ := protectedHome(t)
	profile, _, err := NormalizeForPersistence(Profile{
		Name: "p",
		BreakGlassFilesystem: []BreakGlassGrant{
			{Path: tclaudeData, Access: AccessWrite},
			{Path: tclaudeData, Access: AccessRead},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, []BreakGlassGrant{{Path: tclaudeData, Access: AccessWrite}}, profile.BreakGlassFilesystem)
}

// A symlink into a protected tree must be canonicalized before the protected
// check, so an alias can neither smuggle protected access through the ordinary
// filesystem field nor escape the break-glass danger marker.
func TestBreakGlassCanonicalizesSymlinkAliases(t *testing.T) {
	home, tclaudeData, _, _ := protectedHome(t)
	alias := filepath.Join(home, "alias-to-state")
	require.NoError(t, os.Symlink(tclaudeData, alias))

	profile, _, err := NormalizeForPersistence(Profile{
		Name:                 "p",
		BreakGlassFilesystem: []BreakGlassGrant{{Path: alias, Access: AccessRead}},
	})
	require.NoError(t, err)
	assert.Equal(t, []BreakGlassGrant{{Path: tclaudeData, Access: AccessRead}}, profile.BreakGlassFilesystem)

	_, _, err = NormalizeForPersistence(Profile{
		Name:       "p",
		Filesystem: []FilesystemGrant{{Path: alias, Access: AccessRead}},
	})
	require.Error(t, err, "a symlink alias must not bypass the ordinary protected-path rejection")
}

func TestBreakGlassResolvesAsUnionWithVisibleProvenance(t *testing.T) {
	_, tclaudeData, _, codexHome := protectedHome(t)
	global := &Profile{
		Name:                 "global-debug",
		BreakGlassFilesystem: []BreakGlassGrant{{Path: tclaudeData, Access: AccessRead}},
	}
	explicit := &Profile{
		Name: "explicit-debug",
		BreakGlassFilesystem: []BreakGlassGrant{
			{Path: tclaudeData, Access: AccessWrite},
			{Path: codexHome, Access: AccessRead},
		},
	}
	got, err := Resolve(Scopes{Global: global, Explicit: explicit})
	require.NoError(t, err)
	assert.Equal(t, []BreakGlassGrant{
		{Path: codexHome, Access: AccessRead},
		{Path: tclaudeData, Access: AccessWrite},
	}, got.BreakGlassFilesystem)
	assert.Equal(t, []ProfileSource{
		{Scope: ScopeGlobal, Profile: "global-debug"},
		{Scope: ScopeExplicit, Profile: "explicit-debug"},
	}, got.Provenance.BreakGlassFilesystem[tclaudeData],
		"every scope contributing dangerous authority must remain visible")
	assert.True(t, got.HasBreakGlass())
}

// Composition must never hide where dangerous authority came from — an include
// that supplies break-glass keeps contributing it even when the including
// profile says nothing.
func TestBreakGlassSurvivesIncludeComposition(t *testing.T) {
	_, tclaudeData, _, _ := protectedHome(t)
	registry := map[string]*Profile{
		"danger": {Name: "danger", BreakGlassFilesystem: []BreakGlassGrant{{Path: tclaudeData, Access: AccessWrite}}},
	}
	// The including profile asks only for read; the union must keep write,
	// because an include may not silently downgrade recorded authority either.
	got, err := Flatten(Profile{
		Name:                 "child",
		Includes:             []string{"danger"},
		BreakGlassFilesystem: []BreakGlassGrant{{Path: tclaudeData, Access: AccessRead}},
	}, func(name string) (*Profile, error) { return registry[name], nil })
	require.NoError(t, err)
	require.Len(t, got.BreakGlassFilesystem, 1)
	assert.Equal(t, tclaudeData, got.BreakGlassFilesystem[0].Path)
	assert.Equal(t, AccessWrite, got.BreakGlassFilesystem[0].Access)
	effective, err := Resolve(Scopes{Explicit: &got})
	require.NoError(t, err)
	assert.Equal(t, []ProfileSource{
		{Scope: ScopeExplicit, Profile: "child"},
		{Scope: ScopeExplicit, Profile: "danger", IncludedBy: "child", Chain: []string{"danger", "child"}},
	}, effective.Provenance.BreakGlassFilesystem[tclaudeData],
		"direct same-path authorship and the included route must both survive")
}

// Include provenance must survive arbitrary nesting: an operator auditing a
// dangerous grant has to be pointed at the profile that actually authored it,
// not at whichever innocent-looking wrapper happens to be assigned.
func TestBreakGlassOriginSurvivesNestedIncludes(t *testing.T) {
	_, tclaudeData, _, _ := protectedHome(t)
	registry := map[string]*Profile{
		"leaf":   {Name: "leaf", BreakGlassFilesystem: []BreakGlassGrant{{Path: tclaudeData, Access: AccessWrite}}},
		"middle": {Name: "middle", Includes: []string{"leaf"}},
		"outer":  {Name: "outer", Includes: []string{"middle"}},
	}
	lookup := func(name string) (*Profile, error) { return registry[name], nil }

	flattened, err := Flatten(Profile{Name: "assigned", Includes: []string{"outer"}}, lookup)
	require.NoError(t, err)
	require.Len(t, flattened.BreakGlassFilesystem, 1)

	effective, err := Resolve(Scopes{Explicit: &flattened})
	require.NoError(t, err)
	sources := effective.Provenance.BreakGlassFilesystem[tclaudeData]
	require.Len(t, sources, 1)
	assert.Equal(t, "leaf", sources[0].Profile, "audit must name the author")
	assert.Equal(t, "assigned", sources[0].IncludedBy, "and the assignment that pulled it in")
	assert.Equal(t, ScopeExplicit, sources[0].Scope)
	assert.Equal(t, []string{"leaf", "middle", "outer", "assigned"}, sources[0].Chain)
}

// A diamond graph must not lose or duplicate attribution.
func TestBreakGlassOriginSurvivesDiamondIncludes(t *testing.T) {
	_, tclaudeData, claudeSessions, _ := protectedHome(t)
	registry := map[string]*Profile{
		"shared": {Name: "shared", BreakGlassFilesystem: []BreakGlassGrant{{Path: tclaudeData, Access: AccessRead}}},
		"left":   {Name: "left", Includes: []string{"shared"}},
		"right": {Name: "right", Includes: []string{"shared"},
			BreakGlassFilesystem: []BreakGlassGrant{{Path: claudeSessions, Access: AccessRead}}},
	}
	lookup := func(name string) (*Profile, error) { return registry[name], nil }

	flattened, err := Flatten(Profile{Name: "assigned", Includes: []string{"left", "right"}}, lookup)
	require.NoError(t, err)
	effective, err := Resolve(Scopes{Explicit: &flattened})
	require.NoError(t, err)
	assert.Equal(t, []ProfileSource{
		{Scope: ScopeExplicit, Profile: "shared", IncludedBy: "assigned", Chain: []string{"shared", "left", "assigned"}},
		{Scope: ScopeExplicit, Profile: "shared", IncludedBy: "assigned", Chain: []string{"shared", "right", "assigned"}},
	}, effective.Provenance.BreakGlassFilesystem[tclaudeData],
		"BOTH diamond arms are preserved, not just one")
	assert.Equal(t, []ProfileSource{{
		Scope: ScopeExplicit, Profile: "right", IncludedBy: "assigned", Chain: []string{"right", "assigned"},
	}}, effective.Provenance.BreakGlassFilesystem[claudeSessions])
}

// A rule the assigned profile authored itself keeps an empty Origin, so a
// directly-authored profile round-trips byte-identically and operators are not
// shown redundant self-attribution.
func TestBreakGlassOriginEmptyForDirectlyAuthoredRule(t *testing.T) {
	_, tclaudeData, _, _ := protectedHome(t)
	registry := map[string]*Profile{"other": {Name: "other"}}
	flattened, err := Flatten(Profile{
		Name:                 "assigned",
		Includes:             []string{"other"},
		BreakGlassFilesystem: []BreakGlassGrant{{Path: tclaudeData, Access: AccessRead}},
	}, func(name string) (*Profile, error) { return registry[name], nil })
	require.NoError(t, err)
	require.Len(t, flattened.BreakGlassFilesystem, 1)

	effective, err := Resolve(Scopes{Explicit: &flattened})
	require.NoError(t, err)
	sources := effective.Provenance.BreakGlassFilesystem[tclaudeData]
	require.Len(t, sources, 1)
	assert.Equal(t, "assigned", sources[0].Profile)
	assert.Empty(t, sources[0].IncludedBy)
}

func TestBreakGlassProvenanceSnapshotIsDeepFrozenAndRoundTrips(t *testing.T) {
	_, tclaudeData, claudeSessions, _ := protectedHome(t)
	registry := map[string]*Profile{
		"shared": {Name: "shared", BreakGlassFilesystem: []BreakGlassGrant{{Path: tclaudeData, Access: AccessRead}}},
		"left":   {Name: "left", Includes: []string{"shared"}},
		"right":  {Name: "right", Includes: []string{"shared"}},
		"leaf":   {Name: "leaf", BreakGlassFilesystem: []BreakGlassGrant{{Path: claudeSessions, Access: AccessWrite}}},
		"middle": {Name: "middle", Includes: []string{"leaf"}},
	}
	flattened, err := Flatten(Profile{
		Name: "assigned", Includes: []string{"left", "right", "middle"},
		BreakGlassFilesystem: []BreakGlassGrant{{Path: tclaudeData, Access: AccessRead}},
	}, func(name string) (*Profile, error) { return registry[name], nil })
	require.NoError(t, err)
	effective, err := Resolve(Scopes{Explicit: &flattened})
	require.NoError(t, err)

	wantDiamondAndDirect := []ProfileSource{
		{Scope: ScopeExplicit, Profile: "assigned"},
		{Scope: ScopeExplicit, Profile: "shared", IncludedBy: "assigned", Chain: []string{"shared", "left", "assigned"}},
		{Scope: ScopeExplicit, Profile: "shared", IncludedBy: "assigned", Chain: []string{"shared", "right", "assigned"}},
	}
	wantNested := []ProfileSource{{
		Scope: ScopeExplicit, Profile: "leaf", IncludedBy: "assigned", Chain: []string{"leaf", "middle", "assigned"},
	}}
	assert.Equal(t, wantDiamondAndDirect, effective.Provenance.BreakGlassFilesystem[tclaudeData])
	assert.Equal(t, wantNested, effective.Provenance.BreakGlassFilesystem[claudeSessions])

	snapshot := NewSnapshot(effective, nil)
	effective.Provenance.BreakGlassFilesystem[tclaudeData][1].Chain[0] = "mutated-after-freeze"
	assert.Equal(t, wantDiamondAndDirect, snapshot.Effective.Provenance.BreakGlassFilesystem[tclaudeData],
		"NewSnapshot must deep-copy nested ProfileSource.Chain slices")

	raw, err := json.Marshal(snapshot)
	require.NoError(t, err)
	var roundTrip Snapshot
	require.NoError(t, json.Unmarshal(raw, &roundTrip))
	assert.Equal(t, wantDiamondAndDirect, roundTrip.Effective.Provenance.BreakGlassFilesystem[tclaudeData])
	assert.Equal(t, wantNested, roundTrip.Effective.Provenance.BreakGlassFilesystem[claudeSessions])
}

func TestV2DirectBreakGlassProvenanceUpgradesWithoutSyntheticChain(t *testing.T) {
	_, tclaudeData, _, _ := protectedHome(t)
	raw := `{"version":2,"effective":{"filesystem":[],"break_glass_filesystem":[{"path":` +
		mustJSON(t, tclaudeData) + `,"access":"read"}],"environment":[],"agent_directories":[],"provenance":{` +
		`"applied":[{"scope":"explicit","profile":"direct"}],"filesystem":{},` +
		`"break_glass_filesystem":{` + mustJSON(t, tclaudeData) + `:[{"scope":"explicit","profile":"direct"}]},` +
		`"environment":{},"agent_directories":{}}},"applied":[]}`
	var snapshot Snapshot
	require.NoError(t, json.Unmarshal([]byte(raw), &snapshot))
	upgraded, err := NormalizeSnapshotVersion(snapshot)
	require.NoError(t, err)
	assert.Equal(t, []ProfileSource{{Scope: ScopeExplicit, Profile: "direct"}},
		upgraded.Effective.Provenance.BreakGlassFilesystem[tclaudeData])
}

func TestSnapshotCapabilitiesIncludeBreakGlass(t *testing.T) {
	_, tclaudeData, _, _ := protectedHome(t)
	effective, err := Resolve(Scopes{Explicit: &Profile{
		Name:                 "danger",
		BreakGlassFilesystem: []BreakGlassGrant{{Path: tclaudeData, Access: AccessRead}},
	}})
	require.NoError(t, err)
	assert.True(t, HasCapabilities(NewSnapshot(effective, nil)),
		"protected access is inherited host authority and must gate agent-initiated spawns")
}

func TestRequireContainedRefusesBreakGlassEscalation(t *testing.T) {
	_, tclaudeData, claudeSessions, _ := protectedHome(t)

	snapshotFor := func(t *testing.T, profile Profile) Snapshot {
		t.Helper()
		effective, err := Resolve(Scopes{Explicit: &profile})
		require.NoError(t, err)
		return NewSnapshot(effective, nil)
	}

	none := snapshotFor(t, Profile{Name: "none"})
	read := snapshotFor(t, Profile{Name: "read", BreakGlassFilesystem: []BreakGlassGrant{{Path: tclaudeData, Access: AccessRead}}})
	write := snapshotFor(t, Profile{Name: "write", BreakGlassFilesystem: []BreakGlassGrant{{Path: tclaudeData, Access: AccessWrite}}})
	other := snapshotFor(t, Profile{Name: "other", BreakGlassFilesystem: []BreakGlassGrant{{Path: claudeSessions, Access: AccessRead}}})

	// A parent with no protected access can never mint a child that has some.
	require.Error(t, RequireContained(none, read))
	// Protected read → protected write is widening.
	require.Error(t, RequireContained(read, write))
	// A different protected root is not covered by an unrelated one.
	require.Error(t, RequireContained(read, other))

	// Narrowing and equal authority are allowed.
	require.NoError(t, RequireContained(read, read))
	require.NoError(t, RequireContained(write, read))
	require.NoError(t, RequireContained(read, none))
}

func TestRequireContainedTreatsMinimalToDefaultAsWidening(t *testing.T) {
	protectedHome(t)
	snapshotFor := func(t *testing.T, baseline ReadBaseline) Snapshot {
		t.Helper()
		effective, err := Resolve(Scopes{Explicit: &Profile{Name: "p", ReadBaseline: baseline}})
		require.NoError(t, err)
		return NewSnapshot(effective, nil)
	}
	minimal := snapshotFor(t, ReadBaselineMinimal)
	broad := snapshotFor(t, ReadBaselineDefault)

	err := RequireContained(minimal, broad)
	require.Error(t, err, "a minimal parent must not hand a child the broad harness baseline")
	assert.Contains(t, err.Error(), "read baseline")

	require.NoError(t, RequireContained(minimal, minimal))
	require.NoError(t, RequireContained(broad, minimal), "tightening is always allowed")
	require.NoError(t, RequireContained(broad, broad))
}

// Resume/relaunch must not silently gain authority: a snapshot whose recorded
// break-glass target has been retargeted or removed fails closed rather than
// launching with different authority than was acknowledged.
func TestSnapshotRevalidationFailsClosedOnBreakGlassDrift(t *testing.T) {
	_, tclaudeData, _, _ := protectedHome(t)
	effective, err := Resolve(Scopes{Explicit: &Profile{
		Name:                 "danger",
		BreakGlassFilesystem: []BreakGlassGrant{{Path: tclaudeData, Access: AccessRead}},
	}})
	require.NoError(t, err)
	snapshot := NewSnapshot(effective, nil)
	_, err = RevalidateSnapshot(snapshot)
	require.NoError(t, err)

	// Hand-edited authority that no longer normalizes to itself is rejected.
	tampered := snapshot
	tampered.Effective = cloneEffectiveProfile(snapshot.Effective)
	tampered.Effective.BreakGlassFilesystem = []BreakGlassGrant{{Path: tclaudeData + "/..", Access: AccessWrite}}
	_, err = RevalidateSnapshot(tampered)
	require.Error(t, err)
}

func TestBreakGlassForLaunchFailsClosedWhenMissing(t *testing.T) {
	home, tclaudeData, _, _ := protectedHome(t)

	effective, err := Resolve(Scopes{Explicit: &Profile{
		Name:                 "danger",
		BreakGlassFilesystem: []BreakGlassGrant{{Path: tclaudeData, Access: AccessRead}},
	}})
	require.NoError(t, err)
	grants, err := BreakGlassForLaunch(effective)
	require.NoError(t, err)
	assert.Equal(t, []BreakGlassGrant{{Path: tclaudeData, Access: AccessRead}}, grants)

	// Unlike an ordinary missing grant (which is skipped), a missing protected
	// path must not silently launch with less authority than the audit record.
	missing := effective
	missing.BreakGlassFilesystem = []BreakGlassGrant{{Path: filepath.Join(home, ".tclaude", "data", "gone"), Access: AccessRead}}
	_, err = BreakGlassForLaunch(missing)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not exist")

	// No break-glass at all stays a no-op, preserving today's launch path.
	none, err := BreakGlassForLaunch(EffectiveProfile{})
	require.NoError(t, err)
	assert.Nil(t, none)
}

func TestBreakGlassCountIsBounded(t *testing.T) {
	_, tclaudeData, _, _ := protectedHome(t)
	grants := make([]BreakGlassGrant, 0, MaxBreakGlassCount+1)
	for i := 0; i <= MaxBreakGlassCount; i++ {
		grants = append(grants, BreakGlassGrant{Path: tclaudeData, Access: AccessRead})
	}
	_, _, err := NormalizeForPersistence(Profile{Name: "p", BreakGlassFilesystem: grants})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "too many entries")
}

// Snapshot v2 is what the current release persists. Rejecting it would break
// every existing session, actor, and pending-spawn row on upgrade, so the
// version bump must UPGRADE v1/v2 rather than fail closed on them.
func TestSnapshotVersionsOneAndTwoUpgradeToCurrent(t *testing.T) {
	home, _, _, _ := protectedHome(t)
	workspace := filepath.Join(home, "workspace")
	require.NoError(t, os.Mkdir(workspace, 0o755))

	for _, version := range []int{1, 2, SnapshotVersion} {
		// Decode from real persisted JSON, not a hand-built struct, so this
		// exercises the same path a stored row takes.
		raw := `{"version":` + itoa(version) + `,"effective":{` +
			`"filesystem":[{"path":` + mustJSON(t, workspace) + `,"access":"write"}],` +
			`"environment":[],"agent_directories":[],` +
			`"provenance":{"applied":[],"filesystem":{},"environment":{},"agent_directories":{}}},"applied":[]}`
		var snapshot Snapshot
		require.NoError(t, json.Unmarshal([]byte(raw), &snapshot), "version %d", version)

		upgraded, err := NormalizeSnapshotVersion(snapshot)
		require.NoErrorf(t, err, "version %d must upgrade, not fail closed", version)
		assert.Equal(t, SnapshotVersion, upgraded.Version)
		assert.Equal(t, ReadBaselineDefault, upgraded.Effective.ReadBaseline,
			"a pre-TCL-609 snapshot means today's behavior")
		assert.Empty(t, upgraded.Effective.BreakGlassFilesystem)

		// And it must still be usable as launch authority.
		revalidated, err := RevalidateSnapshot(snapshot)
		require.NoErrorf(t, err, "version %d", version)
		assert.Equal(t, SnapshotVersion, revalidated.Version)
		assert.Len(t, revalidated.Effective.Filesystem, 1)
	}

	// A genuinely unknown future version still fails closed.
	_, err := NormalizeSnapshotVersion(Snapshot{Version: SnapshotVersion + 1})
	require.Error(t, err)
}

func itoa(v int) string { return strconv.Itoa(v) }

// Provenance is DERIVED, never authored. The public grant shape has no
// provenance field, and unknown wire keys are ignored.
func TestBreakGlassProvenanceIsNotAcceptedFromInput(t *testing.T) {
	_, tclaudeData, _, _ := protectedHome(t)

	profile, _, err := NormalizeForPersistence(Profile{
		Name:                 "attacker",
		BreakGlassFilesystem: []BreakGlassGrant{{Path: tclaudeData, Access: AccessWrite}},
	})
	require.NoError(t, err)
	require.Len(t, profile.BreakGlassFilesystem, 1)

	// And it is not part of the wire shape at all, so it cannot arrive by JSON.
	encoded, err := json.Marshal(profile)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "chain")
	assert.NotContains(t, string(encoded), "origin")

	var decoded Profile
	require.NoError(t, json.Unmarshal([]byte(
		`{"name":"attacker","break_glass_filesystem":[{"path":`+mustJSON(t, tclaudeData)+
			`,"access":"write","chains":[["trusted"]],"origin":"trusted"}]}`), &decoded))
	require.Len(t, decoded.BreakGlassFilesystem, 1)
}

// Mutating a public Profile returned by Flatten invalidates its opaque derived
// provenance. Resolve must not trust stale authorship for the changed rule.
func TestResolveIgnoresStaleFlattenedProvenanceAfterMutation(t *testing.T) {
	_, tclaudeData, _, _ := protectedHome(t)
	registry := map[string]*Profile{
		"leaf": {Name: "leaf", BreakGlassFilesystem: []BreakGlassGrant{{Path: tclaudeData, Access: AccessRead}}},
	}
	flattened, err := Flatten(Profile{Name: "assigned", Includes: []string{"leaf"}},
		func(name string) (*Profile, error) { return registry[name], nil })
	require.NoError(t, err)
	flattened.BreakGlassFilesystem[0].Access = AccessWrite

	effective, err := Resolve(Scopes{Explicit: &flattened})
	require.NoError(t, err)
	sources := effective.Provenance.BreakGlassFilesystem[tclaudeData]
	require.Len(t, sources, 1)
	assert.Equal(t, "assigned", sources[0].Profile)
	assert.Empty(t, sources[0].Chain)
}

// A profile that never touches break-glass must serialize with neither key,
// including after a flatten/resolve round trip — the compatibility guarantee
// the whole feature rests on.
func TestDirectAndLegacyProfilesSerializeWithoutProvenanceKeys(t *testing.T) {
	home, tclaudeData, _, _ := protectedHome(t)
	workspace := filepath.Join(home, "workspace")
	require.NoError(t, os.Mkdir(workspace, 0o755))

	legacy, _, err := NormalizeForPersistence(Profile{
		Name: "legacy", Filesystem: []FilesystemGrant{{Path: workspace, Access: AccessWrite}},
	})
	require.NoError(t, err)
	encoded, err := json.Marshal(legacy)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "break_glass")
	assert.NotContains(t, string(encoded), "chain")

	// A directly-authored break-glass profile carries the rule but no chain.
	direct, _, err := NormalizeForPersistence(Profile{
		Name:                 "direct",
		BreakGlassFilesystem: []BreakGlassGrant{{Path: tclaudeData, Access: AccessRead}},
	})
	require.NoError(t, err)
	encoded, err = json.Marshal(direct)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), "break_glass_filesystem")
	assert.NotContains(t, string(encoded), "chain")
	assert.NotContains(t, string(encoded), "origin")
}
