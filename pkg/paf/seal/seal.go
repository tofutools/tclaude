// Package seal encrypts PAF files to named recipients with age
// (https://age-encryption.org/v1), using age's standard X25519 recipient
// type. A tclaude instance's age key is derived from its federation
// Ed25519 identity, so any age implementation can decrypt a PAF addressed
// to an instance given that instance's identity. See docs/paf.md
// "Encryption".
package seal

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha512"
	"errors"
	"fmt"
	"io"
	"strings"

	"filippo.io/age"

	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// Ext is the file extension of an encrypted PAF.
const Ext = ".paf.age"

// header starts every age file.
var header = []byte("age-encryption.org/v1\n")

// IsEncrypted reports whether head, the start of a file, is an age file.
func IsEncrypted(head []byte) bool { return bytes.HasPrefix(head, header) }

// Recipient returns the age recipient (age1…) of an instance's federation
// public key: the X25519 image of the Ed25519 key.
func Recipient(pub ed25519.PublicKey) (string, error) {
	u, err := proto.X25519PublicFromEd25519(pub)
	if err != nil {
		return "", err
	}
	return encode("age", u)
}

// Identity returns the age identity (AGE-SECRET-KEY-1…) of an instance's
// federation private key: the X25519 scalar Ed25519 derives from the seed.
func Identity(priv ed25519.PrivateKey) (string, error) {
	h := sha512.Sum512(priv.Seed())
	s, err := encode("AGE-SECRET-KEY-", h[:32])
	return strings.ToUpper(s), err
}

// Encrypt encrypts src to the given age recipients (age1… strings).
func Encrypt(dst io.Writer, src io.Reader, recipients ...string) error {
	if len(recipients) == 0 {
		return errors.New("no recipients")
	}
	var rs []age.Recipient
	seen := map[string]bool{}
	for _, s := range recipients {
		if seen[s] {
			continue
		}
		seen[s] = true
		r, err := age.ParseX25519Recipient(s)
		if err != nil {
			return fmt.Errorf("recipient %q: %w", s, err)
		}
		rs = append(rs, r)
	}
	w, err := age.Encrypt(dst, rs...)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, src); err != nil {
		return err
	}
	return w.Close()
}

// ErrNotRecipient means none of the identities can decrypt the file.
var ErrNotRecipient = errors.New("this node is not a recipient of the encrypted PAF")

// Decrypt returns a reader of src's plaintext, decrypted with any of the
// given age identities (AGE-SECRET-KEY-1… strings).
func Decrypt(src io.Reader, identities ...string) (io.Reader, error) {
	var ids []age.Identity
	for _, s := range identities {
		id, err := age.ParseX25519Identity(s)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	r, err := age.Decrypt(src, ids...)
	var nm *age.NoIdentityMatchError
	if errors.As(err, &nm) {
		return nil, ErrNotRecipient
	}
	return r, err
}
