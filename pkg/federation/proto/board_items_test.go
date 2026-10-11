package proto

import (
	"crypto/sha256"
	"encoding/hex"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestBoardItemPublisherProvenanceAndReceipts(t *testing.T) {
	publisher, e := NewIdentity()
	require.NoError(t, e)
	republisher, e := NewIdentity()
	require.NoError(t, e)
	raw := []byte(`{"format":"tclaude-config-bundle"}`)
	sum := sha256.Sum256(raw)
	m := BoardItemManifest{Item: NewEnvelopeID(), Version: NewEnvelopeID(), Kind: "config", Name: "recipe", SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(raw))}
	m.Sign(publisher)
	require.NoError(t, m.VerifyPayload(raw))
	require.Error(t, m.VerifyPayload(append(raw, ' ')))
	receipt := BoardItemVersion{Board: NewEnvelopeID(), Item: NewEnvelopeID(), Version: NewEnvelopeID(), Epoch: 1, Blob: NewEnvelopeID(), SHA256: m.SHA256, Bytes: m.Bytes, Metadata: []byte("encrypted metadata")}
	receipt.Sign(republisher)
	require.NoError(t, receipt.Verify())
	require.NoError(t, m.Verify(), "republication preserves original provenance")
	receipt.Epoch++
	require.Error(t, receipt.Verify(), "receipt binds epoch")
	m.Name = "forged"
	require.Error(t, m.Verify(), "metadata belongs to publisher signature")
}
