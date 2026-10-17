package proto

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

func selfSignedPEM(t *testing.T, cn string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		DNSNames: []string{cn}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestJoinCodeRoundTripsAndToleratesWrapping(t *testing.T) {
	ca := selfSignedPEM(t, "hub.home")
	exp := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	code, err := EncodeJoinCode(JoinCode{Hub: "wss://hub.home:8470", Invite: "tchi_abc123", Space: "home", Expires: exp, CA: ca})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(code, JoinCodePrefix) || !IsJoinCode("  "+code) {
		t.Fatalf("code %q lacks the prefix", code)
	}
	wrapped := ""
	for i := 0; i < len(code); i += 60 {
		wrapped += code[i:min(i+60, len(code))] + "\n  "
	}
	got, err := DecodeJoinCode(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if got.V != 1 || got.Hub != "wss://hub.home:8470" || got.Invite != "tchi_abc123" || got.Space != "home" || !got.Expires.Equal(exp) || got.CA != ca {
		t.Fatalf("decoded %+v", got)
	}
	certs, err := ParseCABundle(got.CA)
	if err != nil || len(certs) != 1 || certs[0].Subject != "hub.home" || !certs[0].SelfSigned || len(certs[0].SHA256) != 95 {
		t.Fatalf("certs %+v, %v", certs, err)
	}

	bare, err := EncodeJoinCode(JoinCode{Hub: "wss://hub.example"})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := DecodeJoinCode(bare); err != nil || got.Invite != "" || got.CA != "" || !got.Expires.IsZero() {
		t.Fatalf("an address-only code: %+v, %v", got, err)
	}
}

func TestJoinCodeRejectsWhatItCannotTrust(t *testing.T) {
	for name, c := range map[string]JoinCode{
		"http hub":      {Hub: "https://hub.example"},
		"no host":       {Hub: "wss://"},
		"foreign token": {Hub: "wss://hub.example", Invite: "tcle1.secret"},
		"not a cert":    {Hub: "wss://hub.example", CA: "-----BEGIN PRIVATE KEY-----\nMC4CAQAwBQYDK2VwBCIEIA==\n-----END PRIVATE KEY-----\n"},
		"empty bundle":  {Hub: "wss://hub.example", CA: "hello"},
	} {
		if _, err := EncodeJoinCode(c); !errors.Is(err, ErrJoinCode) {
			t.Errorf("%s: encode err %v, want ErrJoinCode", name, err)
		}
	}
	good, _ := EncodeJoinCode(JoinCode{Hub: "wss://hub.example", Invite: "tchi_x"})
	for name, s := range map[string]string{
		"invite":        "tchi_abc",
		"truncated":     good[:len(good)-5],
		"altered":       good[:12] + "!" + good[13:],
		"too long":      JoinCodePrefix + strings.Repeat("A", MaxJoinCodeBytes),
		"future":        JoinCodePrefix + base64.RawURLEncoding.EncodeToString([]byte(`{"v":2,"hub":"wss://hub.example"}`)),
		"unknown field": JoinCodePrefix + base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"hub":"wss://hub.example","run":"x"}`)),
	} {
		if _, err := DecodeJoinCode(s); !errors.Is(err, ErrJoinCode) {
			t.Errorf("%s: decode err %v, want ErrJoinCode", name, err)
		}
	}
	if _, err := DecodeJoinCode(JoinCodePrefix + base64.RawURLEncoding.EncodeToString([]byte(`{"v":2,"hub":"wss://hub.example"}`))); err == nil || !strings.Contains(err.Error(), "update it") {
		t.Errorf("a newer version should say to update, got %v", err)
	}
}
