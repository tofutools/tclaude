package agentbundle

import (
	"bytes"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestDiskArchiveHistoryIntegrityAndCleanup(t *testing.T) {
	dir := testutil.CanonicalTempDir(t)
	history, err := os.CreateTemp(dir, "native")
	require.NoError(t, err)
	_, err = io.Copy(history, bytes.NewReader(bytes.Repeat([]byte("history\n"), 1<<20)))
	require.NoError(t, err)
	require.NoError(t, history.Close())
	b := fixtureBundle()
	require.NoError(t, b.SetHistoryFile("claude-jsonl", "source", history.Name(), false))
	archive, err := os.CreateTemp(dir, "archive")
	require.NoError(t, err)
	defer archive.Close()
	require.NoError(t, b.EncodeTo(archive))
	decoded, err := DecodeFile(archive, MaxBytes, dir)
	require.NoError(t, err)
	require.Empty(t, decoded.Transcript)
	require.NotEmpty(t, decoded.TranscriptPath)
	require.Equal(t, b.Manifest.History, decoded.Manifest.History)
	extracted := decoded.TranscriptPath
	require.NoError(t, decoded.Close())
	_, err = os.Stat(extracted)
	require.True(t, os.IsNotExist(err))
	_, err = DecodeFile(archive, 1024, dir)
	require.ErrorContains(t, err, "federation.agent_transfer_max_bytes")
	require.NoError(t, os.WriteFile(history.Name(), []byte("tampered"), 0600))
	require.ErrorContains(t, b.Validate(), "checksum")
}
