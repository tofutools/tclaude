package proto

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"
)

const BoardSnapshotsCapability = "board-snapshots-v1"
const MaxBoardSnapshotBytes int64 = 64 << 20
const MaxBoardSnapshotChunks = 4
const DefaultBoardSnapshotTTL = 7 * 24 * time.Hour
const MaxBoardSnapshotTTL = 30 * 24 * time.Hour

// Board content uses an XChaCha20 nonce and authentication tag.
const BoardContentOverhead int64 = 40

type BoardSnapshotChunk struct {
	Blob   string `json:"blob"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// BoardSnapshotVersion binds recipient-encrypted payload provenance to ordered
// board-epoch ciphertext chunks. Existing BoardItemVersion bytes stay unchanged.
type BoardSnapshotVersion struct {
	Board         string               `json:"board"`
	Item          string               `json:"item"`
	Version       string               `json:"version"`
	Parent        string               `json:"parent,omitempty"`
	Epoch         int64                `json:"epoch"`
	Chunks        []BoardSnapshotChunk `json:"chunks"`
	PayloadSHA256 string               `json:"payload_sha256"`
	PayloadBytes  int64                `json:"payload_bytes"`
	Recipients    []string             `json:"recipients"`
	ExpiresAt     time.Time            `json:"expires_at"`
	Metadata      []byte               `json:"metadata"`
	Publisher     string               `json:"publisher"`
	Pub           []byte               `json:"pubkey"`
	Signature     []byte               `json:"signature"`
}

func (v BoardSnapshotVersion) signingBytes() []byte {
	v.Signature = nil
	raw, _ := json.Marshal(v)
	return append([]byte("tclaude-board-snapshot-v1\n"), raw...)
}
func (v *BoardSnapshotVersion) Sign(id *Identity) {
	v.Publisher = id.ID()
	v.Pub = id.Pub
	v.Signature = ed25519.Sign(id.Priv, v.signingBytes())
}
func validSnapshotPayload(item, version, digest string, size int64, recipients []string, expiry time.Time) bool {
	hash, e := hex.DecodeString(digest)
	if e != nil || len(hash) != sha256.Size || !ValidStreamID(item) || !ValidStreamID(version) || size < 1 || size > MaxBoardSnapshotBytes || len(recipients) < 1 || len(recipients) > 100 || expiry.IsZero() || expiry.Location() != time.UTC {
		return false
	}
	for i, id := range recipients {
		if !ValidInstanceID(id) || (i > 0 && recipients[i-1] >= id) {
			return false
		}
	}
	return true
}
func (v BoardSnapshotVersion) Verify() error {
	if !validSnapshotPayload(v.Item, v.Version, v.PayloadSHA256, v.PayloadBytes, v.Recipients, v.ExpiresAt) || !ValidStreamID(v.Board) || v.Epoch < 1 || (v.Parent != "" && !ValidStreamID(v.Parent)) || len(v.Chunks) < 1 || len(v.Chunks) > MaxBoardSnapshotChunks || len(v.Metadata) < 1 || len(v.Metadata) > 16<<10 || len(v.Pub) != ed25519.PublicKeySize || InstanceID(v.Pub) != v.Publisher || !ed25519.Verify(v.Pub, v.signingBytes(), v.Signature) {
		return errors.New("invalid signed board snapshot version")
	}
	var size int64
	seen := map[string]bool{}
	for _, c := range v.Chunks {
		hash, e := hex.DecodeString(c.SHA256)
		if e != nil || len(hash) != sha256.Size || !ValidStreamID(c.Blob) || seen[c.Blob] || c.Bytes <= BoardContentOverhead || c.Bytes > MaxBoardItemBytes+BoardContentOverhead {
			return errors.New("invalid board snapshot chunk")
		}
		seen[c.Blob] = true
		size += c.Bytes - BoardContentOverhead
	}
	if size != v.PayloadBytes {
		return errors.New("snapshot chunk lengths differ from payload")
	}
	return nil
}

// BoardSnapshotManifest is the original publisher statement inside encrypted
// metadata. Payload refers to the recipient-sealed bytes, never plaintext PAF.
type BoardSnapshotManifest struct {
	Item          string    `json:"item"`
	Version       string    `json:"version"`
	Name          string    `json:"name"`
	Kind          string    `json:"kind"`
	PayloadSHA256 string    `json:"payload_sha256"`
	PayloadBytes  int64     `json:"payload_bytes"`
	Recipients    []string  `json:"recipients"`
	ExpiresAt     time.Time `json:"expires_at"`
	Publisher     string    `json:"publisher"`
	Pub           []byte    `json:"pubkey"`
	Signature     []byte    `json:"signature"`
}

func (m BoardSnapshotManifest) signingBytes() []byte {
	m.Signature = nil
	raw, _ := json.Marshal(m)
	return append([]byte("tclaude-board-snapshot-manifest-v1\n"), raw...)
}
func (m *BoardSnapshotManifest) Sign(id *Identity) {
	m.Publisher = id.ID()
	m.Pub = id.Pub
	m.Signature = ed25519.Sign(id.Priv, m.signingBytes())
}
func (m BoardSnapshotManifest) Verify() error {
	if !validSnapshotPayload(m.Item, m.Version, m.PayloadSHA256, m.PayloadBytes, m.Recipients, m.ExpiresAt) || m.Kind != "agent-snapshot" || m.Name == "" || len(m.Name) > 128 || !utf8.ValidString(m.Name) || len(m.Pub) != ed25519.PublicKeySize || InstanceID(m.Pub) != m.Publisher || !ed25519.Verify(m.Pub, m.signingBytes(), m.Signature) {
		return errors.New("invalid publisher-signed board snapshot manifest")
	}
	return nil
}
func (m BoardSnapshotManifest) VerifyPayload(raw []byte) error {
	if e := m.Verify(); e != nil {
		return e
	}
	sum := sha256.Sum256(raw)
	if int64(len(raw)) != m.PayloadBytes || hex.EncodeToString(sum[:]) != m.PayloadSHA256 {
		return errors.New("snapshot payload differs from publisher manifest")
	}
	return nil
}
