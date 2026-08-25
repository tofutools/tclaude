package sandboxpolicy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A read/write row may name one regular file. This is the rule a directory
// grant cannot express: reopen exactly ~/.gitconfig without also reopening
// everything else that shares its directory.
func TestNormalizeFilesystemAcceptsRegularFileRows(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	gitconfig := filepath.Join(home, ".gitconfig")
	netrc := filepath.Join(home, ".netrc")
	require.NoError(t, os.WriteFile(gitconfig, []byte("[user]\n"), 0o600))
	require.NoError(t, os.WriteFile(netrc, nil, 0o600))

	normalized, err := Normalize(Profile{Name: "p", Filesystem: []FilesystemGrant{
		{Path: "~/.gitconfig", Access: AccessRead},
		{Path: netrc, Access: AccessWrite},
	}})
	require.NoError(t, err)
	require.Len(t, normalized.Filesystem, 2)
	assert.Equal(t, FilesystemGrant{Path: gitconfig, Access: AccessRead}, normalized.Filesystem[0])
	assert.Equal(t, FilesystemGrant{Path: netrc, Access: AccessWrite}, normalized.Filesystem[1])
}

// A file row may be projected, exactly as a directory row may. The guest path
// is validated syntactically only, so the renderer's purity is unaffected.
func TestNormalizeFilesystemAcceptsRemappedRegularFileRow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	source := filepath.Join(home, "corpus.jsonl")
	require.NoError(t, os.WriteFile(source, nil, 0o600))

	normalized, err := Normalize(Profile{Name: "p", Filesystem: []FilesystemGrant{
		{Path: source, Access: AccessRead, MountPath: "/data/corpus.jsonl"},
	}})
	require.NoError(t, err)
	require.Len(t, normalized.Filesystem, 1)
	assert.Equal(t, "/data/corpus.jsonl", normalized.Filesystem[0].GuestPath())
	assert.True(t, normalized.Filesystem[0].IsRemapped())
}

// The renderer performs no filesystem access, so a file row reaches the plan as
// an ordinary bind entry. Kind is decided by the layer that owns filesystem
// questions, never re-derived here.
func TestRenderMountPlanEmitsRegularFileRow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	gitconfig := filepath.Join(home, ".gitconfig")
	require.NoError(t, os.WriteFile(gitconfig, nil, 0o600))

	plan, err := RenderMountPlanFromGrants([]FilesystemGrant{
		{Path: gitconfig, Access: AccessRead},
	})
	require.NoError(t, err)
	require.Len(t, plan.Entries, 1)
	assert.Equal(t, MountEntry{Path: gitconfig, Mode: MountRO}, plan.Entries[0])
}

func TestFilesystemForLaunchKeepsRegularFileRow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	gitconfig := filepath.Join(home, ".gitconfig")
	require.NoError(t, os.WriteFile(gitconfig, nil, 0o600))

	launch, err := FilesystemForLaunch(EffectiveProfile{Filesystem: []FilesystemGrant{
		{Path: gitconfig, Access: AccessRead},
	}})
	require.NoError(t, err)
	require.Len(t, launch, 1)
	assert.Equal(t, gitconfig, launch[0].Path)
}

func TestSupportsFileGrants(t *testing.T) {
	assert.True(t, SupportsFileGrants(ImplementationTclaudeLayer, "linux"))
	assert.False(t, SupportsFileGrants(ImplementationTclaudeLayer, "darwin"),
		"Seatbelt is a path filter over the host namespace, with no mount to make")
	assert.False(t, SupportsFileGrants(ImplementationStacked, "linux"),
		"the inner harness wall is fed from directory lists that cannot carry a file")
	assert.False(t, SupportsFileGrants(ImplementationHarnessBuiltin, "linux"))
	assert.False(t, SupportsFileGrants(ImplementationResourceOnly, "linux"))
	assert.False(t, SupportsFileGrants(ImplementationOff, "linux"))
}

func TestValidateFileGrantSupport(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "workspace")
	file := filepath.Join(root, "gitconfig")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	grants := []FilesystemGrant{
		{Path: dir, Access: AccessWrite},
		{Path: file, Access: AccessRead},
	}

	require.NoError(t, ValidateFileGrantSupport(grants, ImplementationTclaudeLayer, "linux"))

	for _, unsupported := range []struct {
		implementation Implementation
		goos           string
	}{
		{ImplementationTclaudeLayer, "darwin"},
		{ImplementationStacked, "linux"},
		{ImplementationHarnessBuiltin, "linux"},
		{ImplementationResourceOnly, "linux"},
		{ImplementationOff, "linux"},
	} {
		err := ValidateFileGrantSupport(grants, unsupported.implementation, unsupported.goos)
		require.Error(t, err, "%s/%s", unsupported.implementation, unsupported.goos)
		assert.Contains(t, err.Error(), "unsupported_sandbox_profile_file_grant")
		assert.Contains(t, err.Error(), file)
	}

	require.NoError(t,
		ValidateFileGrantSupport([]FilesystemGrant{{Path: dir, Access: AccessWrite}},
			ImplementationHarnessBuiltin, "linux"),
		"a directory-only policy is unaffected by the new capability")
}

// The bare directory lists a launch hands a harness must not carry a file row.
// Dropping it is safe only because the capability gate above refuses every
// implementation that would have needed the list to express it.
func TestDirectoryGrantsDropsFileRows(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "workspace")
	file := filepath.Join(root, "gitconfig")
	missing := filepath.Join(root, "not-created-yet")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(file, nil, 0o600))

	got := DirectoryGrants([]FilesystemGrant{
		{Path: dir, Access: AccessWrite},
		{Path: file, Access: AccessRead},
		{Path: missing, Access: AccessRead},
	})
	require.Len(t, got, 2)
	assert.Equal(t, dir, got[0].Path)
	assert.Equal(t, missing, got[1].Path,
		"a path that does not exist yet is not a file rule; it is a rule skipped at launch")
	assert.Equal(t, []string{file}, FileGrantPaths([]FilesystemGrant{
		{Path: dir, Access: AccessWrite},
		{Path: file, Access: AccessRead},
	}))
}

// Path kind can change after a profile is saved. A deny row whose directory was
// replaced by a file must refuse the launch, not reach an applier with a hide it
// cannot express.
func TestFilesystemForLaunchRefusesDenyThatBecameAFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, "secrets")
	require.NoError(t, os.WriteFile(path, []byte("was a directory once"), 0o600))

	_, err := FilesystemForLaunch(EffectiveProfile{Filesystem: []FilesystemGrant{
		{Path: path, Access: AccessDeny},
	}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "a deny rule cannot name")
}
