package remoteaccess

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Passphrase hashing parameters. PBKDF2-HMAC-SHA256 at the OWASP-recommended
// iteration count is a stdlib-only (crypto/pbkdf2, Go 1.24+) password hash —
// no x/crypto/argon2 dependency. The cost is a one-time login on the phone.
const (
	pbkdf2Iter    = 600_000
	pbkdf2SaltLen = 16
	pbkdf2KeyLen  = 32
	pbkdf2Prefix  = "pbkdf2_sha256"
)

// HashPassphrase derives a salted PBKDF2-HMAC-SHA256 hash of pw and returns it
// in a self-describing string: "pbkdf2_sha256$<iter>$<b64salt>$<b64hash>"
// (Django-style), so VerifyPassphraseHash can recompute without separate
// parameter storage. The salt is fresh per call.
func HashPassphrase(pw string) (string, error) {
	salt := make([]byte, pbkdf2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	dk, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, pbkdf2KeyLen)
	if err != nil {
		return "", err
	}
	enc := base64.RawStdEncoding.EncodeToString
	return fmt.Sprintf("%s$%d$%s$%s", pbkdf2Prefix, pbkdf2Iter, enc(salt), enc(dk)), nil
}

// VerifyPassphraseHash reports whether pw matches encoded (produced by
// HashPassphrase), comparing in constant time. A malformed encoded string
// returns false rather than erroring — callers treat it as "wrong passphrase".
func VerifyPassphraseHash(encoded, pw string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != pbkdf2Prefix {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// SignCookie produces a signed, self-expiring session token:
// "<b64payload>.<b64hmac>" where payload is "<subject>|<expiryUnix>" and the
// HMAC is keyed by the persisted cookie key. Because the secret is the
// persisted key (not per-process state), a token stays valid across agentd
// restarts — so a phone logs in once and stays logged in until the TTL lapses.
func SignCookie(key []byte, subject string, ttl time.Duration) string {
	exp := time.Now().Add(ttl).Unix()
	payload := subject + "|" + strconv.FormatInt(exp, 10)
	mac := cookieMAC(key, payload)
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(payload)) + "." + enc(mac)
}

// VerifyCookie validates a token from SignCookie: correct HMAC (constant-time)
// and not expired. Returns the subject and true on success; "", false
// otherwise. Any malformed/expired/tampered token fails closed.
func VerifyCookie(key []byte, value string) (string, bool) {
	dot := strings.IndexByte(value, '.')
	if dot < 0 {
		return "", false
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(value[:dot])
	if err != nil {
		return "", false
	}
	gotMAC, err := base64.RawURLEncoding.DecodeString(value[dot+1:])
	if err != nil {
		return "", false
	}
	payload := string(payloadBytes)
	if !hmac.Equal(gotMAC, cookieMAC(key, payload)) {
		return "", false
	}
	bar := strings.LastIndexByte(payload, '|')
	if bar < 0 {
		return "", false
	}
	exp, err := strconv.ParseInt(payload[bar+1:], 10, 64)
	if err != nil || time.Now().Unix() >= exp {
		return "", false
	}
	return payload[:bar], true
}

func cookieMAC(key []byte, payload string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(payload))
	return m.Sum(nil)
}
