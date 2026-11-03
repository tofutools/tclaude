package agentd_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
)

// stageFake is the fake page upstream: it records what reached it and answers
// with whatever the test set.
type stageFake struct {
	mu   sync.Mutex
	seen []*http.Request
	h    http.HandlerFunc
}

func (f *stageFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.seen = append(f.seen, r.Clone(r.Context()))
	h := f.h
	f.mu.Unlock()
	if h == nil {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "upstream:"+r.URL.Path)
		return
	}
	h(w, r)
}

func (f *stageFake) last(t *testing.T) *http.Request {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotEmpty(t, f.seen, "the upstream was never reached")
	return f.seen[len(f.seen)-1]
}

func (f *stageFake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.seen)
}

type stageEnv struct {
	srv  *httptest.Server
	port string
	fake *stageFake
	page *agentd.StagePageForTest
}

func newStageEnv(t *testing.T) *stageEnv {
	t.Helper()
	_ = newFlow(t)
	srv := httptest.NewServer(agentd.BuildPopupHandlerForTest())
	t.Cleanup(srv.Close)
	t.Cleanup(agentd.SetPopupBaseURLForTest(srv.URL))
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	require.NoError(t, err)
	fake := &stageFake{}
	page := agentd.OpenStagePageForTest("agt_owner", "landing", agentd.HandlerTransport{Handler: fake})
	t.Cleanup(func() { agentd.CloseStagePageForTest(page.Token) })
	return &stageEnv{srv: srv, port: port, fake: fake, page: page}
}

// do sends a request to the dashboard listener with the given Host. Browser
// frame requests carry Sec-Fetch-Dest: iframe unless the test overrides it.
func (e *stageEnv) do(t *testing.T, method, host, path string, hdr map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+path, nil)
	require.NoError(t, err)
	req.Host = host
	req.Header.Set("Sec-Fetch-Dest", "iframe")
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
			continue
		}
		req.Header.Set(k, v)
	}
	res, err := e.srv.Client().Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

func (e *stageEnv) host() string { return e.page.Host + ":" + e.port }

func readBody(t *testing.T, res *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return string(b)
}

func assertStageCSP(t *testing.T, res *http.Response) {
	t.Helper()
	csp := res.Header.Values("Content-Security-Policy")
	require.Len(t, csp, 1, "exactly one daemon-set CSP")
	assert.Contains(t, csp[0], "sandbox")
	assert.Contains(t, csp[0], "frame-ancestors")
}

func TestStageUnknownOrMissingTokenIs404(t *testing.T) {
	e := newStageEnv(t)
	closed := agentd.OpenStagePageForTest("agt_owner", "gone", agentd.HandlerTransport{Handler: e.fake})
	agentd.CloseStagePageForTest(closed.Token)

	for _, host := range []string{
		"ui.localhost:" + e.port,
		"f-.ui.localhost:" + e.port,
		"f-nothex.ui.localhost:" + e.port,
		"f-" + strings.Repeat("0", 32) + ".ui.localhost:" + e.port,
		closed.Host + ":" + e.port,
		"x." + e.page.Host + ":" + e.port,
		strings.ToUpper("f-"+strings.Repeat("0", 32)) + ".UI.LOCALHOST.:" + e.port,
	} {
		res := e.do(t, http.MethodGet, host, "/", nil)
		assert.Equal(t, http.StatusNotFound, res.StatusCode, host)
		assertStageCSP(t, res)
	}
	assert.Zero(t, e.fake.count(), "no upstream request for an unknown token")

	// The live token admits, also in a case-folded, fully-qualified Host.
	res := e.do(t, http.MethodGet, strings.ToUpper(e.page.Host)+".:"+e.port, "/", nil)
	assert.Equal(t, http.StatusOK, res.StatusCode)
}

func TestStageTokenRotatesOnClose(t *testing.T) {
	e := newStageEnv(t)
	assert.Equal(t, http.StatusOK, e.do(t, http.MethodGet, e.host(), "/", nil).StatusCode)
	old := e.page.Host
	agentd.CloseStagePageForTest(e.page.Token)
	assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodGet, old+":"+e.port, "/", nil).StatusCode)

	again := agentd.OpenStagePageForTest("agt_owner", "landing", agentd.HandlerTransport{Handler: e.fake})
	t.Cleanup(func() { agentd.CloseStagePageForTest(again.Token) })
	assert.NotEqual(t, e.page.Token, again.Token)
	assert.NotEqual(t, e.page.SnapshotToken, again.SnapshotToken)
	assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodGet, old+":"+e.port, "/", nil).StatusCode)
	assert.Equal(t, http.StatusOK, e.do(t, http.MethodGet, again.Host+":"+e.port, "/", nil).StatusCode)
}

