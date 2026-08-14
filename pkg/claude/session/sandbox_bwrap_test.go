package session

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc/agentipctest"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/common"
)

func TestBwrapArgsRenderOrderedMountPlan(t *testing.T) {
	root := t.TempDir()
	readPath := root + "/read"
	writePath := root + "/work"
	privatePath := writePath + "/private"
	projectPath := root + "/project"
	reopenPath := projectPath + "/reopen"
	require.NoError(t, os.MkdirAll(readPath, 0o755))
	require.NoError(t, os.MkdirAll(privatePath, 0o755))
	require.NoError(t, os.MkdirAll(reopenPath, 0o755))
	for _, tc := range []struct {
		name string
		plan sandboxpolicy.MountPlan
		want []string
	}{
		{
			name: "ro rw and hide",
			plan: sandboxpolicy.MountPlan{Entries: []sandboxpolicy.MountEntry{
				{Path: readPath, Mode: sandboxpolicy.MountRO},
				{Path: writePath, Mode: sandboxpolicy.MountRW},
				{Path: root + "/secret", Mode: sandboxpolicy.MountHide},
			}},
			want: []string{
				"--ro-bind", readPath, readPath,
				"--bind", writePath, writePath,
				"--tmpfs", root + "/secret",
				"--remount-ro", root + "/secret",
			},
		},
		{
			name: "deny inside allow remains later",
			plan: sandboxpolicy.MountPlan{Entries: []sandboxpolicy.MountEntry{
				{Path: writePath, Mode: sandboxpolicy.MountRW},
				{Path: privatePath, Mode: sandboxpolicy.MountHide},
			}},
			want: []string{
				"--bind", writePath, writePath,
				"--tmpfs", privatePath,
				"--remount-ro", privatePath,
			},
		},
		{
			name: "allow inside deny reopens later",
			plan: sandboxpolicy.MountPlan{Entries: []sandboxpolicy.MountEntry{
				{Path: projectPath, Mode: sandboxpolicy.MountHide},
				{Path: reopenPath, Mode: sandboxpolicy.MountRW},
			}},
			want: []string{
				"--tmpfs", projectPath,
				"--bind", reopenPath, reopenPath,
				"--remount-ro", projectPath,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := bwrapArgs(nil, tc.plan)
			require.NoError(t, err)
			assert.Equal(t, tc.want, bwrapFilesystemArgsWithin(got, root),
				"the builder must preserve MountPlan order verbatim")
			assert.NotContains(t, got, "--unshare-net")
			assert.NotContains(t, got, "--unshare-pid")
			assert.NotContains(t, got, "--unshare-ipc")
			assert.Equal(t, -1, indexOfBwrapTriplet(got, "--remount-ro", "/"),
				"host-open already inherits a read-only host root")
		})
	}
}

func TestBwrapArgsSkipsMissingBindsButStillAppliesMissingHide(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-created")
	got, err := bwrapArgs(nil, sandboxpolicy.MountPlan{Entries: []sandboxpolicy.MountEntry{
		{Path: missing + "-ro", Mode: sandboxpolicy.MountRO},
		{Path: missing + "-rw", Mode: sandboxpolicy.MountRW},
		{Path: missing + "-hide", Mode: sandboxpolicy.MountHide},
	}})
	require.NoError(t, err)
	hide := indexOfBwrapTriplet(got, "--tmpfs", missing+"-hide")
	remount := indexOfBwrapTriplet(got, "--remount-ro", missing+"-hide")
	require.NotEqual(t, -1, hide)
	require.NotEqual(t, -1, remount)
	assert.Less(t, hide, remount)
	assert.NotContains(t, got, missing+"-ro")
	assert.NotContains(t, got, missing+"-rw")
}

// TestBwrapArgsHidesProtectedRootsWithNoReopens is the applier-layer half of
// the absolute protected-root invariant (TCL-791). Its policy-layer half lives
// in sandboxpolicy.TestRenderedMountPlanNeverTouchesAProtectedRoot.
//
// The plan here is built the only way production builds one — through
// Normalize and RenderMountPlan — because that is the path a reintroduced
// reopen would have to travel. Two things are asserted together: that the
// broadest ancestor grant is refused outright, and that the broadest grant
// which IS legal still leaves every protected root hidden with no reopen.
func TestBwrapArgsHidesProtectedRootsWithNoReopens(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("HOME", home)
	workspace := filepath.Join(home, "work")
	for _, relative := range []string{
		filepath.Join(".tclaude", "data"),
		filepath.Join(".claude", "sessions"),
	} {
		require.NoError(t, os.MkdirAll(filepath.Join(home, relative), 0o700))
	}
	require.NoError(t, os.MkdirAll(workspace, 0o700))
	protected, err := sandboxpolicy.ProtectedPaths()
	require.NoError(t, err)
	require.Len(t, protected, 2)

	// A read grant on the protected roots' common ancestor is the cheapest way
	// to reach them, and it is refused. This is why no bind below can cover a
	// protected root in the first place.
	_, err = sandboxpolicy.Normalize(sandboxpolicy.Profile{
		Name:       "ancestor-read",
		Filesystem: []sandboxpolicy.FilesystemGrant{{Path: home, Access: sandboxpolicy.AccessRead}},
	})
	require.ErrorContains(t, err, "intersects protected directory")

	// The widest legal shape: deny the ancestor, reopen an ordinary child.
	normalized, err := sandboxpolicy.Normalize(sandboxpolicy.Profile{
		Name: "deny-home-reopen-work",
		Filesystem: []sandboxpolicy.FilesystemGrant{
			{Path: home, Access: sandboxpolicy.AccessDeny},
			{Path: workspace, Access: sandboxpolicy.AccessWrite},
		},
	})
	require.NoError(t, err)
	plan, err := sandboxpolicy.RenderMountPlan(sandboxpolicy.EffectiveProfile{
		Filesystem: normalized.Filesystem,
	})
	require.NoError(t, err)

	got, err := bwrapArgs(nil, plan)
	require.NoError(t, err)

	workspaceBind := indexOfBwrapTriplet(got, "--bind", workspace)
	require.NotEqual(t, -1, workspaceBind,
		"the legal reopen must actually land, or the checks below are vacuous")
	for _, root := range protected {
		hides := indicesOfBwrapTriplet(got, "--tmpfs", root)
		require.NotEmpty(t, hides, "protected root %s must be hidden", root)
		assert.Equal(t, -1, indexOfBwrapTriplet(got, "--ro-bind", root),
			"no read reopen may exist for protected root %s", root)
		assert.Equal(t, -1, indexOfBwrapTriplet(got, "--bind", root),
			"no write reopen may exist for protected root %s", root)
	}
}

func TestBwrapArgsHidesTmuxSocketAfterPlan(t *testing.T) {
	tmuxBase := t.TempDir()
	t.Setenv("TMUX_TMPDIR", tmuxBase)
	tmuxSocketDir, err := clcommon.TclaudeTmuxSocketDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(tmuxSocketDir, 0o700))

	got, err := bwrapArgs(nil, sandboxpolicy.MountPlan{Entries: []sandboxpolicy.MountEntry{
		{Path: tmuxBase, Mode: sandboxpolicy.MountRW},
		{Path: tmuxSocketDir, Mode: sandboxpolicy.MountRW},
	}})
	require.NoError(t, err)

	parentGrant := indexOfBwrapTriplet(got, "--bind", tmuxBase)
	exactGrant := indexOfBwrapTriplet(got, "--bind", tmuxSocketDir)
	hide := indexOfBwrapTriplet(got, "--tmpfs", tmuxSocketDir)
	remount := indexOfBwrapTriplet(got, "--remount-ro", tmuxSocketDir)
	require.NotEqual(t, -1, parentGrant)
	require.NotEqual(t, -1, exactGrant)
	require.NotEqual(t, -1, hide)
	require.NotEqual(t, -1, remount)
	assert.Less(t, parentGrant, hide)
	assert.Less(t, exactGrant, hide, "host-control hide must be unshadowable by any plan entry")
	assert.Equal(t, len(got)-4, hide, "tmux socket hide must be the final applier phase")
	assert.Equal(t, len(got)-2, remount, "tmux socket read-only hardening must be the final operation")
}

func TestBwrapArgsCreatesHostControlMountpointBeforeAncestorRemount(t *testing.T) {
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	tmuxSocketDir, err := clcommon.TclaudeTmuxSocketDir()
	require.NoError(t, err)
	tmuxBase := filepath.Dir(tmuxSocketDir)

	got, err := bwrapArgs(nil, sandboxpolicy.MountPlan{Entries: []sandboxpolicy.MountEntry{{
		Path: tmuxBase, Mode: sandboxpolicy.MountHide,
	}}})
	require.NoError(t, err)

	parentHide := indexOfBwrapTriplet(got, "--tmpfs", tmuxBase)
	mountpoint := indexOfBwrapTriplet(got, "--dir", tmuxSocketDir)
	parentRemount := indexOfBwrapTriplet(got, "--remount-ro", tmuxBase)
	finalHide := indexOfBwrapTriplet(got, "--tmpfs", tmuxSocketDir)
	require.NotEqual(t, -1, parentHide)
	require.NotEqual(t, -1, mountpoint)
	require.NotEqual(t, -1, parentRemount)
	require.NotEqual(t, -1, finalHide)
	assert.Less(t, parentHide, mountpoint)
	assert.Less(t, mountpoint, parentRemount,
		"the final class-4 destination must exist before its hidden ancestor becomes read-only")
	assert.Less(t, parentRemount, finalHide)
}

