package agentd

// The live-page stage listener (epic tcl-qo90av, "Security boundary").
//
// An agent can show the operator any web page it serves. That page's code runs
// in the operator's browser, so it must never run with the dashboard's origin,
// cookies or API. The stage is the daemon side of that boundary:
//
//   - Every page has its own origin, Host-routed on the dashboard port as
//     f-<token>.ui.localhost:<port>. That is a different site from the
//     dashboard (127.0.0.1 / localhost), so the dashboard's host-only,
//     SameSite=Strict session cookie is never sent to it.
//   - Requests for a *.ui.localhost Host never reach the dashboard mux, and the
//     stage handler has no route to /api, /static or any dashboard asset: it
//     only ever proxies to the page's declared upstream.
//   - Admission is by the unguessable per-page token in the host label. A
//     closed page's token is forgotten; showing again mints a new one.
//   - Documents are served only to Sec-Fetch-Dest: iframe (a top-level visit to
//     the stage origin is refused), plus the daemon's own snapshot client,
//     which presents a separate per-page snapshot token.
//   - Cookie, Authorization and Proxy-* are always stripped going in, and
//     Set-Cookie coming out. Host-only cookies are not port-scoped, so this is a
//     rule, not an optimisation.
//   - The daemon sets the CSP as a header and drops any CSP the upstream sends,
//     so the page cannot loosen it.
//   - HTML documents get the probe script inserted (stage_probe.js).
//
// The upstream is an http.RoundTripper. Real dial targets (the agent's own
// port, a static directory) arrive with the page-sources ticket; the stage
// itself never resolves a host name or dials anything.

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/net/http/httpguts"
)

//go:embed stage_probe.js
var stageProbeJS []byte

const (
	// stageHostSuffix is the parent domain of every page origin. Browsers
	// resolve *.localhost to loopback without DNS, so no rebinding attacker
	// controls what these names resolve to.
	stageHostSuffix = "ui.localhost"
	// stageHostPrefix starts every page's host label.
	stageHostPrefix = "f-"
	// stageTokenBytes is the entropy of a page token (128 bits, hex in the
	// label, so case-insensitive host matching cannot collapse it).
	stageTokenBytes = 16
	// stageSnapshotHeader carries the per-page snapshot token the daemon's own
	// headless renderer presents in place of Sec-Fetch-Dest: iframe.
	stageSnapshotHeader = "X-Tclaude-Stage-Snapshot"
	// stageInjectScanLimit bounds how much of an HTML document is buffered
	// while looking for <head> before the probe falls back to an earlier
	// insertion point.
	stageInjectScanLimit = 64 << 10
)

// stagePage is one page the stage serves.
type stagePage struct {
	token         string
	snapshotToken string
	owner         string // agent ID that showed the page
	name          string
	// allowOtherHosts widens the CSP to other hosts. It is an operator toggle,
	// off by default; nothing an agent sends can set it.
	allowOtherHosts bool

	mu         sync.RWMutex
	upstream   http.RoundTripper
	reloadSubs map[chan struct{}]struct{}
	// done is closed when the page's origin closes, ending its live views.
	done      chan struct{}
	closeOnce sync.Once
}

func (p *stagePage) transport() http.RoundTripper {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.upstream
}

// setUpstream swaps the page's source in place, releasing the old one.
func (p *stagePage) setUpstream(up http.RoundTripper) {
	p.mu.Lock()
	old := p.upstream
	p.upstream = up
	p.mu.Unlock()
	if old != nil && old != up {
		closeStageUpstream(old)
	}
}

func (p *stagePage) otherHostsAllowed() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.allowOtherHosts
}

// stageRegistry holds the open pages, keyed by token.
type stageRegistry struct {
	mu      sync.RWMutex
	byToken map[string]*stagePage
}

var stagePages = &stageRegistry{byToken: map[string]*stagePage{}}

