package agentd

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/remoteaccess"
	pkcs12 "software.sslmate.com/src/go-pkcs12"
)

// startRemoteTestServer sets up remote-access material in a temp home, mounts
// the remote auth middleware over a tiny stub dashboard mux (so the test
// doesn't need the full agentd world), and returns the TLS server plus the
// loaded material. The stub /api/ping handler goes through the real
// checkDashboardAuth, so the test exercises the pre-auth honouring too.
func startRemoteTestServer(t *testing.T) (*httptest.Server, *remoteaccess.Material) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	if _, err := remoteaccess.Setup(remoteaccess.SetupOptions{
		Bind:        "0.0.0.0:8443",
		Passphrase:  "lan-passphrase",
		ClientName:  "phone",
		P12Password: "p12pw",
	}); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	m, err := remoteaccess.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleDashboardRoot)
	mux.HandleFunc("/api/ping", func(w http.ResponseWriter, r *http.Request) {
		if !checkDashboardAuth(w, r) {
			return
		}
		_, _ = w.Write([]byte("pong"))
	})

	srv := httptest.NewUnstartedServer(remoteAuthMiddleware(m, mux))
	srv.TLS = m.TLSConfig()
	srv.StartTLS()
	t.Cleanup(srv.Close)
	// Reset the global login limiter so tests don't bleed into each other.
	remoteLoginSucceeded()
	return srv, m
}

// clientWithIdentity builds an HTTPS client that presents the issued client
// identity (from the .p12) and trusts the CA. withJar adds a cookie jar so a
// login persists across requests. followRedirects=false lets a test inspect a
// 3xx.
func clientWithIdentity(t *testing.T, withJar, followRedirects bool) *http.Client {
	t.Helper()
	pfx, err := os.ReadFile(clientP12Path(t))
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
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{cert.Raw}, PrivateKey: key}},
		RootCAs:      caPool,
	}}}
	if withJar {
		jar, _ := cookiejar.New(nil)
		c.Jar = jar
	}
	if !followRedirects {
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	return c
}

func clientP12Path(t *testing.T) string {
	t.Helper()
	// Default .p12 location chosen by Setup: <remoteaccess.Dir>/clients/phone.p12.
	return remoteaccess.Dir() + "/clients/phone.p12"
}

// TestRemoteListener_MTLSRequired: a client with no client certificate cannot
// even complete the TLS handshake — the first factor is enforced at the
// transport, before any handler.
func TestRemoteListener_MTLSRequired(t *testing.T) {
	srv, m := startRemoteTestServer(t)

	caPool := x509.NewCertPool()
	caPool.AddCert(mustClientCA(t, m))
	noCert := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: caPool}}}
	if _, err := noCert.Get(srv.URL + "/login"); err == nil {
		t.Fatal("request without a client cert was accepted; mTLS not enforced")
	}
}

// TestRemoteListener_PassphraseGate: with a valid client cert but no session,
// API calls are 401 and page loads redirect to /login; a correct passphrase
// mints a cookie that then unlocks the page + API, and pre-auth is honoured by
// the shared checkDashboardAuth.
func TestRemoteListener_PassphraseGate(t *testing.T) {
	srv, _ := startRemoteTestServer(t)

	// No session yet: /api/ping is 401.
	noSession := clientWithIdentity(t, false, false)
	resp, err := noSession.Get(srv.URL + "/api/ping")
	if err != nil {
		t.Fatalf("GET /api/ping: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("/api/ping without session = %d, want 401", resp.StatusCode)
	}

	// Page load redirects to /login.
	resp, err = noSession.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Errorf("GET / unauthenticated = %d Location=%q, want 303 /login", resp.StatusCode, resp.Header.Get("Location"))
	}

	// The login form renders.
	resp, err = noSession.Get(srv.URL + "/login")
	if err != nil {
		t.Fatalf("GET /login: %v", err)
	}
	body := readClose(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "passphrase") {
		t.Errorf("GET /login = %d, body has passphrase=%v", resp.StatusCode, strings.Contains(body, "passphrase"))
	}

	// Wrong passphrase is rejected.
	jarClient := clientWithIdentity(t, true, false)
	resp, err = jarClient.PostForm(srv.URL+"/login", url.Values{"passphrase": {"wrong"}})
	if err != nil {
		t.Fatalf("POST /login wrong: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong passphrase = %d, want 401", resp.StatusCode)
	}

	// Correct passphrase mints a session cookie + redirects.
	resp, err = jarClient.PostForm(srv.URL+"/login", url.Values{"passphrase": {"lan-passphrase"}})
	if err != nil {
		t.Fatalf("POST /login correct: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("correct passphrase = %d, want 303", resp.StatusCode)
	}

	// With the session: /api/ping is served (pre-auth honoured by
	// checkDashboardAuth) and the dashboard page loads.
	resp, err = jarClient.Get(srv.URL + "/api/ping")
	if err != nil {
		t.Fatalf("GET /api/ping authed: %v", err)
	}
	if got := readClose(t, resp); resp.StatusCode != http.StatusOK || got != "pong" {
		t.Errorf("authed /api/ping = %d %q, want 200 pong", resp.StatusCode, got)
	}

	resp, err = jarClient.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET / authed: %v", err)
	}
	if body := readClose(t, resp); resp.StatusCode != http.StatusOK || !strings.Contains(body, "<!DOCTYPE html>") {
		t.Errorf("authed GET / = %d, looks like dashboard=%v", resp.StatusCode, strings.Contains(body, "<!DOCTYPE html>"))
	}
}

// TestDashboardPreAuthed_Default: a request with no remote-auth tag is never
// treated as pre-authed — the loopback path is unaffected by the new flag.
func TestDashboardPreAuthed_Default(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/snapshot", nil)
	if dashboardPreAuthed(r) {
		t.Error("a plain request must not be pre-authed")
	}
}

func mustClientCA(t *testing.T, _ *remoteaccess.Material) *x509.Certificate {
	t.Helper()
	pfx, err := os.ReadFile(clientP12Path(t))
	if err != nil {
		t.Fatalf("read .p12: %v", err)
	}
	_, _, caCerts, err := pkcs12.DecodeChain(pfx, "p12pw")
	if err != nil || len(caCerts) == 0 {
		t.Fatalf("decode .p12 CA: %v", err)
	}
	return caCerts[0]
}

func readClose(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b = append(b, buf[:n]...)
		if err != nil {
			break
		}
	}
	return string(b)
}
