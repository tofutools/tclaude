package proto

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"errors"
	"fmt"
	"io"
	"math/big"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
)

// End-to-end payload encryption.
//
// Every envelope payload is encrypted to the recipient instance so the hub
// routes ciphertext only. The recipient's X25519 key is derived from its
// ed25519 identity key (the standard Edwards→Montgomery map), so no extra key
// needs distributing or pinning: trusting a peer's identity key is also
// trusting its encryption key.
//
// Scheme (ECIES-style): the sender draws an ephemeral X25519 key, computes
// shared = X25519(eph, recipient), derives key = HKDF-SHA256(shared,
// salt=envelope id, info="tclaude-fed-payload-v1"||eph_pub||recipient_pub),
// and seals the payload with ChaCha20-Poly1305 (random nonce, AAD = envelope
// id). The whole envelope, ciphertext included, is then signed with the
// sender's identity key as before, which authenticates the sender; the
// ephemeral key gives forward secrecy against a later compromise of the
// sender's key (not of the recipient's).

// Encrypted is an envelope's sealed payload.
type Encrypted struct {
	EphemeralPub []byte `json:"epk"`
	Nonce        []byte `json:"nonce"`
	Ciphertext   []byte `json:"ct"`
}

var curveP = func() *big.Int {
	p := new(big.Int).Lsh(big.NewInt(1), 255)
	return p.Sub(p, big.NewInt(19))
}()

// X25519PublicFromEd25519 maps an ed25519 public key to its X25519 public
// key: u = (1 + y) / (1 - y) mod p.
func X25519PublicFromEd25519(pub ed25519.PublicKey) ([]byte, error) {
	if len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("bad ed25519 public key length")
	}
	le := make([]byte, 32)
	copy(le, pub)
	le[31] &= 0x7f // drop the sign bit of x
	y := new(big.Int).SetBytes(reverse(le))
	if y.Cmp(curveP) >= 0 {
		return nil, errors.New("non-canonical ed25519 public key")
	}
	one := big.NewInt(1)
	num := new(big.Int).Add(one, y)
	den := new(big.Int).Sub(one, y)
	den.Mod(den, curveP)
	if den.Sign() == 0 {
		return nil, errors.New("ed25519 public key maps to the point at infinity")
	}
	inv := new(big.Int).ModInverse(den, curveP)
	if inv == nil {
		return nil, errors.New("ed25519 public key has no X25519 image")
	}
	u := num.Mul(num, inv)
	u.Mod(u, curveP)
	out := make([]byte, 32)
	b := u.Bytes()
	copy(out[32-len(b):], b)
	return reverse(out), nil
}

// x25519Private derives the identity's X25519 scalar the way ed25519 derives
// its signing scalar (SHA-512 of the seed, low half; X25519 clamps).
func (i *Identity) x25519Private() []byte {
	h := sha512.Sum512(i.Priv.Seed())
	return h[:32]
}

func reverse(b []byte) []byte {
	out := make([]byte, len(b))
	for i := range b {
		out[len(b)-1-i] = b[i]
	}
	return out
}

func payloadKey(shared, envelopeID, ephPub, recipientPub []byte) ([]byte, error) {
	info := append([]byte("tclaude-fed-payload-v1"), ephPub...)
	info = append(info, recipientPub...)
	key := make([]byte, chacha20poly1305.KeySize)
	if _, err := io.ReadFull(hkdf.New(sha256.New, shared, []byte(envelopeID), info), key); err != nil {
		return nil, err
	}
	return key, nil
}

// encryptPayload seals plaintext to the recipient's identity key.
func encryptPayload(recipient ed25519.PublicKey, envelopeID string, plaintext []byte) (*Encrypted, error) {
	rpub, err := X25519PublicFromEd25519(recipient)
	if err != nil {
		return nil, err
	}
	eph := make([]byte, curve25519.ScalarSize)
	if _, err := rand.Read(eph); err != nil {
		return nil, err
	}
	ephPub, err := curve25519.X25519(eph, curve25519.Basepoint)
	if err != nil {
		return nil, err
	}
	shared, err := curve25519.X25519(eph, rpub)
	if err != nil {
		return nil, fmt.Errorf("x25519: %w", err)
	}
	key, err := payloadKey(shared, []byte(envelopeID), ephPub, rpub)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return &Encrypted{EphemeralPub: ephPub, Nonce: nonce, Ciphertext: aead.Seal(nil, nonce, plaintext, []byte(envelopeID))}, nil
}

// ErrDecrypt is returned when a payload does not decrypt for this instance.
var ErrDecrypt = errors.New("envelope payload does not decrypt")

// decryptPayload opens an Encrypted addressed to id.
func decryptPayload(id *Identity, envelopeID string, enc *Encrypted) ([]byte, error) {
	if enc == nil || len(enc.EphemeralPub) != curve25519.PointSize || len(enc.Nonce) != chacha20poly1305.NonceSize {
		return nil, ErrDecrypt
	}
	rpub, err := X25519PublicFromEd25519(id.Pub)
	if err != nil {
		return nil, err
	}
	shared, err := curve25519.X25519(id.x25519Private(), enc.EphemeralPub)
	if err != nil {
		return nil, ErrDecrypt
	}
	key, err := payloadKey(shared, []byte(envelopeID), enc.EphemeralPub, rpub)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	pt, err := aead.Open(nil, enc.Nonce, enc.Ciphertext, []byte(envelopeID))
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}
