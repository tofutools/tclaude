package agentd

import (
	"bytes"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/jobstream"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestIncrementalJobOutputBoundariesAndPartialAppend(t *testing.T) {
	f, err := os.Create(filepath.Join(testutil.CanonicalTempDir(t), "output.frames"))
	require.NoError(t, err)
	defer f.Close()
	j := &db.FederationJob{ID: "job", Fingerprint: "fingerprint", State: "running"}
	enc := jobstream.NewEncoder(f)
	_, err = enc.Write(jobstream.Stdout, bytes.Repeat([]byte("x"), jobstream.MaxChunk))
	require.NoError(t, err)
	_, err = enc.Write(jobstream.Stderr, []byte("warning"))
	require.NoError(t, err)
	c := fedJobOutputCursor{Job: j.ID, Fingerprint: j.Fingerprint}
	first, err := readFederationJobOutput(f, j, c, jobstream.MaxChunk)
	require.NoError(t, err)
	require.Len(t, first.Chunks, 1)
	require.True(t, first.Truncated)
	c, err = decodeJobOutputCursor(first.Cursor, j)
	require.NoError(t, err)
	require.Equal(t, uint64(jobstream.MaxChunk), c.Stdout)
	second, err := readFederationJobOutput(f, j, c, jobstream.MaxChunk)
	require.NoError(t, err)
	require.Len(t, second.Chunks, 1)
	require.Equal(t, "stderr", second.Chunks[0].Stream)
	require.False(t, second.Truncated)
	c, err = decodeJobOutputCursor(second.Cursor, j)
	require.NoError(t, err)
	// A producer can append a header before its payload. Polls must never
	// consume the partial frame, duplicate earlier chunks, or wait for it.
	var frame bytes.Buffer
	tail := jobstream.NewEncoder(&frame)
	_, err = tail.Write(jobstream.Stderr, []byte("tail"))
	require.NoError(t, err)
	raw := frame.Bytes()
	// Tail encoder starts at zero; set the already emitted stderr offset.
	raw[8] = 7
	_, err = f.Write(raw[:14])
	require.NoError(t, err)
	partial, err := readFederationJobOutput(f, j, c, jobstream.MaxChunk)
	require.NoError(t, err)
	require.Empty(t, partial.Chunks)
	require.Equal(t, second.Cursor, partial.Cursor)
	_, err = f.Write(raw[14:])
	require.NoError(t, err)
	appended, err := readFederationJobOutput(f, j, c, jobstream.MaxChunk)
	require.NoError(t, err)
	require.Len(t, appended.Chunks, 1)
	b, err := base64.StdEncoding.DecodeString(appended.Chunks[0].Data)
	require.NoError(t, err)
	require.Equal(t, "tail", string(b))
	c, err = decodeJobOutputCursor(appended.Cursor, j)
	require.NoError(t, err)
	empty, err := readFederationJobOutput(f, j, c, jobstream.MaxChunk)
	require.NoError(t, err)
	require.Empty(t, empty.Chunks)
	c.Offset++
	_, err = readFederationJobOutput(f, j, c, jobstream.MaxChunk)
	require.Error(t, err)
}

func TestIncrementalJobOutputPreservesSplitUTF8AndBoundsSmallWrites(t *testing.T) {
	f, err := os.Create(filepath.Join(testutil.CanonicalTempDir(t), "output.frames"))
	require.NoError(t, err)
	defer f.Close()
	j := &db.FederationJob{ID: "job", Fingerprint: "fp", State: "running"}
	enc := jobstream.NewEncoder(f)
	for _, b := range [][]byte{{0xe2}, {0x82, 0xac}, {0xff, 0x00}} {
		_, err = enc.Write(jobstream.Stdout, b)
		require.NoError(t, err)
	}
	for range 300 {
		_, err = enc.Write(jobstream.Stderr, []byte("x"))
		require.NoError(t, err)
	}
	c := fedJobOutputCursor{Job: j.ID, Fingerprint: j.Fingerprint}
	var stdout, stderr bytes.Buffer
	for i := 0; i < 2; i++ {
		out, err := readFederationJobOutput(f, j, c, jobstream.MaxChunk)
		require.NoError(t, err)
		require.LessOrEqual(t, len(out.Chunks), 256)
		for _, chunk := range out.Chunks {
			require.Equal(t, "base64", chunk.Encoding)
			b, err := base64.StdEncoding.DecodeString(chunk.Data)
			require.NoError(t, err)
			var w io.Writer = &stdout
			if chunk.Stream == "stderr" {
				w = &stderr
			}
			_, err = w.Write(b)
			require.NoError(t, err)
		}
		c, err = decodeJobOutputCursor(out.Cursor, j)
		require.NoError(t, err)
	}
	require.Equal(t, []byte{0xe2, 0x82, 0xac, 0xff, 0x00}, stdout.Bytes())
	require.Equal(t, bytes.Repeat([]byte("x"), 300), stderr.Bytes())
}
