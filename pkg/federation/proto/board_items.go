package proto

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

// BoardItemManifest is publisher provenance. Re-publication preserves this
// signature and exact payload; edits require a new publisher statement.
type BoardItemManifest struct {
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

func (m BoardItemManifest) signingBytes() []byte {
	m.Signature = nil
	raw, _ := json.Marshal(m)
	return append([]byte("tclaude-board-item-v1\n"), raw...)
}
func (m *BoardItemManifest) Sign(id *Identity) {
	m.Publisher = id.ID()
	m.Pub = id.Pub
	m.Signature = ed25519.Sign(id.Priv, m.signingBytes())
}
func (m BoardItemManifest) Verify() error {
	hash, e := hex.DecodeString(m.SHA256)
	if e != nil || len(hash) != sha256.Size || !ValidStreamID(m.Item) || !ValidStreamID(m.Version) || m.Kind != "config" || m.Name == "" || len(m.Name) > 128 || m.Bytes < 1 || m.Bytes > MaxBoardItemBytes || InstanceID(m.Pub) != m.Publisher || len(m.Pub) != ed25519.PublicKeySize || !ed25519.Verify(m.Pub, m.signingBytes(), m.Signature) {
		return errors.New("invalid publisher-signed board manifest")
	}
	if m.Parent != "" && !ValidStreamID(m.Parent) {
		return errors.New("invalid parent version")
	}
	return nil
}
func (m BoardItemManifest) VerifyPayload(raw []byte) error {
	if e := m.Verify(); e != nil {
		return e
	}
	sum := sha256.Sum256(raw)
	if int64(len(raw)) != m.Bytes || hex.EncodeToString(sum[:]) != m.SHA256 {
		return errors.New("board item payload differs from publisher manifest")
	}
	return nil
}

// BoardItemVersion is a republisher's receipt for ciphertext stored on one
// board. Descriptive metadata and original provenance stay encrypted.
type BoardItemVersion struct {
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

func (v BoardItemVersion) signingBytes() []byte {
	v.Signature = nil
	raw, _ := json.Marshal(v)
	return append([]byte("tclaude-board-version-v1\n"), raw...)
}
func (v *BoardItemVersion) Sign(id *Identity) {
	v.Publisher = id.ID()
	v.Pub = id.Pub
	v.Signature = ed25519.Sign(id.Priv, v.signingBytes())
}
func (v BoardItemVersion) Verify() error {
	hash, e := hex.DecodeString(v.SHA256)
	if e != nil || len(hash) != sha256.Size || !ValidStreamID(v.Board) || !ValidStreamID(v.Item) || !ValidStreamID(v.Version) || !ValidStreamID(v.Blob) || v.Epoch < 1 || v.Bytes < 1 || v.Bytes > MaxBoardItemBytes+64 || len(v.Metadata) > 16<<10 || len(v.Metadata) < 1 || len(v.Pub) != ed25519.PublicKeySize || InstanceID(v.Pub) != v.Publisher || !ed25519.Verify(v.Pub, v.signingBytes(), v.Signature) {
		return errors.New("invalid signed board version")
	}
	if v.Parent != "" && !ValidStreamID(v.Parent) {
		return errors.New("invalid board parent version")
	}
	return nil
}
