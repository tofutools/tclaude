package agentd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMoveFileDoesNotClobberDestinationCreatedAfterPreflight(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "staged.jsonl")
	dst := filepath.Join(dir, "destination.jsonl")
	require.NoError(t, os.WriteFile(src, []byte("archive"), 0o600))
	require.NoError(t, os.WriteFile(dst, []byte("local conversation"), 0o600))

	err := moveFile(src, dst)
	require.Error(t, err)
	got, readErr := os.ReadFile(dst)
	require.NoError(t, readErr)
	assert.Equal(t, []byte("local conversation"), got)
	_, statErr := os.Stat(src)
	assert.NoError(t, statErr, "failed no-clobber placement keeps its staged source")
}

func TestReserveMissingConversationDoesNotClobberLateDestination(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "missing-conv.jsonl")
	require.NoError(t, os.WriteFile(dst, []byte("late local conversation"), 0o600))

	err := reserveFile(dst)
	require.Error(t, err)
	got, readErr := os.ReadFile(dst)
	require.NoError(t, readErr)
	assert.Equal(t, []byte("late local conversation"), got)
}

func TestMoveFileRemovesDestinationWhenSourceUnlinkFails(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "staged.jsonl")
	dst := filepath.Join(dir, "destination.jsonl")
	require.NoError(t, os.WriteFile(src, []byte("archive"), 0o600))

	err := moveFileWithRemove(src, dst, func(string) error { return assert.AnError })
	require.ErrorIs(t, err, assert.AnError)
	_, statErr := os.Stat(dst)
	assert.ErrorIs(t, statErr, os.ErrNotExist, "failed unlink rolls back the created destination")
	_, statErr = os.Stat(src)
	assert.NoError(t, statErr, "failed unlink keeps the staged source")
}
