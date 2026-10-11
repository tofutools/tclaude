package paf

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"regexp"
)

// AlgEd25519 is the only signature algorithm PAF 0.1 defines.
const AlgEd25519 = "ed25519"

const signatureContext = "paf-manifest-signature-v1\n"

var keyIDRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func signedMessage(manifest []byte) []byte {
	return []byte(signatureContext + hexSHA256(manifest))
}

// Sign produces a detached signature over the exact manifest bytes.
func Sign(manifest []byte, k SigningKey) (Signature, error) {
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
		Signature:      ed25519.Sign(k.Key, signedMessage(manifest)),
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
	if s.Alg != AlgEd25519 {
		return SignatureUnsupported, nil
	}
	if len(s.PublicKey) != ed25519.PublicKeySize {
		return SignatureInvalid, errors.New("public key has the wrong size")
	}
	if !ed25519.Verify(ed25519.PublicKey(s.PublicKey), signedMessage(manifest), s.Signature) {
		return SignatureInvalid, errors.New("signature does not match the manifest")
	}
	return SignatureValid, nil
}

// SignedBy reports whether a valid signature in results carries pub.
func SignedBy(results []SignatureResult, pub ed25519.PublicKey) bool {
	for _, r := range results {
		if r.Status == SignatureValid && bytes.Equal(r.Signature.PublicKey, pub) {
			return true
		}
	}
	return false
}
