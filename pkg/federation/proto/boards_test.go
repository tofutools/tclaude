package proto

import (
	"crypto/rand"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestBoardEncryptionDomains(t *testing.T) {
	id, err := NewIdentity()
	require.NoError(t, err)
	key := make([]byte, 32)
	_, err = rand.Read(key)
	require.NoError(t, err)
	board, blob := NewEnvelopeID(), NewEnvelopeID()
	enc, err := SealBoardKey(id.Pub, board, 1, key)
	require.NoError(t, err)
	opened, err := OpenBoardKey(id, board, 1, enc)
	require.NoError(t, err)
	require.Equal(t, key, opened)
	_, err = OpenBoardKey(id, board, 2, enc)
	require.Error(t, err)
	ciphertext, err := SealBoardContent(key, board, 1, blob, []byte("private context"))
	require.NoError(t, err)
	raw, err := OpenBoardContent(key, board, 1, blob, ciphertext)
	require.NoError(t, err)
	require.Equal(t, "private context", string(raw))
	_, err = OpenBoardContent(key, board, 1, NewEnvelopeID(), ciphertext)
	require.Error(t, err)
	now := time.Now()
	r := &BoardRequest{ID: NewEnvelopeID(), HubID: "hub", Nonce: "connection", Method: "boards.list", Payload: json.RawMessage(`{}`), IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
	r.Sign(id)
	require.NoError(t, r.Verify(id.Pub, "hub", "connection", now))
	require.Error(t, (*HubAdminRequest)(r).Verify(id.Pub, "hub", "connection", now), "board signatures must not authorize admin calls")
	r.Method = "boards.create"
	require.Error(t, r.Verify(id.Pub, "hub", "connection", now))
}
