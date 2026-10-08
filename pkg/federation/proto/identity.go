// Package proto holds the wire-level building blocks of tclaude federation:
// instance identities, signed envelopes, and the WebSocket frames exchanged
// between an agentd instance and a hub.
//
// The package is deliberately free of tclaude state (no DB, no config paths)
// so the hub binary, agentd, and test peers share exactly one implementation
// of the protocol.
package proto

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// InstanceIDPrefix prefixes every instance id.
const InstanceIDPrefix = "inst_"

// instanceIDHashChars is how many base32 characters of the key hash an
// instance id carries: 26 chars = 130 bits, far beyond collision range.
const instanceIDHashChars = 26

const pemType = "TCLAUDE FEDERATION ED25519 PRIVATE KEY"

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// Identity is an instance's signing keypair.
type Identity struct {
	Priv ed25519.PrivateKey
	Pub  ed25519.PublicKey
}

// ID returns the instance id derived from the public key.
func (i *Identity) ID() string { return InstanceID(i.Pub) }

// InstanceID derives the instance id from a public key. Ids are derived
// rather than assigned so a hub cannot swap the key behind an id: a receiver
// always recomputes the id from the key it was handed.
func InstanceID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return InstanceIDPrefix + strings.ToLower(b32.EncodeToString(sum[:]))[:instanceIDHashChars]
}

// ValidInstanceID reports whether s is syntactically an instance id.
func ValidInstanceID(s string) bool {
	if !strings.HasPrefix(s, InstanceIDPrefix) || len(s) != len(InstanceIDPrefix)+instanceIDHashChars {
		return false
	}
	for _, c := range s[len(InstanceIDPrefix):] {
		if (c < 'a' || c > 'z') && (c < '2' || c > '7') {
			return false
		}
	}
	return true
}

// Fingerprint renders the instance id hash in 4-char groups for humans
// comparing ids out of band.
func Fingerprint(pub ed25519.PublicKey) string {
	h := strings.TrimPrefix(InstanceID(pub), InstanceIDPrefix)
	var parts []string
	for len(h) > 4 {
		parts = append(parts, h[:4])
		h = h[4:]
	}
	parts = append(parts, h)
	return strings.Join(parts, "-")
}

// NewIdentity generates a fresh keypair.
func NewIdentity() (*Identity, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Identity{Priv: priv, Pub: pub}, nil
}

// LoadOrCreateIdentity loads the key at path, creating it (0600, parent
// 0700) when absent.
func LoadOrCreateIdentity(path string) (*Identity, error) {
	id, err := LoadIdentity(path)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	id, err = NewIdentity()
	if err != nil {
		return nil, err
	}
	if err := SaveIdentity(path, id); err != nil {
		return nil, err
	}
	return id, nil
}

// LoadIdentity reads a key written by SaveIdentity.
func LoadIdentity(path string) (*Identity, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != pemType || len(block.Bytes) != ed25519.SeedSize {
		return nil, fmt.Errorf("federation identity %s: not a %s", path, pemType)
	}
	priv := ed25519.NewKeyFromSeed(block.Bytes)
	return &Identity{Priv: priv, Pub: priv.Public().(ed25519.PublicKey)}, nil
}

// SaveIdentity writes the key atomically with 0600 permissions. It refuses
// to overwrite an existing file; rotation is an explicit remove-then-create.
func SaveIdentity(path string, id *Identity) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data := pem.EncodeToMemory(&pem.Block{Type: pemType, Bytes: id.Priv.Seed()})
	tmp, err := os.CreateTemp(filepath.Dir(path), ".instance-key-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Link instead of rename so an existing key is never replaced.
	if err := os.Link(tmp.Name(), path); err != nil {
		return err
	}
	return nil
}

// InstanceFingerprint renders an already verified key-derived ID for an
// operator comparing admission recovery targets before the key is online.
func InstanceFingerprint(instance string) string {
	if !ValidInstanceID(instance) {
		return ""
	}
	h := strings.TrimPrefix(instance, InstanceIDPrefix)
	parts := []string{}
	for len(h) > 4 {
		parts = append(parts, h[:4])
		h = h[4:]
	}
	return strings.Join(append(parts, h), "-")
}