func newStageToken() string {
	b := make([]byte, stageTokenBytes)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail on supported platforms; a predictable
		// token would break the boundary, so refuse to continue.
		panic("stage: crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// open registers a page and mints its tokens.
func (s *stageRegistry) open(owner, name string, up http.RoundTripper) *stagePage {
	p := &stagePage{
		token:         newStageToken(),
		snapshotToken: newStageToken(),
		owner:         owner,
		name:          name,
		upstream:      up,
		done:          make(chan struct{}),
	}
	s.mu.Lock()
	s.byToken[p.token] = p
	s.mu.Unlock()
	return p
}

// close forgets a page. Its token stops admitting immediately and is never
// reused: a page shown again gets a fresh one.
func (s *stageRegistry) close(token string) {
	s.mu.Lock()
	p := s.byToken[token]
	delete(s.byToken, token)
	s.mu.Unlock()
	if p != nil {
		p.setUpstream(nil)
		p.closeOnce.Do(func() { close(p.done) })
	}
}

func (s *stageRegistry) lookup(token string) *stagePage {
	if token == "" {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.byToken[token]
}

// stageHost returns the host name (without port) of a page's origin.
func stageHost(token string) string {
	return stageHostPrefix + token + "." + stageHostSuffix
}

// stageHostToken classifies a request Host. isStage is true for any name at or
// under ui.localhost, valid token or not, so such a request can never fall
// through to the dashboard. token is non-empty only for a well-formed
// f-<token> label.
func stageHostToken(hostport string) (token string, isStage bool) {
	h := hostport
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		h = host
	}
	h = strings.TrimSuffix(strings.ToLower(h), ".")
	if h != stageHostSuffix && !strings.HasSuffix(h, "."+stageHostSuffix) {
		return "", false
	}
	label, ok := strings.CutSuffix(h, "."+stageHostSuffix)
	if !ok {
		return "", true
	}
	tok, ok := strings.CutPrefix(label, stageHostPrefix)
	if !ok || len(tok) != 2*stageTokenBytes || !isLowerHex(tok) {
		return "", true
	}
	return tok, true
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// buildPopupHandler splits the dashboard listener by Host: page origins go to
// the stage handler, everything else to the dashboard. The two share no
// handlers or middleware.
func buildPopupHandler(dashboard http.Handler) http.Handler {
	stage := newStageHandler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, isStage := stageHostToken(r.Host); isStage {
			stage.ServeHTTP(w, r)
			return
		}
		dashboard.ServeHTTP(w, r)
	})
}

// stageRefusalCSP is the policy on every response that is not a page's own:
// unknown tokens, refusals, errors. Nothing renders, nothing frames it.
const stageRefusalCSP = "sandbox; default-src 'none'; frame-ancestors 'none'"

// stageHandler serves page origins. It never routes anywhere but the page's
// upstream.
type stageHandler struct{}

func newStageHandler() http.Handler { return stageHandler{} }

func stageNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", stageRefusalCSP)
	http.Error(w, "404 page not found", http.StatusNotFound)
}

