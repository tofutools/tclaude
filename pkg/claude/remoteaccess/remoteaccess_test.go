package remoteaccess_test

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/remoteaccess"
	pkcs12 "software.sslmate.com/src/go-pkcs12"
)

func mustParseCert(t *testing.T, pemBytes []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		t.Fatal("no PEM block")
		return nil // unreachable: t.Fatal exits the goroutine. Kept so staticcheck's
		// SA5011 nil-analysis sees block is non-nil below without relying on it
		// recognizing t.Fatal as terminating (which it does only with a warm cache).
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	return c
}

// TestCertChain: the CA signs server + client certs that verify against it for
// their respective key usages.
func TestCertChain(t *testing.T) {
	caCertPEM, caKeyPEM, err := remoteaccess.GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	caCert := mustParseCert(t, caCertPEM)
	pool := x509.NewCertPool()
	pool.AddCert(caCert)

	serverPEM, _, err := remoteaccess.GenerateServerCert(caCertPEM, caKeyPEM, []string{"127.0.0.1", "localhost"})
	if err != nil {
		t.Fatalf("GenerateServerCert: %v", err)
	}
	if _, err := mustParseCert(t, serverPEM).Verify(x509.VerifyOptions{
		Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("server cert does not verify for ServerAuth: %v", err)
	}

	clientPEM, _, err := remoteaccess.GenerateClientCert(caCertPEM, caKeyPEM, "phone")
	if err != nil {
		t.Fatalf("GenerateClientCert: %v", err)
	}
	clientCert := mustParseCert(t, clientPEM)
	if _, err := clientCert.Verify(x509.VerifyOptions{
		Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Errorf("client cert does not verify for ClientAuth: %v", err)
	}
	if clientCert.Subject.CommonName != "phone" {
		t.Errorf("client CN = %q, want phone", clientCert.Subject.CommonName)
	}
}

func TestExportPKCS12RoundTrip(t *testing.T) {
	caCertPEM, caKeyPEM, _ := remoteaccess.GenerateCA()
	clientPEM, clientKeyPEM, _ := remoteaccess.GenerateClientCert(caCertPEM, caKeyPEM, "phone")
	pfx, err := remoteaccess.ExportPKCS12(clientPEM, clientKeyPEM, caCertPEM, "p12pw")
	if err != nil {
		t.Fatalf("ExportPKCS12: %v", err)
	}
	key, cert, caCerts, err := pkcs12.DecodeChain(pfx, "p12pw")
	if err != nil {
		t.Fatalf("decode .p12: %v", err)
	}
	if key == nil || cert == nil {
		t.Fatal("decoded .p12 missing key or cert")
	}
	if cert.Subject.CommonName != "phone" {
		t.Errorf("p12 cert CN = %q, want phone", cert.Subject.CommonName)
	}
	if len(caCerts) != 1 {
		t.Errorf("p12 CA chain len = %d, want 1", len(caCerts))
	}
	if _, _, err := pkcs12.Decode(pfx, "wrong"); err == nil {
		t.Error("decoding .p12 with the wrong password should fail")
	}
}

func TestPassphraseHashVerify(t *testing.T) {
	h, err := remoteaccess.HashPassphrase("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassphrase: %v", err)
	}
	if !remoteaccess.VerifyPassphraseHash(h, "correct horse battery staple") {
		t.Error("correct passphrase did not verify")
	}
	if remoteaccess.VerifyPassphraseHash(h, "wrong") {
		t.Error("wrong passphrase verified")
	}
	if remoteaccess.VerifyPassphraseHash("garbage", "x") {
		t.Error("malformed hash verified")
	}
	// Two hashes of the same passphrase differ (fresh salt) but both verify.
	h2, _ := remoteaccess.HashPassphrase("correct horse battery staple")
	if h == h2 {
		t.Error("two hashes of the same passphrase are identical (salt not random)")
	}
	if !remoteaccess.VerifyPassphraseHash(h2, "correct horse battery staple") {
		t.Error("second hash did not verify")
	}
}

func TestSignedCookie(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	tok := remoteaccess.SignCookie(key, "human", time.Hour)
	sub, ok := remoteaccess.VerifyCookie(key, tok)
	if !ok || sub != "human" {
		t.Fatalf("round-trip: ok=%v sub=%q", ok, sub)
	}
	// Wrong key fails.
	if _, ok := remoteaccess.VerifyCookie([]byte("different-key-different-key-xxxx"), tok); ok {
		t.Error("cookie verified under the wrong key")
	}
	// Tampered token fails.
	if _, ok := remoteaccess.VerifyCookie(key, tok+"x"); ok {
		t.Error("tampered cookie verified")
	}
	// Expired token fails.
	expired := remoteaccess.SignCookie(key, "human", -time.Second)
	if _, ok := remoteaccess.VerifyCookie(key, expired); ok {
		t.Error("expired cookie verified")
	}
}

// TestSetupRefusesOverwrite: once material exists, Setup must REFUSE to clobber
// the CA (which would invalidate installed client certs) unless
// RegenerateCerts is set — so a stray re-run can't silently rotate everything.
func TestSetupRefusesOverwrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	opts := remoteaccess.SetupOptions{
		Bind: "0.0.0.0:8443", Passphrase: "passphrase-1", ClientName: "phone", P12Password: "p12pw",
	}
	if _, err := remoteaccess.Setup(opts); err != nil {
		t.Fatalf("first Setup: %v", err)
	}

	// Re-run without the flag: refused.
	if _, err := remoteaccess.Setup(opts); err == nil {
		t.Fatal("second Setup without RegenerateCerts should refuse to overwrite existing material")
	}

	// Re-run with the flag: allowed.
	regen := opts
	regen.RegenerateCerts = true
	if _, err := remoteaccess.Setup(regen); err != nil {
		t.Fatalf("Setup with RegenerateCerts should succeed: %v", err)
	}
}

