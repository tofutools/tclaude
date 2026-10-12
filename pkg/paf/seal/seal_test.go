package seal

import (
	"bytes"
	"crypto/ed25519"
	"io"
	"testing"

	"filippo.io/age"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The keys tclaude derives are ordinary age keys: the identity age parses
// from our encoding has the recipient we derive from the public key.
func TestKeysAreStandardAgeKeys(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	idStr, err := Identity(priv)
	require.NoError(t, err)
	id, err := age.ParseX25519Identity(idStr)
	require.NoError(t, err)
	rec, err := Recipient(pub)
	require.NoError(t, err)
	assert.Equal(t, id.Recipient().String(), rec)

	// And the encoder round-trips age's own keys.
	gen, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	hrp, data, err := decode(gen.String())
	require.NoError(t, err)
	assert.Equal(t, "age-secret-key-", hrp)
	enc, err := encode(hrp, data)
	require.NoError(t, err)
	assert.Equal(t, gen.String(), bytesUpper(enc))
}

func bytesUpper(s string) string { return string(bytes.ToUpper([]byte(s))) }

func TestEncryptDecrypt(t *testing.T) {
	pubA, privA, _ := ed25519.GenerateKey(nil)
	_, privB, _ := ed25519.GenerateKey(nil)
	recA, _ := Recipient(pubA)
	plain := []byte("PK\x03\x04 a paf")

	var enc bytes.Buffer
	require.NoError(t, Encrypt(&enc, bytes.NewReader(plain), recA, recA))
	assert.True(t, IsEncrypted(enc.Bytes()))
	assert.False(t, IsEncrypted(plain))

	idA, _ := Identity(privA)
	r, err := Decrypt(bytes.NewReader(enc.Bytes()), idA)
	require.NoError(t, err)
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, plain, got)

	idB, _ := Identity(privB)
	_, err = Decrypt(bytes.NewReader(enc.Bytes()), idB)
	assert.ErrorIs(t, err, ErrNotRecipient)
	assert.Error(t, Encrypt(io.Discard, bytes.NewReader(plain)))
	assert.Error(t, Encrypt(io.Discard, bytes.NewReader(plain), "age1nope"))
}