func (stageHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", stageRefusalCSP)

	port, ok := stageLoopbackPort(r)
	if !ok || !collabRoutesEnabled() {
		stageNotFound(w)
		return
	}
	token, _ := stageHostToken(r.Host)
	page := stagePages.lookup(token)
	if page == nil {
		stageNotFound(w)
		return
	}

	snapshot := false
	if v := r.Header.Get(stageSnapshotHeader); v != "" {
		if subtle.ConstantTimeCompare([]byte(v), []byte(page.snapshotToken)) != 1 {
			stageNotFound(w)
			return
		}
		snapshot = true
	}
	if !snapshot && !stageFetchDestAllowed(r.Header.Get("Sec-Fetch-Dest")) && !stageWebSocketHandshake(r) {
		http.Error(w, "stage pages load only inside a live-page frame", http.StatusForbidden)
		return
	}

	// A frame's requests carry Origin: null (opaque origin) or none (no-cors
	// loads and navigations). Any other origin is some other site holding
	// the page token, and is refused before the dev server can treat it as a
	// same-origin request (the upstream sees Origin rewritten to localhost).
	if o := r.Header.Get("Origin"); o != "" && o != "null" &&
		o != "http://"+net.JoinHostPort(stageHost(page.token), strconv.Itoa(port)) {
		http.Error(w, "stage pages answer only their own frame", http.StatusForbidden)
		return
	}

	// The page's own headers are built once and stamped on the final
	// response in ModifyResponse, after the upstream's are filtered. They are
	// not pre-set on w: ReverseProxy clears w's header map after relaying a
	// 1xx, which would silently drop a pre-set CSP.
	fixed := stagePageHeaders(page, port, r.Header.Get("Origin") == "null")
	if r.Header.Get("Origin") == "null" && r.Method == http.MethodOptions &&
		r.Header.Get("Access-Control-Request-Method") != "" {
		maps.Copy(h, fixed)
		h.Set("Vary", "Origin")
		stageAnswerPreflight(w, r)
		return
	}

	up := page.transport()
	if up == nil {
		http.Error(w, "this page has no source", http.StatusBadGateway)
		return
	}
	proxy := &httputil.ReverseProxy{
		Rewrite:   stageRewrite,
		Transport: up,
		ModifyResponse: func(res *http.Response) error {
			if err := stageModifyResponse(res); err != nil {
				return err
			}
			maps.Copy(res.Header, fixed)
			res.Header.Add("Vary", "Origin")
			return nil
		},
		ErrorHandler: func(ew http.ResponseWriter, r *http.Request, err error) {
			slog.Debug("stage: upstream error", "page", page.name, "path", r.URL.Path, "err", err)
			eh := ew.Header()
			clear(eh)
			eh.Set("X-Content-Type-Options", "nosniff")
			eh.Set("Referrer-Policy", "no-referrer")
			eh.Set("Content-Security-Policy", stageRefusalCSP)
			msg := stageSourceMessage(err)
			if msg == "" {
				msg = "nothing is answering for this page"
			}
			http.Error(ew, msg, http.StatusBadGateway)
		},
	}
	clear(h)
	proxy.ServeHTTP(stageNo1xxWriter{w}, r)
}

// stagePageHeaders are the headers the stage owns on a page response. A
// sandboxed frame has an opaque origin, so its own subresource fetches and
// module scripts are CORS requests carrying Origin: null: those are answered
// for null only, never with credentials (there are none to send). Vary:
// Origin goes on every response, so a no-cors load cached without CORS
// headers is not reused for a later CORS load of the same URL.
func stagePageHeaders(page *stagePage, port int, nullOrigin bool) http.Header {
	h := http.Header{}
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", stageCSP(page.token, port, page.otherHostsAllowed()))
	if nullOrigin {
		h.Set("Access-Control-Allow-Origin", "null")
		h.Set("Access-Control-Expose-Headers", "*")
	}
	return h
}

// stageNo1xxWriter drops informational responses (103 Early Hints and the
// like) from the upstream. ReverseProxy relays them straight to the client,
// bypassing ModifyResponse, so their headers would reach the browser
// unfiltered. 101 Switching Protocols is a final response and passes.
type stageNo1xxWriter struct{ http.ResponseWriter }

func (w stageNo1xxWriter) WriteHeader(code int) {
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w stageNo1xxWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// stageLoopbackPort admits only connections that arrived on a loopback
// listener and returns that listener's port. Page origins are loopback names;
// a request to them that came in over the network is not a browser on this
// host and is refused.
func stageLoopbackPort(r *http.Request) (int, bool) {
	local, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	tcp, ok := local.(*net.TCPAddr)
	if !ok || !tcp.IP.IsLoopback() {
		return 0, false
	}
	remote, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return 0, false
	}
	if ip := net.ParseIP(remote); ip == nil || !ip.IsLoopback() {
		return 0, false
	}
	return tcp.Port, true
}

// stageFetchDestAllowed decides from Sec-Fetch-Dest whether a browser request
// may be served. Documents load only into an iframe: a top-level visit or a
// frame/embed/object load is refused, and a request without the header is not
// a modern browser request (the daemon's snapshot client uses its own token).
// Subresource destinations (script, style, image, fetch/XHR as "empty",
// websocket, worker, ...) are served: they run in, or are read by, the page.
func stageFetchDestAllowed(dest string) bool {
	switch strings.ToLower(strings.TrimSpace(dest)) {
	case "", "document", "frame", "embed", "object", "fencedframe":
		return false
	default:
		return true
	}
}