// TestBwrapArgsPrivateWriteDirOverridesPolicyButPrecedesHostControl covers the
// daemon-owned spawn-attachment exception, which lives under a protected root
// but is not policy-reachable: the daemon supplies it directly to the applier,
// no profile can name it, and it hides its own shared parent so sibling
// sessions stay absent. It survives TCL-791 unchanged — it grants nothing an
// operator can ask for, which is precisely what break-glass did.
func TestBwrapArgsPrivateWriteDirOverridesPolicyButPrecedesHostControl(t *testing.T) {
	// Canonicalized: ProtectedPaths resolves symlinks, so on macOS (where
	// TempDir sits under /var → /private/var) an unresolved HOME would build
	// protected paths that never match the ones the renderer hides, and the
	// assertions below would stop testing anything.
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("HOME", home)
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	protected := filepath.Join(home, ".tclaude", "data")
	parent := filepath.Join(protected, "spawn-attachments")
	current := filepath.Join(parent, "current-session")
	sibling := filepath.Join(parent, "sibling-session")
	workspace := filepath.Join(home, "work")
	for _, dir := range []string{current, sibling, workspace} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}

	got, err := bwrapArgs(nil, sandboxpolicy.MountPlan{
		Entries: []sandboxpolicy.MountEntry{
			{Path: workspace, Mode: sandboxpolicy.MountRW},
		},
	}, TclaudeLayerPrivateWriteDir{Parent: parent, Current: current})
	require.NoError(t, err)

	protectedHide := indexOfBwrapTriplet(got, "--tmpfs", protected)
	policyGrant := indexOfBwrapTriplet(got, "--bind", workspace)
	privateHide := indexOfBwrapTriplet(got, "--tmpfs", parent)
	currentReopen := indexOfBwrapTriplet(got, "--bind", current)
	parentRemount := indexOfBwrapTriplet(got, "--remount-ro", parent)
	tmuxHide := indexOfBwrapTriplet(got, "--tmpfs", mustTmuxSocketDir(t))
	if runtime.GOOS == "linux" {
		require.NotEqual(t, -1, protectedHide)
		assert.Less(t, protectedHide, privateHide,
			"the daemon exception replays on top of the class-3 baseline, not instead of it")
	}
	require.NotEqual(t, -1, policyGrant)
	require.NotEqual(t, -1, privateHide)
	require.NotEqual(t, -1, currentReopen)
	require.NotEqual(t, -1, parentRemount)
	require.NotEqual(t, -1, tmuxHide)
	assert.Equal(t, -1, indexOfBwrapTriplet(got, "--bind", sibling),
		"no surface remains that could reopen a sibling session's attachments")
	assert.Less(t, policyGrant, privateHide,
		"private sibling concealment must beat ordinary policy replay")
	assert.Less(t, privateHide, currentReopen)
	assert.Less(t, currentReopen, parentRemount)
	assert.Less(t, parentRemount, tmuxHide,
		"host tmux control remains the final unshadowable phase")
}

func TestCleanTclaudeLayerPrivateWriteDirsRequiresExistingDirectChild(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "private")
	current := filepath.Join(parent, "current")
	require.NoError(t, os.MkdirAll(current, 0o700))
	got, err := cleanTclaudeLayerPrivateWriteDirs([]TclaudeLayerPrivateWriteDir{{
		Parent: parent, Current: current,
	}})
	require.NoError(t, err)
	require.Equal(t, []TclaudeLayerPrivateWriteDir{{Parent: parent, Current: current}}, got)

	symlinkCurrent := filepath.Join(parent, "symlink-current")
	require.NoError(t, os.Symlink(current, symlinkCurrent))
	for _, invalid := range []TclaudeLayerPrivateWriteDir{
		{Parent: parent, Current: filepath.Join(parent, "nested", "current")},
		{Parent: parent, Current: filepath.Dir(parent)},
		{Parent: "relative", Current: current},
		{Parent: parent, Current: filepath.Join(parent, "missing")},
		{Parent: parent, Current: symlinkCurrent},
	} {
		_, err := cleanTclaudeLayerPrivateWriteDirs([]TclaudeLayerPrivateWriteDir{invalid})
		require.Error(t, err, invalid)
	}
}

func TestBwrapArgsDeferredRemountTracksTopmostExactMount(t *testing.T) {
	path := t.TempDir()
	for _, tc := range []struct {
		name        string
		entries     []sandboxpolicy.MountEntry
		wantRemount bool
	}{
		{
			name: "later exact bind cancels hide remount",
			entries: []sandboxpolicy.MountEntry{
				{Path: path, Mode: sandboxpolicy.MountHide},
				{Path: path, Mode: sandboxpolicy.MountRW},
			},
		},
		{
			name: "final exact hide restores pending remount",
			entries: []sandboxpolicy.MountEntry{
				{Path: path, Mode: sandboxpolicy.MountHide},
				{Path: path, Mode: sandboxpolicy.MountRW},
				{Path: path, Mode: sandboxpolicy.MountHide},
			},
			wantRemount: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := bwrapArgs(nil, sandboxpolicy.MountPlan{Entries: tc.entries})
			require.NoError(t, err)
			if tc.wantRemount {
				assert.NotEqual(t, -1, indexOfBwrapTriplet(got, "--remount-ro", path))
			} else {
				assert.Equal(t, -1, indexOfBwrapTriplet(got, "--remount-ro", path))
			}
		})
	}
}

func TestBwrapArgsSkipsProtectedRemountsShadowedByAncestorHide(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("HOME", home)
	stateRoot := filepath.Join(home, ".codex")
	require.NoError(t, os.MkdirAll(stateRoot, 0o700))
	protectedRoots, err := sandboxpolicy.ProtectedPaths()
	require.NoError(t, err)

	got, err := bwrapArgs([]string{stateRoot}, sandboxpolicy.MountPlan{
		Entries: []sandboxpolicy.MountEntry{{Path: home, Mode: sandboxpolicy.MountHide}},
	})
	require.NoError(t, err)

	require.NotEqual(t, -1, indexOfBwrapTriplet(got, "--remount-ro", home))
	for _, protected := range protectedRoots {
		require.NotEqual(t, -1, indexOfBwrapTriplet(got, "--tmpfs", protected),
			"the protected baseline must still be established before plan replay")
		assert.Equal(t, -1, indexOfBwrapTriplet(got, "--remount-ro", protected),
			"the later home tmpfs shadows this exact mountpoint; the home remount hardens its view")
	}
}

func TestBwrapArgsLaunchContractPrecedesProtectedHides(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("HOME", home)
	claudeRoot := filepath.Join(home, ".claude")
	claudeSessions := filepath.Join(claudeRoot, "sessions")
	workspace := filepath.Join(home, "work")
	agentDir := filepath.Join(home, "agent-cache")
	for _, path := range []string{claudeSessions, workspace, agentDir} {
		require.NoError(t, os.MkdirAll(path, 0o700))
	}
	effective := sandboxpolicy.EffectiveProfile{
		AgentDirectories: []string{"AGENT_CACHE"},
		Environment: []sandboxpolicy.EnvironmentEntry{{
			Name: "AGENT_CACHE", Value: agentDir,
		}},
	}
	phase0, err := tclaudeLayerPhase0WriteDirs(TclaudeLayerLaunchContract{
		HarnessName: harness.DefaultName,
		WriteDirs:   []string{workspace},
	}, effective)
	require.NoError(t, err)

	got, err := bwrapArgs(phase0, sandboxpolicy.MountPlan{})
	require.NoError(t, err)
	stateBind := indexOfBwrapTriplet(got, "--bind", claudeRoot)
	workspaceBind := indexOfBwrapTriplet(got, "--bind", workspace)
	agentBind := indexOfBwrapTriplet(got, "--bind", agentDir)
	protectedHide := indexOfBwrapTriplet(got, "--tmpfs", claudeSessions)
	protectedRemount := indexOfBwrapTriplet(got, "--remount-ro", claudeSessions)
	require.NotEqual(t, -1, stateBind)
	require.NotEqual(t, -1, workspaceBind)
	require.NotEqual(t, -1, agentBind)
	require.NotEqual(t, -1, protectedHide)
	require.NotEqual(t, -1, protectedRemount)
	assert.Less(t, stateBind, protectedHide, "protected state must stay hidden above the writable harness root")
	assert.Less(t, workspaceBind, protectedHide)
	assert.Less(t, agentBind, protectedHide)
	assert.Less(t, protectedHide, protectedRemount)
}

// TestBwrapArgsRepairsLaunchContractAfterHomeDeny exercises reopen-under-deny
// (TCL-623), which TCL-791 leaves intact: an ordinary deny on an ancestor with
// a narrower reopen beneath it. The narrower path is deliberately an ordinary
// directory now — under the absolute invariant, the reopen can never be a
// protected child, and the protected root under the same state root must come
// back hidden regardless.
func TestBwrapArgsRepairsLaunchContractAfterHomeDeny(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("HOME", home)
	stateRoot := filepath.Join(home, ".claude")
	protectedRoot := filepath.Join(stateRoot, "sessions")
	reopened := filepath.Join(home, "work", "reopened")
	require.NoError(t, os.MkdirAll(protectedRoot, 0o700))
	require.NoError(t, os.MkdirAll(reopened, 0o700))

	got, err := bwrapArgs([]string{stateRoot}, sandboxpolicy.MountPlan{
		Entries: []sandboxpolicy.MountEntry{
			{Path: home, Mode: sandboxpolicy.MountHide},
			{Path: reopened, Mode: sandboxpolicy.MountRO},
		},
	})
	require.NoError(t, err)

	homeHide := indexOfBwrapTriplet(got, "--tmpfs", home)
	stateBinds := indicesOfBwrapTriplet(got, "--bind", stateRoot)
	protectedHides := indicesOfBwrapTriplet(got, "--tmpfs", protectedRoot)
	reopen := indexOfBwrapTriplet(got, "--ro-bind", reopened)
	homeRemount := indexOfBwrapTriplet(got, "--remount-ro", home)
	protectedRemount := indexOfBwrapTriplet(got, "--remount-ro", protectedRoot)
	require.NotEqual(t, -1, homeHide)
	require.Len(t, stateBinds, 2, "the state root must be rebound after an ordinary ancestor deny")
	require.Len(t, protectedHides, 2, "repairing the state root must restore its protected child hide")
	require.NotEqual(t, -1, reopen)
	require.NotEqual(t, -1, homeRemount)
	require.NotEqual(t, -1, protectedRemount)
	assert.Equal(t, -1, indexOfBwrapTriplet(got, "--ro-bind", protectedRoot),
		"no plan authority beats a protected hide any more")
	assert.Less(t, stateBinds[0], homeHide)
	assert.Less(t, homeHide, stateBinds[1])
	assert.Less(t, stateBinds[1], protectedHides[1])
	assert.Less(t, protectedHides[1], protectedRemount,
		"the restored hide must be hardened after it is re-established")
	assert.Less(t, reopen, homeRemount,
		"the ancestor hide must remain mutable until all narrower repairs have landed")
}

