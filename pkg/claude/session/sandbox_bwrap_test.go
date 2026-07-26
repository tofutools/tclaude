package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
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
			got, err := bwrapArgs(tc.plan)
			require.NoError(t, err)
			require.GreaterOrEqual(t, len(got), len(tc.want))
			assert.Equal(t, tc.want, got[len(got)-len(tc.want):],
				"the builder must preserve MountPlan order verbatim")
			assert.NotContains(t, got, "--unshare-net")
			assert.NotContains(t, got, "--unshare-pid")
			assert.NotContains(t, got, "--unshare-ipc")
		})
	}
}

func TestBwrapArgsSkipsMissingBindsButStillAppliesMissingHide(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-created")
	got, err := bwrapArgs(sandboxpolicy.MountPlan{Entries: []sandboxpolicy.MountEntry{
		{Path: missing + "-ro", Mode: sandboxpolicy.MountRO},
		{Path: missing + "-rw", Mode: sandboxpolicy.MountRW},
		{Path: missing + "-hide", Mode: sandboxpolicy.MountHide},
	}})
	require.NoError(t, err)
	assert.Equal(t, []string{"--tmpfs", missing + "-hide"}, got[len(got)-2:])
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
	got, err := bwrapArgs(plan)
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

func indexOfBwrapTriplet(args []string, flag, path string) int {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == path {
			return i
		}
	}
	return -1
}

func TestBwrapArgsRejectInvalidEntry(t *testing.T) {
	_, err := bwrapArgs(sandboxpolicy.MountPlan{Entries: []sandboxpolicy.MountEntry{
		{Path: "relative", Mode: sandboxpolicy.MountRW},
	}})
	require.ErrorContains(t, err, "non-absolute")

	_, err = bwrapArgs(sandboxpolicy.MountPlan{Entries: []sandboxpolicy.MountEntry{
		{Path: "/work", Mode: sandboxpolicy.MountMode(99)},
	}})
	require.ErrorContains(t, err, "invalid mode")
}

func TestBwrapCommandShellQuotesHarnessCommand(t *testing.T) {
	got, err := bwrapCommand("/usr/bin/bwrap", sandboxpolicy.MountPlan{}, "export X='a b'; exec agent --flag")
	require.NoError(t, err)
	assert.Contains(t, got, " -- sh -c ")
	assert.Contains(t, got, "export X=")
}