// stageWebSocketHandshake reports whether r is a browser's WebSocket opening
// handshake that carries no Sec-Fetch-Dest. Chrome sends no fetch metadata on a
// handshake, so a dev server's HMR socket would otherwise be refused. A
// handshake never renders a document, and no navigation sends Upgrade, so the
// frames-only rule for documents still holds; the Origin check below applies
// as for every request.
func stageWebSocketHandshake(r *http.Request) bool {
	if r.Method != http.MethodGet || strings.TrimSpace(r.Header.Get("Sec-Fetch-Dest")) != "" {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") &&
		httpguts.HeaderValuesContainsToken(r.Header["Connection"], "upgrade") &&
		r.Header.Get("Sec-WebSocket-Key") != ""
}

func stageAnswerPreflight(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	if m := r.Header.Get("Access-Control-Request-Method"); stageTokenChars(m) {
		h.Set("Access-Control-Allow-Methods", m)
	}
	if hdrs := r.Header.Get("Access-Control-Request-Headers"); hdrs != "" && stageHeaderList(hdrs) {
		h.Set("Access-Control-Allow-Headers", hdrs)
	}
	h.Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
}

// stageTokenChars reports whether s is a non-empty HTTP token (RFC 9110).
func stageTokenChars(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= ' ' || c >= 0x7f || strings.IndexByte("\"(),/:;<=>?@[\\]{}", c) >= 0 {
			return false
		}
	}
	return true
}

func stageHeaderList(s string) bool {
	for part := range strings.SplitSeq(s, ",") {
		if !stageTokenChars(strings.TrimSpace(part)) {
			return false
		}
	}
	return true
}

// stageCSP builds the page policy. The page's own origin is named explicitly
// next to 'self': a sandboxed frame's origin is opaque, and the explicit
// source keeps the rules meaning "this page's origin" whatever a browser takes
// 'self' to be there. Inline and eval are allowed (dev servers need them);
// connections reach only the page's own origin (http and ws); only the
// dashboard may frame it. CSP sandbox makes even a stray top-level load of the
// origin an opaque, sandboxed document.
func stageCSP(token string, port int, allowOtherHosts bool) string {
	hp := net.JoinHostPort(stageHost(token), strconv.Itoa(port))
	self := "'self' http://" + hp
	ws := "ws://" + hp
	other := ""
	if allowOtherHosts {
		other = " http: https: ws: wss:"
	}
	local := self + " data: blob:" + other
	directives := []string{
		"default-src " + local,
		"script-src " + local + " 'unsafe-inline' 'unsafe-eval'",
		"style-src " + local + " 'unsafe-inline'",
		"img-src " + local,
		"font-src " + local,
		"media-src " + local,
		"connect-src " + self + " " + ws + other,
		"worker-src " + self + " blob:" + other,
		"frame-src " + local,
		"object-src 'none'",
		"base-uri " + self,
		"form-action " + self + other,
		"frame-ancestors " + strings.Join(append(stageDashboardOrigins(port), "http://"+hp), " "),
		"sandbox allow-scripts allow-forms allow-modals",
	}
	return strings.Join(directives, "; ")
}

// stageDashboardOrigins are the dashboard origins allowed to frame a page:
// the loopback forms of the dashboard's own port. The stage is served on the
// dashboard listener, so that is the port the request arrived on. CSP has no
// IPv6-literal host-source (Chrome rejects "http://[::1]:port" with a console
// error), so a dashboard opened on [::1] cannot frame pages.
func stageDashboardOrigins(port int) []string {
	p := strconv.Itoa(port)
	return []string{
		"http://127.0.0.1:" + p,
		"http://localhost:" + p,
	}
}

// stageRewrite builds the upstream request. The upstream decides where it
// goes and what Host/Origin it presents; the stage only removes what must
// never cross: credentials, proxy headers, forwarding headers, the referrer
// (it carries the page token) and the snapshot token.
func stageRewrite(pr *httputil.ProxyRequest) {
	out := pr.Out
	out.URL.Scheme = "http"
	out.URL.Host = "stage-upstream.invalid"
	out.Host = ""
	stripStageRequestHeaders(out.Header)
}