func TestStageServesDocumentsOnlyToIframes(t *testing.T) {
	e := newStageEnv(t)
	for _, dest := range []string{"", "document", "frame", "embed", "object"} {
		res := e.do(t, http.MethodGet, e.host(), "/", map[string]string{"Sec-Fetch-Dest": dest})
		assert.Equal(t, http.StatusForbidden, res.StatusCode, "dest=%q", dest)
		assertStageCSP(t, res)
	}
	assert.Zero(t, e.fake.count())

	for _, dest := range []string{"iframe", "script", "style", "image", "empty", "websocket", "worker"} {
		res := e.do(t, http.MethodGet, e.host(), "/x", map[string]string{"Sec-Fetch-Dest": dest})
		assert.Equal(t, http.StatusOK, res.StatusCode, "dest=%q", dest)
	}

	// The daemon's snapshot client: no Sec-Fetch-Dest, but the page's
	// snapshot token. A wrong one is a 404, and the token never reaches the
	// upstream.
	res := e.do(t, http.MethodGet, e.host(), "/snap", map[string]string{
		"Sec-Fetch-Dest": "", "X-Tclaude-Stage-Snapshot": e.page.SnapshotToken,
	})
	assert.Equal(t, http.StatusOK, res.StatusCode)
	assert.Empty(t, e.fake.last(t).Header.Get("X-Tclaude-Stage-Snapshot"))
	res = e.do(t, http.MethodGet, e.host(), "/snap", map[string]string{
		"Sec-Fetch-Dest": "", "X-Tclaude-Stage-Snapshot": e.page.Token,
	})
	assert.Equal(t, http.StatusNotFound, res.StatusCode)
}

func TestStageStripsCredentialsGoingIn(t *testing.T) {
	e := newStageEnv(t)
	res := e.do(t, http.MethodGet, e.host(), "/app?q=1", map[string]string{
		"Cookie":              "tclaude_dashboard=" + agentd.DashboardSessionCookieForTest() + "; other=1",
		"Authorization":       "Bearer secret",
		"Proxy-Authorization": "Basic c2VjcmV0",
		"Proxy-Foo":           "bar",
		"X-Forwarded-For":     "10.0.0.1",
		"Forwarded":           "for=10.0.0.1",
		"Referer":             "http://127.0.0.1:" + e.port + "/boards",
	})
	require.Equal(t, http.StatusOK, res.StatusCode)
	got := e.fake.last(t)
	assert.Equal(t, "/app", got.URL.Path)
	assert.Equal(t, "q=1", got.URL.RawQuery)
	for _, h := range []string{"Cookie", "Authorization", "Proxy-Authorization", "Proxy-Foo",
		"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded", "Referer", "Accept-Encoding"} {
		assert.Empty(t, got.Header.Values(h), h)
	}
	assert.NotContains(t, strings.Join(headerDump(got.Header), "\n"), agentd.DashboardSessionCookieForTest())
}

func headerDump(h http.Header) []string {
	var out []string
	for k, vs := range h {
		for _, v := range vs {
			out = append(out, k+": "+v)
		}
	}
	return out
}

func TestStageStripsCookiesAndPolicyComingOut(t *testing.T) {
	e := newStageEnv(t)
	e.fake.h = func(w http.ResponseWriter, _ *http.Request) {
		h := w.Header()
		h.Add("Set-Cookie", "a=1")
		h.Add("Set-Cookie", "b=2; Domain=localhost")
		h.Set("Content-Security-Policy", "default-src *")
		h.Set("Content-Security-Policy-Report-Only", "default-src *")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Credentials", "true")
		h.Set("Content-Type", "application/javascript")
		h.Set("X-Custom", "kept")
		_, _ = io.WriteString(w, "export {}")
	}
	res := e.do(t, http.MethodGet, e.host(), "/main.js", map[string]string{"Sec-Fetch-Dest": "script"})
	require.Equal(t, http.StatusOK, res.StatusCode)
	assert.Empty(t, res.Header.Values("Set-Cookie"))
	assert.Empty(t, res.Cookies())
	assert.Empty(t, res.Header.Values("Content-Security-Policy-Report-Only"))
	assert.Empty(t, res.Header.Values("X-Frame-Options"))
	assert.Empty(t, res.Header.Values("Access-Control-Allow-Origin"))
	assert.Empty(t, res.Header.Values("Access-Control-Allow-Credentials"))
	assert.Equal(t, "kept", res.Header.Get("X-Custom"))
	assert.Equal(t, "nosniff", res.Header.Get("X-Content-Type-Options"))
	assertStageCSP(t, res)
	csp := res.Header.Get("Content-Security-Policy")
	assert.NotContains(t, csp, "default-src *")
	assert.Contains(t, csp, "connect-src 'self' http://"+e.host()+" ws://"+e.host())
	assert.Contains(t, csp, "frame-ancestors http://127.0.0.1:"+e.port)
	assert.Equal(t, "export {}", readBody(t, res), "non-HTML bodies pass untouched")
}

