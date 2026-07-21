package db

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func TestSandboxProfileIncludesRoundTripAndGraphValidation(t *testing.T) {
	setupTestDB(t)
	_, err := CreateSandboxProfile(&SandboxProfile{Name: "base"})
	require.NoError(t, err)

	// Dangling include is rejected before anything is committed.
	_, err = CreateSandboxProfile(&SandboxProfile{Name: "broken", Includes: []string{"ghost"}})
	require.ErrorIs(t, err, ErrSandboxProfileInvalidInclude)
	missing, err := GetSandboxProfile("broken")
	require.NoError(t, err)
	assert.Nil(t, missing, "failed create must not leave a row behind")

	teamID, err := CreateSandboxProfile(&SandboxProfile{Name: "team", Includes: []string{"base"}})
	require.NoError(t, err)
	team, err := GetSandboxProfileByID(teamID)
	require.NoError(t, err)
	assert.Equal(t, []string{"base"}, team.Includes)

	empty, err := GetSandboxProfile("base")
	require.NoError(t, err)
	assert.NotNil(t, empty.Includes, "empty includes round-trip as []")
	assert.Empty(t, empty.Includes)

	// An update that would close a cycle (base → team → base) is rejected.
	base, err := GetSandboxProfile("base")
	require.NoError(t, err)
	base.Includes = []string{"team"}
	err = UpdateSandboxProfile(base)
	require.ErrorIs(t, err, ErrSandboxProfileInvalidInclude)
	require.ErrorContains(t, err, "cycle")
	reloaded, err := GetSandboxProfile("base")
	require.NoError(t, err)
	assert.Empty(t, reloaded.Includes, "rejected update must not persist")
}

func TestSandboxProfileRenameFollowsIntoIncludeRefs(t *testing.T) {
	setupTestDB(t)
	baseID, err := CreateSandboxProfile(&SandboxProfile{Name: "base"})
	require.NoError(t, err)
	_, err = CreateSandboxProfile(&SandboxProfile{Name: "team", Includes: []string{"base"}})
	require.NoError(t, err)

	base, err := GetSandboxProfileByID(baseID)
	require.NoError(t, err)
	base.Name = "base-v2"
	require.NoError(t, UpdateSandboxProfile(base))

	team, err := GetSandboxProfile("team")
	require.NoError(t, err)
	assert.Equal(t, []string{"base-v2"}, team.Includes, "rename follows into referrers")
}

func TestSandboxProfileDeleteBlockedWhileIncluded(t *testing.T) {
	setupTestDB(t)
	_, err := CreateSandboxProfile(&SandboxProfile{Name: "base"})
	require.NoError(t, err)
	_, err = CreateSandboxProfile(&SandboxProfile{Name: "team", Includes: []string{"base"}})
	require.NoError(t, err)

	_, err = DeleteSandboxProfile("base")
	require.ErrorIs(t, err, ErrSandboxProfileIncludedBy)
	require.ErrorContains(t, err, "team")
	still, err := GetSandboxProfile("base")
	require.NoError(t, err)
	assert.NotNil(t, still)

	// Removing the reference unblocks deletion.
	n, err := DeleteSandboxProfile("team")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	n, err = DeleteSandboxProfile("base")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
}

