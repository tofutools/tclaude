// Package webpush sends Web Push messages: the payload encrypted for one
// browser subscription (RFC 8291, the aes128gcm content coding of RFC 8188)
// and the request signed with this server's VAPID key (RFC 8292). It is
// built on the standard library only: P-256 ECDH, HKDF, AES-GCM and ES256.
//
// A push service (Apple's, Google's, Mozilla's) relays the message to the
// device. It sees the endpoint, the headers and the ciphertext, never the
// plaintext: only the browser holding the subscription's private key can
// read it.
package webpush

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/url"
	"strings"
	"time"
)

// recordSize is the single record's size (RFC 8188 rs). One record holds
// the whole payload, so it is the largest message this package sends:
// 4096 is the size every push service must accept (RFC 8291 §4).
const recordSize = 4096

// MaxPayload is the longest plaintext Encrypt accepts. A push service must
// take a 4096-byte body (RFC 8291 §4); the header (salt 16, rs 4, idlen 1,
// the 65-byte sender key), the padding delimiter and the GCM tag leave this.
const MaxPayload = 4096 - (16 + 4 + 1 + 65) - 1 - 16

// Subscription is what a browser's PushManager.subscribe() hands back: the
// push service URL to post to and the browser's keys, base64url as the
// browser gives them (padding optional).
type Subscription struct {
	Endpoint string
	P256DH   string
	Auth     string
}

// b64 decodes base64url with or without padding.
func b64(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// Keys reads the subscription's public key and auth secret.
func (s Subscription) Keys() (*ecdh.PublicKey, []byte, error) {
	raw, err := b64(s.P256DH)
	if err != nil {
		return nil, nil, fmt.Errorf("p256dh: %w", err)
	}
	pub, err := ecdh.P256().NewPublicKey(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("p256dh: %w", err)
	}
	auth, err := b64(s.Auth)
	if err != nil {
		return nil, nil, fmt.Errorf("auth: %w", err)
	}
	if len(auth) != 16 {
		return nil, nil, fmt.Errorf("auth: want 16 bytes, got %d", len(auth))
	}
	return pub, auth, nil
}

// Validate reports whether the subscription can be sent to: an https
// endpoint and readable keys.
func (s Subscription) Validate() error {
	u, err := url.Parse(s.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("endpoint must be an https URL")
	}
	_, _, err = s.Keys()
	return err
}

// Encrypt encrypts plaintext for the browser key uaPublic and its auth
// secret (RFC 8291 §3.4), as one aes128gcm record (RFC 8188 §2). salt and
// asPrivate are random per message; EncryptWith takes them for tests.
func Encrypt(plaintext []byte, uaPublic *ecdh.PublicKey, auth []byte) ([]byte, error) {
	asPrivate, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, err
	}
	return EncryptWith(plaintext, uaPublic, auth, asPrivate, salt)
}

// EncryptWith is Encrypt with a fixed sender key and salt.
func EncryptWith(plaintext []byte, uaPublic *ecdh.PublicKey, auth []byte, asPrivate *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	if len(plaintext) > MaxPayload {
		return nil, fmt.Errorf("payload is %d bytes, over the %d a push message holds", len(plaintext), MaxPayload)
	}
	if len(salt) != 16 {
		return nil, errors.New("salt must be 16 bytes")
	}
	asPublic := asPrivate.PublicKey().Bytes()
	cek, nonce, err := deriveKeys(asPrivate, uaPublic, asPublic, uaPublic.Bytes(), auth, salt)
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(cek)
	if err != nil {
		return nil, err
	}
	// The last (here: only) record ends with the 0x02 delimiter.
	record := append(append(make([]byte, 0, len(plaintext)+1), plaintext...), 0x02)

	var out bytes.Buffer
	out.Write(salt)
	_ = binary.Write(&out, binary.BigEndian, uint32(recordSize))
	out.WriteByte(byte(len(asPublic)))
	out.Write(asPublic)
	out.Write(gcm.Seal(nil, nonce, record, nil))
	return out.Bytes(), nil
}

// Decrypt is the browser's side of Encrypt: it reads one aes128gcm record
// with the subscription's private key and auth secret. tclaude never
// decrypts a push; this is here so tests can check what a device would
// read.
func Decrypt(body []byte, uaPrivate *ecdh.PrivateKey, auth []byte) ([]byte, error) {
	if len(body) < 21 {
		return nil, errors.New("short body")
	}
	salt := body[:16]
	idlen := int(body[20])
	if len(body) < 21+idlen {
		return nil, errors.New("short header")
	}
	asPublicRaw := body[21 : 21+idlen]
	asPublic, err := ecdh.P256().NewPublicKey(asPublicRaw)
	if err != nil {
		return nil, fmt.Errorf("sender key: %w", err)
	}
	cek, nonce, err := deriveKeys(uaPrivate, asPublic, asPublicRaw, uaPrivate.PublicKey().Bytes(), auth, salt)
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(cek)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nonce, body[21+idlen:], nil)
	if err != nil {
		return nil, err
	}
	// Strip the padding back to the delimiter, which must be 0x02 (last record).
	i := bytes.LastIndexFunc(plain, func(r rune) bool { return r != 0 })
	if i < 0 || plain[i] != 0x02 {
		return nil, errors.New("bad padding delimiter")
	}
	return plain[:i], nil
}

