package proto

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const KindEnrollRequest = "enroll_request"
const KindEnrollResult = "enroll_result"
const MaxEnrollmentBytes = 8192
const enrollmentDomain = "tclaude-enrollment-v1\x00"

// EnrollmentClaims are public, signed consent terms. SecretHash commits the
// bearer secret without persisting it. The master key is pinned by this token,
// never by a hub-provided name or an unauthenticated enrollment response.
type EnrollmentClaims struct {
	Version         int       `json:"version"`
	TokenID         string    `json:"token_id"`
	Master          string    `json:"master"`
	MasterKey       []byte    `json:"master_key"`
	ProfileID       string    `json:"profile_id"`
	ProfileName     string    `json:"profile_name"`
	ProfileRevision int64     `json:"profile_revision"`
	TrustLevel      string    `json:"trust_level"`
	SecretHash      []byte    `json:"secret_hash"`
	ExpiresAt       time.Time `json:"expires_at"`
}
type EnrollmentToken struct {
	Claims EnrollmentClaims
	// Public is the version, encoded claims and signature, with NO bearer secret.
	Public string
}

var ErrEnrollmentToken = errors.New("invalid enrollment token")

func NewEnrollmentToken(id *Identity, profileID, name string, revision int64, level string, expires time.Time) (string, *EnrollmentToken, error) {
	secret := make([]byte, 32)
	if _, e := rand.Read(secret); e != nil {
		return "", nil, e
	}
	hash := sha256.Sum256(secret)
	claims := EnrollmentClaims{Version: 1, TokenID: NewEnvelopeID(), Master: id.ID(), MasterKey: append([]byte{}, id.Pub...), ProfileID: profileID, ProfileName: name, ProfileRevision: revision, TrustLevel: level, SecretHash: hash[:], ExpiresAt: expires.UTC()}
	if !validEnrollmentClaims(claims) {
		return "", nil, ErrEnrollmentToken
	}
	raw, e := json.Marshal(claims)
	if e != nil {
		return "", nil, e
	}
	sig := ed25519.Sign(id.Priv, append([]byte(enrollmentDomain), raw...))
	enc := base64.RawURLEncoding
	public := "tcle1." + enc.EncodeToString(raw) + "." + enc.EncodeToString(sig)
	return public + "." + enc.EncodeToString(secret), &EnrollmentToken{Claims: claims, Public: public}, nil
}

// ParseEnrollmentToken verifies the commitment and signature. Expiry is checked
// separately by admission so an already-bound key can recover a lost receipt.
// Errors never include input bytes, which may contain the bearer secret.
func ParseEnrollmentToken(bearer string) (*EnrollmentToken, error) {
	if len(bearer) > MaxEnrollmentBytes {
		return nil, ErrEnrollmentToken
	}
	parts := strings.Split(bearer, ".")
	if len(parts) != 4 {
		return nil, ErrEnrollmentToken
	}
	t, e := ParsePublicEnrollmentToken(strings.Join(parts[:3], "."))
	if e != nil {
		return nil, e
	}
	secret, e := base64.RawURLEncoding.DecodeString(parts[3])
	if e != nil || len(secret) != 32 {
		return nil, ErrEnrollmentToken
	}
	hash := sha256.Sum256(secret)
	if subtle.ConstantTimeCompare(hash[:], t.Claims.SecretHash) != 1 {
		return nil, ErrEnrollmentToken
	}
	return t, nil
}
func ParsePublicEnrollmentToken(public string) (*EnrollmentToken, error) {
	if len(public) > MaxEnrollmentBytes {
		return nil, ErrEnrollmentToken
	}
	parts := strings.Split(public, ".")
	if len(parts) != 3 || parts[0] != "tcle1" {
		return nil, ErrEnrollmentToken
	}
	enc := base64.RawURLEncoding
	raw, e := enc.DecodeString(parts[1])
	if e != nil {
		return nil, ErrEnrollmentToken
	}
	sig, e := enc.DecodeString(parts[2])
	if e != nil || len(sig) != ed25519.SignatureSize {
		return nil, ErrEnrollmentToken
	}
	var claims EnrollmentClaims
	if json.Unmarshal(raw, &claims) != nil || !validEnrollmentClaims(claims) {
		return nil, ErrEnrollmentToken
	}
	if !ed25519.Verify(ed25519.PublicKey(claims.MasterKey), append([]byte(enrollmentDomain), raw...), sig) {
		return nil, ErrEnrollmentToken
	}
	return &EnrollmentToken{Claims: claims, Public: public}, nil
}
func validEnrollmentClaims(c EnrollmentClaims) bool {
	tokenID, e := hex.DecodeString(c.TokenID)
	return e == nil && len(tokenID) == 16 && c.Version == 1 && len(c.MasterKey) == ed25519.PublicKeySize && c.Master == InstanceID(c.MasterKey) && c.ProfileID != "" && len(c.ProfileID) <= 64 && c.ProfileRevision > 0 && c.ProfileName != "" && len(c.ProfileName) <= 64 && SafeName(c.ProfileName, true) == c.ProfileName && (c.TrustLevel == "restricted" || c.TrustLevel == "unrestricted") && len(c.SecretHash) == sha256.Size && !c.ExpiresAt.IsZero()
}

type EnrollmentRequest struct {
	Token string `json:"token"`
}
type EnrollmentResult struct {
	Accepted        bool   `json:"accepted"`
	Code            string `json:"code,omitempty"`
	TokenID         string `json:"token_id"`
	Node            string `json:"node"`
	ProfileID       string `json:"profile_id,omitempty"`
	ProfileRevision int64  `json:"profile_revision,omitempty"`
	TrustLevel      string `json:"trust_level,omitempty"`
}