func TestBwrapArgsGeneratedHomeWriteRehidesProtectedRoots(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("HOME", home)
	stateRoot := filepath.Join(home, ".claude")
	claudeProtected := filepath.Join(stateRoot, "sessions")
	tclaudeProtected := filepath.Join(home, ".tclaude", "data")
	for _, path := range []string{claudeProtected, tclaudeProtected} {
		require.NoError(t, os.MkdirAll(path, 0o700))
	}

	got, err := bwrapArgs(
		[]string{stateRoot, home},
		sandboxpolicy.MountPlan{Entries: []sandboxpolicy.MountEntry{{
			Path: home, Mode: sandboxpolicy.MountRW,
		}}},
	)
	require.NoError(t, err)

	homeBinds := indicesOfBwrapTriplet(got, "--bind", home)
	claudeHides := indicesOfBwrapTriplet(got, "--tmpfs", claudeProtected)
	tclaudeHides := indicesOfBwrapTriplet(got, "--tmpfs", tclaudeProtected)
	claudeRemount := indexOfBwrapTriplet(got, "--remount-ro", claudeProtected)
	tclaudeRemount := indexOfBwrapTriplet(got, "--remount-ro", tclaudeProtected)
	require.Len(t, homeBinds, 2, "the generated plan contains the workspace/Home reopen")
	require.Len(t, claudeHides, 2)
	require.Len(t, tclaudeHides, 2)
	require.NotEqual(t, -1, claudeRemount)
	require.NotEqual(t, -1, tclaudeRemount)
	assert.Less(t, homeBinds[1], claudeHides[1])
	assert.Less(t, homeBinds[1], tclaudeHides[1],
		"generated launch grants must not acquire protected authority")
	assert.Less(t, claudeHides[1], claudeRemount)
	assert.Less(t, tclaudeHides[1], tclaudeRemount)
}

func TestBwrapArgsRefusesLaunchContractInsideProtectedRoot(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("HOME", home)
	stateRoot := filepath.Join(home, ".claude")
	protectedRoot := filepath.Join(stateRoot, "sessions")
	workspace := filepath.Join(protectedRoot, "work")
	require.NoError(t, os.MkdirAll(workspace, 0o700))

	_, err = bwrapArgs([]string{stateRoot, workspace}, sandboxpolicy.MountPlan{})
	require.ErrorContains(t, err, workspace)
	require.ErrorContains(t, err, protectedRoot)
}

func TestWrapTclaudeLayerRefusesProfileRuleWithinHarnessState(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("HOME", home)
	stateRoot := filepath.Join(home, ".claude")
	require.NoError(t, os.MkdirAll(stateRoot, 0o700))
	offending := sandboxpolicy.FilesystemGrant{
		Path: stateRoot, Access: sandboxpolicy.AccessDeny,
	}

	_, err = WrapTclaudeLayer("/usr/bin/bwrap", sandboxpolicy.EffectiveProfile{
		Filesystem: []sandboxpolicy.FilesystemGrant{offending},
	}, TclaudeLayerLaunchContract{
		HarnessName:       harness.DefaultName,
		ProfileFilesystem: []sandboxpolicy.FilesystemGrant{offending},
	}, "agent")
	require.ErrorContains(t, err, stateRoot)
	require.ErrorContains(t, err, "requires write access")
	require.ErrorContains(t, err, "conflicting harness-state authority")
}

func TestValidateTclaudeLayerHarnessStateRulesAllowsEquivalentAccess(t *testing.T) {
	stateRoot := "/var/lib/harness"
	writableRule := []tclaudeLayerHarnessStateRule{{
		Path: stateRoot, Access: sandboxpolicy.AccessWrite,
	}}

	for _, grant := range []sandboxpolicy.FilesystemGrant{
		{Path: stateRoot, Access: sandboxpolicy.AccessWrite},
		{Path: filepath.Join(stateRoot, "cache"), Access: sandboxpolicy.AccessWrite},
	} {
		require.NoError(t, validateTclaudeLayerHarnessStateRules(
			writableRule,
			[]sandboxpolicy.FilesystemGrant{grant}),
			"a write rule inside a writable contract root is redundant, not conflicting")
	}

	for _, access := range []sandboxpolicy.Access{
		sandboxpolicy.AccessRead,
		sandboxpolicy.AccessDeny,
	} {
		err := validateTclaudeLayerHarnessStateRules(
			writableRule,
			[]sandboxpolicy.FilesystemGrant{{Path: stateRoot, Access: access}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "requires write access")
	}

	require.NoError(t, validateTclaudeLayerHarnessStateRules(
		[]tclaudeLayerHarnessStateRule{{Path: stateRoot, Access: sandboxpolicy.AccessRead}},
		[]sandboxpolicy.FilesystemGrant{{Path: stateRoot, Access: sandboxpolicy.AccessRead}}),
		"a read rule inside a read-only contract root is likewise redundant")
	require.Error(t, validateTclaudeLayerHarnessStateRules(
		[]tclaudeLayerHarnessStateRule{{Path: stateRoot, Access: sandboxpolicy.AccessRead}},
		[]sandboxpolicy.FilesystemGrant{{Path: stateRoot, Access: sandboxpolicy.AccessWrite}}),
		"a write rule must not broaden a read-only harness-state contract")
}

func TestValidateTclaudeLayerLaunchSpecAllowsEquivalentNestedReadOnlyAccess(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("HOME", home)
	stateRoot := filepath.Join(home, ".opencode")
	readOnlyBin := filepath.Join(stateRoot, "bin")
	workspace := filepath.Join(home, "work")
	for _, path := range []string{stateRoot, readOnlyBin, workspace} {
		require.NoError(t, os.MkdirAll(path, 0o700))
	}
	for _, name := range []string{
		"XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME",
	} {
		t.Setenv(name, "")
	}

	grant := sandboxpolicy.FilesystemGrant{
		Path: readOnlyBin, Access: sandboxpolicy.AccessRead,
	}
	spec, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
		HarnessName: harness.OpenCodeName,
		Cwd:         workspace,
		Snapshot: &sandboxpolicy.Snapshot{Effective: sandboxpolicy.EffectiveProfile{
			Filesystem: []sandboxpolicy.FilesystemGrant{grant},
		}},
	})
	require.NoError(t, err)
	require.Contains(t, spec.Contract.ReadOnlyStateDirs, readOnlyBin)
	require.NoError(t, ValidateTclaudeLayerLaunchSpec(spec),
		"the read-only child is more specific than its writable state ancestor")
}

// A Codex-only host may never have created Claude's per-process session-state
// directory. The outer layer still hides that cross-harness protected root,
// and bubblewrap requires the target to exist before its read-only root is
// established. Preparing a Codex launch must therefore create the mountpoint
// without adding it to Codex's writable launch contract.
func TestPrepareTclaudeLayerHarnessStateCreatesCrossHarnessProtectedMountpoints(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := filepath.Join(home, "work")
	require.NoError(t, os.MkdirAll(cwd, 0o700))

	spec, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
		HarnessName: harness.CodexName,
		Cwd:         cwd,
	})
	require.NoError(t, err)
	claudeSessions := filepath.Join(home, ".claude", "sessions")
	assert.NoDirExists(t, claudeSessions)

	require.NoError(t, PrepareTclaudeLayerHarnessState(spec))
	assert.DirExists(t, claudeSessions)
	assert.NotContains(t, spec.Contract.WriteDirs, claudeSessions,
		"the host mountpoint must remain outside Codex's writable launch contract")
}