// deriveKeys runs RFC 8291 §3.3-3.4: the ECDH secret between own and peer,
// mixed with the auth secret into the IKM, then the content key and nonce
// from the salt (RFC 8188 §2.2-2.3).
func deriveKeys(own *ecdh.PrivateKey, peer *ecdh.PublicKey, asPublic, uaPublic, auth, salt []byte) (cek, nonce []byte, err error) {
	shared, err := own.ECDH(peer)
	if err != nil {
		return nil, nil, err
	}
	keyInfo := append(append([]byte("WebPush: info\x00"), uaPublic...), asPublic...)
	ikm, err := hkdf.Key(sha256.New, shared, auth, string(keyInfo), 32)
	if err != nil {
		return nil, nil, err
	}
	if cek, err = hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16); err != nil {
		return nil, nil, err
	}
	if nonce, err = hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12); err != nil {
		return nil, nil, err
	}
	return cek, nonce, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// VAPID is this server's application server key (RFC 8292): a P-256
// signing key. Its public half is what the browser subscribes with
// (applicationServerKey); every push request is signed with the private
// half, so a push service only takes messages for a subscription from the
// server it was made for.
type VAPID struct {
	key *ecdsa.PrivateKey
}

// NewVAPID generates a fresh key.
func NewVAPID() (*VAPID, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return &VAPID{key: key}, nil
}

// ParseVAPID reads a key MarshalPrivate wrote.
func ParseVAPID(raw []byte) (*VAPID, error) {
	key, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), raw)
	if err != nil {
		return nil, fmt.Errorf("vapid key: %w", err)
	}
	return &VAPID{key: key}, nil
}

// MarshalPrivate is the private key's raw 32-byte scalar, for storage.
func (v *VAPID) MarshalPrivate() ([]byte, error) { return v.key.Bytes() }

// PublicKey is the uncompressed public point, base64url without padding:
// what the browser takes as applicationServerKey and the k= of the header.
func (v *VAPID) PublicKey() string {
	raw, err := v.key.PublicKey.Bytes()
	if err != nil {
		// A key from GenerateKey or ParseRawPrivateKey is always on the curve.
		panic("webpush: vapid public key: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// vapidLifetime is how long one signature is good for: under the 24 h RFC
// 8292 §2 allows.
const vapidLifetime = 12 * time.Hour

// Authorization is the Authorization header for a push to endpoint, signed
// at now: "vapid t=<JWT>, k=<public key>". sub is the contact claim.
func (v *VAPID) Authorization(endpoint, sub string, now time.Time) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", errors.New("endpoint must be an absolute URL")
	}
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, err := json.Marshal(map[string]any{
		"aud": u.Scheme + "://" + u.Host,
		"exp": now.Add(vapidLifetime).Unix(),
		"sub": sub,
	})
	if err != nil {
		return "", err
	}
	signing := header + "." + enc.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, v.key, digest[:])
	if err != nil {
		return "", err
	}
	// JWS ES256 is r||s, each 32 bytes (RFC 7518 §3.4), not ASN.1.
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return "vapid t=" + signing + "." + enc.EncodeToString(sig) + ", k=" + v.PublicKey(), nil
}

// VerifyAuthorization checks a VAPID Authorization header as a push service
// would: the signature against the k= key, and the audience. It returns the
// claims. tclaude never verifies a push; tests use it as the fake push
// service.
func VerifyAuthorization(header, wantAudience string, now time.Time) (map[string]any, error) {
	rest, ok := strings.CutPrefix(header, "vapid ")
	if !ok {
		return nil, errors.New("not a vapid header")
	}
	var token, key string
	for part := range strings.SplitSeq(rest, ",") {
		part = strings.TrimSpace(part)
		if v, ok := strings.CutPrefix(part, "t="); ok {
			token = v
		} else if v, ok := strings.CutPrefix(part, "k="); ok {
			key = v
		}
	}
	rawKey, err := b64(key)
	if err != nil {
		return nil, fmt.Errorf("k=: %w", err)
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), rawKey)
	if err != nil {
		return nil, fmt.Errorf("k=: %w", err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("t= is not a JWT")
	}
	sig, err := b64(parts[2])
	if err != nil || len(sig) != 64 {
		return nil, errors.New("bad signature encoding")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return nil, errors.New("signature does not verify")
	}
	rawClaims, err := b64(parts[1])
	if err != nil {
		return nil, err
	}
	var claims map[string]any
	if err := json.Unmarshal(rawClaims, &claims); err != nil {
		return nil, err
	}
	if claims["aud"] != wantAudience {
		return nil, fmt.Errorf("aud %v, want %s", claims["aud"], wantAudience)
	}
	if exp, _ := claims["exp"].(float64); int64(exp) <= now.Unix() {
		return nil, errors.New("expired")
	}
	return claims, nil
}
