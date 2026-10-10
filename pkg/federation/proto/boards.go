package proto

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

const BoardWSPath = "/v1/boards/connect"
const FrameBoardRequest = "board_request"
const FrameBoardResult = "board_result"
const CodeBoardOnly = "board_only"
const MaxBoardItemBytes = 16 << 20

// Board requests use a distinct signature domain from hub administration.
// The authenticated connection nonce binds every call to this connection.
type BoardRequest HubAdminRequest

func (r *BoardRequest) signingBytes() []byte {
	raw := (*HubAdminRequest)(r).signingBytes()
	return append([]byte("tclaude-board-rpc-v1\n"), raw...)
}
func (r *BoardRequest) Sign(id *Identity) { r.Signature = ed25519.Sign(id.Priv, r.signingBytes()) }
func (r *BoardRequest) Verify(pub ed25519.PublicKey, hubID, nonce string, now time.Time) error {
	if !ValidStreamID(r.ID) || r.HubID != hubID || r.Nonce != nonce || len(r.Method) > 64 || len(r.Payload) > MaxAdminPayload || !json.Valid(r.Payload) || r.IssuedAt.After(now.Add(30*time.Second)) || !r.ExpiresAt.After(now) || r.ExpiresAt.Before(r.IssuedAt) || r.ExpiresAt.Sub(r.IssuedAt) > time.Minute {
		return errors.New("invalid or expired board request")
	}
	if len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, r.signingBytes(), r.Signature) {
		return errors.New("board signature does not verify")
	}
	return nil
}

// Board key envelopes are encrypted directly to an instance key; they grant
// access to this board only and never confer peer trust on either instance.
func SealBoardKey(pub ed25519.PublicKey, board string, epoch int64, key []byte) (*Encrypted, error) {
	if len(key) != chacha20poly1305.KeySize || !ValidStreamID(board) || epoch < 1 {
		return nil, errors.New("invalid board key")
	}
	domain := fmt.Sprintf("tclaude-board-key-v1/%s/%d", board, epoch)
	return encryptPayload(pub, domain, []byte(domain), key)
}
func OpenBoardKey(id *Identity, board string, epoch int64, envelope *Encrypted) ([]byte, error) {
	if !ValidStreamID(board) || epoch < 1 || envelope == nil {
		return nil, errors.New("invalid board key envelope")
	}
	domain := fmt.Sprintf("tclaude-board-key-v1/%s/%d", board, epoch)
	key, err := decryptPayload(id, domain, []byte(domain), envelope)
	if err == nil && len(key) != chacha20poly1305.KeySize {
		return nil, errors.New("invalid board key length")
	}
	return key, err
}

// SealBoardContent authenticates the board, epoch and logical blob ID along
// with its ciphertext. Metadata is encrypted with the same boundary.
func SealBoardContent(key []byte, board string, epoch int64, blob string, plain []byte) ([]byte, error) {
	if !ValidStreamID(board) || !ValidStreamID(blob) || epoch < 1 || len(plain) > MaxBoardItemBytes {
		return nil, errors.New("invalid board content")
	}
	a, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, a.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, err
	}
	return a.Seal(nonce, nonce, plain, []byte(fmt.Sprintf("tclaude-board-blob-v1/%s/%d/%s", board, epoch, blob))), nil
}
func OpenBoardContent(key []byte, board string, epoch int64, blob string, ciphertext []byte) ([]byte, error) {
	if !ValidStreamID(board) || !ValidStreamID(blob) || epoch < 1 || len(ciphertext) > MaxBoardItemBytes+64 {
		return nil, errors.New("invalid board content")
	}
	a, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < a.NonceSize() {
		return nil, errors.New("short board ciphertext")
	}
	return a.Open(nil, ciphertext[:a.NonceSize()], ciphertext[a.NonceSize():], []byte(fmt.Sprintf("tclaude-board-blob-v1/%s/%d/%s", board, epoch, blob)))
}

// BoardKeyProof binds a member's identity to possession of an epoch key. A
// hub cannot inject a new rotation recipient using unverified roster metadata.
// Members already know the key and can disclose it: this protects against the
// storage/relay host, not against a member intentionally sharing its content.
func BoardKeyProof(key []byte, board string, epoch int64, instance string) []byte {
	mac := hmac.New(sha256.New, key)
	raw, _ := json.Marshal([]string{"tclaude-board-member-key-v1", board, strconv.FormatInt(epoch, 10), instance})
	mac.Write(raw)
	return mac.Sum(nil)
}
func VerifyBoardKeyProof(key []byte, board string, epoch int64, instance string, proof []byte) bool {
	return len(key) == 32 && ValidStreamID(board) && epoch > 0 && ValidInstanceID(instance) && hmac.Equal(BoardKeyProof(key, board, epoch, instance), proof)
}
