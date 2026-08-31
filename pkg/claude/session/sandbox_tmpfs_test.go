package session

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// indexOfBwrapFlagPath finds a two-token bubblewrap operation such as
// `--tmpfs /scratch`.
func indexOfBwrapFlagPath(args []string, flag, path string) int {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == path {
			return i
		}
	}
	return -1
}

func constructedRootPlan(entries ...sandboxpolicy.MountEntry) sandboxpolicy.MountPlan {
	return sandboxpolicy.MountPlan{
		Entries:     entries,
		RootPosture: sandboxpolicy.RootConstructed,
	}
}

// The whole point of a tmpfs row, at the applier layer: it renders `--tmpfs`
// like a hide does, and then — unlike a hide — it is NOT remounted read-only at
// flush time, because the operator asked for writable scratch space.
func TestBwrapArgsMountsTmpfsAndLeavesItWritable(t *testing.T) {
	got, err := bwrapArgs(nil, constructedRootPlan(
		sandboxpolicy.MountEntry{Path: "/scratch", Mode: sandboxpolicy.MountTmpfs},
	))
	require.NoError(t, err)
	require.NotEqual(t, -1, indexOfBwrapFlagPath(got, "--tmpfs", "/scratch"))
	assert.Equal(t, -1, indexOfBwrapFlagPath(got, "--remount-ro", "/scratch"),
		"a temporary filesystem must stay writable; only a hide is remounted read-only")
}

func TestBwrapArgsRendersTmpfsSizeImmediatelyBeforeItsMount(t *testing.T) {
	got, err := bwrapArgs(nil, constructedRootPlan(
		sandboxpolicy.MountEntry{
			Path: "/scratch", Mode: sandboxpolicy.MountTmpfs, SizeBytes: 512 << 20,
		},
	))
	require.NoError(t, err)
	at := indexOfBwrapFlagPath(got, "--tmpfs", "/scratch")
	require.NotEqual(t, -1, at)
	// --size modifies the NEXT filesystem operation, so it has to be the two
	// tokens directly in front of this mount and no other.
	require.GreaterOrEqual(t, at, 2)
	assert.Equal(t, "--size", got[at-2])
	assert.Equal(t, "536870912", got[at-1])
}

func TestBwrapArgsOmitsSizeForAnUncappedTmpfs(t *testing.T) {
	got, err := bwrapArgs(nil, constructedRootPlan(
		sandboxpolicy.MountEntry{Path: "/scratch", Mode: sandboxpolicy.MountTmpfs},
	))
	require.NoError(t, err)
	assert.NotContains(t, got, "--size")
}

// Ordering is the contract the plan promises: a narrower write row authored
// beneath a tmpfs still lands on top of it, so the agent gets a real host
// directory inside its scratch tree.
func TestBwrapArgsLetsANarrowerBindLandOnTopOfATmpfs(t *testing.T) {
	source := t.TempDir()
	got, err := bwrapArgs(nil, constructedRootPlan(
		sandboxpolicy.MountEntry{Path: "/scratch", Mode: sandboxpolicy.MountTmpfs},
		sandboxpolicy.MountEntry{
			Path: "/scratch/out", Mode: sandboxpolicy.MountRW, Source: source,
		},
	))
	require.NoError(t, err)
	tmpfsAt := indexOfBwrapFlagPath(got, "--tmpfs", "/scratch")
	bindAt := indexOfBwrapBind(got, "--bind", source, "/scratch/out")
	require.NotEqual(t, -1, tmpfsAt)
	require.NotEqual(t, -1, bindAt)
	assert.Less(t, tmpfsAt, bindAt,
		"the bind must execute after the tmpfs so it is not shadowed by it")
}

// A deny on an ANCESTOR of a tmpfs keeps its read-only remount: --remount-ro is
// non-recursive, so the hidden parent stays hidden while the scratch child
// beneath it stays writable.
func TestBwrapArgsKeepsAnAncestorHideReadOnlyAboveATmpfs(t *testing.T) {
	got, err := bwrapArgs(nil, constructedRootPlan(
		sandboxpolicy.MountEntry{Path: "/opt/vendor", Mode: sandboxpolicy.MountHide},
		sandboxpolicy.MountEntry{Path: "/opt/vendor/scratch", Mode: sandboxpolicy.MountTmpfs},
	))
	require.NoError(t, err)
	assert.NotEqual(t, -1, indexOfBwrapFlagPath(got, "--remount-ro", "/opt/vendor"))
	assert.Equal(t, -1, indexOfBwrapFlagPath(got, "--remount-ro", "/opt/vendor/scratch"))
}

