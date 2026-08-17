package session

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// The fold is the harness-agnostic seam of TCL-1201: every harness's declared
// requirements pass through it, so the invariants proven here hold for Claude,
// Codex, OpenCode and Copilot alike — including the two production regression
// shapes (#2285 duplicated state root, #2286 socket in a directory bucket).

func TestFoldTclaudeLayerRequirementsBuckets(t *testing.T) {
	stateRoot := "/home/user/.harness"
	buckets, err := foldTclaudeLayerRequirements(stateRoot, []harness.LayerPathRequirement{
		// The #2285 shape: a catalog repeating the state root must not turn
		// it into an auxiliary StateDirs row.
		{Path: stateRoot, Kind: harness.LayerPathDirectory,
			Access: harness.LayerPathWrite, MayCreate: true, PolicyGrant: true},
		{Path: "/home/user/.cache/harness", Kind: harness.LayerPathDirectory,
			Access: harness.LayerPathWrite, MayCreate: true, PolicyGrant: true},
		// Same row twice: the fold deduplicates before preparation.
		{Path: "/home/user/.cache/harness", Kind: harness.LayerPathDirectory,
			Access: harness.LayerPathWrite, MayCreate: true, PolicyGrant: true},
		// Contract-only state (the OpenCode shape): no policy write grant.
		{Path: "/home/user/.local/state/harness", Kind: harness.LayerPathDirectory,
			Access: harness.LayerPathWrite, MayCreate: true},
		// Read-only creatable subtree below the state root.
		{Path: filepath.Join(stateRoot, "bin"), Kind: harness.LayerPathDirectory,
			Access: harness.LayerPathRead, MayCreate: true},
		// The #2286 shape: a writable socket is a live endpoint, never state.
		{Path: "/run/agentd.sock", Kind: harness.LayerPathSocket,
			Access: harness.LayerPathWrite},
		{Path: "/run/agentd-legacy.sock", Kind: harness.LayerPathSocket,
			Access: harness.LayerPathRead},
		{Path: "/usr/local/bin/harness", Kind: harness.LayerPathFile,
			Access: harness.LayerPathRead},
	})
	require.NoError(t, err)

	assert.Equal(t, []string{
		"/home/user/.cache/harness",
		"/home/user/.local/state/harness",
	}, buckets.StateDirs, "state root and duplicates stay out of StateDirs")
	assert.Equal(t, []string{filepath.Join(stateRoot, "bin")}, buckets.ReadOnlyStateDirs)
	assert.Equal(t, []string{
		stateRoot,
		"/home/user/.cache/harness",
		"/home/user/.cache/harness",
		"/home/user/.local/state/harness",
	}, buckets.ContractWriteDirs, "every writable directory row keeps a contract entry")
	assert.Equal(t, []string{
		filepath.Join(stateRoot, "bin"),
		"/run/agentd-legacy.sock",
		"/usr/local/bin/harness",
	}, buckets.LaunchReadDirs)
	assert.Equal(t, []string{
		stateRoot,
		"/home/user/.cache/harness",
		"/run/agentd.sock",
	}, buckets.LaunchWriteDirs,
		"policy write grants carry directories with PolicyGrant plus writable sockets")
}

func TestFoldTclaudeLayerRequirementsRefusals(t *testing.T) {
	stateRoot := "/home/user/.harness"
	cases := []struct {
		name        string
		requirement harness.LayerPathRequirement
		want        string
	}{
		{
			name: "socket marked creatable",
			requirement: harness.LayerPathRequirement{
				Path: "/run/agentd.sock", Kind: harness.LayerPathSocket,
				Access: harness.LayerPathWrite, MayCreate: true,
			},
			want: "cannot be materialized",
		},
		{
			name: "file marked creatable",
			requirement: harness.LayerPathRequirement{
				Path: "/usr/local/bin/harness", Kind: harness.LayerPathFile,
				Access: harness.LayerPathRead, MayCreate: true,
			},
			want: "cannot be materialized",
		},
		{
			name: "writable file",
			requirement: harness.LayerPathRequirement{
				Path: "/home/user/notes.txt", Kind: harness.LayerPathFile,
				Access: harness.LayerPathWrite,
			},
			want: "unsupported shape",
		},
		{
			name: "unknown kind",
			requirement: harness.LayerPathRequirement{
				Path: "/home/user/thing", Kind: "device",
				Access: harness.LayerPathRead,
			},
			want: "unsupported shape",
		},
		{
			name: "relative path",
			requirement: harness.LayerPathRequirement{
				Path: "relative/path", Kind: harness.LayerPathDirectory,
				Access: harness.LayerPathWrite, MayCreate: true,
			},
			want: "not an absolute path",
		},
		{
			name: "read-only state outside the state root",
			requirement: harness.LayerPathRequirement{
				Path: "/home/user/elsewhere", Kind: harness.LayerPathDirectory,
				Access: harness.LayerPathRead, MayCreate: true,
			},
			want: "is not below state root",
		},
		{
			name: "read-only state equal to the state root",
			requirement: harness.LayerPathRequirement{
				Path: stateRoot, Kind: harness.LayerPathDirectory,
				Access: harness.LayerPathRead, MayCreate: true,
			},
			want: "is not below state root",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := foldTclaudeLayerRequirements(
				stateRoot, []harness.LayerPathRequirement{testCase.requirement})
			require.ErrorContains(t, err, testCase.want)
		})
	}
}

// TestPrepareTclaudeLayerHarnessStateRefusesExistingNonDirectory covers the
// frozen-spec side of the #2286 shape: a persisted contract whose state bucket
// names an existing socket or file is refused before any mkdir, for every
// harness, with an error that names the node kind.
func TestPrepareTclaudeLayerHarnessStateRefusesExistingNonDirectory(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "tcl-layer-nondir-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	if resolved, resolveErr := filepath.EvalSymlinks(home); resolveErr == nil {
		home = resolved
	}
	t.Setenv("HOME", home)
	t.Setenv("TMUX_TMPDIR", filepath.Join(home, "tmux"))
	t.Setenv("TMPDIR", filepath.Join(home, "tmp"))
	cwd := filepath.Join(home, "work")
	require.NoError(t, os.MkdirAll(cwd, 0o755))

	socketPath := filepath.Join(home, "live.sock")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	filePath := filepath.Join(home, "plain-file")
	require.NoError(t, os.WriteFile(filePath, []byte("x"), 0o600))

	for _, testCase := range []struct {
		name, path, want string
	}{
		{name: "socket", path: socketPath, want: "refusing to materialize a socket"},
		{name: "file", path: filePath, want: "refusing to materialize a file"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			spec, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
				HarnessName: harness.DefaultName,
				Cwd:         cwd,
			})
			require.NoError(t, err)
			spec.Contract.StateDirs = append(
				[]string(nil), spec.Contract.StateDirs...)
			spec.Contract.StateDirs = append(spec.Contract.StateDirs, testCase.path)

			err = PrepareTclaudeLayerHarnessState(spec)
			require.ErrorContains(t, err, "exists and is not a directory")
			require.ErrorContains(t, err, testCase.want)
		})
	}
}
