package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// A plan entry naming a single FILE binds like any other entry. The applier
// deliberately asks nothing about kind on this path: bubblewrap's --ro-bind and
// --bind take a file source as readily as a directory one, and the launch
// contract has bound individual files (the harness-config floor) since before
// profiles could name one.
func TestBwrapArgsBindsRegularFileEntry(t *testing.T) {
	root := t.TempDir()
	readFile := filepath.Join(root, "gitconfig")
	writeFile := filepath.Join(root, "netrc")
	require.NoError(t, os.WriteFile(readFile, []byte("[user]\n"), 0o600))
	require.NoError(t, os.WriteFile(writeFile, nil, 0o600))

	got, err := bwrapArgs(nil, sandboxpolicy.MountPlan{Entries: []sandboxpolicy.MountEntry{
		{Path: readFile, Mode: sandboxpolicy.MountRO},
		{Path: writeFile, Mode: sandboxpolicy.MountRW},
	}})
	require.NoError(t, err)
	assert.NotEqual(t, -1, indexOfBwrapBind(got, "--ro-bind", readFile, readFile))
	assert.NotEqual(t, -1, indexOfBwrapBind(got, "--bind", writeFile, writeFile))
}

// A projected file lands at its guest path, the same way a projected directory
// does. Under an inherited root the mount point has to exist on the host, so the
// test supplies one — as a file, which is what the source requires.
func TestBwrapArgsBindsRemappedRegularFileAtGuestPath(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "corpus.jsonl")
	guest := filepath.Join(root, "guest.jsonl")
	require.NoError(t, os.WriteFile(source, []byte("{}\n"), 0o600))
	require.NoError(t, os.WriteFile(guest, nil, 0o600))

	got, err := bwrapArgs(nil, sandboxpolicy.MountPlan{Entries: []sandboxpolicy.MountEntry{
		{Path: guest, Mode: sandboxpolicy.MountRO, Source: source},
	}})
	require.NoError(t, err)
	assert.NotEqual(t, -1, indexOfBwrapBind(got, "--ro-bind", source, guest))
}

// Under an inherited root the mount point is a real host path, so a file source
// and a directory mount point cannot be reconciled. bubblewrap refuses the
// combination with a message naming neither the rule nor the profile it came
// from, so tclaude refuses first and says which side is which.
func TestBwrapArgsRefusesRemappedKindMismatchUnderInheritedRoot(t *testing.T) {
	root := t.TempDir()
	sourceFile := filepath.Join(root, "corpus.jsonl")
	guestDir := filepath.Join(root, "guest")
	require.NoError(t, os.WriteFile(sourceFile, nil, 0o600))
	require.NoError(t, os.MkdirAll(guestDir, 0o755))

	_, err := bwrapArgs(nil, sandboxpolicy.MountPlan{Entries: []sandboxpolicy.MountEntry{
		{Path: guestDir, Mode: sandboxpolicy.MountRO, Source: sourceFile},
	}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tclaude_layer_mount_point_kind")
	assert.Contains(t, err.Error(), "the host path "+sourceFile+" is a file")
	assert.Contains(t, err.Error(), "the sandbox mount point "+guestDir+" is a directory")

	sourceDir := filepath.Join(root, "source")
	guestFile := filepath.Join(root, "guest.jsonl")
	require.NoError(t, os.MkdirAll(sourceDir, 0o755))
	require.NoError(t, os.WriteFile(guestFile, nil, 0o600))

	_, err = bwrapArgs(nil, sandboxpolicy.MountPlan{Entries: []sandboxpolicy.MountEntry{
		{Path: guestFile, Mode: sandboxpolicy.MountRO, Source: sourceDir},
	}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tclaude_layer_mount_point_kind")
	assert.Contains(t, err.Error(), "the sandbox mount point "+guestFile+" is a file")
	assert.Contains(t, err.Error(), "the host path "+sourceDir+" is a directory")
}

// A file rule contributes nothing to the bare directory lists a launch hands the
// harness, and everything to the mount plan the outer layer replays. That split
// is what keeps `--add-dir <file>` and a file in Claude Code's sandbox filesystem
// array from ever being emitted, without losing the rule.
func TestSandboxSnapshotDirsExcludesFileRows(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "workspace")
	file := filepath.Join(root, "gitconfig")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(file, nil, 0o600))

	snapshot := &sandboxpolicy.Snapshot{Effective: sandboxpolicy.EffectiveProfile{
		Filesystem: []sandboxpolicy.FilesystemGrant{
			{Path: dir, Access: sandboxpolicy.AccessWrite},
			{Path: file, Access: sandboxpolicy.AccessWrite},
		},
	}}
	assert.Equal(t, []string{dir},
		sandboxSnapshotDirs(snapshot, sandboxpolicy.AccessWrite))
	assert.Equal(t, []string{dir},
		sandboxSnapshotHostDirs(snapshot, sandboxpolicy.AccessWrite))
	assert.Equal(t, []string{file},
		sandboxSnapshotHostFiles(snapshot, sandboxpolicy.AccessWrite),
		"the file rule keeps an integrity check of its own, in the shape a file can answer")

	// The layer's own list is a path list feeding the mount plan, not a harness
	// argument, so it keeps the file rule.
	assert.Equal(t, []string{dir, file},
		sandboxDirsForEffective(snapshot.Effective, sandboxpolicy.AccessWrite))
}

// The end-to-end statement: a file rule survives the launch composition — which
// flattens the policy through bare path lists — and reaches the plan the applier
// replays. This is the step a directory-only assumption would silently drop.
func TestBuildTclaudeLayerLaunchSpecKeepsRegularFileRow(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("HOME", home)
	cwd := filepath.Join(home, "work")
	require.NoError(t, os.MkdirAll(cwd, 0o755))
	gitconfig := filepath.Join(home, "gitconfig")
	require.NoError(t, os.WriteFile(gitconfig, []byte("[user]\n"), 0o600))

	effective := sandboxpolicy.EffectiveProfile{
		Filesystem: []sandboxpolicy.FilesystemGrant{
			{Path: gitconfig, Access: sandboxpolicy.AccessRead},
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
		if entry.Target == gitconfig {
			found = true
			assert.Equal(t, SandboxPlanPresent, entry.Disposition)
		}
	}
	assert.True(t, found, "the file rule must appear in the described plan")
}