func TestBuildTclaudeLayerLaunchSpecMaterializesSharedContract(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("HOME", home)
	for _, name := range []string{
		"XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME",
	} {
		t.Setenv(name, "")
	}
	cwd := filepath.Join(home, "work")
	agentDir := filepath.Join(home, "agent-cache")
	privateParent := filepath.Join(home, "spawn-attachments")
	privateCurrent := filepath.Join(privateParent, "opencode-session")
	for _, path := range []string{
		cwd,
		agentDir,
		filepath.Join(home, ".opencode"),
		privateCurrent,
	} {
		require.NoError(t, os.MkdirAll(path, 0o700))
	}
	snapshot := sandboxpolicy.EmptySnapshot()
	snapshot.Effective.Filesystem = []sandboxpolicy.FilesystemGrant{
		{Path: home, Access: sandboxpolicy.AccessDeny},
		{Path: cwd, Access: sandboxpolicy.AccessWrite},
	}
	snapshot.Effective.AgentDirectories = []string{"AGENT_CACHE"}
	snapshot.Effective.Environment = []sandboxpolicy.EnvironmentEntry{{
		Name: "AGENT_CACHE", Value: agentDir,
	}}
	materializedSocket := filepath.Join(cwd, "build.sock")
	snapshot.UnixSocketMaterialization = &sandboxpolicy.UnixSocketMaterialization{
		Paths: []string{materializedSocket},
	}

	spec, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
		HarnessName:  harness.OpenCodeName,
		Cwd:          cwd,
		GitWriteDirs: []string{cwd},
		Snapshot:     &snapshot,
		PrivateWriteDirs: []TclaudeLayerPrivateWriteDir{{
			Parent:  privateParent,
			Current: privateCurrent,
		}},
	})
	require.NoError(t, err)
	require.NotNil(t, spec.Contract.MaterializedUnixSocketPaths)
	assert.Equal(t, []string{materializedSocket},
		*spec.Contract.MaterializedUnixSocketPaths)
	assert.Equal(t, TclaudeLayerLaunchSpecVersion, spec.Version)
	assert.Equal(t, filepath.Join(home, ".opencode"), spec.Contract.StateRoot)
	assert.Equal(t, []string{filepath.Join(home, ".opencode", "bin")},
		spec.Contract.ReadOnlyStateDirs)
	assert.ElementsMatch(t, []string{
		cwd,
		agentDir,
		filepath.Join(home, ".local", "share", "opencode"),
		filepath.Join(home, ".cache", "opencode"),
		filepath.Join(home, ".config", "opencode"),
		filepath.Join(home, ".local", "state", "opencode"),
	}, spec.Contract.WriteDirs)
	require.NoError(t, PrepareTclaudeLayerHarnessState(spec))
	for _, path := range spec.Contract.StateDirs {
		info, statErr := os.Stat(path)
		require.NoError(t, statErr)
		assert.True(t, info.IsDir())
	}
	for _, path := range spec.Contract.ReadOnlyStateDirs {
		info, statErr := os.Stat(path)
		require.NoError(t, statErr)
		assert.True(t, info.IsDir())
		access, covered := sandboxpolicy.EffectiveAccessAt(spec.Effective.Filesystem, path)
		assert.True(t, covered)
		assert.Equal(t, sandboxpolicy.AccessRead, access)
	}
	assert.Equal(t, snapshot.Effective.Filesystem, spec.Contract.ProfileFilesystem,
		"the authored launch-active rows stay separate from generated contract reopens")
	assert.Equal(t, []TclaudeLayerPrivateWriteDir{{
		Parent:  privateParent,
		Current: privateCurrent,
	}}, spec.Contract.PrivateWriteDirs)
	assert.Contains(t, spec.Effective.Filesystem, sandboxpolicy.FilesystemGrant{
		Path: cwd, Access: sandboxpolicy.AccessWrite,
	})
	cwdAccess, covered := sandboxpolicy.EffectiveAccessAt(spec.Effective.Filesystem, cwd)
	assert.True(t, covered)
	assert.Equal(t, sandboxpolicy.AccessWrite, cwdAccess,
		"the generated read reopen folds into the stronger exact write grant")
	assert.Contains(t, spec.Effective.Filesystem, sandboxpolicy.FilesystemGrant{
		Path: agentDir, Access: sandboxpolicy.AccessRead,
	})
	if runtime.GOOS == "linux" {
		wrapped, err := WrapTclaudeLayerServerSpec("/usr/bin/bwrap", spec, "opencode serve")
		require.NoError(t, err)
		stateBind := strings.Index(wrapped,
			"--bind "+clcommon.ShellQuoteArg(spec.Contract.StateRoot)+" "+
				clcommon.ShellQuoteArg(spec.Contract.StateRoot))
		binReadOnly := strings.Index(wrapped,
			"--ro-bind "+clcommon.ShellQuoteArg(spec.Contract.ReadOnlyStateDirs[0])+" "+
				clcommon.ShellQuoteArg(spec.Contract.ReadOnlyStateDirs[0]))
		require.GreaterOrEqual(t, stateBind, 0)
		require.GreaterOrEqual(t, binReadOnly, 0)
		assert.Less(t, stateBind, binReadOnly,
			"the executable subtree must be re-hardened after the mutable state bind")
		privateHide := strings.Index(wrapped,
			"--tmpfs "+clcommon.ShellQuoteArg(privateParent))
		privateReopen := strings.Index(wrapped,
			"--bind "+clcommon.ShellQuoteArg(privateCurrent)+" "+
				clcommon.ShellQuoteArg(privateCurrent))
		require.GreaterOrEqual(t, privateHide, 0)
		require.GreaterOrEqual(t, privateReopen, 0)
		assert.Less(t, privateHide, privateReopen,
			"the server executor sees only its own private attachment child")
	}
}

func TestBuildTclaudeLayerLaunchSpecBindsDarwinReservationToContract(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := filepath.Join(home, "work")
	require.NoError(t, os.MkdirAll(cwd, 0o700))
	reservation := &DarwinRouteSlotReservation{slots: []int{41301, 41302}}
	spec, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
		HarnessName:            harness.DefaultName,
		Cwd:                    cwd,
		DarwinRouteSlots:       []int{41301, 41302},
		DarwinRouteReservation: reservation,
	})
	require.NoError(t, err)
	assert.Equal(t, []int{41301, 41302}, spec.Contract.DarwinRouteSlots)
	assert.Same(t, reservation, spec.Contract.DarwinRouteReservation)
	_, err = BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
		HarnessName:            harness.DefaultName,
		Cwd:                    cwd,
		DarwinRouteSlots:       []int{41301},
		DarwinRouteReservation: reservation,
	})
	require.ErrorContains(t, err, "does not match")
}

func TestTclaudeLayerOpenCodeStateDirsRespectXDGRoots(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("HOME", home)
	for _, name := range []string{"DATA", "CACHE", "CONFIG", "STATE"} {
		t.Setenv("XDG_"+name+"_HOME", filepath.Join(home, strings.ToLower(name)))
	}
	dirs, err := tclaudeLayerOpenCodeStateDirs()
	require.NoError(t, err)
	assert.Equal(t, []string{
		filepath.Join(home, "data", "opencode"),
		filepath.Join(home, "cache", "opencode"),
		filepath.Join(home, "config", "opencode"),
		filepath.Join(home, "state", "opencode"),
	}, dirs)
}

func TestTclaudeLayerOpenCodeStateDirsResolveMissingLeafBelowSymlink(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("HOME", home)
	realCache := filepath.Join(home, "real-cache")
	require.NoError(t, os.MkdirAll(realCache, 0o700))
	cacheAlias := filepath.Join(home, "cache-alias")
	require.NoError(t, os.Symlink(realCache, cacheAlias))
	t.Setenv("XDG_CACHE_HOME", cacheAlias)

	dirs, err := tclaudeLayerOpenCodeStateDirs()
	require.NoError(t, err)
	assert.Contains(t, dirs, filepath.Join(realCache, "opencode"))
}

func TestBuildTclaudeLayerLaunchSpecFreezesGeneratedPathsThroughParentAlias(t *testing.T) {
	physicalHome := t.TempDir()
	aliasHome := filepath.Join(t.TempDir(), "home-alias")
	require.NoError(t, os.Symlink(physicalHome, aliasHome))
	t.Setenv("HOME", aliasHome)
	for _, name := range []string{
		"XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME",
	} {
		t.Setenv(name, "")
	}
	cwd := filepath.Join(physicalHome, "work")
	require.NoError(t, os.MkdirAll(cwd, 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(physicalHome, ".opencode", "bin"), 0o700))

	spec, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
		HarnessName: harness.OpenCodeName,
		Cwd:         cwd,
	})
	require.NoError(t, err)
	_, err = sandboxpolicy.RevalidateSnapshot(
		sandboxpolicy.NewSnapshot(spec.Effective, nil))
	require.NoError(t, err,
		"daemon-generated filesystem rows must be frozen before the spec is persisted")
}

func TestBuildTclaudeLayerLaunchSpecRefusesOpenCodeBinSymlinkOutsideState(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("HOME", home)
	for _, name := range []string{
		"XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME",
	} {
		t.Setenv(name, "")
	}
	stateRoot := filepath.Join(home, ".opencode")
	outside := filepath.Join(home, "outside-bin")
	cwd := filepath.Join(home, "work")
	for _, path := range []string{stateRoot, outside, cwd} {
		require.NoError(t, os.MkdirAll(path, 0o700))
	}
	require.NoError(t, os.Symlink(outside, filepath.Join(stateRoot, "bin")))

	_, err = BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
		HarnessName: harness.OpenCodeName,
		Cwd:         cwd,
	})
	require.ErrorContains(t, err, "resolves outside state root")
}

func TestValidateTclaudeLayerHarnessSupportsOpenCodeOnUnixSandboxHosts(t *testing.T) {
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		require.NoError(t, ValidateTclaudeLayerHarness(harness.OpenCodeName))
	} else {
		require.Error(t, ValidateTclaudeLayerHarness(harness.OpenCodeName))
	}
	require.NoError(t, ValidateTclaudeLayerHarness(harness.DefaultName))
	require.NoError(t, ValidateTclaudeLayerHarness(harness.CodexName))
	assert.False(t, tclaudeLayerWrapsPane(harness.OpenCodeName),
		"OpenCode attaches outside the boundary; agentd wraps its tool executor")
	assert.True(t, tclaudeLayerWrapsPane(harness.CodexName))
}

func TestTclaudeLayerVerdictRecordsPartialSocketFidelity(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux bubblewrap verdict; platform verdicts have build-tagged tests")
	}
	verdict := TclaudeLayerLaunchOSSandbox(
		sandboxpolicy.NetworkHostOpen, sandboxpolicy.RootHostInherited)
	assert.Equal(t, "on", verdict.State)
	assert.Equal(t, "tclaude-layer (bubblewrap; host network)", verdict.Source)
	assert.NotContains(t, verdict.Source, "ambient host Unix sockets reachable",
		"the socket caveat is the badge's partial-fidelity sentence to state, once")
	assert.True(t, verdict.Unverified, "the badge must render the partial-fidelity caveat")

	isolated := TclaudeLayerLaunchOSSandbox(
		sandboxpolicy.NetworkIsolatedWithAgentd, sandboxpolicy.RootConstructed)
	assert.Equal(t, "on", isolated.State)
	assert.Contains(t, isolated.Source, "isolated network")
	assert.Contains(t, isolated.Source, "host loopback/IDE bridge unavailable")
	assert.Contains(t, isolated.Source, "isolated PIDs")
	assert.Contains(t, isolated.Source, "agentd socket allowlisted")
	assert.False(t, isolated.Unverified, "constructed-root socket isolation has full fidelity")

	openCode := TclaudeLayerLaunchOSSandboxForHarness(
		harness.OpenCodeName, sandboxpolicy.NetworkHostOpen,
		sandboxpolicy.RootHostInherited, sandboxpolicy.NetworkEngineUnset)
	assert.Equal(t, "on", openCode.State)
	assert.Equal(t,
		"tclaude-layer (bubblewrap; OpenCode tool-executing server confined)",
		openCode.Source)
	assert.True(t, openCode.Unverified)
}

