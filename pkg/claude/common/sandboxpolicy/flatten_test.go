package sandboxpolicy

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func registryLookup(registry map[string]*Profile) LookupProfile {
	return func(name string) (*Profile, error) {
		return registry[name], nil
	}
}

func TestFlattenExpandsIncludesWithLocalOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	shared := filepath.Join(home, "shared")
	extra := filepath.Join(home, "extra")
	for _, path := range []string{shared, extra} {
		require.NoError(t, os.Mkdir(path, 0o755))
	}
	canonicalShared, err := filepath.EvalSymlinks(shared)
	require.NoError(t, err)
	canonicalExtra, err := filepath.EvalSymlinks(extra)
	require.NoError(t, err)

	registry := map[string]*Profile{
		"base": {
			Name:        "base",
			Filesystem:  []FilesystemGrant{{Path: shared, Access: AccessDeny}},
			Environment: []EnvironmentEntry{{Name: "SHARED", Value: "base"}, {Name: "BASE_ONLY", Value: "yes"}},
		},
		"team": {
			Name:        "team",
			Includes:    []string{"base"},
			Filesystem:  []FilesystemGrant{{Path: extra, Access: AccessRead}},
			Environment: []EnvironmentEntry{{Name: "SHARED", Value: "team"}},
		},
	}
	top := Profile{
		Name:        "top",
		Includes:    []string{"team"},
		Filesystem:  []FilesystemGrant{{Path: shared, Access: AccessWrite}},
		Environment: []EnvironmentEntry{{Name: "TOP_ONLY", Value: "yes"}},
	}

	got, err := Flatten(top, registryLookup(registry))
	require.NoError(t, err)
	assert.Empty(t, got.Includes, "a flattened profile carries no remaining includes")
	// The local exact-path write overrides base's deny (same-author layering);
	// team's read on a different path survives untouched.
	assert.Equal(t, []FilesystemGrant{
		{Path: canonicalExtra, Access: AccessRead},
		{Path: canonicalShared, Access: AccessWrite},
	}, got.Filesystem)
	assert.Equal(t, []EnvironmentEntry{
		{Name: "BASE_ONLY", Value: "yes"},
		{Name: "SHARED", Value: "team"},
		{Name: "TOP_ONLY", Value: "yes"},
	}, got.Environment)
}

func TestFlattenLaterIncludeOverridesEarlier(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	registry := map[string]*Profile{
		"first":  {Name: "first", Environment: []EnvironmentEntry{{Name: "SHARED", Value: "first"}}},
		"second": {Name: "second", Environment: []EnvironmentEntry{{Name: "SHARED", Value: "second"}}},
	}
	got, err := Flatten(Profile{Name: "top", Includes: []string{"first", "second"}}, registryLookup(registry))
	require.NoError(t, err)
	assert.Equal(t, []EnvironmentEntry{{Name: "SHARED", Value: "second"}}, got.Environment)
}

func TestFlattenDiamondIncludesResolveOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	lookups := 0
	registry := map[string]*Profile{
		"common": {Name: "common", Environment: []EnvironmentEntry{{Name: "COMMON", Value: "yes"}}},
		"left":   {Name: "left", Includes: []string{"common"}, Environment: []EnvironmentEntry{{Name: "LEFT", Value: "yes"}}},
		"right":  {Name: "right", Includes: []string{"common"}, Environment: []EnvironmentEntry{{Name: "RIGHT", Value: "yes"}}},
	}
	lookup := func(name string) (*Profile, error) {
		lookups++
		return registry[name], nil
	}
	got, err := Flatten(Profile{Name: "top", Includes: []string{"left", "right"}}, lookup)
	require.NoError(t, err)
	assert.Equal(t, []EnvironmentEntry{
		{Name: "COMMON", Value: "yes"},
		{Name: "LEFT", Value: "yes"},
		{Name: "RIGHT", Value: "yes"},
	}, got.Environment)
	assert.Equal(t, 3, lookups, "each distinct profile is loaded once")
}

func TestFlattenFailsClosedOnCycleAndMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	registry := map[string]*Profile{
		"a": {Name: "a", Includes: []string{"b"}},
		"b": {Name: "b", Includes: []string{"a"}},
	}
	_, err := Flatten(Profile{Name: "top", Includes: []string{"a"}}, registryLookup(registry))
	require.ErrorContains(t, err, "cycle")

	_, err = Flatten(Profile{Name: "top", Includes: []string{"ghost"}}, registryLookup(registry))
	require.ErrorContains(t, err, `"ghost" was not found`)

	// A cycle that re-enters the flattened profile itself, not just a nested pair.
	registry["loop"] = &Profile{Name: "loop", Includes: []string{"top"}}
	registry["top"] = &Profile{Name: "top", Includes: []string{"loop"}}
	_, err = Flatten(*registry["top"], registryLookup(registry))
	require.ErrorContains(t, err, "cycle")
}

func TestFlattenEnforcesDepthBoundInEdgesAtExactLimit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	registry := map[string]*Profile{}
	for i := range MaxIncludeDepth + 2 {
		p := &Profile{Name: fmt.Sprintf("level-%d", i), Environment: []EnvironmentEntry{{Name: fmt.Sprintf("L%d", i), Value: "yes"}}}
		if i > 0 {
			p.Includes = []string{fmt.Sprintf("level-%d", i-1)}
		}
		registry[p.Name] = p
	}
	// A chain of exactly MaxIncludeDepth include edges is the deepest policy
	// the registry accepts, so flattening it must succeed too.
	got, err := Flatten(*registry[fmt.Sprintf("level-%d", MaxIncludeDepth)], registryLookup(registry))
	require.NoError(t, err)
	assert.Len(t, got.Environment, MaxIncludeDepth+1)

	// One more edge fails closed.
	_, err = Flatten(*registry[fmt.Sprintf("level-%d", MaxIncludeDepth+1)], registryLookup(registry))
	require.ErrorContains(t, err, "deeper than")
}

func TestFlattenDepthBoundIsIncludeOrderIndependent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	registry := map[string]*Profile{}
	for i := range MaxIncludeDepth + 2 {
		p := &Profile{Name: fmt.Sprintf("level-%d", i)}
		if i > 0 {
			p.Includes = []string{fmt.Sprintf("level-%d", i-1)}
		}
		registry[p.Name] = p
	}
	deepBranch := fmt.Sprintf("level-%d", MaxIncludeDepth)
	// The root reaches the leaf both directly (depth 1) and through the full
	// chain (depth MaxIncludeDepth+1 > cap). The over-deep branch must be
	// rejected no matter which include is listed — and thus warmed up — first.
	for _, includes := range [][]string{
		{"level-0", deepBranch},
		{deepBranch, "level-0"},
	} {
		_, err := Flatten(Profile{Name: "root", Includes: includes}, registryLookup(registry))
		require.ErrorContains(t, err, "deeper than", "includes order %v", includes)
	}
}

func TestFlattenWithoutIncludesNeedsNoLookup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err := Flatten(Profile{Name: "plain", Environment: []EnvironmentEntry{{Name: "A", Value: "1"}}}, nil)
	require.NoError(t, err)
	assert.Equal(t, "plain", got.Name)

	_, err = Flatten(Profile{Name: "composed", Includes: []string{"other"}}, nil)
	require.ErrorContains(t, err, "no registry lookup")
}

func TestNormalizeValidatesIncludes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_, err := Normalize(Profile{Name: "self", Includes: []string{"self"}})
	require.ErrorContains(t, err, "must not include itself")

	_, err = Normalize(Profile{Name: "dup", Includes: []string{"other", " other "}})
	require.ErrorContains(t, err, "more than once")

	many := make([]string, MaxIncludeCount+1)
	for i := range many {
		many[i] = fmt.Sprintf("p%d", i)
	}
	_, err = Normalize(Profile{Name: "big", Includes: many})
	require.ErrorContains(t, err, "too many entries")

	got, err := Normalize(Profile{Name: "ok", Includes: []string{" zeta ", "alpha"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"zeta", "alpha"}, got.Includes, "include order is semantic and never sorted")
}

func TestResolveRejectsUnflattenedIncludes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_, err := Resolve(Scopes{Global: &Profile{Name: "composed", Includes: []string{"base"}}})
	require.ErrorContains(t, err, "unresolved includes")
}