func TestStageOtherHostsToggleWidensCSP(t *testing.T) {
	e := newStageEnv(t)
	csp := e.do(t, http.MethodGet, e.host(), "/", nil).Header.Get("Content-Security-Policy")
	assert.NotContains(t, csp, "https:")
	e.page.SetAllowOtherHosts(true)
	csp = e.do(t, http.MethodGet, e.host(), "/", nil).Header.Get("Content-Security-Policy")
	assert.Contains(t, csp, "https:")
	assert.Contains(t, csp, "frame-ancestors http://127.0.0.1:"+e.port, "framing stays dashboard-only")
}

func TestStageAnswersNullOriginCORS(t *testing.T) {
	e := newStageEnv(t)
	res := e.do(t, http.MethodOptions, e.host(), "/api/items", map[string]string{
		"Sec-Fetch-Dest":                 "empty",
		"Origin":                         "null",
		"Access-Control-Request-Method":  "PUT",
		"Access-Control-Request-Headers": "content-type, x-thing",
	})
	assert.Equal(t, http.StatusNoContent, res.StatusCode)
	assert.Equal(t, "null", res.Header.Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "PUT", res.Header.Get("Access-Control-Allow-Methods"))
	assert.Equal(t, "content-type, x-thing", res.Header.Get("Access-Control-Allow-Headers"))
	assert.Empty(t, res.Header.Get("Access-Control-Allow-Credentials"))
	assertStageCSP(t, res)
	assert.Zero(t, e.fake.count(), "the stage answers the preflight itself")

	res = e.do(t, http.MethodGet, e.host(), "/api/items", map[string]string{"Sec-Fetch-Dest": "empty", "Origin": "null"})
	assert.Equal(t, "null", res.Header.Get("Access-Control-Allow-Origin"))
	res = e.do(t, http.MethodGet, e.host(), "/api/items", map[string]string{"Sec-Fetch-Dest": "empty", "Origin": "http://evil.example"})
	assert.Empty(t, res.Header.Get("Access-Control-Allow-Origin"), "only opaque-origin frames get CORS")
}

func TestStageNeverRoutesToTheDashboard(t *testing.T) {
	e := newStageEnv(t)
	// The same paths on the dashboard Host answer from the dashboard...
	dash := e.do(t, http.MethodGet, "127.0.0.1:"+e.port, "/api/instance", nil)
	require.Equal(t, http.StatusOK, dash.StatusCode, readBody(t, dash))

	// ...but on a page origin they only ever reach the page's own upstream,
	// which here has no /api, and without a page they are a 404.
	e.fake.h = func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }
	for _, p := range []string{"/api/instance", "/api/snapshot", "/static/app.js", "/", "/dashboard/login", "/v1/info"} {
		res := e.do(t, http.MethodGet, e.host(), p, nil)
		assert.Equal(t, http.StatusNotFound, res.StatusCode, p)
		assert.Equal(t, p, e.fake.last(t).URL.Path, "reached the upstream, not the dashboard")
		assertStageCSP(t, res)

		res = e.do(t, http.MethodGet, "f-"+strings.Repeat("a", 32)+".ui.localhost:"+e.port, p, nil)
		assert.Equal(t, http.StatusNotFound, res.StatusCode, p)
	}
}

const stageProbeMarker = "<script data-tclaude-probe>"

