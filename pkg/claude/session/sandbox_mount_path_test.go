package session

import (
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc/agentipctest"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// TestBwrapArgsBindsRemappedEntryAtGuestPath is the applier-layer statement of
// TCL-866: the host directory is the bind SOURCE and the sandbox path is the
// bind TARGET, in both access modes. The real-kernel proof that this actually
// lands lives in the host smoke (TestTclaudeLayerHostSmoke).
func TestBwrapArgsBindsRemappedEntryAtGuestPath(t *testing.T) {
	root := t.TempDir()
	readSource := filepath.Join(root, "dataset")
	writeSource := filepath.Join(root, "scratch")
	guestParent := filepath.Join(root, "guest")
	readGuest := filepath.Join(guestParent, "dataset")
	writeGuest := filepath.Join(guestParent, "scratch")
	for _, dir := range []string{readSource, writeSource, readGuest, writeGuest} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}

	got, err := bwrapArgs(nil, sandboxpolicy.MountPlan{Entries: []sandboxpolicy.MountEntry{
		{Path: readGuest, Mode: sandboxpolicy.MountRO, Source: readSource},
		{Path: writeGuest, Mode: sandboxpolicy.MountRW, Source: writeSource},
	}})
	require.NoError(t, err)
	assert.NotEqual(t, -1, indexOfBwrapBind(got, "--ro-bind", readSource, readGuest))
	assert.NotEqual(t, -1, indexOfBwrapBind(got, "--bind", writeSource, writeGuest))
	assert.Equal(t, -1, indexOfBwrapBind(got, "--ro-bind", readSource, readSource),
		"the host path must not also be exposed at its own location")
	assert.Equal(t, -1, indexOfBwrapBind(got, "--bind", writeSource, writeSource))
}

func TestBwrapArgsSkipsRemappedEntryWithMissingHostSource(t *testing.T) {
	root := t.TempDir()
	guest := filepath.Join(root, "guest")
	require.NoError(t, os.MkdirAll(guest, 0o755))
	missing := filepath.Join(root, "never-created")

	got, err := bwrapArgs(nil, sandboxpolicy.MountPlan{Entries: []sandboxpolicy.MountEntry{
		{Path: guest, Mode: sandboxpolicy.MountRO, Source: missing},
	}})
	require.NoError(t, err)
	assert.NotContains(t, got, missing,
		"a missing host source is skipped, exactly as for a same-path grant")
	assert.Equal(t, -1, indexOfBwrapBind(got, "--ro-bind", missing, guest))
}