func TestValidateTclaudeLayerNetworkRequiresDescriptorAndExplicitTransportAssertion(t *testing.T) {
	claude := harness.Default()
	codex, err := harness.Resolve(harness.CodexName)
	require.NoError(t, err)
	openCode, err := harness.Resolve(harness.OpenCodeName)
	require.NoError(t, err)

	closed := sandboxpolicy.EffectiveProfile{NetworkAccess: sandboxpolicy.NetworkAccessNone}
	_, validateErr := ValidateTclaudeLayerNetwork(openCode, closed, harness.ResolvedModelTransport{})
	require.ErrorContains(t, validateErr, "requires hosted model traffic")
	_, validateErr = ValidateTclaudeLayerNetwork(claude, closed, harness.ResolvedModelTransport{})
	require.ErrorContains(t, validateErr, "requires hosted model traffic")
	_, validateErr = ValidateTclaudeLayerNetwork(codex, closed, harness.ResolvedModelTransport{})
	require.ErrorContains(t, validateErr, sandboxpolicy.OfflineModelTransportEnv+"=1")

	closed.Environment = []sandboxpolicy.EnvironmentEntry{{Name: sandboxpolicy.OfflineModelTransportEnv, Value: "0"}}
	_, validateErr = ValidateTclaudeLayerNetwork(codex, closed, harness.ResolvedModelTransport{})
	require.ErrorContains(t, validateErr, sandboxpolicy.OfflineModelTransportEnv+"=1")
	closed.Environment[0].Value = "1"
	_, validateErr = ValidateTclaudeLayerNetwork(codex, closed, harness.ResolvedModelTransport{})
	require.NoError(t, validateErr)

	_, validateErr = ValidateTclaudeLayerNetwork(claude, sandboxpolicy.EffectiveProfile{
		NetworkAccess: sandboxpolicy.NetworkAccessInternet,
	}, harness.ResolvedModelTransport{})
	require.NoError(t, validateErr)
	_, validateErr = ValidateTclaudeLayerNetwork(openCode, sandboxpolicy.EffectiveProfile{
		NetworkAccess: sandboxpolicy.NetworkAccessInternet,
	}, harness.ResolvedModelTransport{})
	require.NoError(t, validateErr)
}

func TestValidateTclaudeLayerFilteredNetworkRequiresHonestModelResolution(t *testing.T) {
	claude := harness.Default()
	effective := sandboxpolicy.EffectiveProfile{
		Network: &sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.NetworkAllowEntry{{
				Domain: "api.anthropic.com",
				Ports:  []int{443},
			}},
		},
	}

	_, err := ValidateTclaudeLayerNetwork(
		claude, effective, harness.ResolvedModelTransport{Model: "claude-sonnet"})
	require.ErrorContains(t, err, "model provider configuration was not resolved")
	require.ErrorContains(t, err, "use network open")

	notices, err := ValidateTclaudeLayerNetwork(claude, effective, harness.ResolvedModelTransport{
		Model:            "claude-sonnet",
		Provider:         "anthropic",
		ProviderResolved: true,
	})
	require.NoError(t, err)
	require.Len(t, notices, 1)
	assert.Contains(t, notices[0].Detail, "no hidden model-traffic bypass")
	assert.Contains(t, notices[0].Detail, "empirically audited")
	assert.Contains(t, notices[0].Detail, "Claude Code 2.1.220")
	assert.Contains(t, notices[0].Detail, "Codex CLI 0.145.0")
	assert.Contains(t, notices[0].Detail, "M2c")
	assert.Contains(t, notices[0].Detail, "remote managed settings")
	assert.Contains(t, notices[0].Detail, "applies in-process")
	assert.Contains(t, notices[0].Detail, "denied fail-closed for new flows")

	custom := sandboxpolicy.EffectiveProfile{
		Network: &sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.NetworkAllowEntry{{
				Host: "gateway.example", Ports: []int{443},
			}},
		},
	}
	notices, err = ValidateTclaudeLayerNetwork(
		claude, custom, harness.ResolvedModelTransport{
			Model:            "claude-sonnet",
			Provider:         "anthropic",
			BaseURL:          "https://gateway.example/v1",
			ProviderResolved: true,
		})
	require.NoError(t, err)
	require.Len(t, notices, 1)
	assert.Contains(t, notices[0].Detail, "remote managed settings")
	assert.Contains(t, notices[0].Detail, "denied fail-closed for new flows")

	openCode, resolveErr := harness.Resolve(harness.OpenCodeName)
	require.NoError(t, resolveErr)
	notices, err = ValidateTclaudeLayerNetwork(
		openCode, custom, harness.ResolvedModelTransport{
			Model:            "test/test-model",
			Provider:         "test",
			BaseURL:          "https://gateway.example/v1",
			ProviderResolved: true,
		})
	require.NoError(t, err)
	require.Len(t, notices, 1)
	assert.Contains(t, notices[0].Detail, "explicit-provider configs only")
	assert.Contains(t, notices[0].Detail, "OPENCODE_CONFIG_CONTENT")
	assert.Contains(t, notices[0].Detail, "without a provider override")
	assert.Contains(t, notices[0].Detail, "read-only, provider-empty private XDG and HOME")
	assert.Contains(t, notices[0].Detail, "before every initial exec or restart")
	assert.Contains(t, notices[0].Detail, "persistent account/org authority")
	assert.Contains(t, notices[0].Detail, "soft tool policy")
	assert.Contains(t, notices[0].Detail, "packet-enforced floor")
}

func TestValidateTclaudeLayerDefaultAllowDenyPreflightsModelTransport(t *testing.T) {
	claude := harness.Default()
	effective := sandboxpolicy.EffectiveProfile{
		Network: &sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeOpen,
			Deny: []sandboxpolicy.NetworkAllowEntry{{
				Domain: "blocked.example", Ports: []int{443},
			}},
		},
	}
	resolved := harness.ResolvedModelTransport{
		Model: "claude-sonnet", Provider: "anthropic", ProviderResolved: true,
	}
	notices, err := ValidateTclaudeLayerNetwork(claude, effective, resolved)
	require.NoError(t, err)
	require.Len(t, notices, 1)
	assert.Equal(t, sandboxpolicy.AccessNoticeReasonFilteredModelTraffic,
		notices[0].Reason)
	assert.Equal(t, sandboxpolicy.AccessNoticeEffectLaunchGated,
		notices[0].Effect)
	assert.Contains(t, notices[0].Detail,
		"default allow with explicit deny rules")
	assert.Contains(t, notices[0].Detail, "api.anthropic.com:443")
	assert.Contains(t, notices[0].Detail, "shared IP and port boundary")

	effective.Network.Deny[0] = sandboxpolicy.NetworkAllowEntry{
		Domain: "api.anthropic.com", Ports: []int{443},
	}
	_, err = ValidateTclaudeLayerNetwork(claude, effective, resolved)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "deny rules block required model destinations")
	assert.Contains(t, err.Error(), "api.anthropic.com:443")
}

func TestValidateTclaudeLayerLocalNetworkPresetsKeepModelTransportExplicit(t *testing.T) {
	strictLocal := sandboxpolicy.EffectiveProfile{
		Network: &sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.NetworkAllowEntry{{
				Loopback: true,
			}},
		},
	}
	_, err := ValidateTclaudeLayerNetwork(
		harness.Default(), strictLocal, harness.ResolvedModelTransport{
			Model: "claude-sonnet", Provider: "anthropic", ProviderResolved: true,
		})
	require.Error(t, err)
	var capabilityErr *harness.SandboxCapabilityError
	require.ErrorAs(t, err, &capabilityErr)
	assert.Equal(t, harness.SandboxCapabilityModelTransport, capabilityErr.Kind)
	assert.Contains(t, err.Error(), "api.anthropic.com:443")
	assert.Contains(t, err.Error(), "include template net-anthropic")
	assert.Contains(t, err.Error(), "no hidden model-traffic bypass")

	localBaseURL := "http://host.tclaude.internal:11434/v1"
	localEndpoint := "host.tclaude.internal:11434"
	if runtime.GOOS == "darwin" {
		localBaseURL = "http://localhost:11434/v1"
		localEndpoint = "localhost:11434"
	}
	notices, err := ValidateTclaudeLayerNetwork(
		harness.Default(), strictLocal, harness.ResolvedModelTransport{
			Model:            "local/llama",
			Provider:         "ollama",
			BaseURL:          localBaseURL,
			ProviderResolved: true,
		})
	require.NoError(t, err)
	require.Len(t, notices, 1)
	assert.Equal(t, sandboxpolicy.AccessNoticeEffectLaunchGated, notices[0].Effect)
	assert.Equal(t, sandboxpolicy.AccessNoticeReasonFilteredModelTraffic, notices[0].Reason)
	assert.Contains(t, notices[0].Detail, localEndpoint)
	for _, localHarness := range []*harness.Harness{
		harness.MustGet(harness.CodexName),
		harness.MustGet(harness.OpenCodeName),
	} {
		got, localErr := ValidateTclaudeLayerNetwork(
			localHarness, strictLocal, harness.ResolvedModelTransport{
				Model:            "local/llama",
				Provider:         "ollama",
				BaseURL:          localBaseURL,
				ProviderResolved: true,
			})
		require.NoError(t, localErr)
		require.Len(t, got, 1)
		assert.Contains(t, got[0].Detail, localEndpoint)
	}

	localModelAPIs := sandboxpolicy.EffectiveProfile{
		Network: &sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.NetworkAllowEntry{
				{Loopback: true},
				{Domain: "api.anthropic.com", Ports: []int{443}},
				{Domain: "api.openai.com", Ports: []int{443}},
			},
		},
	}
	for _, tc := range []struct {
		name      string
		harness   *harness.Harness
		transport harness.ResolvedModelTransport
		endpoint  string
	}{
		{
			name: "claude", harness: harness.Default(),
			transport: harness.ResolvedModelTransport{
				Model: "claude-sonnet", Provider: "anthropic", ProviderResolved: true,
			},
			endpoint: "api.anthropic.com:443",
		},
		{
			name: "codex", harness: harness.MustGet(harness.CodexName),
			transport: harness.ResolvedModelTransport{
				Model: "gpt-5.4", Provider: "openai", ProviderResolved: true,
			},
			endpoint: "api.openai.com:443",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, validateErr := ValidateTclaudeLayerNetwork(
				tc.harness, localModelAPIs, tc.transport)
			require.NoError(t, validateErr)
			require.Len(t, got, 1)
			assert.Equal(t, sandboxpolicy.AccessNoticeEffectLaunchGated, got[0].Effect)
			assert.Contains(t, got[0].Detail, tc.endpoint)
			assert.Contains(t, got[0].Detail, "no hidden model-traffic bypass")
		})
	}
}

