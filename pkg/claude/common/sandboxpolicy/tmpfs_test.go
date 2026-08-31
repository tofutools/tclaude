package sandboxpolicy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeTmpfsCanonicalizesAndSorts(t *testing.T) {
	home, _, _, _ := protectedHome(t)
	profile, _, err := NormalizeForPersistence(Profile{
		Name: "scratch",
		Tmpfs: []TmpfsMount{
			{Path: "/srv/build/"},
			{Path: "/scratch/./inner", Size: "512MiB"},
			{Path: "~/work-cache"},
		},
	})
	require.NoError(t, err)
	require.Len(t, profile.Tmpfs, 3)
	assert.Equal(t, []TmpfsMount{
		{Path: "/scratch/inner", Size: "512MiB", SizeBytes: 512 << 20},
		{Path: "/srv/build"},
		{Path: filepath.Join(home, "work-cache")},
	}, profile.Tmpfs)
}

func TestNormalizeTmpfsRejectsMalformedRows(t *testing.T) {
	protectedHome(t)
	for name, mount := range map[string]TmpfsMount{
		"relative":     {Path: "scratch"},
		"root":         {Path: "/"},
		"empty":        {Path: "   "},
		"bad size":     {Path: "/scratch", Size: "lots"},
		"zero size":    {Path: "/scratch", Size: "0"},
		"derived only": {Path: "/scratch", SizeBytes: 1024},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := NormalizeForPersistence(Profile{
				Name: "scratch", Tmpfs: []TmpfsMount{mount},
			})
			require.Error(t, err)
		})
	}
}

// A tmpfs is unshadowable once mounted, so a row over tclaude's own machinery
// would either be silently overridden or cut the agent off from it. Both walls
// are refusals, and both are checked in the canonical spelling as well as the
// authored one.
func TestTmpfsCannotShadowProtectedRoots(t *testing.T) {
	home, tclaudeData, claudeSessions, _ := protectedHome(t)
	for _, path := range []string{
		home,                                // an ancestor of both protected roots
		tclaudeData,                         // exactly a protected root
		claudeSessions,                      // exactly a protected root
		filepath.Join(tclaudeData, "inner"), // strictly beneath one
		filepath.Join(home, ".claude"),      // an ancestor of one
	} {
		_, _, err := NormalizeForPersistence(Profile{
			Name: "scratch", Tmpfs: []TmpfsMount{{Path: path}},
		})
		require.Error(t, err, "tmpfs at %q must be refused", path)
		assert.Contains(t, err.Error(), "protected directory")
	}
}

