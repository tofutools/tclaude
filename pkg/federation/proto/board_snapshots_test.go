package proto

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBoardSnapshotSignaturesAndBounds(t *testing.T) {
	id, e := NewIdentity()
	require.NoError(t, e)
	payload := []byte("recipient-sealed bytes")
	sum := sha256.Sum256(payload)
	m := BoardSnapshotManifest{Item: NewEnvelopeID(), Version: NewEnvelopeID(), Name: "worker", Kind: "agent-snapshot", PayloadSHA256: hex.EncodeToString(sum[:]), PayloadBytes: int64(len(payload)), Recipients: []string{id.ID()}, ExpiresAt: time.Now().UTC().Add(time.Hour)}
	m.Sign(id)
	require.NoError(t, m.VerifyPayload(payload))
	require.Error(t, m.VerifyPayload([]byte("different")))
	v := BoardSnapshotVersion{Board: NewEnvelopeID(), Item: m.Item, Version: m.Version, Epoch: 1, Chunks: []BoardSnapshotChunk{{Blob: NewEnvelopeID(), SHA256: m.PayloadSHA256, Bytes: m.PayloadBytes + BoardContentOverhead}}, PayloadSHA256: m.PayloadSHA256, PayloadBytes: m.PayloadBytes, Recipients: m.Recipients, ExpiresAt: m.ExpiresAt, Metadata: []byte("ciphertext")}
	v.Sign(id)
	require.NoError(t, v.Verify())
	for _, mutate := range []func(*BoardSnapshotVersion){
		func(v *BoardSnapshotVersion) { v.Recipients = nil }, func(v *BoardSnapshotVersion) { v.Recipients = []string{id.ID(), id.ID()} }, func(v *BoardSnapshotVersion) { v.ExpiresAt = time.Time{} }, func(v *BoardSnapshotVersion) { v.Chunks = append(v.Chunks, v.Chunks[0]) }, func(v *BoardSnapshotVersion) { v.PayloadBytes++ }, func(v *BoardSnapshotVersion) { v.Metadata = make([]byte, (16<<10)+1) },
	} {
		bad := v
		bad.Chunks = append([]BoardSnapshotChunk(nil), v.Chunks...)
		mutate(&bad)
		bad.Sign(id)
		require.Error(t, bad.Verify())
	}
	changed := v
	changed.ExpiresAt = changed.ExpiresAt.Add(time.Second)
	require.Error(t, changed.Verify(), "expiry is signed")
	max := v
	max.PayloadBytes = MaxBoardSnapshotBytes
	max.Chunks = nil
	for range MaxBoardSnapshotChunks {
		max.Chunks = append(max.Chunks, BoardSnapshotChunk{Blob: NewEnvelopeID(), SHA256: m.PayloadSHA256, Bytes: MaxBoardItemBytes + BoardContentOverhead})
	}
	max.Sign(id)
	require.NoError(t, max.Verify())
	max.PayloadBytes++
	max.Sign(id)
	require.Error(t, max.Verify())
}