func TestModelTransportLoopbackInterpretationMatchesPlatform(t *testing.T) {
	strictLocal := sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{{
			Loopback: true,
		}},
	}
	for _, baseURL := range []string{
		"http://127.0.0.1:11434/v1",
		"http://[::1]:11434/v1",
		"http://localhost:11434/v1",
	} {
		resolved := harness.ResolvedModelTransport{
			Model:            "local/llama",
			Provider:         "ollama",
			BaseURL:          baseURL,
			ProviderResolved: true,
		}
		linuxErr := validateModelTransportLoopbackForPlatform(
			harness.Default(), resolved, "linux",
			sandboxpolicy.NetworkEngineUnset)
		require.Error(t, linuxErr)
		assert.Contains(t, linuxErr.Error(), "sandbox-private localhost on Linux")
		assert.Contains(t, linuxErr.Error(), sandboxpolicy.FilteredNetworkHostLoopbackName)

		require.NoError(t, validateModelTransportLoopbackForPlatform(
			harness.Default(), resolved, "darwin",
			sandboxpolicy.NetworkEngineUnset))
		requirement, err := harness.ResolveModelTransportRequirement(
			harness.Default(), resolved)
		require.NoError(t, err)
		assert.Equal(t, []sandboxpolicy.NetworkAllowEntry{{
			Loopback: true, Ports: []int{11434},
		}}, requirement.Destinations)
		require.NoError(t, harness.ValidateModelTransportCoverage(
			harness.Default(), strictLocal, requirement))
		detail := describeModelTransportRequirementForPlatform(
			strictLocal, requirement, "darwin")
		assert.Contains(t, detail, "localhost:11434")
		assert.NotContains(t, detail, sandboxpolicy.FilteredNetworkHostLoopbackName)
	}

	synthetic := harness.ResolvedModelTransport{
		Model:            "local/llama",
		Provider:         "ollama",
		BaseURL:          "http://host.tclaude.internal:11434/v1",
		ProviderResolved: true,
	}
	require.NoError(t, validateModelTransportLoopbackForPlatform(
		harness.Default(), synthetic, "linux",
		sandboxpolicy.NetworkEngineUnset))
	darwinErr := validateModelTransportLoopbackForPlatform(
		harness.Default(), synthetic, "darwin",
		sandboxpolicy.NetworkEngineUnset)
	require.Error(t, darwinErr)
	assert.Contains(t, darwinErr.Error(), "Linux-only synthetic")
	assert.Contains(t, darwinErr.Error(), "localhost")
	assert.Contains(t, darwinErr.Error(), "127.0.0.1")
	assert.Contains(t, darwinErr.Error(), "::1")
}

func TestBwrapArgsConstructsIsolatedRootAndRepairsAgentdSocket(t *testing.T) {
	home := agentipctest.ShortSocketDir(t)
	t.Setenv("HOME", home)
	t.Setenv(agentipc.SocketEnv, "")
	socket := agentipc.CanonicalSocketPath()
	for _, floorSocket := range sandboxpolicy.AgentdSocketFloor() {
		require.NoError(t, os.MkdirAll(filepath.Dir(floorSocket), 0o700))
		listener, listenErr := net.Listen("unix", floorSocket)
		require.NoError(t, listenErr)
		t.Cleanup(func() { _ = listener.Close() })
	}
	policySocket := filepath.Join(home, "runtime", "build.sock")
	require.NoError(t, os.MkdirAll(filepath.Dir(policySocket), 0o700))
	policyListener, err := net.Listen("unix", policySocket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = policyListener.Close() })

	stateRoot := filepath.Join(home, ".codex")
	workspace := filepath.Join(home, "work")
	require.NoError(t, os.MkdirAll(stateRoot, 0o700))
	require.NoError(t, os.MkdirAll(workspace, 0o700))
	plan := sandboxpolicy.MountPlan{
		NetworkPosture: sandboxpolicy.NetworkIsolatedWithAgentd,
		Entries: []sandboxpolicy.MountEntry{
			{Path: home, Mode: sandboxpolicy.MountHide},
			{Path: workspace, Mode: sandboxpolicy.MountRW},
		},
	}
	socketPaths := append(sandboxpolicy.AgentdSocketFloor(), policySocket)
	got, err := bwrapArgsWithDaemonFinal(
		[]string{stateRoot, workspace}, plan, nil, nil, nil, socketPaths, "", nil)
	require.NoError(t, err)

	assert.Contains(t, got, "--unshare-net")
	assert.Contains(t, got, "--unshare-pid")
	assert.NotContains(t, got, "--as-pid-1",
		"bubblewrap must remain PID 1 so orphaned harness subprocesses are reaped")
	rootTmpfs := indexOfBwrapTriplet(got, "--tmpfs", "/")
	rootRemount := indexOfBwrapTriplet(got, "--remount-ro", "/")
	require.NotEqual(t, -1, rootTmpfs)
	require.NotEqual(t, -1, rootRemount)
	assert.Equal(t, -1, indexOfBwrapTriplet(got, "--ro-bind", "/"),
		"isolated posture must not blanket-bind the host root")
	for _, forbidden := range []string{"/run", "/var", "/srv", "/media", "/mnt", "/boot", "/root"} {
		assert.NotContains(t, got, forbidden, "constructed root must not expose ambient runtime path %s", forbidden)
	}
	assert.NotEqual(t, -1, indexOfBwrapTriplet(got, "--ro-bind", "/usr"),
		"constructed root must expose the static /usr surface")

	homeHide := indexOfBwrapTriplet(got, "--tmpfs", home)
	homeRemount := indexOfBwrapTriplet(got, "--remount-ro", home)
	stateRepairs := indicesOfBwrapTriplet(got, "--bind", stateRoot)
	socketBinds := indicesOfBwrapTriplet(got, "--ro-bind", socket)
	require.NotEqual(t, -1, homeHide)
	require.NotEqual(t, -1, homeRemount)
	require.Len(t, stateRepairs, 2, "class-1 state root must survive an ordinary ancestor deny")
	require.Len(t, socketBinds, 2,
		"agentd socket must be rebound after an ordinary ancestor deny")
	assert.Less(t, homeHide, stateRepairs[1])
	assert.Less(t, homeHide, socketBinds[1])
	assert.Less(t, socketBinds[1], rootRemount,
		"all explicit child mounts must land before the constructed root becomes read-only")
	assert.Less(t, stateRepairs[1], homeRemount)
	assert.Less(t, socketBinds[1], homeRemount,
		"the child socket bind must land before its hidden parent becomes read-only")
	for _, floorSocket := range sandboxpolicy.AgentdSocketFloor() {
		binds := indicesOfBwrapTriplet(got, "--ro-bind", floorSocket)
		require.Lenf(t, binds, 2, "every live agentd socket spelling must survive the ancestor deny: %s", floorSocket)
		assert.Less(t, binds[1], rootRemount)
	}
	require.Len(t, indicesOfBwrapTriplet(got, "--ro-bind", policySocket), 2,
		"an authored socket must be bound and repaired beneath the ancestor deny")
	require.NoError(t, policyListener.Close())
	_ = os.Remove(policySocket)
	_, err = bwrapArgsWithDaemonFinal(
		[]string{stateRoot, workspace}, plan, nil, nil, nil, socketPaths, "", nil)
	require.ErrorContains(t, err, "disappeared before the tclaude-layer adapter rendered it",
		"a changed post-materialization surface must refuse instead of diverging from disclosure")

	tmuxSocketDir, err := clcommon.TclaudeTmuxSocketDir()
	require.NoError(t, err)
	assert.Equal(t, len(got)-4, indexOfBwrapTriplet(got, "--tmpfs", tmuxSocketDir),
		"class-4 tmux hide remains final under the constructed root")
}