func TestStageInjectsProbeIntoHTML(t *testing.T) {
	e := newStageEnv(t)
	doc := "<!doctype html><html lang=en><HEAD data-x=1><title>t</title></head><body>hi</body></html>"
	e.fake.h = func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gz":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Content-Encoding", "gzip")
			var b bytes.Buffer
			zw := gzip.NewWriter(&b)
			_, _ = io.WriteString(zw, doc)
			_ = zw.Close()
			_, _ = w.Write(b.Bytes())
		case "/headless":
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, "<!DOCTYPE html><p>no head</p>")
		case "/br":
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Content-Encoding", "br")
			_, _ = io.WriteString(w, "xx")
		default:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Content-Length", "999")
			_, _ = io.WriteString(w, doc)
		}
	}
	res := e.do(t, http.MethodGet, e.host(), "/", nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	body := readBody(t, res)
	assert.True(t, strings.HasPrefix(body, "<!doctype html><html lang=en><HEAD data-x=1>"+stageProbeMarker), body)
	assert.True(t, strings.HasSuffix(body, "</script><title>t</title></head><body>hi</body></html>"), body)
	assert.Contains(t, body, "__tclaudeProbe")

	gz := e.do(t, http.MethodGet, e.host(), "/gz", nil)
	assert.Empty(t, gz.Header.Get("Content-Encoding"))
	assert.Equal(t, body, readBody(t, gz), "gzip HTML is decoded and injected the same way")

	headless := readBody(t, e.do(t, http.MethodGet, e.host(), "/headless", nil))
	assert.True(t, strings.HasPrefix(headless, "<!DOCTYPE html>"+stageProbeMarker), headless)

	br := e.do(t, http.MethodGet, e.host(), "/br", nil)
	assert.Equal(t, http.StatusBadGateway, br.StatusCode, "HTML the stage cannot inspect is not served")
	assertStageCSP(t, br)
}

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("dial tcp: connection refused")
}

func TestStageUpstreamDownIs502WithCSP(t *testing.T) {
	e := newStageEnv(t)
	e.page.SetUpstream(failingTransport{})
	res := e.do(t, http.MethodGet, e.host(), "/", nil)
	assert.Equal(t, http.StatusBadGateway, res.StatusCode)
	assertStageCSP(t, res)
	assert.NotContains(t, readBody(t, res), "connection refused", "upstream errors are not echoed")
}

func TestStageInformationalResponsesCannotDropTheCSP(t *testing.T) {
	e := newStageEnv(t)
	e.fake.h = func(w http.ResponseWriter, r *http.Request) {
		tr := httptrace.ContextClientTrace(r.Context())
		if assert.NotNil(t, tr, "ReverseProxy installs the 1xx trace") {
			_ = tr.Got1xxResponse(http.StatusEarlyHints, textproto.MIMEHeader{
				"Set-Cookie": {"early=1"},
				"Link":       {"</x.css>; rel=preload"},
			})
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><head></head></html>")
	}
	res := e.do(t, http.MethodGet, e.host(), "/", nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	assertStageCSP(t, res)
	assert.Equal(t, "nosniff", res.Header.Get("X-Content-Type-Options"))
	assert.Empty(t, res.Header.Values("Set-Cookie"))
	assert.Contains(t, readBody(t, res), stageProbeMarker)
}

func TestStageVariesOnOriginAlways(t *testing.T) {
	e := newStageEnv(t)
	e.fake.h = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Vary", "Accept")
		_, _ = io.WriteString(w, "x")
	}
	res := e.do(t, http.MethodGet, e.host(), "/a.js", map[string]string{"Sec-Fetch-Dest": "script"})
	assert.ElementsMatch(t, []string{"Accept", "Origin"}, res.Header.Values("Vary"))
	assert.Empty(t, res.Header.Get("Access-Control-Allow-Origin"))
}

func TestStageProbeStaysBehindMetaCharsetAndOutOfRanges(t *testing.T) {
	e := newStageEnv(t)
	e.fake.h = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/part" {
			w.Header().Set("Content-Range", "bytes 0-9/100")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, "<head>0123")
			return
		}
		_, _ = io.WriteString(w, "<!doctype html><html><head><title>t</title><meta charset=\"windows-1252\"><style></style></head></html>")
	}
	body := readBody(t, e.do(t, http.MethodGet, e.host(), "/", nil))
	assert.Contains(t, body, "<meta charset=\"windows-1252\">"+stageProbeMarker, body)
	assert.Equal(t, "<head>0123", readBody(t, e.do(t, http.MethodGet, e.host(), "/part", nil)))
}