// TestBwrapArgsRefusesRemapWithoutHostMountPointUnderHostOpenRoot pins the one
// host-side precondition a projection has: under the host-open posture the
// sandbox root IS the read-only host root, so bubblewrap has nowhere to create
// a new mount point. tclaude refuses with a named error instead of letting
// bubblewrap fail with something unattributable, and never mkdirs on the host.
func TestBwrapArgsRefusesRemapWithoutHostMountPointUnderHostOpenRoot(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "dataset")
	require.NoError(t, os.MkdirAll(source, 0o755))
	guest := filepath.Join(root, "not-created")

	_, err := bwrapArgs(nil, sandboxpolicy.MountPlan{Entries: []sandboxpolicy.MountEntry{
		{Path: guest, Mode: sandboxpolicy.MountRO, Source: source},
	}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tclaude_layer_missing_mount_point")
	assert.Contains(t, err.Error(), guest)
	_, statErr := os.Stat(guest)
	assert.True(t, os.IsNotExist(statErr), "the applier must not create the mount point")
}

// Under a constructed root the sandbox root is a fresh tmpfs, so bubblewrap
// creates the mount point inside the namespace and nothing on the host has to
// exist for it.
func TestBwrapArgsAllowsRemapWithoutHostMountPointUnderConstructedRoot(t *testing.T) {
	// The isolated posture requires the canonical agentd socket to be live, so
	// stand one up the way the other constructed-root tests do.
	home := agentipctest.ShortSocketDir(t)
	t.Setenv("HOME", home)
	t.Setenv(agentipc.SocketEnv, "")
	for _, floorSocket := range sandboxpolicy.AgentdSocketFloor() {
		require.NoError(t, os.MkdirAll(filepath.Dir(floorSocket), 0o700))
		listener, listenErr := net.Listen("unix", floorSocket)
		require.NoError(t, listenErr)
		t.Cleanup(func() { _ = listener.Close() })
	}
	source := filepath.Join(home, "dataset")
	require.NoError(t, os.MkdirAll(source, 0o755))
	guest := "/srv/shared"

	got, err := bwrapArgs(nil, sandboxpolicy.MountPlan{
		NetworkPosture: sandboxpolicy.NetworkIsolatedWithAgentd,
		Entries: []sandboxpolicy.MountEntry{
			{Path: guest, Mode: sandboxpolicy.MountRO, Source: source},
		},
	})
	require.NoError(t, err)
	assert.NotEqual(t, -1, indexOfBwrapBind(got, "--ro-bind", source, guest))
}

// TCL-798 retires the missing-mount-point refusal for a host-open plan that
// constructs its root: the network posture is unchanged, but there is now a
// fresh tmpfs root for bubblewrap to create the mount point in. The refusal is
// a property of the ROOT posture, not of the network posture it used to be
// welded to.
func TestBwrapArgsAllowsRemapWithoutHostMountPointUnderHostOpenConstructedRoot(t *testing.T) {
	home := agentipctest.ShortSocketDir(t)
	t.Setenv("HOME", home)
	t.Setenv(agentipc.SocketEnv, "")
	for _, floorSocket := range sandboxpolicy.AgentdSocketFloor() {
		require.NoError(t, os.MkdirAll(filepath.Dir(floorSocket), 0o700))
		listener, listenErr := net.Listen("unix", floorSocket)
		require.NoError(t, listenErr)
		t.Cleanup(func() { _ = listener.Close() })
	}
	source := filepath.Join(home, "dataset")
	require.NoError(t, os.MkdirAll(source, 0o755))
	guest := "/srv/shared"

	plan := sandboxpolicy.MountPlan{
		NetworkPosture: sandboxpolicy.NetworkHostOpen,
		RootPosture:    sandboxpolicy.RootConstructed,
		Entries: []sandboxpolicy.MountEntry{
			{Path: guest, Mode: sandboxpolicy.MountRO, Source: source},
		},
	}
	got, err := bwrapArgs(nil, plan)
	require.NoError(t, err)
	assert.NotEqual(t, -1, indexOfBwrapBind(got, "--ro-bind", source, guest))
	_, statErr := os.Stat(guest)
	assert.True(t, os.IsNotExist(statErr),
		"the applier still must not create the mount point on the host")

	// Mutation guard: the same plan with the root posture removed must go back
	// to refusing, so this test cannot pass because the refusal was deleted.
	plan.RootPosture = sandboxpolicy.RootHostInherited
	_, err = bwrapArgs(nil, plan)
	require.ErrorContains(t, err, "tclaude_layer_missing_mount_point")
}

func TestBwrapArgsRefusesRemappedHideEntry(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "dataset")
	require.NoError(t, os.MkdirAll(source, 0o755))

	_, err := bwrapArgs(nil, sandboxpolicy.MountPlan{Entries: []sandboxpolicy.MountEntry{
		{Path: "/srv/shared", Mode: sandboxpolicy.MountHide, Source: source},
	}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "a hide is always same-path")
}

// TestBuildTclaudeLayerLaunchSpecCarriesRemappedGrants covers the seam where the
// launch contract flattens policy back into bare path lists: a projection has no
// single path that means both sides, so it has to be carried around that
// flattening rather than dropped.
func TestBuildTclaudeLayerLaunchSpecCarriesRemappedGrants(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("HOME", home)
	cwd := filepath.Join(home, "work")
	dataset := filepath.Join(home, "dataset")
	require.NoError(t, os.MkdirAll(cwd, 0o755))
	require.NoError(t, os.MkdirAll(dataset, 0o755))

	spec, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
		HarnessName: "claude",
		Cwd:         cwd,
		Snapshot: &sandboxpolicy.Snapshot{
			Effective: sandboxpolicy.EffectiveProfile{
				Filesystem: []sandboxpolicy.FilesystemGrant{
					{Path: dataset, Access: sandboxpolicy.AccessRead, MountPath: "/srv/shared"},
				},
			},
		},
	})
	require.NoError(t, err)
	assert.Contains(t, spec.Effective.Filesystem, sandboxpolicy.FilesystemGrant{
		Path: dataset, Access: sandboxpolicy.AccessRead, MountPath: "/srv/shared",
	})

	plan, err := sandboxpolicy.RenderMountPlan(spec.Effective)
	require.NoError(t, err)
	assert.Contains(t, plan.Entries, sandboxpolicy.MountEntry{
		Path: "/srv/shared", Mode: sandboxpolicy.MountRO, Source: dataset,
	})
}

