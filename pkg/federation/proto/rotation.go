package proto

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

const MaxRotationHops = 4
const KindIdentityRotation = "identity_rotation"

// Rotation is public, independently verifiable evidence of a key succession.
// It never changes the key-derived meaning of an instance ID.
type Rotation struct {
	Version      int       `json:"version"`
	ID           string    `json:"id"`
	OldID        string    `json:"old_id"`
	NewID        string    `json:"new_id"`
	OldKey       []byte    `json:"old_key"`
	NewKey       []byte    `json:"new_key"`
	Previous     string    `json:"previous,omitempty"`
	Sequence     int       `json:"sequence"`
	IssuedAt     time.Time `json:"issued_at"`
	ActivateAt   time.Time `json:"activate_at"`
	OldSignature []byte    `json:"old_signature"`
	NewSignature []byte    `json:"new_signature"`
}

func (r Rotation) signingBytes() []byte {
	r.OldSignature, r.NewSignature = nil, nil
	raw, _ := json.Marshal(r)
	return append([]byte("tclaude-fed-identity-rotation-v1\n"), raw...)
}

func NewRotation(old, next *Identity, previous string, sequence int, now time.Time, delay time.Duration) (Rotation, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return Rotation{}, err
	}
	r := Rotation{Version: 1, ID: hex.EncodeToString(nonce[:]), OldID: old.ID(), NewID: next.ID(), OldKey: old.Pub, NewKey: next.Pub, Previous: previous, Sequence: sequence, IssuedAt: now.UTC(), ActivateAt: now.Add(delay).UTC()}
	r.OldSignature = ed25519.Sign(old.Priv, r.signingBytes())
	r.NewSignature = ed25519.Sign(next.Priv, r.signingBytes())
	return r, r.Verify()
}

func (r Rotation) Verify() error {
	nonce, err := hex.DecodeString(r.ID)
	if err != nil || len(nonce) != 16 || len(r.ID) != 32 || r.Version != 1 || r.Sequence < 1 || r.Sequence > MaxRotationHops || len(r.Previous) > 32 {
		return errors.New("invalid identity rotation header")
	}
	if len(r.OldKey) != ed25519.PublicKeySize || len(r.NewKey) != ed25519.PublicKeySize || InstanceID(r.OldKey) != r.OldID || InstanceID(r.NewKey) != r.NewID || r.OldID == r.NewID {
		return errors.New("identity rotation keys do not match instance IDs")
	}
	if r.IssuedAt.IsZero() || r.ActivateAt.Before(r.IssuedAt) || r.ActivateAt.Sub(r.IssuedAt) > 7*24*time.Hour {
		return errors.New("invalid identity rotation activation time")
	}
	if !ed25519.Verify(r.OldKey, r.signingBytes(), r.OldSignature) || !ed25519.Verify(r.NewKey, r.signingBytes(), r.NewSignature) {
		return errors.New("identity rotation signatures do not verify")
	}
	return nil
}

// VerifyRotationChain verifies each hop against a receiver's own pinned root.
// Acceptance timing, revocation and competing-successor checks are local state.
func VerifyRotationChain(chain []Rotation, root string, rootKey []byte, successor string) error {
	if len(chain) == 0 || len(chain) > MaxRotationHops {
		return errors.New("rotation chain must contain 1..4 hops; re-pair offline peers before rotating further")
	}
	previous := ""
	sequence := 0
	for i, r := range chain {
		if err := r.Verify(); err != nil {
			return err
		}
		if r.OldID != root || string(r.OldKey) != string(rootKey) {
			return errors.New("rotation chain is not rooted at the pinned identity")
		}
		if i > 0 && (r.Previous != previous || r.Sequence != sequence+1) {
			return errors.New("rotation chain predecessor mismatch")
		}
		previous, sequence, root, rootKey = r.ID, r.Sequence, r.NewID, r.NewKey
	}
	if root != successor {
		return errors.New("rotation chain does not end at the authenticated successor")
	}
	return nil
}