func TestTmpfsCannotShadowTheAgentdSocket(t *testing.T) {
	protectedHome(t)
	sockets := AgentdSocketFloor()
	require.NotEmpty(t, sockets)
	_, _, err := NormalizeForPersistence(Profile{
		Name: "scratch", Tmpfs: []TmpfsMount{{Path: filepath.Dir(sockets[0])}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agentd control socket")
}

// A tmpfs and a filesystem rule at ONE sandbox path disagree about what that
// mount is. Folding them would discard an authored rule, so both are named.
func TestTmpfsConflictsWithSamePathFilesystemRule(t *testing.T) {
	home, _, _, _ := protectedHome(t)
	workspace := filepath.Join(home, "workspace")
	require.NoError(t, os.MkdirAll(workspace, 0o755))
	_, _, err := NormalizeForPersistence(Profile{
		Name:       "scratch",
		Filesystem: []FilesystemGrant{{Path: workspace, Access: AccessWrite}},
		Tmpfs:      []TmpfsMount{{Path: workspace}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not both")
}

// Overlap that is NOT equality is ordinary most-specific-wins and must be kept.
func TestTmpfsMayNestWithFilesystemRules(t *testing.T) {
	home, _, _, _ := protectedHome(t)
	workspace := filepath.Join(home, "workspace")
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, "target"), 0o755))
	profile, _, err := NormalizeForPersistence(Profile{
		Name:       "scratch",
		Filesystem: []FilesystemGrant{{Path: workspace, Access: AccessWrite}},
		Tmpfs:      []TmpfsMount{{Path: filepath.Join(workspace, "target")}},
	})
	require.NoError(t, err)
	require.Len(t, profile.Tmpfs, 1)
	require.Len(t, profile.Filesystem, 1)
}

// Duplicate rows fold to the SMALLEST ceiling, and an omitted size is the
// loosest value rather than a neutral one, so the fold stays commutative.
func TestNormalizeTmpfsFoldsDuplicatesToTheStrictestCeiling(t *testing.T) {
	protectedHome(t)
	for _, rows := range [][]TmpfsMount{
		{{Path: "/scratch", Size: "1GiB"}, {Path: "/scratch", Size: "256MiB"}},
		{{Path: "/scratch", Size: "256MiB"}, {Path: "/scratch", Size: "1GiB"}},
		{{Path: "/scratch"}, {Path: "/scratch", Size: "256MiB"}},
		{{Path: "/scratch", Size: "256MiB"}, {Path: "/scratch"}},
	} {
		profile, _, err := NormalizeForPersistence(Profile{Name: "scratch", Tmpfs: rows})
		require.NoError(t, err)
		assert.Equal(t,
			[]TmpfsMount{{Path: "/scratch", Size: "256MiB", SizeBytes: 256 << 20}},
			profile.Tmpfs)
	}
}

func TestNormalizeTmpfsBoundsRowCount(t *testing.T) {
	protectedHome(t)
	rows := make([]TmpfsMount, MaxTmpfsMountCount+1)
	for i := range rows {
		rows[i] = TmpfsMount{Path: filepath.Join("/scratch", string(rune('a'+i%26)), string(rune('a'+i/26)))}
	}
	_, _, err := NormalizeForPersistence(Profile{Name: "scratch", Tmpfs: rows})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "too many entries")
}

// Cross-scope composition is strictest-wins, like the filesystem union: a group
// or explicit profile must not be able to widen a ceiling a global one set.
func TestResolveFoldsTmpfsStrictestAcrossScopes(t *testing.T) {
	protectedHome(t)
	global := Profile{Name: "global", Tmpfs: []TmpfsMount{{Path: "/scratch", Size: "256MiB"}}}
	explicit := Profile{Name: "explicit", Tmpfs: []TmpfsMount{
		{Path: "/scratch", Size: "4GiB"},
		{Path: "/build"},
	}}
	effective, err := Resolve(Scopes{Global: &global, Explicit: &explicit})
	require.NoError(t, err)
	assert.Equal(t, []TmpfsMount{
		{Path: "/build"},
		{Path: "/scratch", Size: "256MiB", SizeBytes: 256 << 20},
	}, effective.Tmpfs)
	assert.Equal(t,
		[]ProfileSource{
			{Scope: ScopeGlobal, Profile: "global"},
			{Scope: ScopeExplicit, Profile: "explicit"},
		},
		effective.Provenance.Tmpfs["/scratch"])
}

// Include composition is later-wins, exactly as it is for filesystem rules: it
// is an authoring convenience inside one registry, where overriding an
// inherited row is the point.
func TestFlattenTmpfsLetsTheIncludingProfileOverride(t *testing.T) {
	protectedHome(t)
	base := Profile{Name: "base", Tmpfs: []TmpfsMount{{Path: "/scratch", Size: "128MiB"}}}
	out, err := Flatten(
		Profile{
			Name:     "derived",
			Includes: []string{"base"},
			Tmpfs:    []TmpfsMount{{Path: "/scratch", Size: "2GiB"}},
		},
		func(name string) (*Profile, error) {
			if name == "base" {
				return &base, nil
			}
			return nil, nil
		},
	)
	require.NoError(t, err)
	assert.Equal(t,
		[]TmpfsMount{{Path: "/scratch", Size: "2GiB", SizeBytes: 2 << 30}},
		out.Tmpfs)
}

// Two scopes composing into a conflict neither profile could author on its own
// must be refused where the composition happens.
func TestResolveRefusesTmpfsColidingWithAnotherScopesGrant(t *testing.T) {
	home, _, _, _ := protectedHome(t)
	shared := filepath.Join(home, "shared")
	require.NoError(t, os.MkdirAll(shared, 0o755))
	global := Profile{Name: "global", Filesystem: []FilesystemGrant{
		{Path: shared, Access: AccessRead},
	}}
	explicit := Profile{Name: "explicit", Tmpfs: []TmpfsMount{{Path: shared}}}
	_, err := Resolve(Scopes{Global: &global, Explicit: &explicit})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not both")
}

func TestRenderMountPlanOrdersTmpfsWithEverythingElse(t *testing.T) {
	plan, err := RenderMountPlanWithEngine(EffectiveProfile{
		Filesystem: []FilesystemGrant{
			{Path: "/srv", Access: AccessRead},
			{Path: "/srv/work/out", Access: AccessWrite},
		},
		Tmpfs: []TmpfsMount{{Path: "/srv/work", SizeBytes: 1 << 20}},
	}, NetworkEngineUnset)
	require.NoError(t, err)
	assert.Equal(t, []MountEntry{
		{Path: "/srv", Mode: MountRO},
		{Path: "/srv/work", Mode: MountTmpfs, SizeBytes: 1 << 20},
		{Path: "/srv/work/out", Mode: MountRW},
	}, plan.Entries)
	assert.Contains(t, plan.String(), "tmpfs /srv/work (max 1048576 bytes)")
}

func TestRenderMountPlanRefusesTmpfsOverAGrantAtTheSamePath(t *testing.T) {
	_, err := RenderMountPlanWithEngine(EffectiveProfile{
		Filesystem: []FilesystemGrant{{Path: "/srv/work", Access: AccessWrite}},
		Tmpfs:      []TmpfsMount{{Path: "/srv/work"}},
	}, NetworkEngineUnset)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "claimed by both")
}

func TestValidateTmpfsSupportRefusesOutsidePlainLinuxTclaudeLayer(t *testing.T) {
	mounts := []TmpfsMount{{Path: "/scratch"}}
	require.NoError(t, ValidateTmpfsSupport(mounts, ImplementationTclaudeLayer, "linux"))
	require.NoError(t, ValidateTmpfsSupport(nil, ImplementationHarnessBuiltin, "darwin"))
	for _, tc := range []struct {
		implementation Implementation
		goos           string
	}{
		{ImplementationTclaudeLayer, "darwin"},
		// stacked refuses even on Linux: the inner harness-native wall is fed
		// from host-path directory lists and would deny every write to a mount
		// the outer layer created.
		{ImplementationStacked, "linux"},
		{ImplementationHarnessBuiltin, "linux"},
		{ImplementationResourceOnly, "linux"},
		{ImplementationOff, "linux"},
	} {
		err := ValidateTmpfsSupport(mounts, tc.implementation, tc.goos)
		require.Error(t, err, "%s/%s must refuse", tc.implementation, tc.goos)
		assert.Contains(t, err.Error(), "unsupported_sandbox_profile_tmpfs")
		assert.Contains(t, err.Error(), "/scratch")
	}
}

// A frozen snapshot is revalidated against the CURRENT host before it becomes
// authority, so a tmpfs path that has since become a spelling of a protected
// root is refused rather than mounted over it.
func TestRevalidateSnapshotRefusesEditedTmpfsRows(t *testing.T) {
	protectedHome(t)
	effective, err := Resolve(Scopes{Explicit: &Profile{
		Name: "scratch", Tmpfs: []TmpfsMount{{Path: "/scratch", Size: "64MiB"}},
	}})
	require.NoError(t, err)
	snapshot := NewSnapshot(effective, nil)
	if _, err := RevalidateSnapshot(snapshot); err != nil {
		t.Fatalf("an untouched snapshot must revalidate: %v", err)
	}
	// A payload edited after resolution — here the derived byte count no longer
	// matches the authored spelling — must not become launch authority.
	tampered := snapshot
	tampered.Effective.Tmpfs = []TmpfsMount{{Path: "/scratch", Size: "64MiB", SizeBytes: 1}}
	_, err = RevalidateSnapshot(tampered)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "temporary filesystems changed since resolution")
}

func TestUnconfinedLaunchSnapshotWithholdsTmpfs(t *testing.T) {
	protectedHome(t)
	effective, err := Resolve(Scopes{Explicit: &Profile{
		Name: "scratch", Tmpfs: []TmpfsMount{{Path: "/scratch"}},
	}})
	require.NoError(t, err)
	out := UnconfinedLaunchSnapshot(NewSnapshot(effective, nil))
	assert.Empty(t, out.Effective.Tmpfs)
	assert.Empty(t, out.Effective.Provenance.Tmpfs)
}

// A sandbox-off launch that recorded a tmpfs must say the rule is not enforced,
// rather than staying silent about a mount the agent will not get.
func TestUnconfinedNoticeCoversATmpfsOnlyProfile(t *testing.T) {
	notice, ok := UnconfinedAccessRulesNotice(ImplementationOff, EffectiveProfile{
		Tmpfs: []TmpfsMount{{Path: "/scratch"}},
	})
	require.True(t, ok)
	assert.Equal(t, AccessNoticeClassDegradation, notice.Class)
}
