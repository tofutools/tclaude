package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
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
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := bwrapArgs(nil, tc.plan)
			require.NoError(t, err)
			require.GreaterOrEqual(t, len(got), len(tc.want))
			tmuxHide := len(got) - 2
			assert.Equal(t, tc.want, got[tmuxHide-len(tc.want):tmuxHide],
				"the builder must preserve MountPlan order verbatim")
			assert.NotContains(t, got, "--unshare-net")
			assert.NotContains(t, got, "--unshare-pid")
			assert.NotContains(t, got, "--unshare-ipc")
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
	assert.Equal(t, []string{"--tmpfs", missing + "-hide"}, got[len(got)-4:len(got)-2])
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
	got, err := bwrapArgs(nil, plan)
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
	require.NotEqual(t, -1, parentGrant)
	require.NotEqual(t, -1, exactGrant)
	require.NotEqual(t, -1, hide)
	assert.Less(t, parentGrant, hide)
	assert.Less(t, exactGrant, hide, "host-control hide must be unshadowable by any plan entry")
	assert.Equal(t, len(got)-2, hide, "tmux socket hide must be the final applier phase")
}

func TestBwrapArgsLaunchContractPrecedesProtectedHides(t *testing.T) {
	home := t.TempDir()
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
	require.NotEqual(t, -1, stateBind)
	require.NotEqual(t, -1, workspaceBind)
	require.NotEqual(t, -1, agentBind)
	require.NotEqual(t, -1, protectedHide)
	assert.Less(t, stateBind, protectedHide, "protected state must stay hidden above the writable harness root")
	assert.Less(t, workspaceBind, protectedHide)
	assert.Less(t, agentBind, protectedHide)
}

func TestValidateTclaudeLayerHarnessRejectsOpenCode(t *testing.T) {
	require.ErrorContains(t, ValidateTclaudeLayerHarness(harness.OpenCodeName), "runs outside the wrapped pane")
	require.NoError(t, ValidateTclaudeLayerHarness(harness.DefaultName))
	require.NoError(t, ValidateTclaudeLayerHarness(harness.CodexName))
}

func TestTclaudeLayerVerdictRecordsPartialSocketFidelity(t *testing.T) {
	verdict := TclaudeLayerLaunchOSSandbox()
	assert.Equal(t, "on", verdict.State)
	assert.Contains(t, verdict.Source, "ambient host Unix sockets reachable")
	assert.True(t, verdict.Unverified, "the dashboard must not present a full-fidelity padlock")
}

func TestStatusCallbackSoftDisablesInsideTclaudeLayer(t *testing.T) {
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
	assert.Equal(t, []string{"--tmpfs", path}, got[len(got)-4:len(got)-2])
}

func TestBwrapCommandShellQuotesHarnessCommand(t *testing.T) {
	got, err := bwrapCommand("/usr/bin/bwrap", nil, sandboxpolicy.MountPlan{}, "export X='a b'; exec agent --flag")
	require.NoError(t, err)
	assert.Contains(t, got, " -- sh -c ")
	assert.Contains(t, got, "export X=")
	assert.Contains(t, got, "--new-session")
}
