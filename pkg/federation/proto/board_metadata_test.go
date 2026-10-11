package proto

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// Frozen pre-extension wire structs emulate an older daemon's verifier.
type legacyBoardItemManifest struct {
	Item      string `json:"item"`
	Version   string `json:"version"`
	Parent    string `json:"parent,omitempty"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Publisher string `json:"publisher"`
	Pub       []byte `json:"pubkey"`
	SHA256    string `json:"sha256"`
	Bytes     int64  `json:"bytes"`
	Signature []byte `json:"signature"`
}

type legacyBoardItemVersion struct {
	Board     string `json:"board"`
	Item      string `json:"item"`
	Version   string `json:"version"`
	Parent    string `json:"parent,omitempty"`
	Epoch     int64  `json:"epoch"`
	Blob      string `json:"blob"`
	SHA256    string `json:"sha256"`
	Bytes     int64  `json:"bytes"`
	Metadata  []byte `json:"metadata"`
	Publisher string `json:"publisher"`
	Pub       []byte `json:"pubkey"`
	Signature []byte `json:"signature"`
}

func TestBoardDescriptionExtensionAcceptedByLegacyVerifier(t *testing.T) {
	id, e := NewIdentity()
	require.NoError(t, e)
	raw := []byte("verified payload")
	sum := sha256.Sum256(raw)
	m := BoardItemManifest{Item: NewEnvelopeID(), Version: NewEnvelopeID(), Kind: "skill", Name: "Demo", SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(raw))}
	m.Sign(id)
	d := &BoardItemDescription{Description: "A useful skill."}
	d.Sign(m, id)
	metadata := BoardItemMetadata{BoardItemManifest: m, DescriptionProvenance: d}
	encoded, e := json.Marshal(metadata)
	require.NoError(t, e)
	var old legacyBoardItemManifest
	require.NoError(t, json.Unmarshal(encoded, &old))
	sig := old.Signature
	old.Signature = nil
	legacyBytes, e := json.Marshal(old)
	require.NoError(t, e)
	require.True(t, ed25519.Verify(old.Pub, append([]byte("tclaude-board-item-v1\n"), legacyBytes...), sig), "older verifier accepts manifest despite optional metadata")
	v := BoardItemVersion{Board: NewEnvelopeID(), Item: NewEnvelopeID(), Version: NewEnvelopeID(), Blob: NewEnvelopeID(), Epoch: 1, SHA256: m.SHA256, Bytes: 1, Metadata: encoded}
	key := bytes.Repeat([]byte{7}, 32)
	v.Metadata, e = SealBoardContent(key, v.Board, v.Epoch, v.Version, encoded)
	require.NoError(t, e)
	v.Sign(id)
	wire, e := json.Marshal(v)
	require.NoError(t, e)
	var receipt legacyBoardItemVersion
	require.NoError(t, json.Unmarshal(wire, &receipt))
	sig = receipt.Signature
	receipt.Signature = nil
	legacyBytes, e = json.Marshal(receipt)
	require.NoError(t, e)
	require.True(t, ed25519.Verify(receipt.Pub, append([]byte("tclaude-board-version-v1\n"), legacyBytes...), sig), "receipt bytes stay compatible")
	require.Equal(t, d.Description, metadata.Description())
	metadata.DescriptionProvenance.Description = "forged description"
	require.Empty(t, metadata.Description(), "never project unverified description")
	require.NoError(t, metadata.Verify(), "bad optional description cannot hide valid content")
	d.Description = "A useful skill."
	other := m
	other.Item = NewEnvelopeID()
	other.Sign(id)
	require.Error(t, d.Verify(other), "sidecar cannot be attached to another signed item")
}

func TestBoardDescriptionMalformedExtensionDoesNotHideManifest(t *testing.T) {
	id, err := NewIdentity()
	require.NoError(t, err)
	m := BoardItemManifest{Item: NewEnvelopeID(), Version: NewEnvelopeID(), Kind: "skill", Name: "Demo", SHA256: "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", Bytes: 3}
	m.Sign(id)
	raw, err := json.Marshal(m)
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &fields))
	for _, invalid := range []string{`"bad"`, `{"description":"fake","signature":123}`, `{"description":4}`, `["x"]`, `null`} {
		t.Run(invalid, func(t *testing.T) {
			fields["description_provenance"] = json.RawMessage(invalid)
			raw, err := json.Marshal(fields)
			require.NoError(t, err)
			var decoded BoardItemMetadata
			require.NoError(t, json.Unmarshal(raw, &decoded))
			require.NoError(t, decoded.Verify())
			require.Empty(t, decoded.Description())
		})
	}
}