// TCL-798. The whole point of the new posture is that these two decisions are
// now independent, so the assertions come in pairs: everything the constructed
// root does, and the network namespace still being the host's.
func TestBwrapArgsConstructsHostOpenRootWithoutUnsharingNetwork(t *testing.T) {
	home := agentipctest.ShortSocketDir(t)
	t.Setenv("HOME", home)
	t.Setenv(agentipc.SocketEnv, "")
	socket := agentipc.CanonicalSocketPath()
	for _, floorSocket := range sandboxpolicy.AgentdSocketFloor() {
		require.NoError(t, os.MkdirAll(filepath.Dir(floorSocket), 0o700))
		listener, listenErr := net.Listen("unix", floorSocket)
		require.NoError(t, listenErr)
		t.Cleanup(func() { _ = listener.Close() })
	}
	policySocket := filepath.Join(home, "runtime", "build.sock")
	require.NoError(t, os.MkdirAll(filepath.Dir(policySocket), 0o700))
	policyListener, err := net.Listen("unix", policySocket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = policyListener.Close() })
	workspace := filepath.Join(home, "work")
	require.NoError(t, os.MkdirAll(workspace, 0o700))

	plan := sandboxpolicy.MountPlan{
		NetworkPosture: sandboxpolicy.NetworkHostOpen,
		RootPosture:    sandboxpolicy.RootConstructed,
		Entries: []sandboxpolicy.MountEntry{
			{Path: home, Mode: sandboxpolicy.MountHide},
			{Path: workspace, Mode: sandboxpolicy.MountRW},
		},
	}
	socketPaths := append(sandboxpolicy.AgentdSocketFloor(), policySocket)
	got, err := bwrapArgsWithDaemonFinal(
		[]string{workspace}, plan, nil, nil, nil, socketPaths, "", nil)
	require.NoError(t, err)

	assert.NotContains(t, got, "--unshare-net",
		"host IP networking is exactly what this posture preserves")
	assert.NotContains(t, got, "--unshare-user")
	assert.Contains(t, got, "--unshare-pid",
		"without a PID namespace a host process's /proc/<pid>/root reopens every hidden socket")
	rootTmpfs := indexOfBwrapTriplet(got, "--tmpfs", "/")
	rootRemount := indexOfBwrapTriplet(got, "--remount-ro", "/")
	require.NotEqual(t, -1, rootTmpfs)
	require.NotEqual(t, -1, rootRemount)
	assert.Equal(t, -1, indexOfBwrapTriplet(got, "--ro-bind", "/"),
		"a constructed root must not also blanket-bind the host root")
	assert.NotEqual(t, -1, indexOfBwrapTriplet(got, "--ro-bind", "/usr"),
		"the static OS surface is what makes the constructed root usable")

	socketBinds := indicesOfBwrapTriplet(got, "--ro-bind", socket)
	require.Len(t, socketBinds, 2,
		"the agentd floor must survive an ordinary ancestor deny here too")
	assert.Less(t, socketBinds[1], rootRemount)
	require.Len(t, indicesOfBwrapTriplet(got, "--ro-bind", policySocket), 2,
		"an authored socket must be bound and repaired beneath the ancestor deny")

	// Mutation guard: with the root posture back at its zero value the same
	// plan must render the pre-TCL-798 host-open arguments, so no assertion
	// above can be satisfied by making the constructed root unconditional.
	plan.RootPosture = sandboxpolicy.RootHostInherited
	inherited, err := bwrapArgsWithDaemonFinal(
		[]string{workspace}, plan, nil, nil, nil, socketPaths, "", nil)
	require.NoError(t, err)
	assert.NotEqual(t, -1, indexOfBwrapTriplet(inherited, "--ro-bind", "/"))
	assert.NotContains(t, inherited, "--unshare-pid")
	assert.Equal(t, -1, indexOfBwrapTriplet(inherited, "--ro-bind", "/usr"))
	assert.Empty(t, indicesOfBwrapTriplet(inherited, "--ro-bind", policySocket),
		"an inherited root has no constructed root to allowlist sockets into")
}

func TestBwrapArgsIsolatedAliasesRespectHideAndRemountOrdering(t *testing.T) {
	home, err := filepath.EvalSymlinks(agentipctest.ShortSocketDir(t))
	require.NoError(t, err)
	t.Setenv("HOME", home)
	t.Setenv(agentipc.SocketEnv, "")
	socket := agentipc.CanonicalSocketPath()
	require.NoError(t, os.MkdirAll(filepath.Dir(socket), 0o700))
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	fixture := filepath.Join(home, "fixture")
	link := filepath.Join(fixture, "alias")
	target := filepath.Join(fixture, "real")
	plan := sandboxpolicy.MountPlan{
		NetworkPosture: sandboxpolicy.NetworkIsolatedWithAgentd,
		Aliases: []sandboxpolicy.MountAlias{{
			Link: link, Target: target,
		}},
		Entries: []sandboxpolicy.MountEntry{
			{Path: fixture, Mode: sandboxpolicy.MountHide},
			{Path: target, Mode: sandboxpolicy.MountHide},
		},
	}
	got, err := bwrapArgs(nil, plan)
	require.NoError(t, err)

	aliases := indicesOfBwrapSymlink(got, target, link)
	require.Len(t, aliases, 2,
		"the constructed root gets an initial alias and an ordinary-hide repair")
	scratch := indexOfBwrapTriplet(got, "--tmpfs", "/tmp")
	fixtureHide := indexOfBwrapTriplet(got, "--tmpfs", fixture)
	targetHide := indexOfBwrapTriplet(got, "--tmpfs", target)
	fixtureRemount := indexOfBwrapTriplet(got, "--remount-ro", fixture)
	targetRemount := indexOfBwrapTriplet(got, "--remount-ro", target)
	rootRemount := indexOfBwrapTriplet(got, "--remount-ro", "/")
	staticBind := indexOfBwrapTriplet(got, "--ro-bind", "/usr")
	for _, index := range []int{scratch, fixtureHide, targetHide, fixtureRemount, targetRemount, rootRemount, staticBind} {
		require.NotEqual(t, -1, index)
	}
	assert.Less(t, scratch, aliases[0],
		"aliases under /tmp must be created in the fresh scratch mount")
	assert.Less(t, aliases[0], fixtureHide,
		"initial aliases precede host binds and plan replay")
	assert.Less(t, aliases[0], staticBind,
		"initial aliases precede the constructed root's host binds")
	assert.Less(t, fixtureHide, aliases[1],
		"an ordinary ancestor hide must recreate its narrower alias")
	assert.Less(t, aliases[1], targetHide,
		"hiding the resolved target after alias repair must still block traversal")
	for _, remount := range []int{fixtureRemount, targetRemount, rootRemount} {
		assert.Less(t, aliases[1], remount,
			"every alias repair must land before TCL-758's read-only flush")
	}

	hostOpen, err := bwrapArgs(nil, sandboxpolicy.MountPlan{
		Aliases: plan.Aliases,
	})
	require.NoError(t, err)
	assert.Empty(t, indicesOfBwrapSymlink(hostOpen, target, link),
		"host-open inherits the real host symlink from its read-only root")
}

func TestBwrapArgsProtectedHideBeatsAliasRepair(t *testing.T) {
	home, err := filepath.EvalSymlinks(agentipctest.ShortSocketDir(t))
	require.NoError(t, err)
	t.Setenv("HOME", home)
	t.Setenv(agentipc.SocketEnv, "")
	socket := agentipc.CanonicalSocketPath()
	require.NoError(t, os.MkdirAll(filepath.Dir(socket), 0o700))
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	protectedRoots, err := sandboxpolicy.ProtectedPaths()
	require.NoError(t, err)
	link := filepath.Join(protectedRoots[0], "alias")
	target := filepath.Join(home, "safe-target")
	got, err := bwrapArgs(nil, sandboxpolicy.MountPlan{
		NetworkPosture: sandboxpolicy.NetworkIsolatedWithAgentd,
		Aliases:        []sandboxpolicy.MountAlias{{Link: link, Target: target}},
		Entries: []sandboxpolicy.MountEntry{{
			Path: home, Mode: sandboxpolicy.MountHide,
		}},
	})
	require.NoError(t, err)

	aliases := indicesOfBwrapSymlink(got, target, link)
	require.Len(t, aliases, 2)
	protectedHides := indicesOfBwrapTriplet(got, "--tmpfs", protectedRoots[0])
	require.GreaterOrEqual(t, len(protectedHides), 2)
	assert.Less(t, aliases[0], protectedHides[0],
		"the protected baseline shadows the initial alias")
	assert.Less(t, aliases[1], protectedHides[len(protectedHides)-1],
		"repairing an ordinary ancestor must immediately restore the protected hide")
	assert.Equal(t, len(got)-4, indexOfBwrapTriplet(got, "--tmpfs", mustTmuxSocketDir(t)),
		"class-4 host control remains the final phase after every alias")
}

func TestBwrapArgsRequiresCompiledFilteredPolicy(t *testing.T) {
	_, err := bwrapArgs(nil, sandboxpolicy.MountPlan{
		NetworkPosture: sandboxpolicy.NetworkFiltered,
	})
	require.ErrorContains(t, err, "no compiled gateway policy")
}

func TestBwrapArgsFilteredBootstrapUsesNamespaceRootIdentity(t *testing.T) {
	home := agentipctest.ShortSocketDir(t)
	t.Setenv("HOME", home)
	t.Setenv(agentipc.SocketEnv, "")
	socket := agentipc.CanonicalSocketPath()
	require.NoError(t, os.MkdirAll(filepath.Dir(socket), 0o700))
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	policySocket := filepath.Join(home, "runtime", "build.sock")
	require.NoError(t, os.MkdirAll(filepath.Dir(policySocket), 0o700))
	policyListener, err := net.Listen("unix", policySocket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = policyListener.Close() })
	ir, err := sandboxpolicy.CompileFilteredNetworkRules(sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{{
			CIDR: "192.0.2.0/24", Ports: []int{443},
		}},
	})
	require.NoError(t, err)

	socketPaths := append([]string{}, sandboxpolicy.AgentdSocketFloor()...)
	socketPaths = append(socketPaths, policySocket)
	args, err := bwrapArgsWithDaemonFinal(
		nil,
		sandboxpolicy.MountPlan{
			NetworkPosture:  sandboxpolicy.NetworkFiltered,
			FilteredNetwork: &ir,
		},
		nil,
		nil,
		nil,
		socketPaths,
		"",
		nil,
	)
	require.NoError(t, err)
	assert.Contains(t, strings.Join(args, " "),
		"--unshare-user --uid 0 --gid 0 --unshare-net --unshare-pid")
	runMount := indexOfBwrapTriplet(args, "--tmpfs", "/run")
	socketBind := indexOfBwrapTriplet(args, "--ro-bind", policySocket)
	rootRemount := indexOfBwrapTriplet(args, "--remount-ro", "/")
	require.NotEqual(t, -1, runMount)
	require.NotEqual(t, -1, socketBind)
	require.NotEqual(t, -1, rootRemount)
	assert.Less(t, runMount, socketBind,
		"the private runtime filesystem must predate authored Unix-socket binds")
	assert.Less(t, runMount, rootRemount,
		"the private resolver runtime filesystem must predate root sealing")
	assert.Equal(t, -1, indexOfBwrapTriplet(args, "--ro-bind", "/run"),
		"filtered root must never expose ambient host runtime state")
}