// A tmpfs covering a path an earlier hide occupied must cancel that hide's
// pending remount, or the scratch space the operator asked for would come back
// read-only.
func TestBwrapArgsTmpfsCancelsAnEarlierHideAtTheSamePath(t *testing.T) {
	got, err := bwrapArgs(nil, constructedRootPlan(
		sandboxpolicy.MountEntry{Path: "/scratch", Mode: sandboxpolicy.MountHide},
		sandboxpolicy.MountEntry{Path: "/scratch/inner", Mode: sandboxpolicy.MountTmpfs},
	))
	require.NoError(t, err)
	assert.Equal(t, -1, indexOfBwrapFlagPath(got, "--remount-ro", "/scratch/inner"))
}

// Under an INHERITED root the sandbox root is the host root bound read-only, so
// bubblewrap has nowhere to create a mount point. Refuse with a message naming
// the path and the fix rather than letting bwrap fail unattributably.
func TestBwrapArgsRefusesTmpfsWithoutHostMountPointUnderInheritedRoot(t *testing.T) {
	_, err := bwrapArgs(nil, sandboxpolicy.MountPlan{
		Entries: []sandboxpolicy.MountEntry{
			{Path: "/scratch-that-does-not-exist", Mode: sandboxpolicy.MountTmpfs},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tclaude_layer_missing_mount_point")
	assert.Contains(t, err.Error(), "/scratch-that-does-not-exist")
}

func TestBwrapArgsAcceptsTmpfsOnAnExistingHostMountPointUnderInheritedRoot(t *testing.T) {
	mountPoint := t.TempDir()
	got, err := bwrapArgs(nil, sandboxpolicy.MountPlan{
		Entries: []sandboxpolicy.MountEntry{
			{Path: mountPoint, Mode: sandboxpolicy.MountTmpfs},
		},
	})
	require.NoError(t, err)
	assert.NotEqual(t, -1, indexOfBwrapFlagPath(got, "--tmpfs", mountPoint))
}

// A temporary filesystem is a directory, so a host mount point that is a file
// cannot carry one. bubblewrap's own refusal names neither the rule nor the
// profile, so this one does.
func TestBwrapArgsRefusesTmpfsOnAFileMountPoint(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	_, err := bwrapArgs(nil, sandboxpolicy.MountPlan{
		Entries: []sandboxpolicy.MountEntry{
			{Path: file, Mode: sandboxpolicy.MountTmpfs},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tclaude_layer_mount_point_kind")
}

// A tmpfs entry has no host side. A plan that names one is malformed rather
// than a projection, and reading Path as a bind source is exactly the mistake
// the refusal exists to catch.
func TestBwrapArgsRefusesTmpfsEntryCarryingAHostSource(t *testing.T) {
	_, err := bwrapArgs(nil, constructedRootPlan(
		sandboxpolicy.MountEntry{
			Path: "/scratch", Mode: sandboxpolicy.MountTmpfs, Source: "/host/data",
		},
	))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "a tmpfs has no host side")
}

// The launch contract is not made of protected roots, so the policy layer's
// protected-root wall does not cover it. Shadowing the workspace or the harness
// state root is refused where the contract is actually known.
func TestValidateTmpfsPathsAgainstContractRefusesShadowingRequiredDirs(t *testing.T) {
	contract := []string{"/home/dev/project", "/home/dev/.codex"}
	require.NoError(t, validateTmpfsPathsAgainstContract(
		[]sandboxpolicy.TmpfsMount{{Path: "/home/dev/project/node_modules"}}, contract),
		"a tmpfs strictly beneath a contract directory is ordinary most-specific-wins")
	for _, path := range []string{"/home/dev/project", "/home/dev", "/home/dev/.codex"} {
		err := validateTmpfsPathsAgainstContract(
			[]sandboxpolicy.TmpfsMount{{Path: path}}, contract)
		require.Error(t, err, "a tmpfs at %q must be refused", path)
		assert.Contains(t, err.Error(), "unsupported_sandbox_profile_tmpfs")
	}
}

// Seatbelt filters paths in the host namespace; it has no mount to create. The
// launch gate refuses first, so this is the backstop that keeps a future caller
// from rendering a policy the platform cannot enforce.
func TestRenderSeatbeltProfileRefusesTmpfs(t *testing.T) {
	_, _, err := renderSeatbeltProfile(
		[]string{"/Users/dev/work"},
		nil,
		sandboxpolicy.MountPlan{
			NetworkPosture: sandboxpolicy.NetworkHostOpen,
			Entries: []sandboxpolicy.MountEntry{
				{Path: "/scratch", Mode: sandboxpolicy.MountTmpfs},
			},
		},
		netip.AddrPort{},
		[]string{"/Users/dev/.tclaude/data"},
		"/private/tmp/tmux-501",
		"/private/var/folders/ab/runtime/T",
		nil,
		nil,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "seatbelt_tmpfs_mount")
}

// The end-to-end policy → spec → arguments path, which is the one a launch
// actually walks. A tmpfs in the frozen snapshot must survive the launch
// contract's own composition of the filesystem — that composition flattens the
// grants back into bare host-path lists, which cannot carry a mount with no
// host side, so this is exactly where a tmpfs could silently vanish.
func TestTclaudeLayerLaunchSpecCarriesTmpfsThroughToTheArguments(t *testing.T) {
	cwd := t.TempDir()
	snapshot := sandboxpolicy.EmptySnapshot()
	snapshot.Effective.FilesystemRoot = sandboxpolicy.FilesystemRootSeparate
	snapshot.Effective.Tmpfs = []sandboxpolicy.TmpfsMount{
		{Path: "/scratch", Size: "64MiB", SizeBytes: 64 << 20},
	}
	spec, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
		HarnessName: harness.DefaultName, Cwd: cwd, Snapshot: &snapshot,
	})
	require.NoError(t, err)
	require.Equal(t, snapshot.Effective.Tmpfs, spec.Effective.Tmpfs,
		"the launch contract must not drop the mount while recomposing the filesystem")

	plan, err := sandboxpolicy.RenderMountPlanWithEngine(
		spec.Effective, spec.Contract.NetworkEngine)
	require.NoError(t, err)
	assert.Contains(t, plan.Entries, sandboxpolicy.MountEntry{
		Path: "/scratch", Mode: sandboxpolicy.MountTmpfs, SizeBytes: 64 << 20,
	}, "recomposing the launch filesystem must not collide with the mount")
}

// A tmpfs over the workspace would mount an empty filesystem on top of the
// directory the agent was launched to work in. The protected-root wall does not
// cover this — the workspace is not a protected root — so the refusal has to
// come from the seam that knows the launch contract.
func TestTclaudeLayerLaunchSpecRefusesATmpfsOverTheWorkspace(t *testing.T) {
	cwd := t.TempDir()
	snapshot := sandboxpolicy.EmptySnapshot()
	snapshot.Effective.FilesystemRoot = sandboxpolicy.FilesystemRootSeparate
	snapshot.Effective.Tmpfs = []sandboxpolicy.TmpfsMount{{Path: cwd}}
	_, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
		HarnessName: harness.DefaultName, Cwd: cwd, Snapshot: &snapshot,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported_sandbox_profile_tmpfs")
	assert.Contains(t, err.Error(), "launch-required directory")
}

// The gate has to compare BOTH spellings of the mount. An authored tmpfs path
// stays lexical by design, so a row naming an existing symlink that resolves to
// the workspace would otherwise sail past a canonical-only comparison and then
// have bubblewrap resolve the destination through that symlink and mount an
// empty filesystem over the directory the agent was launched to work in.
func TestValidateTmpfsPathsAgainstContractResolvesSymlinkedSpellings(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(workspace, 0o755))
	link := filepath.Join(root, "scratch-link")
	require.NoError(t, os.Symlink(workspace, link))

	err := validateTmpfsPathsAgainstContract(
		[]sandboxpolicy.TmpfsMount{{Path: link}}, []string{workspace})
	require.Error(t, err, "a symlinked spelling of the workspace must be refused")
	assert.Contains(t, err.Error(), "unsupported_sandbox_profile_tmpfs")
	assert.Contains(t, err.Error(), link,
		"the refusal must name the spelling the operator actually wrote")

	// A symlink that resolves somewhere harmless is still allowed: the extra
	// spelling is a second thing to check, not a blanket refusal of symlinks.
	elsewhere := filepath.Join(root, "elsewhere")
	require.NoError(t, os.MkdirAll(elsewhere, 0o755))
	harmless := filepath.Join(root, "harmless-link")
	require.NoError(t, os.Symlink(elsewhere, harmless))
	require.NoError(t, validateTmpfsPathsAgainstContract(
		[]sandboxpolicy.TmpfsMount{{Path: harmless}}, []string{workspace}))
}

// …and the same through the production seam, so the fix is pinned where a
// launch actually reaches it rather than only at the helper.
func TestTclaudeLayerLaunchSpecRefusesATmpfsSymlinkedOntoTheWorkspace(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(cwd, 0o755))
	link := filepath.Join(root, "scratch-link")
	require.NoError(t, os.Symlink(cwd, link))

	snapshot := sandboxpolicy.EmptySnapshot()
	snapshot.Effective.FilesystemRoot = sandboxpolicy.FilesystemRootSeparate
	snapshot.Effective.Tmpfs = []sandboxpolicy.TmpfsMount{{Path: link}}
	_, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
		HarnessName: harness.DefaultName, Cwd: cwd, Snapshot: &snapshot,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported_sandbox_profile_tmpfs")
}
