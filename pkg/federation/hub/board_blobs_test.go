package hub_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/federation/hub"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

func TestBoardCiphertextStorageQuotaIntegrityAndAuthority(t *testing.T) {
	_, live, url := newHub(t, hub.Config{})
	owner, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, live.Admit(owner.ID()))
	ws, ch := boardSocket(t, url, owner, "")
	board := proto.NewEnvelopeID()
	result := boardCall(t, ws, owner, ch, "boards.create", map[string]any{"board": board, "name": "objects", "key_proofs": map[string][]byte{owner.ID(): make([]byte, 32)}, "envelopes": map[string]any{owner.ID(): map[string]any{"ct": "opaque"}}})
	require.Equal(t, 200, result.Status)
	data := []byte("encrypted ciphertext fixture")
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	id := proto.NewEnvelopeID()
	require.NoError(t, live.StoreBoardBlob(owner.ID(), board, id, digest, int64(len(data)), bytes.NewReader(data)))
	var output bytes.Buffer
	require.NoError(t, live.ReadBoardBlob(owner.ID(), board, id, &output))
	require.Equal(t, data, output.Bytes())
	outsider, err := proto.NewIdentity()
	require.NoError(t, err)
	require.Error(t, live.ReadBoardBlob(outsider.ID(), board, id, &output))
	require.Error(t, live.StoreBoardBlob(outsider.ID(), board, proto.NewEnvelopeID(), digest, int64(len(data)), bytes.NewReader(data)))
	require.Error(t, live.StoreBoardBlob(owner.ID(), board, id, digest, int64(len(data)), bytes.NewReader(data)), "immutable blob ID")
	bad := proto.NewEnvelopeID()
	require.Error(t, live.StoreBoardBlob(owner.ID(), board, bad, digest, int64(len(data)), strings.NewReader("wrong")))
	require.NoError(t, live.StoreBoardBlob(owner.ID(), board, bad, digest, int64(len(data)), bytes.NewReader(data)), "failed upload releases reservation")
	require.Error(t, live.StoreBoardBlob(owner.ID(), board, proto.NewEnvelopeID(), digest, proto.MaxBoardItemBytes+65, bytes.NewReader(data)))
	// Moderation freezes publication, while readers retain ciphertext access.
	claim, err := live.PrepareAdminClaim(false, time.Now())
	require.NoError(t, err)
	admin, challenge, welcome := adminSocket(t, url, owner)
	require.Equal(t, 200, adminWireCall(t, admin, adminRequest(owner, challenge, welcome, "claim", map[string]any{"token": claim})).Status)
	require.Equal(t, 200, adminWireCall(t, admin, adminRequest(owner, challenge, welcome, "boards.patch", map[string]any{"board": board, "frozen": true, "quota_bytes": int64(1 << 20)})).Status)
	require.Error(t, live.StoreBoardBlob(owner.ID(), board, proto.NewEnvelopeID(), digest, int64(len(data)), bytes.NewReader(data)))
	require.NoError(t, live.ReadBoardBlob(owner.ID(), board, id, &output))
}

func TestBoardMetadataQuotaCountsUTF8Bytes(t *testing.T) {
	_, live, url := newHub(t, hub.Config{})
	owner, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, live.Admit(owner.ID()))
	ws, ch := boardSocket(t, url, owner, "")
	board := proto.NewEnvelopeID()
	box := map[string]any{"ct": strings.Repeat("界", 5000)}
	boxes := map[string]any{owner.ID(): box}
	proofs := map[string][]byte{owner.ID(): make([]byte, 32)}
	r := boardCall(t, ws, owner, ch, "boards.create", map[string]any{"board": board, "name": "byte-quota", "envelopes": boxes, "key_proofs": proofs})
	require.Equal(t, 200, r.Status)
	claim, err := live.PrepareAdminClaim(false, time.Now())
	require.NoError(t, err)
	admin, challenge, welcome := adminSocket(t, url, owner)
	require.Equal(t, 200, adminWireCall(t, admin, adminRequest(owner, challenge, welcome, "claim", map[string]any{"token": claim})).Status)
	require.Equal(t, 200, adminWireCall(t, admin, adminRequest(owner, challenge, welcome, "boards.patch", map[string]any{"board": board, "quota_bytes": int64(1 << 20)})).Status)
	encoded, err := json.Marshal(box)
	require.NoError(t, err)
	used := len(encoded)
	refused := false
	for epoch := int64(2); epoch < 100; epoch++ {
		r = boardCall(t, ws, owner, ch, "keys.rotate", map[string]any{"board": board, "epoch": epoch, "envelopes": boxes, "key_proofs": proofs})
		if r.Status == 409 {
			require.Equal(t, "board_quota", r.Code)
			refused = true
			break
		}
		require.Equal(t, 200, r.Status, r.Error)
		used += len(encoded)
	}
	require.True(t, refused, "multibyte metadata must reach the byte quota")
	require.LessOrEqual(t, used, 1<<20, "retained UTF-8 bytes cannot exceed quota")
}