func stripStageRequestHeaders(h http.Header) {
	for k := range h {
		lk := strings.ToLower(k)
		switch {
		case lk == "cookie", lk == "cookie2", lk == "authorization",
			strings.HasPrefix(lk, "proxy-"),
			lk == "forwarded", strings.HasPrefix(lk, "x-forwarded-"), lk == "x-real-ip",
			lk == "via", lk == "true-client-ip", lk == "x-client-ip",
			lk == "x-original-url", lk == "x-rewrite-url", lk == "x-http-method-override",
			lk == "referer",
			lk == strings.ToLower(stageSnapshotHeader),
			// Bodies are rewritten for probe injection; ask for identity.
			lk == "accept-encoding":
			delete(h, k)
		}
	}
}

// stageDroppedResponseHeader reports upstream response headers the stage
// never passes on: cookies, any policy the page could use to change its own
// boundary (the stage sets CSP and CORS itself), and headers that would make
// the browser remember something about the page origin.
func stageDroppedResponseHeader(lk string) bool {
	switch lk {
	case "set-cookie", "set-cookie2",
		"content-security-policy", "content-security-policy-report-only",
		"x-frame-options", "x-content-type-options", "referrer-policy",
		"clear-site-data", "service-worker-allowed", "alt-svc",
		"strict-transport-security", "report-to", "reporting-endpoints", "nel",
		"set-login", "origin-trial", "speculation-rules", "supports-loading-mode",
		"accept-ch", "critical-ch", "timing-allow-origin":
		return true
	}
	return strings.HasPrefix(lk, "access-control-") || strings.HasPrefix(lk, "attribution-reporting-")
}

func stageModifyResponse(res *http.Response) error {
	for k := range res.Header {
		if stageDroppedResponseHeader(strings.ToLower(k)) {
			delete(res.Header, k)
		}
	}
	// No trailers at all: a page has no use for them. The transport fills (or
	// replaces) res.Trailer only at body EOF, after any filtering here could
	// run, so the body's Close, which ReverseProxy calls just before relaying
	// trailers, drops whatever arrived.
	res.Trailer = nil
	if res.StatusCode == http.StatusSwitchingProtocols {
		// The body of an upgrade is the raw connection (an io.ReadWriteCloser
		// ReverseProxy splices); it has no trailers and must not be wrapped.
		return nil
	}
	res.Body = &stageNoTrailerBody{ReadCloser: res.Body, res: res}
	// Browsers take the last Content-Type; decide (and serve) on that one so
	// a leading text/plain cannot hide an HTML document from the probe.
	if cts := res.Header.Values("Content-Type"); len(cts) > 1 {
		res.Header.Set("Content-Type", cts[len(cts)-1])
	}
	if !stageInjectable(res) {
		return nil
	}
	body := res.Body
	encs := res.Header.Values("Content-Encoding")
	if len(encs) > 1 {
		return fmt.Errorf("stage: html with %d content-encoding headers", len(encs))
	}
	switch enc := strings.ToLower(strings.TrimSpace(res.Header.Get("Content-Encoding"))); enc {
	case "", "identity":
	case "gzip":
		zr, err := gzip.NewReader(body)
		if err != nil {
			return fmt.Errorf("stage: gzip html: %w", err)
		}
		body = struct {
			io.Reader
			io.Closer
		}{zr, res.Body}
	default:
		return fmt.Errorf("stage: html with unsupported content-encoding %q", enc)
	}
	res.Header.Del("Content-Encoding")
	res.Header.Del("Content-Length")
	res.ContentLength = -1
	res.Body = newStageInjector(body)
	return nil
}

// stageNoTrailerBody forgets the response's trailers when the body closes.
type stageNoTrailerBody struct {
	io.ReadCloser
	res *http.Response
}

func (b *stageNoTrailerBody) Close() error {
	err := b.ReadCloser.Close()
	b.res.Trailer = nil
	return err
}

// stageInjectable reports whether a response is an HTML document body the
// probe goes into.
func stageInjectable(res *http.Response) bool {
	if res.Request != nil && res.Request.Method == http.MethodHead {
		return false
	}
	if res.StatusCode < 200 || res.StatusCode == http.StatusNoContent ||
		res.StatusCode == http.StatusNotModified || res.StatusCode == http.StatusSwitchingProtocols ||
		res.StatusCode == http.StatusPartialContent {
		return false
	}
	mt, _, err := mime.ParseMediaType(res.Header.Get("Content-Type"))
	return err == nil && mt == "text/html"
}

