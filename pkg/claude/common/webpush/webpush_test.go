package webpush

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	raw, err := b64(s)
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return raw
}

// TestEncrypt_RFC8291Vector checks the encryption byte for byte against the
// worked example in RFC 8291 Appendix A, and that it decrypts back.
func TestEncrypt_RFC8291Vector(t *testing.T) {
	plaintext := []byte("When I grow up, I want to be a watermelon")
	asPrivate, err := ecdh.P256().NewPrivateKey(mustB64(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}
	uaPrivate, err := ecdh.P256().NewPrivateKey(mustB64(t, "q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94"))
	if err != nil {
		t.Fatal(err)
	}
	auth := mustB64(t, "BTBZMqHH6r4Tts7J_aSIgg")
	salt := mustB64(t, "DGv6ra1nlYgDCS1FRnbzlw")

	got, err := EncryptWith(plaintext, uaPrivate.PublicKey(), auth, asPrivate, salt)
	if err != nil {
		t.Fatal(err)
	}
	want := "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
	if enc := base64.RawURLEncoding.EncodeToString(got); enc != want {
		t.Fatalf("ciphertext\n got %s\nwant %s", enc, want)
	}
	back, err := Decrypt(got, uaPrivate, auth)
	if err != nil {
		t.Fatal(err)
	}
	if string(back) != string(plaintext) {
		t.Fatalf("decrypted %q", back)
	}
}

func TestEncrypt_RandomRoundTripAndLimit(t *testing.T) {
	ua, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	msg := []byte(strings.Repeat("x", MaxPayload))
	body, err := Encrypt(msg, ua.PublicKey(), auth)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 4096 {
		t.Fatalf("a full payload is %d bytes on the wire, want 4096", len(body))
	}
	back, err := Decrypt(body, ua, auth)
	if err != nil || string(back) != string(msg) {
		t.Fatalf("round trip: %v", err)
	}
	if _, err := Encrypt(append(msg, 'x'), ua.PublicKey(), auth); err == nil {
		t.Fatal("a payload over MaxPayload must be refused")
	}
	other := make([]byte, 16)
	if _, err := Decrypt(body, ua, other); err == nil {
		t.Fatal("a wrong auth secret must not decrypt")
	}
}

func TestVAPID_SignsAndVerifies(t *testing.T) {
	v, err := NewVAPID()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := v.MarshalPrivate()
	if err != nil {
		t.Fatal(err)
	}
	again, err := ParseVAPID(raw)
	if err != nil || again.PublicKey() != v.PublicKey() {
		t.Fatalf("stored key does not round-trip: %v", err)
	}
	now := time.Unix(1_800_000_000, 0)
	header, err := again.Authorization("https://push.example.net/wpush/abc?x=1", "https://example.invalid/tclaude", now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(header, ", k="+v.PublicKey()) {
		t.Fatalf("header does not carry the public key: %s", header)
	}
	claims, err := VerifyAuthorization(header, "https://push.example.net", now)
	if err != nil {
		t.Fatal(err)
	}
	if claims["sub"] != "https://example.invalid/tclaude" {
		t.Fatalf("claims %v", claims)
	}
	if _, err := VerifyAuthorization(header, "https://other.example", now); err == nil {
		t.Fatal("a wrong audience must not verify")
	}
	if _, err := VerifyAuthorization(header, "https://push.example.net", now.Add(13*time.Hour)); err == nil {
		t.Fatal("an expired token must not verify")
	}
	other, _ := NewVAPID()
	forged := strings.Replace(header, "k="+v.PublicKey(), "k="+other.PublicKey(), 1)
	if _, err := VerifyAuthorization(forged, "https://push.example.net", now); err == nil {
		t.Fatal("a signature under another key must not verify")
	}
}

func TestSubscriptionValidate(t *testing.T) {
	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	key := base64.RawURLEncoding.EncodeToString(ua.PublicKey().Bytes())
	auth := base64.URLEncoding.EncodeToString(make([]byte, 16)) // padded, as some browsers send
	ok := Subscription{Endpoint: "https://push.example.net/x", P256DH: key, Auth: auth}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]Subscription{
		"http endpoint": {Endpoint: "http://push.example.net/x", P256DH: key, Auth: auth},
		"bad key":       {Endpoint: ok.Endpoint, P256DH: "AAAA", Auth: auth},
		"short auth":    {Endpoint: ok.Endpoint, P256DH: key, Auth: "AAAA"},
	} {
		if s.Validate() == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
