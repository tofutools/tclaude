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
			got, err := bwrapArgs(nil, nil, tc.plan)
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
	got, err := bwrapArgs(nil, nil, sandboxpolicy.MountPlan{Entries: []sandboxpolicy.MountEntry{
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

func TestBwrapArgsHidesProtectedRootsBeforeBreakGlassReopens(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, relative := range []string{filepath.Join(".tclaude", "data"), filepath.Join(".claude", "sessions")} {
		require.NoError(t, os.MkdirAll(filepath.Join(home, relative), 0o700))
	}
	protected, err := sandboxpolicy.ProtectedPaths()
	require.NoError(t, err)
	require.Len(t, protected, 2)

	plan := sandboxpolicy.MountPlan{Entries: []sandboxpolicy.MountEntry{
		{Path: protected[0], Mode: sandboxpolicy.MountRO},
		{Path: protected[1], Mode: sandboxpolicy.MountRW},
	}}
	got, err := bwrapArgs(nil, protected, plan)
	require.NoError(t, err)

	hide0 := indexOfBwrapTriplet(got, "--tmpfs", protected[0])
	hide1 := indexOfBwrapTriplet(got, "--tmpfs", protected[1])
	reopen0 := indexOfBwrapTriplet(got, "--ro-bind", protected[0])
	reopen1 := indexOfBwrapTriplet(got, "--bind", protected[1])
	require.NotEqual(t, -1, hide0)
	require.NotEqual(t, -1, hide1)
	require.NotEqual(t, -1, reopen0)
	require.NotEqual(t, -1, reopen1)
	assert.Less(t, hide0, reopen0, "baseline hide must precede the acknowledged read reopen")
	assert.Less(t, hide1, reopen1, "baseline hide must precede the acknowledged write reopen")
	assert.Equal(t, -1, indexOfBwrapTriplet(got, "--remount-ro", protected[0]),
		"an exact read reopen replaces the hidden mount and carries its own mode")
	assert.Equal(t, -1, indexOfBwrapTriplet(got, "--remount-ro", protected[1]),
		"an exact write reopen must not be downgraded by the deferred flush")
}

func TestBwrapArgsHidesTmuxSocketAfterPlan(t *testing.T) {
	tmuxBase := t.TempDir()
	t.Setenv("TMUX_TMPDIR", tmuxBase)
	tmuxSocketDir, err := clcommon.TclaudeTmuxSocketDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(tmuxSocketDir, 0o700))

	got, err := bwrapArgs(nil, nil, sandboxpolicy.MountPlan{Entries: []sandboxpolicy.MountEntry{
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

	got, err := bwrapArgs(nil, nil, sandboxpolicy.MountPlan{Entries: []sandboxpolicy.MountEntry{{
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

func TestBwrapArgsPrivateWriteDirOverridesPolicyButPrecedesHostControl(t *testing.T) {
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	parent := filepath.Join(t.TempDir(), "spawn-attachments")
	current := filepath.Join(parent, "current-session")
	require.NoError(t, os.MkdirAll(current, 0o700))

	got, err := bwrapArgs(nil, nil, sandboxpolicy.MountPlan{
		Entries: []sandboxpolicy.MountEntry{{
			Path: parent,
			Mode: sandboxpolicy.MountRW,
		}},
	}, TclaudeLayerPrivateWriteDir{Parent: parent, Current: current})
	require.NoError(t, err)

	policyGrant := indexOfBwrapTriplet(got, "--bind", parent)
	privateHide := indexOfBwrapTriplet(got, "--tmpfs", parent)
	currentReopen := indexOfBwrapTriplet(got, "--bind", current)
	parentRemount := indexOfBwrapTriplet(got, "--remount-ro", parent)
	tmuxHide := indexOfBwrapTriplet(got, "--tmpfs", mustTmuxSocketDir(t))
	require.NotEqual(t, -1, policyGrant)
	require.NotEqual(t, -1, privateHide)
	require.NotEqual(t, -1, currentReopen)
	require.NotEqual(t, -1, parentRemount)
	require.NotEqual(t, -1, tmuxHide)
	assert.Less(t, policyGrant, privateHide,
		"private sibling concealment must beat ordinary policy and break-glass replay")
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
			got, err := bwrapArgs(nil, nil, sandboxpolicy.MountPlan{Entries: tc.entries})
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

	got, err := bwrapArgs([]string{stateRoot}, nil, sandboxpolicy.MountPlan{
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

	got, err := bwrapArgs(phase0, nil, sandboxpolicy.MountPlan{})
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

func TestBwrapArgsRepairsLaunchContractAfterHomeDeny(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("HOME", home)
	stateRoot := filepath.Join(home, ".claude")
	protectedRoot := filepath.Join(stateRoot, "sessions")
	breakGlass := filepath.Join(protectedRoot, "approved")
	require.NoError(t, os.MkdirAll(breakGlass, 0o700))

	got, err := bwrapArgs([]string{stateRoot}, []string{breakGlass}, sandboxpolicy.MountPlan{
		Entries: []sandboxpolicy.MountEntry{
			{Path: home, Mode: sandboxpolicy.MountHide},
			{Path: breakGlass, Mode: sandboxpolicy.MountRO},
		},
	})
	require.NoError(t, err)

	homeHide := indexOfBwrapTriplet(got, "--tmpfs", home)
	stateBinds := indicesOfBwrapTriplet(got, "--bind", stateRoot)
	protectedHides := indicesOfBwrapTriplet(got, "--tmpfs", protectedRoot)
	breakGlassReopen := indexOfBwrapTriplet(got, "--ro-bind", breakGlass)
	homeRemount := indexOfBwrapTriplet(got, "--remount-ro", home)
	protectedRemount := indexOfBwrapTriplet(got, "--remount-ro", protectedRoot)
	require.NotEqual(t, -1, homeHide)
	require.Len(t, stateBinds, 2, "the state root must be rebound after an ordinary ancestor deny")
	require.Len(t, protectedHides, 2, "repairing the state root must restore its protected child hide")
	require.NotEqual(t, -1, breakGlassReopen)
	require.NotEqual(t, -1, homeRemount)
	require.NotEqual(t, -1, protectedRemount)
	assert.Less(t, stateBinds[0], homeHide)
	assert.Less(t, homeHide, stateBinds[1])
	assert.Less(t, stateBinds[1], protectedHides[1])
	assert.Less(t, protectedHides[1], breakGlassReopen,
		"acknowledged break-glass must remain the only plan authority that beats a protected hide")
	assert.Less(t, breakGlassReopen, protectedRemount,
		"the child reopen mountpoint must exist before its hidden parent becomes read-only")
	assert.Less(t, breakGlassReopen, homeRemount,
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
		nil,
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

	_, err = bwrapArgs([]string{stateRoot, workspace}, nil, sandboxpolicy.MountPlan{})
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
	require.ErrorContains(t, err, "cannot persist harness state")
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

func TestValidateTclaudeLayerHarnessSupportsOpenCodeOnLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
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
	verdict := TclaudeLayerLaunchOSSandbox(sandboxpolicy.NetworkHostOpen)
	assert.Equal(t, "on", verdict.State)
	assert.Contains(t, verdict.Source, "ambient host Unix sockets reachable")
	assert.True(t, verdict.Unverified, "the dashboard must not present a full-fidelity padlock")

	isolated := TclaudeLayerLaunchOSSandbox(sandboxpolicy.NetworkIsolatedWithAgentd)
	assert.Equal(t, "on", isolated.State)
	assert.Contains(t, isolated.Source, "isolated network")
	assert.Contains(t, isolated.Source, "host loopback/IDE bridge unavailable")
	assert.Contains(t, isolated.Source, "isolated PIDs")
	assert.Contains(t, isolated.Source, "agentd socket allowlisted")
	assert.False(t, isolated.Unverified, "constructed-root socket isolation has full fidelity")

	openCode := TclaudeLayerLaunchOSSandboxForHarness(
		harness.OpenCodeName, sandboxpolicy.NetworkHostOpen)
	assert.Equal(t, "on", openCode.State)
	assert.Equal(t,
		"tclaude-layer (bubblewrap; OpenCode tool-executing server confined; attach pane outside the boundary; loopback control plane reachable; host network and ambient host Unix sockets reachable)",
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
	require.ErrorContains(t, ValidateTclaudeLayerNetwork(openCode, closed), "loopback control plane")
	require.ErrorContains(t, ValidateTclaudeLayerNetwork(claude, closed), "requires hosted model traffic")
	require.ErrorContains(t, ValidateTclaudeLayerNetwork(codex, closed), sandboxpolicy.OfflineModelTransportEnv+"=1")

	closed.Environment = []sandboxpolicy.EnvironmentEntry{{Name: sandboxpolicy.OfflineModelTransportEnv, Value: "0"}}
	require.ErrorContains(t, ValidateTclaudeLayerNetwork(codex, closed), sandboxpolicy.OfflineModelTransportEnv+"=1")
	closed.Environment[0].Value = "1"
	require.NoError(t, ValidateTclaudeLayerNetwork(codex, closed))

	require.NoError(t, ValidateTclaudeLayerNetwork(claude, sandboxpolicy.EffectiveProfile{
		NetworkAccess: sandboxpolicy.NetworkAccessInternet,
	}))
	require.NoError(t, ValidateTclaudeLayerNetwork(openCode, sandboxpolicy.EffectiveProfile{
		NetworkAccess: sandboxpolicy.NetworkAccessInternet,
	}))
}

func TestBwrapArgsConstructsIsolatedRootAndRepairsAgentdSocket(t *testing.T) {
	home := agentipctest.ShortSocketDir(t)
	t.Setenv("HOME", home)
	t.Setenv(agentipc.SocketEnv, "")
	socket := agentipc.CanonicalSocketPath()
	require.NoError(t, os.MkdirAll(filepath.Dir(socket), 0o700))
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

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
	got, err := bwrapArgs([]string{stateRoot, workspace}, nil, plan)
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
	require.Len(t, socketBinds, 2, "agentd socket must be rebound after an ordinary ancestor deny")
	assert.Less(t, homeHide, stateRepairs[1])
	assert.Less(t, homeHide, socketBinds[1])
	assert.Less(t, socketBinds[1], rootRemount,
		"all explicit child mounts must land before the constructed root becomes read-only")
	assert.Less(t, stateRepairs[1], homeRemount)
	assert.Less(t, socketBinds[1], homeRemount,
		"the child socket bind must land before its hidden parent becomes read-only")

	tmuxSocketDir, err := clcommon.TclaudeTmuxSocketDir()
	require.NoError(t, err)
	assert.Equal(t, len(got)-4, indexOfBwrapTriplet(got, "--tmpfs", tmuxSocketDir),
		"class-4 tmux hide remains final under the constructed root")
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
	got, err := bwrapArgs(nil, nil, plan)
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

	hostOpen, err := bwrapArgs(nil, nil, sandboxpolicy.MountPlan{
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
	got, err := bwrapArgs(nil, nil, sandboxpolicy.MountPlan{
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

func TestBwrapArgsRefusesReservedFilteredPosture(t *testing.T) {
	_, err := bwrapArgs(nil, nil, sandboxpolicy.MountPlan{
		NetworkPosture: sandboxpolicy.NetworkFiltered,
	})
	require.ErrorContains(t, err, "reserved")
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
	_, err := bwrapArgs(nil, nil, sandboxpolicy.MountPlan{Entries: []sandboxpolicy.MountEntry{
		{Path: "relative", Mode: sandboxpolicy.MountRW},
	}})
	require.ErrorContains(t, err, "non-absolute")

	_, err = bwrapArgs(nil, nil, sandboxpolicy.MountPlan{Entries: []sandboxpolicy.MountEntry{
		{Path: "/work", Mode: sandboxpolicy.MountMode(99)},
	}})
	require.ErrorContains(t, err, "invalid mode")
}

func TestBwrapArgsZeroModeFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "uninitialized-entry")
	got, err := bwrapArgs(nil, nil, sandboxpolicy.MountPlan{Entries: []sandboxpolicy.MountEntry{
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
	got, err := bwrapCommand("/usr/bin/bwrap", nil, nil, nil, sandboxpolicy.MountPlan{}, "export X='a b'; exec agent --flag")
	require.NoError(t, err)
	assert.Contains(t, got, " -- sh -c ")
	assert.Contains(t, got, "export X=")
	assert.Contains(t, got, "--new-session")
}