func TestBuildTclaudeLayerLaunchSpecRefusesRemapShadowingLaunchDirectory(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("HOME", home)
	cwd := filepath.Join(home, "work")
	dataset := filepath.Join(home, "dataset")
	require.NoError(t, os.MkdirAll(cwd, 0o755))
	require.NoError(t, os.MkdirAll(dataset, 0o755))

	_, err = BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
		HarnessName: "claude",
		Cwd:         cwd,
		Snapshot: &sandboxpolicy.Snapshot{
			Effective: sandboxpolicy.EffectiveProfile{
				Filesystem: []sandboxpolicy.FilesystemGrant{
					{Path: dataset, Access: sandboxpolicy.AccessRead, MountPath: cwd},
				},
			},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported_sandbox_profile_mount_path")
	assert.Contains(t, err.Error(), "would shadow the launch-required directory")
}

// TestDescribeTclaudeLayerPlanDisclosesProjection proves the dry-run surface
// separates the two sides rather than printing one path twice.
func TestDescribeTclaudeLayerPlanDisclosesProjection(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("HOME", home)
	cwd := filepath.Join(home, "work")
	dataset := filepath.Join(home, "dataset")
	require.NoError(t, os.MkdirAll(cwd, 0o755))
	require.NoError(t, os.MkdirAll(dataset, 0o755))

	effective := sandboxpolicy.EffectiveProfile{
		Filesystem: []sandboxpolicy.FilesystemGrant{
			{Path: dataset, Access: sandboxpolicy.AccessRead, MountPath: "/srv/shared"},
		},
	}
	spec, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
		HarnessName: "claude", Cwd: cwd,
		Snapshot: &sandboxpolicy.Snapshot{Effective: effective},
	})
	require.NoError(t, err)
	described, err := DescribeTclaudeLayerPlan(spec, effective)
	require.NoError(t, err)

	var found bool
	for _, entry := range described.Entries {
		if entry.Target == "/srv/shared" {
			found = true
			assert.Equal(t, dataset, entry.Source,
				"the row must name the host directory the authority came from")
			assert.Equal(t, SandboxPlanPresent, entry.Disposition,
				"disposition follows the host source, which exists")
		}
	}
	assert.True(t, found, "the projected mount must appear in the described plan")
}

// indexOfBwrapBind finds a full flag/source/target bind triple, which is what a
// projection produces and what indexOfBwrapTriplet (flag + one path) cannot
// distinguish from a same-path bind.
func indexOfBwrapBind(args []string, flag, source, target string) int {
	for i := 0; i+2 < len(args); i++ {
		if args[i] == flag && args[i+1] == source && args[i+2] == target {
			return i
		}
	}
	return -1
}

// TestRenderSeatbeltProfileRefusesProjection is the capability-honesty half on
// the Seatbelt side. Seatbelt is a path filter over the real host namespace, so
// it cannot make a directory appear elsewhere; approximating by allowing the
// host path would expose an unauthorized path AND leave the authorized one
// empty, so the renderer refuses.
func TestRenderSeatbeltProfileRefusesProjection(t *testing.T) {
	_, _, err := renderSeatbeltProfile(
		[]string{"/Users/dev/work"},
		nil,
		sandboxpolicy.MountPlan{
			NetworkPosture: sandboxpolicy.NetworkHostOpen,
			Entries: []sandboxpolicy.MountEntry{
				{Path: "/srv/shared", Mode: sandboxpolicy.MountRO, Source: "/Users/dev/dataset"},
			},
		},
		[]string{"/Users/dev/.tclaude/data"},
		"/private/tmp/tmux-501",
		"/private/var/folders/ab/runtime/T",
		nil,
		nil,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "seatbelt_mount_path_projection")
	assert.Contains(t, err.Error(), "/srv/shared")
	assert.Contains(t, err.Error(), "/Users/dev/dataset")
}