// realUpstream serves h on a real loopback server and returns a RoundTripper
// that always dials it, so 1xx and trailers travel the real http.Transport.
func realUpstream(t *testing.T, h http.HandlerFunc) http.RoundTripper {
	t.Helper()
	up := httptest.NewServer(h)
	t.Cleanup(up.Close)
	addr := up.Listener.Addr().String()
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}}
	t.Cleanup(tr.CloseIdleConnections)
	return tr
}

func TestStageRealTransportEarlyHintsAndTrailers(t *testing.T) {
	e := newStageEnv(t)
	e.page.SetUpstream(realUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Set-Cookie", "early=1")
		w.Header().Set("Content-Security-Policy", "default-src *")
		w.WriteHeader(http.StatusEarlyHints)
		clear(w.Header())
		w.Header().Set("Trailer", "Set-Cookie, X-A")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "<html><head></head></html>")
		w.Header().Set("Set-Cookie", "t=1; Domain=ui.localhost")
		w.Header().Set("X-A", "a")
		w.Header().Set(http.TrailerPrefix+"Content-Security-Policy", "default-src *")
		w.Header().Set(http.TrailerPrefix+"Set-Cookie2", "x=1")
	}))
	res := e.do(t, http.MethodGet, e.host(), "/", nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	body := readBody(t, res)
	assert.Contains(t, body, stageProbeMarker)
	assertStageCSP(t, res)
	assert.NotContains(t, res.Header.Get("Content-Security-Policy"), "default-src *")
	assert.Empty(t, res.Header.Values("Set-Cookie"))
	assert.Empty(t, res.Trailer, "no trailers reach the browser")
}

func TestStageDuplicateContentTypeUsesTheLast(t *testing.T) {
	e := newStageEnv(t)
	e.fake.h = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Content-Type", "text/plain")
		w.Header().Add("Content-Type", "text/html")
		if r.URL.Path == "/enc" {
			w.Header().Add("Content-Encoding", "identity")
			w.Header().Add("Content-Encoding", "gzip")
		}
		_, _ = io.WriteString(w, "<html><head></head></html>")
	}
	res := e.do(t, http.MethodGet, e.host(), "/", nil)
	assert.Equal(t, []string{"text/html"}, res.Header.Values("Content-Type"))
	assert.Contains(t, readBody(t, res), stageProbeMarker)
	assert.Equal(t, http.StatusBadGateway, e.do(t, http.MethodGet, e.host(), "/enc", nil).StatusCode)
}

func TestStageProxiesWebsocketUpgrades(t *testing.T) {
	e := newStageEnv(t)
	var sawCookie string
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	e.page.SetUpstream(realUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		sawCookie = r.Header.Get("Cookie")
		c, err := upgrader.Upgrade(w, r, http.Header{"Set-Cookie": {"ws=1"}})
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		mt, msg, err := c.ReadMessage()
		if err == nil {
			_ = c.WriteMessage(mt, append([]byte("echo:"), msg...))
		}
	}))
	dialer := websocket.Dialer{
		NetDial:          func(string, string) (net.Conn, error) { return net.Dial("tcp", e.srv.Listener.Addr().String()) },
		HandshakeTimeout: 5 * time.Second,
	}
	conn, res, err := dialer.Dial("ws://"+e.host()+"/hmr", http.Header{
		"Origin": {"null"}, "Sec-Fetch-Dest": {"websocket"}, "Cookie": {"tclaude_dashboard=x"},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	assert.Equal(t, http.StatusSwitchingProtocols, res.StatusCode)
	assert.Empty(t, res.Header.Values("Set-Cookie"))
	assertStageCSP(t, res)
	assert.Empty(t, sawCookie)
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte("hi")))
	_, msg, err := conn.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, "echo:hi", string(msg))
}

func TestStageRefusesForeignOrigins(t *testing.T) {
	e := newStageEnv(t)
	for _, o := range []string{"http://evil.example", "http://127.0.0.1:" + e.port, "http://f-" + strings.Repeat("b", 32) + ".ui.localhost:" + e.port} {
		res := e.do(t, http.MethodGet, e.host(), "/", map[string]string{"Origin": o})
		assert.Equal(t, http.StatusForbidden, res.StatusCode, o)
		assertStageCSP(t, res)
	}
	assert.Zero(t, e.fake.count())
	for _, o := range []string{"", "null", "http://" + e.host()} {
		res := e.do(t, http.MethodGet, e.host(), "/", map[string]string{"Origin": o})
		assert.Equal(t, http.StatusOK, res.StatusCode, o)
	}
}
