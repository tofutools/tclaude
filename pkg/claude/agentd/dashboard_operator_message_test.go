package agentd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConsumeOperatorAttachmentBatchCopiesOnlyDaemonStaging(t *testing.T) {
	staging := t.TempDir()
	durable := t.TempDir()
	previousStaging := spawnAttachmentsBase
	previousDurable := operatorMessageAttachmentsBase
	spawnAttachmentsBase = staging
	operatorMessageAttachmentsBase = durable
	t.Cleanup(func() {
		spawnAttachmentsBase = previousStaging
		operatorMessageAttachmentsBase = previousDurable
	})

	batch := filepath.Join(staging, "batch-1")
	require.NoError(t, os.MkdirAll(batch, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(batch, "notes.txt"), []byte("hello"), 0o600))
	attachments, destDir, err := consumeOperatorAttachmentBatch("batch-1")
	require.NoError(t, err)
	require.Len(t, attachments, 1)
	assert.Equal(t, "notes.txt", attachments[0].Filename)
	assert.Equal(t, int64(5), attachments[0].SizeBytes)
	assert.Equal(t, destDir, filepath.Dir(attachments[0].StoragePath))
	got, err := os.ReadFile(attachments[0].StoragePath)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(got))
}

func TestConsumeOperatorAttachmentBatchRejectsForgedPathToken(t *testing.T) {
	_, _, err := consumeOperatorAttachmentBatch("../outside")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid attachment token")
}

func TestAttachmentReconcilePreservesFreshUnreferencedCopy(t *testing.T) {
	setupTestDB(t)
	durable := t.TempDir()
	previousDurable := operatorMessageAttachmentsBase
	operatorMessageAttachmentsBase = durable
	t.Cleanup(func() { operatorMessageAttachmentsBase = previousDurable })

	fresh := filepath.Join(durable, "fresh")
	stale := filepath.Join(durable, "stale")
	require.NoError(t, os.WriteFile(fresh, []byte("copying"), 0o600))
	require.NoError(t, os.WriteFile(stale, []byte("orphan"), 0o600))
	old := time.Now().Add(-2 * agentMessageAttachmentOrphanGrace)
	require.NoError(t, os.Chtimes(stale, old, old))

	reconcileAgentMessageAttachments()

	_, err := os.Stat(fresh)
	require.NoError(t, err, "fresh file may still be in the copy-to-commit window")
	_, err = os.Stat(stale)
	assert.ErrorIs(t, err, os.ErrNotExist, "aged crash orphan should be removed")
}
