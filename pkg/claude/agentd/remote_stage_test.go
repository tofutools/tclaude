package agentd

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// On the remote-access listener a page's path sits ahead of the passphrase
// session (its frame sends no cookie) but behind the client certificate: a
// connection without one reaches nothing, a page included (tcl-balzk0).
func TestRemoteStagePathNeedsTheClientCertificateNotTheSession(t *testing.T) {
	_, m := startRemoteTestServer(t)
	srv := httptest.NewUnstartedServer(buildRemoteDashboardHandler(m))
	srv.TLS = m.TLSConfig()
	srv.StartTLS()
	t.Cleanup(srv.Close)

	var gotCookie string
	page := stagePages.open("agt_remote", "app", HandlerTransport{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><head></head><body>page "+r.URL.Path+"</body></html>")
	})})
	page.static.Store(true)
	t.Cleanup(func() { stagePages.close(page.token) })
	path := remoteStagePrefix + page.remoteToken + "/index.html"

	get := func(c *http.Client, cookie string) (*http.Response, error) {
		req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Sec-Fetch-Dest", "iframe")
		if cookie != "" {
			req.Header.Set("Cookie", cookie)
		}
		return c.Do(req)
	}

	caPool := x509.NewCertPool()
	caPool.AddCert(mustClientCA(t, m))
	noCert := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: caPool}}}
	if res, err := get(noCert, ""); err == nil {
		_ = res.Body.Close()
		t.Fatal("a page was served without the client certificate")
	}

	res, err := get(clientWithIdentity(t, false, false), remoteCookieName+"=whatever")
	if err != nil {
		t.Fatal(err)
	}
	body := readClose(t, res)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("with the certificate and no session: %d %s", res.StatusCode, body)
	}
	if want := "page /index.html"; !strings.Contains(body, want) {
		t.Fatalf("body %q lacks %q", body, want)
	}
	if gotCookie != "" {
		t.Fatalf("the remote session cookie reached the upstream: %q", gotCookie)
	}

	// The rest of the listener still wants the session.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/snapshot", nil)
	res, err = clientWithIdentity(t, false, false).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = readClose(t, res)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/api without a session: %d", res.StatusCode)
	}
}
