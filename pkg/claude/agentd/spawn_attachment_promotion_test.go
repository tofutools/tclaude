package agentd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

func TestPromoteSpawnAttachmentsCopiesIntoPrivateRootAndPreservesStaging(t *testing.T) {
	isolateSpawnAttachmentsBase(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	batch := filepath.Join(spawnAttachmentsBaseDir(), convops.GenerateUUID())
	require.NoError(t, os.MkdirAll(batch, 0o700))
	staged := filepath.Join(batch, "pasted-image.png")
	require.NoError(t, os.WriteFile(staged, []byte("image-bytes"), 0o600))
	registerDaemonStagedAttachment(staged)

	promoted, cleanup, err := promoteSpawnAttachments("spwn-private", []string{staged})
	require.NoError(t, err)
	require.Len(t, promoted, 1)
	assert.True(t, sandboxpolicy.PathContainsOrEqual(
		tclcommon.SpawnAttachmentsPrivateDir("spwn-private"),
		promoted[0],
	))
	got, err := os.ReadFile(promoted[0])
	require.NoError(t, err)
	assert.Equal(t, "image-bytes", string(got))
	assert.Contains(t, buildSpawnAttachmentsSection(promoted), promoted[0],
		"the briefing must receive the exact in-namespace spelling")
	_, err = os.Stat(staged)
	require.NoError(t, err, "promotion must copy so a dashboard retry can reuse staging")

	privateRoot := tclcommon.SpawnAttachmentsPrivateDir("spwn-private")
	cleanup()
	_, err = os.Stat(privateRoot)
	assert.True(t, os.IsNotExist(err), "failed-launch cleanup removes the now-empty root")
}

func TestPromoteSpawnAttachmentsRejectsHostileAndNonRegularPaths(t *testing.T) {
	isolateSpawnAttachmentsBase(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	validBatch := filepath.Join(spawnAttachmentsBaseDir(), convops.GenerateUUID())
	require.NoError(t, os.MkdirAll(validBatch, 0o700))
	validFile := filepath.Join(validBatch, "valid.png")
	require.NoError(t, os.WriteFile(validFile, []byte("valid"), 0o600))

	outside := filepath.Join(t.TempDir(), "outside.png")
	require.NoError(t, os.WriteFile(outside, []byte("secret"), 0o600))
	symlinkFile := filepath.Join(validBatch, "symlink.png")
	require.NoError(t, os.Symlink(outside, symlinkFile))
	nonRegular := filepath.Join(validBatch, "directory.png")
	require.NoError(t, os.Mkdir(nonRegular, 0o700))
	realBatch := filepath.Join(t.TempDir(), "real-batch")
	require.NoError(t, os.Mkdir(realBatch, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(realBatch, "via-parent.png"), []byte("secret"), 0o600))
	symlinkBatch := filepath.Join(spawnAttachmentsBaseDir(), "symlink-batch")
	require.NoError(t, os.Symlink(realBatch, symlinkBatch))
	for _, issued := range []string{
		validFile,
		symlinkFile,
		nonRegular,
		filepath.Join(symlinkBatch, "via-parent.png"),
	} {
		registerDaemonStagedAttachment(issued)
	}

	for i, tc := range []struct {
		name string
		path string
		want string
	}{
		{name: "outside", path: outside, want: "not issued"},
		{name: "file symlink", path: symlinkFile, want: "symlink"},
		{name: "directory", path: nonRegular, want: "not a regular file"},
		{
			name: "ancestor symlink",
			path: filepath.Join(symlinkBatch, "via-parent.png"),
			want: "symlink",
		},
		{
			name: "non canonical",
			path: validBatch + string(filepath.Separator) + ".." +
				string(filepath.Separator) + filepath.Base(validBatch) +
				string(filepath.Separator) + filepath.Base(validFile),
			want: "not issued",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := promoteSpawnAttachments(
				"spwn-hostile-"+string(rune('a'+i)),
				[]string{tc.path},
			)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestPromoteSpawnAttachmentsRejectsIssuedPathReplacedByHostileFile(t *testing.T) {
	isolateSpawnAttachmentsBase(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	batch := filepath.Join(spawnAttachmentsBaseDir(), convops.GenerateUUID())
	require.NoError(t, os.MkdirAll(batch, 0o700))
	staged := filepath.Join(batch, "issued.png")
	require.NoError(t, os.WriteFile(staged, []byte("issued"), 0o600))
	registerDaemonStagedAttachment(staged)

	hostile := filepath.Join(t.TempDir(), "hostile-secret")
	require.NoError(t, os.WriteFile(hostile, []byte("secret"), 0o600))
	require.NoError(t, os.Remove(staged))
	require.NoError(t, os.Link(hostile, staged))

	_, _, err := promoteSpawnAttachments("spwn-swapped", []string{staged})
	require.ErrorContains(t, err, "not the daemon-issued regular file")
}

func TestSweepPrivateAttachmentRootsNeverRemovesLiveRoot(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	now := time.Now()
	old := now.Add(-spawnAttachmentBatchTTL - time.Hour)
	liveRoot := tclcommon.SpawnAttachmentsPrivateDir("live")
	inactiveRoot := tclcommon.SpawnAttachmentsPrivateDir("inactive")
	for _, root := range []string{liveRoot, inactiveRoot} {
		require.NoError(t, os.MkdirAll(root, 0o700))
		require.NoError(t, os.Chtimes(root, old, old))
	}

	sweepPrivateAttachmentRootsAt(now, map[string]bool{liveRoot: true})

	_, err := os.Stat(liveRoot)
	require.NoError(t, err, "a live root's inode must never be removed and recreated")
	_, err = os.Stat(inactiveRoot)
	assert.True(t, os.IsNotExist(err), "an old empty inactive root should be swept")
}