func TestResolveEffectiveSandboxSnapshotFlattensNestedIncludes(t *testing.T) {
	setupTestDB(t)
	home := os.Getenv("HOME")
	shared := filepath.Join(home, "shared")
	local := filepath.Join(home, "local")
	for _, path := range []string{shared, local} {
		require.NoError(t, os.MkdirAll(path, 0o755))
	}
	canonicalShared, err := filepath.EvalSymlinks(shared)
	require.NoError(t, err)
	canonicalLocal, err := filepath.EvalSymlinks(local)
	require.NoError(t, err)

	_, err = CreateSandboxProfile(&SandboxProfile{
		Name:             "innermost",
		Filesystem:       []SandboxFilesystemGrant{{Path: shared, Access: "read"}},
		Environment:      []SandboxEnvironmentEntry{{Name: "LAYER", Value: "innermost"}, {Name: "DEEP", Value: "yes"}},
		AgentDirectories: []string{"GOCACHE"},
	})
	require.NoError(t, err)
	_, err = CreateSandboxProfile(&SandboxProfile{
		Name:        "middle",
		Includes:    []string{"innermost"},
		Environment: []SandboxEnvironmentEntry{{Name: "LAYER", Value: "middle"}},
	})
	require.NoError(t, err)
	_, err = CreateSandboxProfile(&SandboxProfile{
		Name:        "outer",
		Includes:    []string{"middle"},
		Filesystem:  []SandboxFilesystemGrant{{Path: local, Access: "write"}},
		Environment: []SandboxEnvironmentEntry{{Name: "LAYER", Value: "outer"}},
	})
	require.NoError(t, err)

	snapshot, err := ResolveEffectiveSandboxSnapshot(0, "outer")
	require.NoError(t, err)
	assert.Equal(t, []sandboxpolicy.FilesystemGrant{
		{Path: canonicalLocal, Access: sandboxpolicy.AccessWrite},
		{Path: canonicalShared, Access: sandboxpolicy.AccessRead},
	}, snapshot.Effective.Filesystem, "grants from profiles two include-levels deep reach the snapshot")
	assert.Equal(t, []sandboxpolicy.EnvironmentEntry{
		{Name: "DEEP", Value: "yes"},
		{Name: "LAYER", Value: "outer"},
	}, snapshot.Effective.Environment, "the outermost profile wins the shared variable")
	assert.Equal(t, []string{"GOCACHE"}, snapshot.Effective.AgentDirectories, "agent-owned directory declarations flatten through includes")
	require.Len(t, snapshot.Applied, 1)
	assert.Equal(t, "outer", snapshot.Applied[0].Name, "provenance names the assigned profile, not its includes")
}

func TestImportSandboxProfilesValidatesIncludeGraph(t *testing.T) {
	setupTestDB(t)

	// A self-contained bundle whose profiles reference each other imports fine
	// regardless of ordering within the bundle.
	result, err := ImportSandboxProfiles([]*SandboxProfile{
		{Name: "team", Includes: []string{"base"}},
		{Name: "base"},
	}, SandboxProfileImportOptions{OnConflict: "error"})
	require.NoError(t, err)
	assert.Equal(t, []string{"team", "base"}, result.Imported)
	team, err := GetSandboxProfile("team")
	require.NoError(t, err)
	assert.Equal(t, []string{"base"}, team.Includes)

	// A bundle include may also point at an already-local profile.
	_, err = ImportSandboxProfiles([]*SandboxProfile{
		{Name: "extension", Includes: []string{"team"}},
	}, SandboxProfileImportOptions{OnConflict: "error"})
	require.NoError(t, err)

	// A dangling include rolls the whole import back.
	_, err = ImportSandboxProfiles([]*SandboxProfile{
		{Name: "orphan", Includes: []string{"nowhere"}},
	}, SandboxProfileImportOptions{OnConflict: "error"})
	require.ErrorIs(t, err, ErrSandboxProfileInvalidImport)
	missing, err := GetSandboxProfile("orphan")
	require.NoError(t, err)
	assert.Nil(t, missing)
}

func TestSandboxProfileIncludeDepthBoundaryMatchesFlatten(t *testing.T) {
	setupTestDB(t)
	name := func(i int) string { return fmt.Sprintf("level-%d", i) }
	for i := range sandboxpolicy.MaxIncludeDepth + 1 {
		p := &SandboxProfile{Name: name(i), Environment: []SandboxEnvironmentEntry{{Name: fmt.Sprintf("L%d", i), Value: "yes"}}}
		if i > 0 {
			p.Includes = []string{name(i - 1)}
		}
		_, err := CreateSandboxProfile(p)
		require.NoError(t, err, "a chain of %d include edges is within the bound", i)
	}

	// The deepest persistable policy must also resolve at launch time.
	snapshot, err := ResolveEffectiveSandboxSnapshot(0, name(sandboxpolicy.MaxIncludeDepth))
	require.NoError(t, err)
	assert.Len(t, snapshot.Effective.Environment, sandboxpolicy.MaxIncludeDepth+1)

	// One more edge is rejected at write time, in the same unit.
	_, err = CreateSandboxProfile(&SandboxProfile{
		Name: name(sandboxpolicy.MaxIncludeDepth + 1), Includes: []string{name(sandboxpolicy.MaxIncludeDepth)},
	})
	require.ErrorIs(t, err, ErrSandboxProfileInvalidInclude)
	require.ErrorContains(t, err, "deeper than")
}

