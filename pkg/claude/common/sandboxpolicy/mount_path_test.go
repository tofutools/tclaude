package sandboxpolicy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mountPathFixture gives each test its own HOME so protectedPaths() and the
// "~" expansion resolve inside the temp tree rather than the developer's box.
func mountPathFixture(t *testing.T, dirs ...string) (home string, made []string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	for _, dir := range dirs {
		full := filepath.Join(home, dir)
		require.NoError(t, os.MkdirAll(full, 0o755))
		canonical, err := filepath.EvalSymlinks(full)
		require.NoError(t, err)
		made = append(made, canonical)
	}
	return home, made
}

func TestNormalizeRetainsMountPathAndKeepsHostAuthority(t *testing.T) {
	_, dirs := mountPathFixture(t, "data")
	got, err := Normalize(Profile{Name: "mounts", Filesystem: []FilesystemGrant{
		{Path: dirs[0], Access: AccessRead, MountPath: "/srv/shared"},
	}})
	require.NoError(t, err)
	require.Len(t, got.Filesystem, 1)
	assert.Equal(t, dirs[0], got.Filesystem[0].Path,
		"the host path stays the authority-bearing side")
	assert.Equal(t, "/srv/shared", got.Filesystem[0].MountPath)
	assert.Equal(t, "/srv/shared", got.Filesystem[0].GuestPath())
	assert.True(t, got.Filesystem[0].IsRemapped())
}

func TestNormalizeFoldsMountPathEqualToHostPath(t *testing.T) {
	_, dirs := mountPathFixture(t, "data")
	got, err := Normalize(Profile{Name: "same", Filesystem: []FilesystemGrant{
		{Path: dirs[0], Access: AccessWrite, MountPath: dirs[0] + string(filepath.Separator)},
	}})
	require.NoError(t, err)
	assert.Equal(t, []FilesystemGrant{{Path: dirs[0], Access: AccessWrite}}, got.Filesystem,
		"mounting a directory at its own path persists as the ordinary same-path rule it is")
}

