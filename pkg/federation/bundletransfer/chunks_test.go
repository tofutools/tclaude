package bundletransfer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestChunkFetchResumesOnlyVerifiedPrefix(t *testing.T) {
	spool := Spool{Root: testutil.CanonicalTempDir(t)}
	peer, err := proto.NewIdentity()
	require.NoError(t, err)
	raw := bytes.Repeat([]byte("archive bytes"), int(ChunkBytes/13)+200)
	d := New(Agent, raw, "fixture", time.Now().Add(time.Hour))
	digest := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
	n, err := spool.PartialBytes(peer.ID(), d)
	require.NoError(t, err)
	require.Zero(t, n)
	require.NoError(t, spool.ReceiveChunk(peer.ID(), d, 0, ChunkBytes, digest(raw[:ChunkBytes]), bytes.NewReader(raw[:ChunkBytes])))
	tail := raw[ChunkBytes:]
	require.Error(t, spool.ReceiveChunk(peer.ID(), d, ChunkBytes, int64(len(tail)), digest(tail), brokenEOF{Reader: bytes.NewReader(tail)}))
	n, err = spool.PartialBytes(peer.ID(), d)
	require.NoError(t, err)
	require.Equal(t, ChunkBytes, n)
	// Reopening the spool models a receiving daemon restart.
	spool = Spool{Root: spool.Root}
	require.NoError(t, spool.ReceiveChunk(peer.ID(), d, n, int64(len(tail)), digest(tail), bytes.NewReader(tail)))
	require.NoError(t, spool.FinishChunks(peer.ID(), d))
	f, err := spool.Open("in", peer.ID(), d)
	require.NoError(t, err)
	defer f.Close()
	require.NoError(t, d.VerifyReader(f))
}

func TestFileDescriptorUsesStreamAndExplicitCap(t *testing.T) {
	f, err := os.CreateTemp(testutil.CanonicalTempDir(t), "archive")
	require.NoError(t, err)
	defer f.Close()
	_, err = io.Copy(f, bytes.NewReader(bytes.Repeat([]byte("x"), InlineLimit+1)))
	require.NoError(t, err)
	kind := Agent
	kind.MaxBytes = InlineLimit
	_, err = NewFile(kind, f, "fixture", time.Now().Add(time.Hour))
	require.ErrorContains(t, err, "federation.agent_transfer_max_bytes")
	kind.MaxBytes = InlineLimit + 1
	d, err := NewFile(kind, f, "fixture", time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Empty(t, d.Inline)
	require.NoError(t, d.VerifyReader(f))
}