func TestInspectSandboxProfileImportGraph(t *testing.T) {
	setupTestDB(t)
	_, err := CreateSandboxProfile(&SandboxProfile{Name: "local-base"})
	require.NoError(t, err)

	// A bundle referencing itself and the local registry inspects clean under
	// every policy shape.
	inspection, err := InspectSandboxProfileImportGraph([]*SandboxProfile{
		{Name: "team", Includes: []string{"base", "local-base"}},
		{Name: "base"},
	})
	require.NoError(t, err)
	assert.Empty(t, inspection.OverwriteError)
	assert.Empty(t, inspection.SkipError)

	// With no name clashes the two shapes coincide, so both report the flaw.
	inspection, err = InspectSandboxProfileImportGraph([]*SandboxProfile{{Name: "orphan", Includes: []string{"nowhere"}}})
	require.NoError(t, err)
	assert.Contains(t, inspection.OverwriteError, "nowhere")
	assert.Contains(t, inspection.SkipError, "nowhere")

	inspection, err = InspectSandboxProfileImportGraph([]*SandboxProfile{
		{Name: "a", Includes: []string{"b"}},
		{Name: "b", Includes: []string{"a"}},
	})
	require.NoError(t, err)
	assert.Contains(t, inspection.OverwriteError, "cycle")
	assert.Contains(t, inspection.SkipError, "cycle")

	// Inspection writes nothing: the bundle profiles must not exist afterward.
	missing, err := GetSandboxProfile("team")
	require.NoError(t, err)
	assert.Nil(t, missing)
}

// TestInspectSandboxProfileImportGraphPolicyShapesDiverge is the cold-review
// regression: a bundle can be invalid under "overwrite" yet validly imported
// with "skip", because a clashing local profile then keeps its own includes.
// Inspection must report the shapes separately, and the real import must
// agree with each verdict.
func TestInspectSandboxProfileImportGraphPolicyShapesDiverge(t *testing.T) {
	setupTestDB(t)
	_, err := CreateSandboxProfile(&SandboxProfile{Name: "A"})
	require.NoError(t, err)
	bundle := func() []*SandboxProfile {
		return []*SandboxProfile{
			{Name: "A", Includes: []string{"B"}}, // clashes with local A
			{Name: "B", Includes: []string{"A"}}, // new
		}
	}

	inspection, err := InspectSandboxProfileImportGraph(bundle())
	require.NoError(t, err)
	assert.Contains(t, inspection.OverwriteError, "cycle", "overwriting A closes the A↔B cycle")
	assert.Empty(t, inspection.SkipError, "skipping the clash keeps local A includes-free, so B→A is acyclic")

	// The real import agrees with both verdicts.
	_, err = ImportSandboxProfiles(bundle(), SandboxProfileImportOptions{OnConflict: "overwrite"})
	require.ErrorIs(t, err, ErrSandboxProfileInvalidImport)

	result, err := ImportSandboxProfiles(bundle(), SandboxProfileImportOptions{OnConflict: "skip"})
	require.NoError(t, err)
	assert.Equal(t, []string{"A"}, result.Skipped)
	assert.Equal(t, []string{"B"}, result.Imported)
	localA, err := GetSandboxProfile("A")
	require.NoError(t, err)
	assert.Empty(t, localA.Includes, "skip retains the local profile untouched")
	importedB, err := GetSandboxProfile("B")
	require.NoError(t, err)
	assert.Equal(t, []string{"A"}, importedB.Includes)
}

// A missing read/write path inside an INCLUDED profile must survive
// flattening into the effective snapshot exactly like a local one (launch
// filters inactive rules until the directory exists) — the include layer must
// not reintroduce strict normalization the direct path no longer has.
func TestResolveEffectiveSandboxSnapshotKeepsMissingPathsFromIncludes(t *testing.T) {
	setupTestDB(t)
	canonicalParent, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	missing := filepath.Join(canonicalParent, "future", "cache")
	_, err = CreateSandboxProfile(&SandboxProfile{
		Name:       "future-base",
		Filesystem: []SandboxFilesystemGrant{{Path: missing, Access: sandboxpolicy.AccessWrite}},
	})
	require.NoError(t, err)
	_, err = CreateSandboxProfile(&SandboxProfile{Name: "wrapper", Includes: []string{"future-base"}})
	require.NoError(t, err)

	snapshot, err := ResolveEffectiveSandboxSnapshot(0, "wrapper")
	require.NoError(t, err)
	assert.Equal(t, []sandboxpolicy.FilesystemGrant{
		{Path: missing, Access: sandboxpolicy.AccessWrite},
	}, snapshot.Effective.Filesystem)
}