func TestNormalizeRejectsMountPathOnDeny(t *testing.T) {
	_, dirs := mountPathFixture(t, "secret")
	_, err := Normalize(Profile{Name: "denied", Filesystem: []FilesystemGrant{
		{Path: dirs[0], Access: AccessDeny, MountPath: "/srv/shared"},
	}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mount_path is not allowed on a deny rule")
}

func TestNormalizeRejectsRelativeAndRootMountPath(t *testing.T) {
	_, dirs := mountPathFixture(t, "data")
	_, err := Normalize(Profile{Name: "relative", Filesystem: []FilesystemGrant{
		{Path: dirs[0], Access: AccessRead, MountPath: "srv/shared"},
	}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not absolute")

	_, err = Normalize(Profile{Name: "root", Filesystem: []FilesystemGrant{
		{Path: dirs[0], Access: AccessRead, MountPath: "/"},
	}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must not be the sandbox root")
}

func TestNormalizeRejectsTwoHostPathsOnOneMountPath(t *testing.T) {
	_, dirs := mountPathFixture(t, "one", "two")
	_, err := Normalize(Profile{Name: "collide", Filesystem: []FilesystemGrant{
		{Path: dirs[0], Access: AccessRead, MountPath: "/srv/shared"},
		{Path: dirs[1], Access: AccessRead, MountPath: "/srv/shared"},
	}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is claimed by two different host paths")
}

func TestNormalizeAllowsOneHostPathAtTwoMountPaths(t *testing.T) {
	_, dirs := mountPathFixture(t, "data")
	got, err := Normalize(Profile{Name: "twice", Filesystem: []FilesystemGrant{
		{Path: dirs[0], Access: AccessRead, MountPath: "/srv/b"},
		{Path: dirs[0], Access: AccessWrite, MountPath: "/srv/a"},
	}})
	require.NoError(t, err)
	assert.Equal(t, []FilesystemGrant{
		{Path: dirs[0], Access: AccessWrite, MountPath: "/srv/a"},
		{Path: dirs[0], Access: AccessRead, MountPath: "/srv/b"},
	}, got.Filesystem, "one directory may be projected onto several sandbox paths")
}

func TestNormalizeRejectsMountPathIntoProtectedRoot(t *testing.T) {
	home, dirs := mountPathFixture(t, "data")
	_, err := Normalize(Profile{Name: "protected", Filesystem: []FilesystemGrant{
		{Path: dirs[0], Access: AccessWrite, MountPath: filepath.Join(home, ".claude", "sessions")},
	}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "intersects protected directory")
}

func TestNormalizeRejectsMountPathShadowingAgentdSocket(t *testing.T) {
	_, dirs := mountPathFixture(t, "data")
	sockets := AgentdSocketFloor()
	require.NotEmpty(t, sockets)
	_, err := Normalize(Profile{Name: "socket", Filesystem: []FilesystemGrant{
		{Path: dirs[0], Access: AccessRead, MountPath: filepath.Dir(sockets[0])},
	}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "would shadow the agentd control socket")
}

func TestNormalizeRejectsMountOfDeniedHostSubtree(t *testing.T) {
	_, dirs := mountPathFixture(t, "workspace", filepath.Join("workspace", "secret"))
	_, err := Normalize(Profile{Name: "reexpose", Filesystem: []FilesystemGrant{
		{Path: dirs[0], Access: AccessDeny},
		{Path: dirs[1], Access: AccessRead, MountPath: "/srv/leak"},
	}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is denied by rule")
}

func TestRenderMountPlanEmitsProjectedEntry(t *testing.T) {
	plan, err := RenderMountPlanFromGrants([]FilesystemGrant{
		{Path: "/host/data", Access: AccessRead, MountPath: "/srv/shared"},
		{Path: "/host/work", Access: AccessWrite},
	})
	require.NoError(t, err)
	assert.Equal(t, []MountEntry{
		{Path: "/host/work", Mode: MountRW},
		{Path: "/srv/shared", Mode: MountRO, Source: "/host/data"},
	}, plan.Entries, "entries order by the sandbox path they occupy")
	assert.Equal(t, "/host/data", plan.Entries[1].SourcePath())
	assert.Equal(t, "/host/work", plan.Entries[0].SourcePath())
	assert.True(t, PlanHasRemappedEntry(plan))
}

func TestRenderMountPlanFoldsInGuestPathSpace(t *testing.T) {
	plan, err := RenderMountPlanFromGrants([]FilesystemGrant{
		{Path: "/host/data", Access: AccessRead, MountPath: "/srv/shared"},
		{Path: "/host/data", Access: AccessWrite, MountPath: "/srv/shared"},
	})
	require.NoError(t, err)
	assert.Equal(t, []MountEntry{
		{Path: "/srv/shared", Mode: MountRW, Source: "/host/data"},
	}, plan.Entries, "write dominates read for one and the same mount")
}

func TestRenderMountPlanRefusesGuestPathCollision(t *testing.T) {
	_, err := RenderMountPlanFromGrants([]FilesystemGrant{
		{Path: "/host/a", Access: AccessRead, MountPath: "/srv/shared"},
		{Path: "/host/b", Access: AccessRead, MountPath: "/srv/shared"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is claimed by two different host paths")
}

func TestRenderMountPlanRefusesDenyWithMountPath(t *testing.T) {
	_, err := RenderMountPlanFromGrants([]FilesystemGrant{
		{Path: "/host/a", Access: AccessDeny, MountPath: "/srv/shared"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mount_path is not allowed on a deny rule")
}

func TestEffectiveMountModeAtAnswersInGuestSpace(t *testing.T) {
	plan, err := RenderMountPlanFromGrants([]FilesystemGrant{
		{Path: "/host/data", Access: AccessRead, MountPath: "/srv/shared"},
	})
	require.NoError(t, err)
	mode, ok := EffectiveMountModeAt(plan, "/srv/shared/inner")
	assert.True(t, ok)
	assert.Equal(t, MountRO, mode)
	_, ok = EffectiveMountModeAt(plan, "/host/data")
	assert.False(t, ok, "the host path is not itself granted inside the sandbox")
}

func TestMountPlanStringDisclosesProjection(t *testing.T) {
	plan, err := RenderMountPlanFromGrants([]FilesystemGrant{
		{Path: "/host/data", Access: AccessRead, MountPath: "/srv/shared"},
	})
	require.NoError(t, err)
	assert.Contains(t, plan.String(), "ro   /srv/shared <- /host/data")
}

func TestEffectiveAccessAtUsesGuestPath(t *testing.T) {
	grants := []FilesystemGrant{
		{Path: "/host/data", Access: AccessWrite, MountPath: "/srv/shared"},
	}
	access, ok := EffectiveAccessAt(grants, "/srv/shared/file")
	assert.True(t, ok)
	assert.Equal(t, AccessWrite, access)
	_, ok = EffectiveAccessAt(grants, "/host/data/file")
	assert.False(t, ok)
}

func TestValidateMountPathSupportRefusesOutsideLinuxTclaudeLayer(t *testing.T) {
	grants := []FilesystemGrant{
		{Path: "/host/data", Access: AccessRead, MountPath: "/srv/shared"},
	}
	require.NoError(t, ValidateMountPathSupport(
		grants, ImplementationTclaudeLayer, "linux"))
	require.NoError(t, ValidateMountPathSupport(
		grants, ImplementationStacked, "linux"))

	for _, tc := range []struct {
		implementation Implementation
		goos           string
	}{
		{ImplementationTclaudeLayer, "darwin"},
		{ImplementationStacked, "darwin"},
		{ImplementationHarnessBuiltin, "linux"},
		{ImplementationHarnessBuiltin, "darwin"},
	} {
		err := ValidateMountPathSupport(grants, tc.implementation, tc.goos)
		require.Errorf(t, err, "%s/%s must refuse", tc.implementation, tc.goos)
		assert.Contains(t, err.Error(), "unsupported_sandbox_profile_mount_path")
		assert.Contains(t, err.Error(), "/srv/shared")
	}

	require.NoError(t, ValidateMountPathSupport(
		[]FilesystemGrant{{Path: "/host/data", Access: AccessRead}},
		ImplementationHarnessBuiltin, "darwin"),
		"a profile with no mount paths is unaffected")
}

func TestResolveRefusesConflictingMountPathAcrossScopes(t *testing.T) {
	_, dirs := mountPathFixture(t, "one", "two")
	_, err := Resolve(Scopes{
		Global: &Profile{Name: "g", Filesystem: []FilesystemGrant{
			{Path: dirs[0], Access: AccessRead, MountPath: "/srv/shared"},
		}},
		Explicit: &Profile{Name: "e", Filesystem: []FilesystemGrant{
			{Path: dirs[1], Access: AccessRead, MountPath: "/srv/shared"},
		}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is claimed by two different host paths")
}

func TestResolveMergesMountPathAcrossScopesWithLattice(t *testing.T) {
	_, dirs := mountPathFixture(t, "data")
	effective, err := Resolve(Scopes{
		Global: &Profile{Name: "g", Filesystem: []FilesystemGrant{
			{Path: dirs[0], Access: AccessRead, MountPath: "/srv/shared"},
		}},
		Explicit: &Profile{Name: "e", Filesystem: []FilesystemGrant{
			{Path: dirs[0], Access: AccessWrite, MountPath: "/srv/shared"},
		}},
	})
	require.NoError(t, err)
	assert.Equal(t, []FilesystemGrant{
		{Path: dirs[0], Access: AccessWrite, MountPath: "/srv/shared"},
	}, effective.Filesystem)
	assert.Equal(t, []ProfileSource{
		{Scope: ScopeGlobal, Profile: "g"},
		{Scope: ScopeExplicit, Profile: "e"},
	}, effective.Provenance.Filesystem[dirs[0]])
}

func TestResolveRefusesMountOfDeniedSubtreeComposedAcrossScopes(t *testing.T) {
	_, dirs := mountPathFixture(t, "workspace", filepath.Join("workspace", "secret"))
	_, err := Resolve(Scopes{
		Global: &Profile{Name: "g", Filesystem: []FilesystemGrant{
			{Path: dirs[0], Access: AccessDeny},
		}},
		Explicit: &Profile{Name: "e", Filesystem: []FilesystemGrant{
			{Path: dirs[1], Access: AccessRead, MountPath: "/srv/leak"},
		}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is denied by rule")
}

func TestFlattenOverridesMountsByGuestPath(t *testing.T) {
	_, dirs := mountPathFixture(t, "data")
	base := Profile{Name: "base", Filesystem: []FilesystemGrant{
		{Path: dirs[0], Access: AccessRead, MountPath: "/srv/shared"},
	}}
	child := Profile{Name: "child", Includes: []string{"base"}, Filesystem: []FilesystemGrant{
		{Path: dirs[0], Access: AccessWrite, MountPath: "/srv/shared"},
		{Path: dirs[0], Access: AccessRead},
	}}
	got, err := Flatten(child, func(name string) (*Profile, error) {
		if name == "base" {
			copied := base
			return &copied, nil
		}
		return nil, nil
	})
	require.NoError(t, err)
	assert.Equal(t, []FilesystemGrant{
		{Path: dirs[0], Access: AccessRead},
		{Path: dirs[0], Access: AccessWrite, MountPath: "/srv/shared"},
	}, got.Filesystem,
		"the same-path rule and the projection are independent mounts")
}

func TestNormalizeAllowsMountOfReopenedSubtreeBeneathBroadDeny(t *testing.T) {
	_, dirs := mountPathFixture(t,
		"workspace",
		filepath.Join("workspace", "shared"),
		filepath.Join("workspace", "shared", "dataset"),
	)
	got, err := Normalize(Profile{Name: "reopened", Filesystem: []FilesystemGrant{
		{Path: dirs[0], Access: AccessDeny},
		{Path: dirs[1], Access: AccessRead},
		{Path: dirs[2], Access: AccessRead, MountPath: "/srv/dataset"},
	}})
	require.NoError(t, err,
		"most-specific-wins applies on the host side too: the narrower read governs")
	assert.Contains(t, got.Filesystem, FilesystemGrant{
		Path: dirs[2], Access: AccessRead, MountPath: "/srv/dataset",
	})
}

// TestRequireContainedRefusesChildMountOfUngrantedHostPath is the lineage
// half of TCL-866. A child may not smuggle host authority in by mounting an
// arbitrary directory at a sandbox path its parent happens to grant.
func TestRequireContainedRefusesChildMountOfUngrantedHostPath(t *testing.T) {
	_, dirs := mountPathFixture(t, "shared", "secrets")
	parent := snapshotFor(t, FilesystemGrant{Path: dirs[0], Access: AccessWrite})
	child := snapshotFor(t, FilesystemGrant{
		Path: dirs[1], Access: AccessRead, MountPath: filepath.Join(dirs[0], "inner"),
	})
	err := RequireContained(parent, child)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "host path")
	assert.Contains(t, err.Error(), "is not contained by the parent snapshot")
}

func TestRequireContainedAllowsChildRepeatingTheParentsMount(t *testing.T) {
	_, dirs := mountPathFixture(t, "dataset")
	snapshot := snapshotFor(t, FilesystemGrant{
		Path: dirs[0], Access: AccessRead, MountPath: "/data",
	})
	require.NoError(t, RequireContained(snapshot, snapshot))
}