// TestSetupLoadAndMTLS is the end-to-end check: Setup writes material into a
// temp home, Load reads it, and the resulting mTLS TLSConfig accepts the issued
// client identity while refusing a connection that presents no client cert.
func TestSetupLoadAndMTLS(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows

	res, err := remoteaccess.Setup(remoteaccess.SetupOptions{
		Bind:        "0.0.0.0:8443",
		Passphrase:  "hunter2-hunter2",
		ClientName:  "phone",
		P12Password: "p12pw",
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	m, err := remoteaccess.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !m.VerifyPassphrase("hunter2-hunter2") || m.VerifyPassphrase("nope") {
		t.Fatal("loaded material passphrase verification wrong")
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	srv.TLS = m.TLSConfig()
	srv.StartTLS()
	defer srv.Close()

	// Build a client from the issued .p12.
	pfx, err := os.ReadFile(res.P12Path)
	if err != nil {
		t.Fatalf("read .p12: %v", err)
	}
	key, cert, caCerts, err := pkcs12.DecodeChain(pfx, "p12pw")
	if err != nil {
		t.Fatalf("decode .p12: %v", err)
	}
	caPool := x509.NewCertPool()
	for _, c := range caCerts {
		caPool.AddCert(c)
	}
	clientID := tls.Certificate{Certificate: [][]byte{cert.Raw}, PrivateKey: key}

	// With the client cert: accepted.
	withCert := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{clientID},
		RootCAs:      caPool,
	}}}
	resp, err := withCert.Get(srv.URL)
	if err != nil {
		t.Fatalf("mTLS request with client cert failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	// Without the client cert: the handshake must be refused.
	noCert := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: caPool,
	}}}
	if _, err := noCert.Get(srv.URL); err == nil {
		t.Error("mTLS request WITHOUT a client cert was accepted; RequireAndVerifyClientCert not enforced")
	}
}