// TCLAUDE_IGNORE_HOOKS is the blanket soft-disable used by callers that
// must not generate hook traffic at all — agentd's plugin shells, seance
// steps, and `tclaude task` subprocesses. It is NOT how tclaude-layer
// launches are handled any more (TCL-754 brokers those through agentd
// instead), so this pins the switch on its own terms.
func TestStatusCallbackSoftDisablesUnderIgnoreHooks(t *testing.T) {
	t.Setenv("TCLAUDE_IGNORE_HOOKS", "1")
	require.NoError(t, runStatusCallback(&StatusCallbackParams{Status: StatusWorking}))
}

func indexOfBwrapTriplet(args []string, flag, path string) int {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == path {
			return i
		}
	}
	return -1
}

func indicesOfBwrapTriplet(args []string, flag, path string) []int {
	var indices []int
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == path {
			indices = append(indices, i)
		}
	}
	return indices
}

func indicesOfBwrapSymlink(args []string, target, link string) []int {
	var indices []int
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--symlink" && args[i+1] == target && args[i+2] == link {
			indices = append(indices, i)
		}
	}
	return indices
}

func mustTmuxSocketDir(t *testing.T) string {
	t.Helper()
	path, err := clcommon.TclaudeTmuxSocketDir()
	require.NoError(t, err)
	return path
}

func bwrapFilesystemArgsWithin(args []string, root string) []string {
	var filtered []string
	for i, arg := range args {
		switch arg {
		case "--bind", "--ro-bind":
			if i+2 < len(args) && sandboxpolicy.PathContainsOrEqual(root, args[i+2]) {
				filtered = append(filtered, args[i:i+3]...)
			}
		case "--tmpfs", "--remount-ro", "--dir":
			if i+1 < len(args) && sandboxpolicy.PathContainsOrEqual(root, args[i+1]) {
				filtered = append(filtered, args[i:i+2]...)
			}
		}
	}
	return filtered
}

func TestBwrapArgsRejectInvalidEntry(t *testing.T) {
	_, err := bwrapArgs(nil, sandboxpolicy.MountPlan{Entries: []sandboxpolicy.MountEntry{
		{Path: "relative", Mode: sandboxpolicy.MountRW},
	}})
	require.ErrorContains(t, err, "non-absolute")

	_, err = bwrapArgs(nil, sandboxpolicy.MountPlan{Entries: []sandboxpolicy.MountEntry{
		{Path: "/work", Mode: sandboxpolicy.MountMode(99)},
	}})
	require.ErrorContains(t, err, "invalid mode")
}

func TestBwrapArgsZeroModeFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "uninitialized-entry")
	got, err := bwrapArgs(nil, sandboxpolicy.MountPlan{Entries: []sandboxpolicy.MountEntry{
		{Path: path},
	}})
	require.NoError(t, err)
	hide := indexOfBwrapTriplet(got, "--tmpfs", path)
	remount := indexOfBwrapTriplet(got, "--remount-ro", path)
	require.NotEqual(t, -1, hide)
	require.NotEqual(t, -1, remount)
	assert.Less(t, hide, remount)
}

func TestBwrapCommandShellQuotesHarnessCommand(t *testing.T) {
	got, err := bwrapCommand("/usr/bin/bwrap", nil, nil, nil, nil, nil,
		sandboxpolicy.MountPlan{}, "export X='a b'; exec agent --flag")
	require.NoError(t, err)
	assert.Contains(t, got, sandboxExecShellPrefix())
	assert.Contains(t, got, "export X=")
	assert.Contains(t, got, "--new-session")
}

// TestLaunchContractDoesNotReachProtectedRootPath is the enumeration the
// protected-root invariant needs in order to be checkable rather than merely
// asserted (TCL-791, required by the sandbox-v2 lead before merge).
//
// The invariant is absolute for POLICY input: no profile, include,
// acknowledgement, flag, or daemon-authored launch-contract entry reaches a
// protected root. The per-session spawn-attachment drop-box is daemon-owned
// and path-derived from the session identity, but now lives in the
// agent-reachable API tree rather than making an exception below data/.
//
// Prose cannot guard that. This test enumerates EVERY bwrap operation that
// makes host content visible inside the sandbox at or below a protected root
// and asserts the resulting set is empty.
func TestLaunchContractDoesNotReachProtectedRootPath(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("HOME", home)
	workspace := filepath.Join(home, "work")
	require.NoError(t, os.MkdirAll(workspace, 0o700))
	for _, relative := range []string{
		filepath.Join(".tclaude", "data"),
		filepath.Join(".claude", "sessions"),
	} {
		require.NoError(t, os.MkdirAll(filepath.Join(home, relative), 0o700))
	}
	protected, err := sandboxpolicy.ProtectedPaths()
	require.NoError(t, err)
	require.NotEmpty(t, protected)

	// The real drop-box paths must stay outside protected state while remaining
	// part of the launch contract.
	dropBoxParent := common.SpawnAttachmentsPrivateBase()
	dropBoxChild := common.SpawnAttachmentsPrivateDir("session-under-test")
	require.NoError(t, os.MkdirAll(dropBoxChild, 0o700))
	require.Falsef(t, underAnyProtectedRoot(protected, dropBoxChild),
		"the agent-facing drop-box %s must not sit under protected state", dropBoxChild)

	// The widest legal policy shape: deny the ancestor, reopen an ordinary
	// child. Built through Normalize and RenderMountPlan because that is the
	// path a reintroduced reopen would have to travel.
	normalized, err := sandboxpolicy.Normalize(sandboxpolicy.Profile{
		Name: "deny-home-reopen-work",
		Filesystem: []sandboxpolicy.FilesystemGrant{
			{Path: home, Access: sandboxpolicy.AccessDeny},
			{Path: workspace, Access: sandboxpolicy.AccessWrite},
		},
	})
	require.NoError(t, err)
	plan, err := sandboxpolicy.RenderMountPlan(sandboxpolicy.EffectiveProfile{
		Filesystem: normalized.Filesystem,
	})
	require.NoError(t, err)

	args, err := bwrapArgs([]string{workspace}, plan, TclaudeLayerPrivateWriteDir{
		Parent: dropBoxParent, Current: dropBoxChild,
	})
	require.NoError(t, err)

	// --bind and --ro-bind are the only forms that expose host content. --tmpfs
	// and --dir hide or create empty state; --remount-ro narrows.
	reached := map[string]string{}
	for i := 0; i+2 < len(args); i++ {
		flag, dst := args[i], args[i+2]
		if flag != "--bind" && flag != "--ro-bind" {
			continue
		}
		if underAnyProtectedRoot(protected, dst) {
			reached[dst] = flag
		}
	}

	assert.Empty(t, reached,
		"no bind may reach protected state through the launch contract")
	assert.Contains(t, args, dropBoxChild,
		"the drop-box child must still be present in the launch contract")
}

func underAnyProtectedRoot(protected []string, path string) bool {
	for _, root := range protected {
		if sandboxpolicy.PathContainsOrEqual(root, path) {
			return true
		}
	}
	return false
}

func TestTclaudeLayerNetworkDisclosesCodexRemoteProviderRouting(t *testing.T) {
	codex, err := harness.Resolve(harness.CodexName)
	require.NoError(t, err)
	effective := sandboxpolicy.EffectiveProfile{
		Network: &sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.NetworkAllowEntry{
				{Domain: "chatgpt.com", Ports: []int{443}},
				{Domain: "auth.openai.com", Ports: []int{443}},
			},
		},
	}

	notices, err := ValidateTclaudeLayerNetwork(
		codex, effective, harness.ResolvedModelTransport{
			Model:             "gpt-5.4",
			Provider:          "openai",
			BaseURL:           "https://chatgpt.com/backend-api/",
			AuxiliaryBaseURLs: []string{"https://auth.openai.com/oauth/token"},
			ProviderResolved:  true,
		})
	require.NoError(t, err)
	require.Len(t, notices, 1)
	assert.Contains(t, notices[0].Detail, "chatgpt.com:443")
	assert.Contains(t, notices[0].Detail, "auth.openai.com:443")
	assert.Contains(t, notices[0].Detail, "read through its app-server")
	assert.Contains(t, notices[0].Detail, "a running session does not re-route")
	assert.NotContains(t, notices[0].Detail, "Remotely delivered provider routing")

	// A route chosen by a layer the operator cannot read has to say so, or the
	// rendered surface understates where the endpoint came from.
	notices, err = ValidateTclaudeLayerNetwork(
		codex, effective, harness.ResolvedModelTransport{
			Model:             "gpt-5.4",
			Provider:          "openai",
			BaseURL:           "https://chatgpt.com/backend-api/",
			AuxiliaryBaseURLs: []string{"https://auth.openai.com/oauth/token"},
			Provenance: []string{
				`model_provider from enterpriseManaged layer "acme-workspace" (sha256:bundle)`,
			},
			ProviderResolved: true,
		})
	require.NoError(t, err)
	require.Len(t, notices, 1)
	assert.Contains(t, notices[0].Detail,
		`Remotely delivered provider routing in effect: model_provider from enterpriseManaged layer "acme-workspace" (sha256:bundle).`)
}