// stageProbeTag is the inline script inserted into every served document.
var stageProbeTag = append(append([]byte("<script data-tclaude-probe>"), stageProbeJS...), "</script>"...)

// stageInjector streams an HTML body with the probe inserted after the
// document's <head> tag (or, failing that, after <html>, after the doctype,
// or at the start). It reads only until it finds <head> or has buffered
// stageInjectScanLimit bytes, so a streamed document is not held back.
type stageInjector struct {
	src  io.ReadCloser
	out  io.Reader
	once bool
	err  error
}

func newStageInjector(src io.ReadCloser) *stageInjector { return &stageInjector{src: src} }

func (s *stageInjector) Read(p []byte) (int, error) {
	if !s.once {
		s.once = true
		s.prime()
	}
	if s.out == nil {
		return 0, s.err
	}
	return s.out.Read(p)
}

func (s *stageInjector) prime() {
	var buf []byte
	chunk := make([]byte, 4096)
	at := -1
	for len(buf) < stageInjectScanLimit {
		n, err := s.src.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if at < 0 {
			at = stageHeadInsertAt(buf)
		}
		// Once <head> is found, keep reading only until the charset prescan
		// window is buffered, so a <meta charset> there can be kept ahead of
		// the probe.
		if at >= 0 && len(buf) >= stageCharsetPrescan {
			break
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.err = err
				return
			}
			break
		}
	}
	if at < 0 {
		at = stageFallbackInsertAt(buf)
	}
	at = stageAfterMetaCharset(buf, at)
	s.out = io.MultiReader(bytes.NewReader(buf[:at]), bytes.NewReader(stageProbeTag),
		bytes.NewReader(buf[at:]), s.src)
}

// stageCharsetPrescan is how far into a document browsers look for a
// <meta charset> (the HTML encoding-sniffing prescan).
const stageCharsetPrescan = 1024

// stageAfterMetaCharset moves the insertion point past a <meta> that
// declares the charset, when one starts at or after at and inside the
// prescan window. Inserting the probe in front of it could push it out of
// the window and change how the page is decoded.
func stageAfterMetaCharset(buf []byte, at int) int {
	lower := bytes.ToLower(buf)
	from := at
	for from < len(lower) && from < stageCharsetPrescan {
		i := bytes.Index(lower[from:], []byte("<meta"))
		if i < 0 {
			return at
		}
		i += from
		if i >= stageCharsetPrescan {
			return at
		}
		end := bytes.IndexByte(lower[i:], '>')
		if end < 0 {
			return at
		}
		if bytes.Contains(lower[i:i+end], []byte("charset")) {
			return i + end + 1
		}
		from = i + end + 1
	}
	return at
}

func (s *stageInjector) Close() error { return s.src.Close() }

// stageHeadInsertAt returns the offset just past the first complete <head>
// start tag, or -1.
func stageHeadInsertAt(buf []byte) int {
	return stageAfterTag(buf, "<head")
}

func stageFallbackInsertAt(buf []byte) int {
	if at := stageAfterTag(buf, "<html"); at >= 0 {
		return at
	}
	if at := stageAfterTag(buf, "<!doctype"); at >= 0 {
		return at
	}
	return 0
}

// stageAfterTag finds an ASCII-case-insensitive start tag named by open (e.g.
// "<head") followed by '>' or whitespace, and returns the offset after its
// closing '>'. It returns -1 when no complete tag is in buf.
func stageAfterTag(buf []byte, open string) int {
	lower := bytes.ToLower(buf)
	from := 0
	for {
		i := bytes.Index(lower[from:], []byte(open))
		if i < 0 {
			return -1
		}
		i += from
		j := i + len(open)
		if j >= len(lower) {
			return -1
		}
		if c := lower[j]; c == '>' || c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '/' {
			end := bytes.IndexByte(lower[j:], '>')
			if end < 0 {
				return -1
			}
			return j + end + 1
		}
		from = j
	}
}
