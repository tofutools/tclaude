package paf

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"unicode"
)

// AlgEd25519 is the only signature algorithm PAF 0.1 defines.
const AlgEd25519 = "ed25519"

const signatureContext = "paf-manifest-signature-v1\n"

var keyIDRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func signedMessage(context string, manifest []byte) []byte {
	return []byte(context + hexSHA256(manifest))
}

// Sign produces a detached signature over the exact manifest bytes.
func Sign(manifest []byte, k SigningKey) (Signature, error) {
	return SignContext(signatureContext, manifest, k)
}

// SignContext signs a manifest under a format's own domain-separation
// prefix (for PAF "paf-manifest-signature-v1\n"), so a signature made for one
// format can never verify a file of another.
func SignContext(context string, manifest []byte, k SigningKey) (Signature, error) {
	if len(k.Key) != ed25519.PrivateKeySize {
		return Signature{}, errors.New("paf: invalid signing key")
	}
	if !keyIDRE.MatchString(k.KeyID) {
		return Signature{}, fmt.Errorf("paf: invalid key id %q", k.KeyID)
	}
	return Signature{
		Alg:            AlgEd25519,
		PublicKey:      k.Key.Public().(ed25519.PublicKey),
		KeyID:          k.KeyID,
		Signer:         k.Signer,
		ManifestSHA256: hexSHA256(manifest),
		Signature:      ed25519.Sign(k.Key, signedMessage(context, manifest)),
	}, nil
}

// SignatureStatus is the outcome of checking one signature.
type SignatureStatus string

const (
	SignatureValid   SignatureStatus = "valid"
	SignatureInvalid SignatureStatus = "invalid"
	// SignatureUnsupported is a signature with an unknown alg: skipped, not
	// an error.
	SignatureUnsupported SignatureStatus = "unsupported"
)

// SignatureResult is one verified signature file.
type SignatureResult struct {
	Path      string
	Signature Signature
	Status    SignatureStatus
	// Err explains an invalid signature.
	Err error
}

// Verify checks one signature against the manifest bytes. It recomputes the
// manifest hash itself; manifest_sha256 is informational. Trust in the
// signer is the caller's decision and must rest on PublicKey alone.
func Verify(manifest []byte, s Signature) (SignatureStatus, error) {
	return VerifyContext(signatureContext, manifest, s)
}

// VerifyContext is Verify under a format's own domain-separation prefix.
func VerifyContext(context string, manifest []byte, s Signature) (SignatureStatus, error) {
	if s.Alg != AlgEd25519 {
		return SignatureUnsupported, nil
	}
	// key_id and signer sit outside what the signature covers, so a reader
	// shows them only when they are plain identifiers and names.
	if !keyIDRE.MatchString(s.KeyID) {
		return SignatureInvalid, errors.New("key_id is not a valid identifier")
	}
	if s.Signer != nil && (!plainText(s.Signer.Kind, 64) || !plainText(s.Signer.Name, 128)) {
		return SignatureInvalid, errors.New("signer has control characters or is too long")
	}
	if len(s.PublicKey) != ed25519.PublicKeySize {
		return SignatureInvalid, errors.New("public key has the wrong size")
	}
	if !ed25519.Verify(ed25519.PublicKey(s.PublicKey), signedMessage(context, manifest), s.Signature) {
		return SignatureInvalid, errors.New("signature does not match the manifest")
	}
	return SignatureValid, nil
}

// plainText reports whether s is at most max bytes with no control
// characters.
func plainText(s string, max int) bool {
	if len(s) > max {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return false
		}
	}
	return true
}

// algRE bounds an unsupported algorithm name a reader repeats back.
var algRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}$`)

// SignedBy reports whether a valid signature in results carries pub.
// RewriteKeys are the keys a rewrite of a may sign its copy with. A signature on
// a PAF covers the workflows it carries, and the unsigned-import policy reads it
// as vouching for their commands. Redacting or consenting vouches for nothing, so
// while workflows stay only a key that already signed the file signs the copy.
func RewriteKeys(a *Archive, keys []SigningKey) []SigningKey {
	if len(a.Manifest.Workflows) == 0 {
		return keys
	}
	return slices.DeleteFunc(slices.Clone(keys), func(k SigningKey) bool {
		pub, ok := k.Key.Public().(ed25519.PublicKey)
		return !ok || !SignedBy(a.Signatures, pub)
	})
}

func SignedBy(results []SignatureResult, pub ed25519.PublicKey) bool {
	for _, r := range results {
		if r.Status == SignatureValid && bytes.Equal(r.Signature.PublicKey, pub) {
			return true
		}
	}
	return false
}