// The host resolver reopen is what keeps DNS working once a host-network plan
// stops inheriting the host root. Its real-engine evidence is the CI smoke arm;
// this covers the traversal deterministically, against a fixture rather than
// the developer's own /etc/resolv.conf.
func TestAppendTclaudeLayerHostResolverReopensOnlyTheRunTargetFile(t *testing.T) {
	hostOpenConstructed := sandboxpolicy.MountPlan{
		NetworkPosture: sandboxpolicy.NetworkHostOpen,
		RootPosture:    sandboxpolicy.RootConstructed,
	}
	// EvalSymlinks the fixture root: on macOS the temp dir lives under /var,
	// which is itself a symlink to /private/var, so an unresolved root would
	// place the resolved target outside it and the traversal would correctly
	// decline to chase it.
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	runtimeDir := filepath.Join(root, "systemd", "resolve")
	require.NoError(t, os.MkdirAll(runtimeDir, 0o755))
	target := filepath.Join(runtimeDir, "stub-resolv.conf")
	require.NoError(t, os.WriteFile(target, []byte("nameserver 127.0.0.53\n"), 0o644))

	t.Run("symlink into the runtime root binds the file and creates its parents", func(t *testing.T) {
		resolver := filepath.Join(t.TempDir(), "resolv.conf")
		require.NoError(t, os.Symlink(target, resolver))
		swapTclaudeLayerHostResolver(t, resolver, root)

		got, _, err := appendTclaudeLayerHostResolver(nil, hostOpenConstructed)
		require.NoError(t, err)
		assert.NotEqual(t, -1, indexOfBwrapBind(got, "--ro-bind", target, target),
			"the resolver target file must be reopened")
		// Only the target FILE. /run is where the ambient sockets this posture
		// exists to hide tend to live, so its directory must never be bound.
		for i := 0; i+1 < len(got); i++ {
			if got[i] == "--ro-bind" || got[i] == "--bind" {
				assert.NotEqual(t, root, got[i+1],
					"binding the runtime root would hand back the sockets the constructed root took away")
			}
		}
		for _, parent := range []string{root, filepath.Join(root, "systemd"), runtimeDir} {
			assert.Containsf(t, got, parent,
				"the mount point's parent %q must be created inside the namespace", parent)
			assert.Equal(t, "--dir", got[indexOfBwrapArg(got, parent)-1])
		}
	})

	t.Run("a plain file needs nothing; the read-only /etc bind already carries it", func(t *testing.T) {
		resolver := filepath.Join(t.TempDir(), "resolv.conf")
		require.NoError(t, os.WriteFile(resolver, []byte("nameserver 1.1.1.1\n"), 0o644))
		swapTclaudeLayerHostResolver(t, resolver, root)

		got, _, err := appendTclaudeLayerHostResolver(nil, hostOpenConstructed)
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("a target outside the runtime root is not chased", func(t *testing.T) {
		outside := filepath.Join(t.TempDir(), "elsewhere.conf")
		require.NoError(t, os.WriteFile(outside, []byte("nameserver 1.1.1.1\n"), 0o644))
		resolver := filepath.Join(t.TempDir(), "resolv.conf")
		require.NoError(t, os.Symlink(outside, resolver))
		swapTclaudeLayerHostResolver(t, resolver, root)

		got, _, err := appendTclaudeLayerHostResolver(nil, hostOpenConstructed)
		require.NoError(t, err)
		assert.Empty(t, got,
			"silently binding an arbitrary host path to make DNS work would be a wider hole than the failure it prevents")
	})

	// bwrap binds whatever it is given. A resolver pointing at a directory would
	// recursively expose that /run subtree — where the ambient sockets this
	// posture hides actually live — and one pointing at a socket would reopen an
	// ambient socket outright, either way making the Partial rating untrue.
	t.Run("a non-regular target under the runtime root is refused", func(t *testing.T) {
		socketDir := filepath.Join(root, "user", "1000")
		require.NoError(t, os.MkdirAll(socketDir, 0o755))
		// A FIFO stands in for the ambient socket: it is the same "not a regular
		// file" case, without sockaddr_un's 108-byte path limit that a real
		// listener under a long temp path would hit.
		ambient := filepath.Join(socketDir, "bus")
		require.NoError(t, syscall.Mkfifo(ambient, 0o600))

		for _, target := range []string{socketDir, ambient} {
			resolver := filepath.Join(t.TempDir(), "resolv.conf")
			require.NoError(t, os.Symlink(target, resolver))
			swapTclaudeLayerHostResolver(t, resolver, root)

			got, _, err := appendTclaudeLayerHostResolver(nil, hostOpenConstructed)
			require.NoError(t, err)
			assert.Emptyf(t, got,
				"binding %q would re-expose exactly what the constructed root hides", target)
		}
	})

	t.Run("a dangling symlink is left as broken as it is on the host", func(t *testing.T) {
		resolver := filepath.Join(t.TempDir(), "resolv.conf")
		require.NoError(t, os.Symlink(filepath.Join(root, "gone.conf"), resolver))
		swapTclaudeLayerHostResolver(t, resolver, root)

		got, _, err := appendTclaudeLayerHostResolver(nil, hostOpenConstructed)
		require.NoError(t, err, "a resolver already broken on the host must not fail the launch")
		assert.Empty(t, got)
	})

	t.Run("an inherited root and an isolated network are both left alone", func(t *testing.T) {
		resolver := filepath.Join(t.TempDir(), "resolv.conf")
		require.NoError(t, os.Symlink(target, resolver))
		swapTclaudeLayerHostResolver(t, resolver, root)

		for _, plan := range []sandboxpolicy.MountPlan{
			{NetworkPosture: sandboxpolicy.NetworkHostOpen},
			{NetworkPosture: sandboxpolicy.NetworkIsolatedWithAgentd,
				RootPosture: sandboxpolicy.RootConstructed},
			{NetworkPosture: sandboxpolicy.NetworkFiltered,
				RootPosture: sandboxpolicy.RootConstructed},
		} {
			got, _, err := appendTclaudeLayerHostResolver(nil, plan)
			require.NoError(t, err)
			assert.Emptyf(t, got,
				"%v has no host resolver to preserve", plan.NetworkPosture)
		}
	})
}

func swapTclaudeLayerHostResolver(t *testing.T, resolver, runtimeRoot string) {
	t.Helper()
	previousPath := tclaudeLayerHostResolverPath
	previousRoot := tclaudeLayerHostResolverRuntimeRoot
	tclaudeLayerHostResolverPath = resolver
	tclaudeLayerHostResolverRuntimeRoot = runtimeRoot
	t.Cleanup(func() {
		tclaudeLayerHostResolverPath = previousPath
		tclaudeLayerHostResolverRuntimeRoot = previousRoot
	})
}

func indexOfBwrapArg(args []string, want string) int {
	for i, arg := range args {
		if arg == want {
			return i
		}
	}
	return -1
}

// A profile deny on the resolver's parent must not silently break DNS. The
// reopen happens during root construction, before plan replay, so without a
// repair an ordinary ancestor hide would shadow it with no notice — exactly the
// case the agentd socket is already repaired for.
func TestBwrapArgsRepairsHostResolverBeneathAnOrdinaryDeny(t *testing.T) {
	home := agentipctest.ShortSocketDir(t)
	t.Setenv("HOME", home)
	t.Setenv(agentipc.SocketEnv, "")
	for _, floorSocket := range sandboxpolicy.AgentdSocketFloor() {
		require.NoError(t, os.MkdirAll(filepath.Dir(floorSocket), 0o700))
		listener, listenErr := net.Listen("unix", floorSocket)
		require.NoError(t, listenErr)
		t.Cleanup(func() { _ = listener.Close() })
	}
	runtimeRoot, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	target := filepath.Join(runtimeRoot, "systemd", "resolve", "stub-resolv.conf")
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
	require.NoError(t, os.WriteFile(target, []byte("nameserver 127.0.0.53\n"), 0o644))
	resolver := filepath.Join(t.TempDir(), "resolv.conf")
	require.NoError(t, os.Symlink(target, resolver))
	swapTclaudeLayerHostResolver(t, resolver, runtimeRoot)

	plan := sandboxpolicy.MountPlan{
		NetworkPosture: sandboxpolicy.NetworkHostOpen,
		RootPosture:    sandboxpolicy.RootConstructed,
		Entries: []sandboxpolicy.MountEntry{
			{Path: runtimeRoot, Mode: sandboxpolicy.MountHide},
		},
	}
	got, err := bwrapArgs(nil, plan)
	require.NoError(t, err)
	binds := indicesOfBwrapTriplet(got, "--ro-bind", target)
	require.Len(t, binds, 2,
		"the resolver must be reopened during root construction and repaired after the deny")
	denyHide := indexOfBwrapTriplet(got, "--tmpfs", runtimeRoot)
	require.NotEqual(t, -1, denyHide)
	assert.Less(t, denyHide, binds[1],
		"the repair must land after the hide that shadowed the original bind")

	// Mutation guard: with no deny covering it, the reopen stands alone.
	plan.Entries = nil
	got, err = bwrapArgs(nil, plan)
	require.NoError(t, err)
	assert.Len(t, indicesOfBwrapTriplet(got, "--ro-bind", target), 1)
}
