package agentd

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/cronexpr"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/remoteaccess"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/common/buildversion"
)

// The dashboard single-page UI lives under the embedded dashboard/
// directory: dashboard.html, dashboard.css, and the ES-module JS set
// under dashboard/js/. agentd serves dashboard.html at "/" and the CSS
// and JS as static assets under /static/ (see registerDashboardRoutes).
// The dashboard shell, feature islands, and retained imperative integrations
// load source JS as native ES modules.
// Preact, HTM, and Signals are vendored under dashboard/vendor/preact/ and an
// import map gives application modules normal package specifiers. Islands are
// dynamically loaded so a missing optional feature module cannot prevent the
// static entry graph from linking.
//
//go:embed dashboard
var dashboardFS embed.FS

// dashboardAssetsFS is dashboardFS rooted at the dashboard/ directory,
// so its files address as "dashboard.html", "dashboard.css", "js/...".
var dashboardAssetsFS = mustSubFS(dashboardFS, "dashboard")

// dashboardIndexHTML is dashboard.html, read once at init — the page
// handleDashboardRoot serves at "/".
var dashboardIndexHTML = mustReadFS(dashboardAssetsFS, "dashboard.html")

func mustSubFS(f embed.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		panic("agentd: embedded dashboard/ subtree missing: " + err.Error())
	}
	return sub
}

func mustReadFS(fsys fs.FS, name string) []byte {
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		panic("agentd: embedded dashboard asset missing: " + name + ": " + err.Error())
	}
	return b
}

// init pins the MIME types the /static/ route serves, so the browser
// always gets a type it will execute or apply — independent of the
// host's /etc/mime.types. An ES module fetched as text/plain is refused
// outright, so this is load-bearing for the dashboard, not cosmetic.
func init() {
	_ = mime.AddExtensionType(".js", "text/javascript")
	_ = mime.AddExtensionType(".css", "text/css")
	_ = mime.AddExtensionType(".map", "application/json")
}

// dashboardSessionToken is generated once per agentd process and gates every
// /api/* request. On a clean shutdown its SHA-256 digest (never the replayable
// cookie value) is kept briefly in SQLite. The next daemon accepts that old
// cookie only during dashboardSessionGracePeriod and immediately replaces it
// with this process's fresh token, so a connected browser crosses a restart
// without turning the session cookie into a durable credential.
//
// Empty until initDashboardToken runs in startPopupServer.
var dashboardSessionToken string

const dashboardSessionGracePeriod = 30 * time.Minute

// dashboardGraceSessionHashes is restored once at startup and then read-only.
// Values are absolute expiries so a long-running daemon never accepts a grace
// cookie past its bounded handoff window even if its in-memory map remains.
var dashboardGraceSessionHashes = map[string]time.Time{}

// dashboardSessionNow is indirected for focused expiry tests.
var dashboardSessionNow = time.Now

const dashboardCookieName = "tclaude_dashboard_session"

func initDashboardToken() {
	if dashboardSessionToken != "" {
		return
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Cryptographic randomness is required for the token to be
		// unguessable. If we can't get it, leave the dashboard
		// disabled (token stays empty → checkDashboardAuth refuses
		// every /api request).
		return
	}
	dashboardSessionToken = hex.EncodeToString(b[:])
}

func dashboardTokenHash(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

// restoreDashboardGraceSessions loads the still-valid cookie digests left by
// earlier clean shutdowns. The DB helper prunes expired rows as part of the
// read. Failure is non-fatal at the call site: the dashboard remains secure
// and simply falls back to the login flow after a restart.
func restoreDashboardGraceSessions() error {
	now := dashboardSessionNow()
	rows, err := db.ListDashboardSessionGrace(now)
	if err != nil {
		return err
	}
	restored := make(map[string]time.Time, len(rows))
	for _, row := range rows {
		restored[row.TokenHash] = row.ExpiresAt
	}
	dashboardGraceSessionHashes = restored
	return nil
}

// preserveDashboardSessionForRestart records only a digest of the current
// cookie. Multiple quick restarts retain multiple unexpired digests, allowing
// a browser that did not reconnect between restarts to catch up on the next
// successful load.
func preserveDashboardSessionForRestart() error {
	if dashboardSessionToken == "" {
		return nil
	}
	now := dashboardSessionNow()
	return db.PreserveDashboardSessionGrace(
		dashboardTokenHash(dashboardSessionToken), now.Add(dashboardSessionGracePeriod), now)
}

// dashboardSessionCookieMatch reports whether value is the current cookie or
// a still-valid restart handoff. refresh is true only for the latter: callers
// that write a response must replace it with the fresh process token.
func dashboardSessionCookieMatch(value string) (valid, refresh bool) {
	if dashboardSessionToken == "" || value == "" {
		return false, false
	}
	if subtle.ConstantTimeCompare([]byte(value), []byte(dashboardSessionToken)) == 1 {
		return true, false
	}
	expiresAt, ok := dashboardGraceSessionHashes[dashboardTokenHash(value)]
	if !ok || !dashboardSessionNow().Before(expiresAt) {
		return false, false
	}
	return true, true
}

// registerDashboardRoutes wires the dashboard onto the popup-server
// mux. We share the listener since both views are loopback-only and
// the human only ever wants one process serving them.
func registerDashboardRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/", handleDashboardRoot)
	mux.HandleFunc("/terminals", handleDashboardTerminals)
	mux.HandleFunc("/dashboard/login", handleDashboardLogin)
	mux.HandleFunc("/api/auth/session", handleDashboardAuthSession)
	mux.HandleFunc("/api/snapshot", withGzip(withPerfTiming("/api/snapshot", handleDashboardSnapshot)))
	mux.HandleFunc("/api/perf", withGzip(handleDashboardPerf))
	mux.HandleFunc("/api/perf/reset", handleDashboardPerfReset)
	mux.HandleFunc("/api/costs", handleDashboardCosts)
	mux.HandleFunc("/api/audit", handleDashboardAudit)
	mux.HandleFunc("/api/logs", handleDashboardLogs)
	// The Processes tab consumes the same versioned REST surface as other
	// clients. Dashboard auth wraps it before the dynamic feature gate.
	mux.HandleFunc("GET /v1/process/templates", dashboardProcessRoute(handleProcessTemplates))
	mux.HandleFunc("GET /v1/process/template-heads", dashboardProcessRoute(handleProcessTemplateHeads))
	mux.HandleFunc("GET /v1/process/templates/{id}", dashboardProcessRoute(handleProcessTemplate))
	mux.HandleFunc("POST /v1/process/templates/{id}", dashboardProcessRoute(handleProcessTemplate))
	mux.HandleFunc("POST /v1/process/validate", dashboardProcessRoute(handleProcessValidate))
	mux.HandleFunc("GET /v1/process/runs", dashboardProcessRoute(handleProcessRuns))
	mux.HandleFunc("POST /v1/process/runs", dashboardProcessRoute(handleProcessRunCreate))
	mux.HandleFunc("GET /v1/process/runs/{id}", dashboardProcessRoute(handleProcessRun))
	mux.HandleFunc("GET /v1/process/runs/{id}/view", dashboardProcessRoute(handleProcessRunView))
	mux.HandleFunc("POST /v1/process/runs/{id}/nodes/{node}/signal", dashboardProcessRoute(handleProcessSignal))
	// The Worklist sub-view (TCL-297): the derived work-item list and the
	// human action funnel. asDashboardHumanPeer stamps the caller as the
	// operator, which is exactly the actor the action route records.
	mux.HandleFunc("GET /v1/process/worklist", dashboardProcessRoute(handleProcessWorklist))
	mux.HandleFunc("POST /v1/process/worklist/{itemId}/action", dashboardProcessRoute(handleProcessWorklistAction))
	mux.Handle("/static/", handleDashboardStatic())
	registerDashboardEditRoutes(mux)
}

func dashboardProcessRoute(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkDashboardAuth(w, r) {
			return
		}
		processRoute(next)(w, asDashboardHumanPeer(r))
	}
}

// handleDashboardStatic serves the dashboard's static assets — the
// stylesheet and the ES-module JS files — from the embedded dashboard/
// directory, behind the same session-cookie gate as /api/*.
//
// The assets are versioned with the agentd binary (//go:embed) and an
// embed.FS reports a zero modtime, so http.FileServerFS emits no
// Last-Modified / ETag validators. Cache-Control: no-store keeps a
// browser from running stale module JS after an agentd upgrade — on a
// loopback-only tool the lack of caching costs nothing.
func handleDashboardStatic() http.Handler {
	files := http.StripPrefix("/static/", http.FileServerFS(dashboardAssetsFS))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !checkDashboardAuth(w, r) {
			return
		}
		// No directory listings — only the named asset files are served.
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		files.ServeHTTP(w, r)
	})
}

// handleDashboardRoot serves the dashboard HTML behind a token-
// exchange (OAuth authorization-code style) flow:
//
//   - GET /?init_token=X — X is validated + consumed; on success the
//     long-lived session cookie is set and the browser is redirected
//     to the bare path, so the one-shot token never lingers in the
//     address bar, browser history, or an access log.
//   - GET / with a valid session cookie — serves the page (refresh or
//     a second tab in the already-authenticated browser).
//   - GET / with neither — refused. The cookie is NEVER handed out on
//     a bare GET; that is what stops a same-user agent process from
//     scraping it. An init token can only be minted via the human-only
//     `/v1/dashboard/open` endpoint on the daemon's Unix socket (or
//     the in-process tray handler).
//
// dashboardAppTabs are the top-level dashboard "location" path segments the
// SPA owns (TCL-317 back/forward navigation). A deep browser path like /access
// or /jobs must serve the same index HTML so the client router
// (js/nav-history.js) can restore the view on reload or a bookmarked deep link.
// Only the FIRST path segment is validated here; the client normalizes any
// deeper subtab/selection segments (/access/sudo, /processes/runs/<id>).
//
// Kept in sync with ROUTABLE_TABS in js/nav-history.js. Terminals is
// deliberately absent — /terminals is its own standalone popout route
// (handleDashboardTerminals) — and Vegas is a conditional soundtrack tab that
// is not URL-routed; neither is a bookmarkable location.
var dashboardAppTabs = map[string]bool{
	"groups": true, "jobs": true, "processes": true, "plugins": true, "access": true,
	"messages": true, "costs": true, "audit": true, "logs": true, "config": true,
	"debug": true,
}

// isDashboardAppPath reports whether a path should serve the dashboard SPA
// index HTML. It is the SPA fallback allow-list: the bare root, /dashboard, and
// any path whose first segment is a known app tab. Everything else (typos,
// /favicon.ico) still 404s, preserving the "don't render HTML for junk"
// property the strict pre-TCL-317 check gave. /api, /static, /v1, /terminals
// and /dashboard/login are registered as more specific mux patterns and never
// reach this handler, so they are unaffected.
func isDashboardAppPath(p string) bool {
	if p == "/" || p == "/dashboard" {
		return true
	}
	seg, _, _ := strings.Cut(strings.TrimPrefix(p, "/"), "/")
	return dashboardAppTabs[seg]
}

func handleDashboardRoot(w http.ResponseWriter, r *http.Request) {
	// `/` is a catch-all in net/http; serve the SPA only for known app paths
	// (the router restores the view from the URL) and reject anything else so
	// /favicon.ico etc. don't silently render the dashboard HTML.
	if !isDashboardAppPath(r.URL.Path) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	// Remote (mTLS + passphrase) requests are authenticated at the remote
	// listener's boundary; serve the page directly without the loopback
	// init-token / cookie exchange (which is the loopback path's concern).
	if dashboardPreAuthed(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(dashboardIndexHTML)
		return
	}
	if dashboardSessionToken == "" {
		http.Error(w, "dashboard token not initialised", http.StatusServiceUnavailable)
		return
	}

	// Exchange path: a valid init token is swapped for the session
	// cookie, then we 303 to the bare path so the one-shot token
	// drops out of the URL.
	if tok := r.URL.Query().Get("init_token"); tok != "" {
		if !consumeInitToken(tok, initScopeDashboard) {
			// Expired/replayed token (e.g. a stale bookmark, or a
			// browser that lost the race after a daemon restart).
			// Don't dead-end on plain text — render the sign-in page
			// so the human can re-authenticate in place.
			renderDashboardLoginPage(w, r, http.StatusForbidden,
				"That dashboard link has expired or was already used.")
			return
		}
		setDashboardSessionCookie(w)
		// Preserve the client-side URL state the dashboard JS reads on load —
		// the cosmetic theme (?slop=1 / ?wizard=1) AND the deep-link focus
		// (?tab=...&access_request=...) that the approval auto-raise / tray
		// build — across the bare-path bounce that drops the one-shot
		// init_token. Without this the "open the dashboard focused on THIS
		// request" deep link is lost on exactly the token-carrying path it
		// exists for. The init_token itself is intentionally dropped.
		redirectTarget := r.URL.Path
		if q := dashboardRedirectQuery(r); q != "" {
			redirectTarget += "?" + q
		}
		http.Redirect(w, r, redirectTarget, http.StatusSeeOther)
		return
	}

	// Already authenticated: an existing valid cookie (refresh / new
	// tab in the same browser) just gets the page.
	if c, err := r.Cookie(dashboardCookieName); err == nil {
		valid, refresh := dashboardSessionCookieMatch(c.Value)
		if !valid {
			renderDashboardLoginPage(w, r, http.StatusForbidden, "")
			return
		}
		if refresh {
			setDashboardSessionCookie(w)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(dashboardIndexHTML)
		return
	}

	// No token, no (valid) cookie — the common case is a stale cookie
	// after a daemon restart minted a fresh session token. Instead of a
	// dead plain-text 403, serve the sign-in page: it points the human
	// at `tclaude agent dashboard` (the zero-friction, no-secret path)
	// and also offers an operator-token field so they can sign in from
	// the browser without switching back to a terminal.
	renderDashboardLoginPage(w, r, http.StatusForbidden, "")
}

// dashboardLoginReturnTarget returns the same-origin dashboard location to
// restore after an operator-token login. A return_to supplied by the auth
// fetch wrapper wins; otherwise the current app URL becomes the target. Only
// known SPA paths and the standalone terminals page are accepted, so this can
// never become an open redirect. One-shot auth parameters are stripped.
func dashboardLoginReturnTarget(r *http.Request) string {
	raw := r.URL.Query().Get("return_to")
	if raw == "" {
		u := *r.URL
		u.Scheme, u.Host = "", ""
		raw = u.String()
	}
	u, err := url.Parse(raw)
	if err != nil || u.IsAbs() || u.Host != "" || !strings.HasPrefix(raw, "/") ||
		strings.HasPrefix(raw, "//") || strings.Contains(u.Path, `\`) {
		return "/"
	}
	u.Path = path.Clean(u.Path)
	u.RawPath = ""
	if !isDashboardAppPath(u.Path) && u.Path != "/terminals" {
		return "/"
	}
	q := u.Query()
	q.Del("init_token")
	q.Del("return_to")
	u.RawQuery = q.Encode()
	return u.String()
}

func dashboardLoginFormQuery(r *http.Request) string {
	target := dashboardLoginReturnTarget(r)
	if target == "/" {
		return ""
	}
	return "?" + url.Values{"return_to": []string{target}}.Encode()
}

// dashboardThemeParamKV returns the active cosmetic-theme URL param as a
// "key=value" fragment ("slop=1" / "wizard=1"), or "" when neither is set.
// It's returned without a leading ? or & so callers join it with whichever
// separator their URL needs. slop and wizard are mutually exclusive
// client-side re-skins; slop wins if both are somehow present, matching
// applySlopThemeIfRequested in slop.js. The result is one of a fixed set of
// internal constants, never echoed user input, so it is injection-safe to
// splice into a redirect target or the login form action.
func dashboardThemeParamKV(r *http.Request) string {
	q := r.URL.Query()
	if q.Get("slop") == "1" {
		return "slop=1"
	}
	if q.Get("wizard") == "1" {
		return "wizard=1"
	}
	return ""
}

// dashboardRedirectQuery builds the query string the auth redirect carries
// forward: the cosmetic theme param PLUS the deep-link focus (?tab=... &
// access_request=...) the dashboard JS reads on load. Values are url.Values-
// encoded, so a request-controlled tab / id can't inject a header or open-
// redirect (it stays a percent-escaped query on our own path); the dashboard
// JS treats an unknown tab / id as a harmless no-op. Returned without a leading
// separator, or "" when nothing needs carrying.
func dashboardRedirectQuery(r *http.Request) string {
	q := r.URL.Query()
	v := url.Values{}
	if kv := dashboardThemeParamKV(r); kv != "" {
		// kv is one of a fixed internal set ("slop=1"/"wizard=1"); split once.
		if k, val, ok := strings.Cut(kv, "="); ok {
			v.Set(k, val)
		}
	}
	if tab := q.Get("tab"); tab != "" {
		v.Set("tab", tab)
	}
	if ar := q.Get("access_request"); ar != "" {
		v.Set("access_request", ar)
	}
	return v.Encode()
}

// setDashboardSessionCookie writes the long-lived dashboard session
// cookie. HttpOnly keeps it out of page JS; SameSite=Strict keeps a
// cross-site navigation from carrying it. Shared by the init-token
// exchange (handleDashboardRoot) and the operator-token browser login
// (handleDashboardLogin) so both mint byte-identical cookies.
func setDashboardSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     dashboardCookieName,
		Value:    dashboardSessionToken,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

// handleDashboardLogin is the browser-side operator-token login. It is
// the POST target of the sign-in page's "paste your operator token"
// form (renderDashboardLoginPage) and the one place the dashboard
// session cookie can be minted WITHOUT a single-use init token.
//
// SECURITY — why this doesn't widen the agent boundary. The operator
// token (TCLAUDE_HUMAN_TOKEN, printed only on the agentd startup
// banner) is the human's credential: a sandboxed agent cannot read the
// human's environment, so it cannot supply a valid one here, and a
// NON-sandboxed same-uid process can already read ~/.tclaude directly —
// so against neither is this a new boundary. It is exactly as strong as
// the existing operator-token gate (see humantoken.go's threat model).
// The compare is constant-time and fails closed when no operator token
// was ever minted (operatorTokenMatches), so an agentd that could not
// mint one never accepts a login.
//
// CSRF: a cross-site page cannot read the operator token, so it cannot
// forge a valid POST; we additionally pin Origin/Referer to our own
// loopback base URL (same check as /api/*) as defense-in-depth.
func handleDashboardLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if dashboardSessionToken == "" {
		http.Error(w, "dashboard token not initialised", http.StatusServiceUnavailable)
		return
	}
	if !checkLoginOrigin(r) {
		renderDashboardLoginPage(w, r, http.StatusForbidden,
			"Request blocked: it didn't come from the dashboard page.")
		return
	}
	if !operatorTokenMatches(r.FormValue("token")) {
		// Uniform message for "blank", "wrong", and "no token was ever
		// minted" — never disclose which, and never echo the input back.
		renderDashboardLoginPage(w, r, http.StatusForbidden,
			"That operator token wasn't accepted. Copy it from the agentd startup banner (it begins with `tclo_`).")
		return
	}
	setDashboardSessionCookie(w)
	// A refresh now rides the cookie, and the posted token never lingers in
	// history or an access log. Restore the app/terminal location that was open
	// when authentication expired.
	http.Redirect(w, r, dashboardLoginReturnTarget(r), http.StatusSeeOther)
}

// handleDashboardAuthSession is the lightweight preflight for browser
// transports that cannot inspect a failed authentication response themselves
// (notably WebSocket). The normal cookie + Origin gate performs grace-cookie
// rotation or emits the fetch wrapper's login-required signal.
func handleDashboardAuthSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if !checkDashboardAuth(w, r) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// originMatchesBase reports whether an Origin or Referer header value
// belongs to base — the loopback popup base URL, e.g.
// "http://127.0.0.1:54321", with no trailing slash and no path.
//
// It anchors on the origin boundary instead of a bare prefix: an Origin
// header is exactly scheme://host:port (no path), so it must equal base;
// a Referer adds a path, so it must be base followed by "/". A plain
// strings.HasPrefix(value, base) would wrongly accept a port-superstring
// origin — e.g. base "http://127.0.0.1:655" is a string prefix of
// "http://127.0.0.1:6553", a different (valid) port a hostile same-user
// process could bind. Same-site cookies do not scope by port, so the
// origin pin is the real cross-port defense here; keep it exact.
func originMatchesBase(value, base string) bool {
	if base == "" {
		return false
	}
	return value == base || strings.HasPrefix(value, base+"/")
}

// checkLoginOrigin pins the login POST to our own loopback origin. A
// browser sends Origin on a same-origin POST and Referer on the form
// navigation; we accept the request when either matches popupBaseURL.
//
// When popupBaseURL is unset the pin is a no-op, but that case is
// unreachable in production: startPopupServer returns an empty URL only
// when it failed to bind a listener, and then registerDashboardRoutes is
// never called — so this handler does not exist. The no-op only matters
// to tests that register the mux directly; there the operator-token
// compare is still the real gate, so this stays belt-and-braces.
func checkLoginOrigin(r *http.Request) bool {
	// Non-loopback bind: the browser reaches the dashboard through some
	// other hostname/proxy (not the fixed loopback URL), so pin host-relative
	// the way the remote listener does. See dashboardHostRelativeOrigin.
	if !isLoopbackHost(dashboardBindHost) {
		return dashboardHostRelativeOrigin(r)
	}
	if popupBaseURL == "" {
		return true
	}
	if o := r.Header.Get("Origin"); o != "" {
		return originMatchesBase(o, popupBaseURL)
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		return originMatchesBase(ref, popupBaseURL)
	}
	// Neither header present: a real same-origin browser form POST
	// always sends at least one, so treat the absence as suspicious.
	return false
}

// dashboardHostRelativeOrigin is the same-origin check used when the dashboard
// is bound NON-loopback: the Origin (or, absent it, Referer) header's host must
// equal the host the request was sent to (r.Host). A direct analogue of the
// remote listener's remoteSameOrigin (remote_server.go), but with the fixed
// loopback base-URL pin dropped — the operator exposed the dashboard behind
// their own hostname/proxy, which agentd can't know in advance, so it can only
// require internal consistency. The SameSite=Strict dashboard cookie remains
// the primary CSRF defense (a cross-site context never sends it); rejecting a
// cross-host Origin here is belt-and-braces. Neither header present is treated
// as suspicious — a real browser form POST / same-origin fetch always sends one.
func dashboardHostRelativeOrigin(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" {
		return originHostMatchesRequest(o, r.Host)
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		return originHostMatchesRequest(ref, r.Host)
	}
	return false
}

// originHostMatchesRequest reports whether an Origin/Referer header value's
// host equals reqHost (the Host the request was sent to). url.Parse pulls the
// host out of both a bare Origin (scheme://host) and a Referer (scheme://host/path).
func originHostMatchesRequest(value, reqHost string) bool {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Host == reqHost
}

// handleDashboardOpen mints a fresh dashboard init token and returns
// the ready-to-open browser URL with the token embedded. Mounted on
// the daemon's Unix-socket `/v1` mux and gated by requireHuman: an
// agent (any caller with a Claude Code ancestor) is refused.
//
// This is the load-bearing gate of the whole dashboard-auth scheme.
// The dashboard's /api/* surface bypasses the per-agent permission
// system (asDashboardHumanPeer), so it must never be reachable by an
// agent. Peer-credential auth on the Unix socket is what distinguishes
// the human from an agent here — keep this endpoint human-only.
//
// `tclaude agent dashboard` calls this; the tray's "Open dashboard"
// mints in-process via mintInitToken(initScopeDashboard) instead.
func handleDashboardOpen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	if !requireHuman(w, r, "open the dashboard") {
		return
	}
	if popupBaseURL == "" {
		writeError(w, http.StatusServiceUnavailable, "dashboard",
			"daemon has no loopback URL bound; the dashboard is unavailable in this process")
		return
	}
	url := popupBaseURL + "/?init_token=" + mintInitToken(initScopeDashboard)
	if kv := dashboardThemeParamKV(r); kv != "" {
		url += "&" + kv
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": url})
}

// dashboardBrowserOpener is the browser-launch hook used by
// autoLaunchDashboard. Production points it at openBrowser; tests swap
// it for a capture func so the launch path runs without spawning a
// real browser.
var dashboardBrowserOpener = openBrowser

// shouldAutoLaunchDashboard reports whether `tclaude agentd serve`
// should open the dashboard at startup. The --auto-launch-dashboard
// flag (flagSet) and the persistent agent.auto_launch_dashboard config
// field OR together — either one opts in — so a service/autostart
// launch can enable it without carrying the flag.
func shouldAutoLaunchDashboard(flagSet bool, cfg *config.Config) bool {
	if flagSet {
		return true
	}
	return cfg != nil && cfg.Agent != nil && cfg.Agent.AutoLaunchDashboard
}

// autoLaunchDashboard mints a single-use init token in-process and
// opens the dashboard in the default browser. Mirrors the tray's "Open
// dashboard" click: the daemon IS the human side, so no socket round-
// trip through the human-only /v1/dashboard/open is needed.
//
// theme ("slop" / "wizard", or "" for none) tags the URL with ?slop=1 /
// ?wizard=1 so the dashboard JS swaps in the 🎰 slop machine or 🧙 wizard
// theme. Purely cosmetic — the data and routes are identical; the param
// survives the auth redirect (see handleDashboardRoot) so the bare-path URL
// ends up as /?slop=1 (or ?wizard=1) in the address bar.
//
// Best-effort — a missing loopback listener or a failed browser launch
// is logged and otherwise ignored; the daemon keeps running and the
// human can still run `tclaude agent dashboard`.
func autoLaunchDashboard(theme string) {
	if popupBaseURL == "" {
		slog.Warn("auto-launch-dashboard: no loopback URL bound; dashboard unavailable in this process")
		return
	}
	url := popupBaseURL + "/?init_token=" + mintInitToken(initScopeDashboard)
	switch theme {
	case "slop":
		url += "&slop=1"
	case "wizard":
		url += "&wizard=1"
	}
	if err := dashboardBrowserOpener(url); err != nil {
		slog.Warn("auto-launch-dashboard: failed to open browser", "error", err, "url", url)
		return
	}
	fmt.Println("  opening dashboard in your browser…")
}

// checkDashboardAuth gates every /api/* call: dashboard cookie value match +
// Origin/Referer pinned to the popup base URL (or host-relative when bound
// non-loopback). Refuses every /api/* call when the dashboard token isn't set
// (cryptographic randomness failed at startup).
func checkDashboardAuth(w http.ResponseWriter, r *http.Request) bool {
	// The remote (mTLS + passphrase) listener authenticates at its own
	// boundary (remoteAuthMiddleware) and tags the request; such requests
	// have already cleared a STRONGER bar than the loopback cookie, so accept
	// them without the loopback cookie/Origin checks (which pin to the
	// loopback base URL and would never match a remote origin).
	if dashboardPreAuthed(r) {
		return true
	}
	ok, loginRequired, code, msg := dashboardAuthResult(r)
	if !ok {
		if loginRequired {
			w.Header().Set("X-Tclaude-Login-Required", "1")
		}
		http.Error(w, msg, code)
		return false
	}
	if c, err := r.Cookie(dashboardCookieName); err == nil {
		if _, refresh := dashboardSessionCookieMatch(c.Value); refresh {
			setDashboardSessionCookie(w)
		}
	}
	return true
}

// dashboardAuthResult is the pure predicate behind checkDashboardAuth:
// it runs the cookie + Origin/Referer checks and returns whether the
// request is an authenticated dashboard caller, whether the browser must
// log in again, and the HTTP code + message to use on failure. Factored out
// so the audit middleware can
// ask "is this the authenticated operator?" without writing a response —
// attribution keys on a *valid session*, never on the response status (a
// post-auth policy 403, e.g. a blocklisted sudo grant, is still the
// operator and must not be downgraded to "unauthenticated"). See JOH-268.
func dashboardAuthResult(r *http.Request) (ok bool, loginRequired bool, code int, msg string) {
	if dashboardSessionToken == "" {
		return false, false, http.StatusServiceUnavailable, "dashboard not initialised"
	}
	c, err := r.Cookie(dashboardCookieName)
	if err != nil {
		return false, true, http.StatusForbidden, "missing or invalid dashboard cookie; load / first"
	}
	if valid, _ := dashboardSessionCookieMatch(c.Value); !valid {
		return false, true, http.StatusForbidden, "missing or invalid dashboard cookie; load / first"
	}
	// Non-loopback bind: host-relative same-origin (mirror the remote
	// listener), since the browser reaches us through a hostname/proxy the
	// fixed loopback pin would never match. Cookie above + SameSite=Strict
	// stay the primary gate.
	if !isLoopbackHost(dashboardBindHost) {
		if !dashboardHostRelativeOrigin(r) {
			return false, false, http.StatusForbidden, "Origin/Referer host mismatch"
		}
		return true, false, http.StatusOK, ""
	}
	origin := r.Header.Get("Origin")
	referer := r.Header.Get("Referer")
	if origin == "" && referer == "" {
		return false, false, http.StatusForbidden, "missing Origin and Referer"
	}
	// popupBaseURL is always bound in production when these routes are
	// registered; it is empty only in tests that register the mux without
	// a loopback listener, where the session cookie is the gate and the
	// origin pin is disabled (mirrors checkLoginOrigin's early return).
	if popupBaseURL != "" {
		if origin != "" && !originMatchesBase(origin, popupBaseURL) {
			return false, false, http.StatusForbidden, "Origin mismatch"
		}
		if origin == "" && !originMatchesBase(referer, popupBaseURL) {
			return false, false, http.StatusForbidden, "Referer mismatch"
		}
	}
	return true, false, http.StatusOK, ""
}

// snapshotPayload is the wire shape for /api/snapshot. One round-trip
// gives the page everything it needs to render every tab; the page
// re-fetches on a 2s timer.
type snapshotPayload struct {
	GeneratedAt string `json:"generated_at"`
	Version     string `json:"version"`
	// StaticVersion fingerprints the registry-shaped payload fields that rarely
	// change. The browser echoes it on the next poll; when it still matches,
	// StaticUnchanged is true and those fields are sent as null then restored
	// from the previous snapshot client-side.
	StaticVersion   string           `json:"static_version"`
	StaticUnchanged bool             `json:"static_unchanged,omitempty"`
	Groups          []dashboardGroup `json:"groups"`
	// Names and assignments only: environment values remain behind the
	// sandbox-profile management API. Keeping these in the regular snapshot
	// lets global/group quick selectors repaint without extra polling requests.
	SandboxProfiles       []string         `json:"sandbox_profiles"`
	SandboxProfileDefault string           `json:"sandbox_profile_default"`
	Agents                []dashboardAgent `json:"agents"`
	// AgentRosterAuthoritative is false when the active-actor query failed and
	// Agents may therefore be only the partial set recovered from group/grant
	// rows. Clients that react to roster departures must ignore such a snapshot
	// rather than treating a transient DB read failure as mass retirement.
	AgentRosterAuthoritative bool `json:"agent_roster_authoritative"`
	// Ungrouped: every active agent that is NOT a member of any group,
	// online or offline alike. Surfaces fresh-spawned agents, loose
	// convs and freshly-promoted offline conversations so the
	// dashboard's virtual "Ungrouped" group + the `+ add member`
	// overlay can show them as drag/add sources without a second
	// round-trip. (The overlay applies its own online filter on top.)
	// Same wire shape as Agents — empty when no loose convs exist.
	Ungrouped []dashboardAgent `json:"ungrouped"`
	// The retired-agents, non-agent conversations and replaced-generations
	// lists used to ride on this 2s snapshot in full. They grow unbounded
	// (a user can accumulate hundreds of retired agents), so each moved to
	// its own paginated, server-filtered endpoint — GET /api/retired,
	// /api/conversations, /api/replaced (dashboard_lists.go) — and is no
	// longer carried here.
	//
	// Pending: dashboard spawns whose conv-id has not materialised yet
	// (the pending_spawns table — JOH-205 inc2). A pending Codex agent
	// has a live tmux pane but is stuck behind a startup gate (untrusted
	// dir, new-hooks-config prompt, OpenAI auth modal), so it never took
	// the first turn that exposes its conv-id and is NOT an enrolled
	// agent yet. Surfaced as a distinct list so the operator can SEE it
	// and click its focus button to open the pane and clear the gate;
	// the sweeper then promotes it into Agents. Empty slice (not nil) so
	// JS .map() / .length are safe.
	Pending     []dashboardPending      `json:"pending"`
	Permissions snapshotPermissionsView `json:"permissions"`
	Slugs       []PermSlug              `json:"slugs"`
	Cron        []dashboardCronJob      `json:"cron"`
	// ExportJobsActive counts the per-agent export jobs still in flight
	// (neither ready nor failed) — the Jobs nav tab's badge. The job rows
	// themselves do NOT ride the snapshot: the Jobs tab fetches its unified,
	// paginated window from GET /api/jobs alongside the poll
	// (dashboard_jobs.go), mirroring the retired/conversations/replaced split.
	ExportJobsActive int `json:"export_jobs_active"`
	// RetiredTotal drives cross-tab badges/actions without fetching the full,
	// paginated retired roster outside the Groups tab.
	RetiredTotal int `json:"retired_total"`
	// Sudo: every active grant across all agents, ordered by conv-id
	// then soonest expiry. Powers the dedicated "Sudo" tab. Per-agent
	// active state also surfaces on Agents[*].ActiveSudo so the Groups
	// + Agents tabs can render the 🔓 indicator without a second
	// round-trip.
	Sudo []dashboardSudoEntry `json:"sudo"`
	// Links surfaces every inter-group link in the system. The dashboard
	// renders these in a dedicated panel (read-only in v1) and uses them
	// to annotate group rows with outbound/inbound counts. Empty slice
	// (not nil) so JS .length / .map() are safe.
	Links []dashboardLink `json:"links"`
	// Usage is the account-wide subscription usage readout (5h + 7d
	// rolling windows) rendered in the dashboard's top bar. Always
	// present — Available=false carries the graceful "n/a" state.
	Usage dashboardUsage `json:"usage"`
	// Templates are the group-template blueprints rendered in the
	// Templates tab. Empty slice (not nil) so JS .map() is safe.
	Templates []templateJSON `json:"templates"`
	// Profiles + Roles are the spawn-profile and role-library registries,
	// carried on the poll so the retractable right-side palette dock
	// (JOH-374) renders them off the live snapshot like the rest of the
	// dashboard — no separate fetch, so a manager edit shows up on the next
	// tick. Both are the same wire shapes their /api/{spawn-profiles,roles}
	// endpoints serve. Empty slices (not nil) so JS .map() is safe.
	Profiles []spawnProfileJSON `json:"profiles"`
	// SpawnProfileDefault is the global spawn-profile assignment used after a
	// group's own default. It rides the snapshot with the profile registry and
	// sandbox-profile assignments so open dashboards observe CLI changes without
	// a separate request on every two-second poll.
	SpawnProfileDefault string     `json:"spawn_profile_default"`
	Roles               []roleJSON `json:"roles"`
	// Messages are the human-facing notifications agents have sent via
	// `tclaude agent notify-human`, newest first — the Messages tab.
	// MessagesUnread is the count of unread ones, driving the tab badge.
	Messages       []dashboardHumanMessage `json:"messages"`
	MessagesUnread int                     `json:"messages_unread"`
	// AccessRequests are the in-flight human-approval requests — an agent is
	// BLOCKED waiting for the operator to approve/deny a permission-gated
	// action (formerly the loopback-only browser popup). They ride the poll so
	// the Messages tab's "Access requests" folder + the attention overlay
	// render off the live snapshot and work over the remote listener too.
	// AccessRequestsPending is the count, driving the blinking tab badge +
	// overlay. Empty slice (not nil) so JS .map()/.length are safe.
	AccessRequests        []dashboardAccessRequest `json:"access_requests"`
	AccessRequestsPending int                      `json:"access_requests_pending"`
	// Plugins are the human-managed external integrations on the
	// Plugins tab, with their cached step-check statuses (the snapshot
	// never runs the checks itself — see plugins.go). PluginsCatalog is
	// the built-in set offered for one-click install; PluginsWarn
	// counts plugins with a failing check and drives the nav badge.
	Plugins        []dashboardPlugin `json:"plugins"`
	PluginsCatalog []Plugin          `json:"plugins_catalog"`
	PluginsWarn    int               `json:"plugins_warn"`
	// PluginsError carries a plugins.json read/parse failure so the tab
	// shows "registry broken: …" instead of a silently empty list. The
	// poll itself stays 200 — one bad file must not take down every tab.
	PluginsError string `json:"plugins_error,omitempty"`
	// PluginsTabVisible drives the Plugins tab's auto-hide, mirroring the
	// Costs tab's rule: true when at least one plugin is installed, OR
	// plugins.json failed to read (so the error is never hidden), OR the
	// human opted into always showing it (config
	// dashboard.always_show_plugins_tab). When false the dashboard hides the
	// Plugins nav button + section entirely — the "don't show an empty
	// Plugins tab" rule for the majority of users who never define one.
	PluginsTabVisible bool `json:"plugins_tab_visible"`
	// DebugTabVisible drives the Debug tab's auto-hide (config
	// dashboard.show_debug_tab, TCL-376). Display-only: the poll-timing
	// recorder and /api/perf serve regardless, so history exists from
	// before the tab was switched on. Re-read on every snapshot so the
	// Config-tab toggle takes effect without restarting agentd.
	DebugTabVisible bool `json:"debug_tab_visible"`
	// ProcessesEnabled gates both the experimental tab chrome and its REST
	// surface. It is re-read on every snapshot so changing config takes effect
	// without restarting agentd, matching processRoute.
	ProcessesEnabled bool `json:"processes_enabled"`
	// UserDefaultModel is the "model" key from the user-level
	// ~/.claude/settings.json — what every claude launched without
	// --model falls back to. "" = unset (claude's built-in default).
	// Shown in the Groups tab header and used by the spawn modal to
	// label what "Default" actually resolves to.
	UserDefaultModel string `json:"user_default_model"`
	// Harnesses is the catalog of spawnable harnesses (claude, codex) with
	// each one's valid model/effort/sandbox menus and capability flags. The
	// spawn dialog drives its harness selector + per-harness model/effort/
	// sandbox menus off this, and the per-row controls gate rename on the
	// agent's harness can_rename (JOH-162). Built from the harness registry
	// so a newly-registered harness appears with no dashboard edit.
	Harnesses []dashboardHarness `json:"harnesses"`
	PopupBase string             `json:"popup_base"` // for tray-shareable display
	// NotificationsEnabled mirrors config.notifications.enabled — the
	// master OS-notification switch above the per-group / per-agent
	// filters. Drives the top-bar bell toggle.
	NotificationsEnabled bool `json:"notifications_enabled"`
	// SpawnNameNormalize mirrors config.SpawnNameNormalizeEnabled (config
	// agent.spawn_name_normalize, default on). The spawn modal reads it to
	// decide whether to auto-normalize an invalid agent name to the safe
	// branch-token charset (the default) or reject it with the inline error.
	SpawnNameNormalize bool `json:"spawn_name_normalize"`
	// VegasInRegularMode mirrors config slop.vegas_in_regular_mode — the
	// opt-in that surfaces the Vegas music features (the Vegas tab, the
	// header volume mixer + sound switch, the lounge radio) on the PLAIN
	// dashboard, decoupled from the full slop casino re-skin. refresh.js
	// toggles body.vegas off this so the music/tab/volume light up in
	// regular mode without the slot machines and FX. Default false.
	VegasInRegularMode bool `json:"vegas_in_regular_mode"`
	// HidePullLever mirrors config slop.hide_pull_lever — the opt-out that
	// hides the slop-mode side pull-lever (the casino lever on the Groups
	// tab). refresh.js toggles body.hide-slop-lever off this; CSS then drops
	// the lever while leaving the rest of slop mode intact. Default false.
	HidePullLever bool `json:"hide_pull_lever"`
	// ActivityBots mirrors config dashboard.activity_bots — the per-mode
	// STYLE of the deduped "activity bot" indicator in group headers + the
	// top bar. Each field is "emoji" | "sprites" | "off"; the front-end
	// (render.js) emits a regular-mode, a slop-mode and a wizard-mode row and
	// CSS shows the one for the current mode. Defaults: regular + wizard emoji
	// (wizard's opt-in "sprites" resolves to the wizard sheets), slop sprites.
	// See ActivityBotsRegular / ActivityBotsSlop / ActivityBotsWizard.
	ActivityBots activityBotsView `json:"activity_bots"`
	// HScrollFollow mirrors config dashboard.hscroll_follow — whether the
	// full-bleed chrome bars (header / nav / slop marquee) keep their content
	// pinned to the viewport (follow, the default) while the page is scrolled
	// sideways, or let it scroll off (static). refresh.js toggles
	// body.hscroll-follow off this each poll; it replaces the old per-browser
	// header toggle button. Default true. See Config.HScrollFollow.
	HScrollFollow bool `json:"hscroll_follow"`
	// GroupQuickOptions mirrors config dashboard.group_quick_options — the
	// display mode for the editable chips in each group <summary> header
	// ("hover" folds them to icon-only at rest + expands on hover, the
	// default; "expanded" always shows them). refresh.js toggles
	// body.group-quick-fold off this each poll. See Config.GroupQuickOptions.
	GroupQuickOptions string `json:"group_quick_options"`
	// DefaultTerminal mirrors config dashboard.default_terminal — how the
	// dashboard's per-agent focus / open-window / open-terminal actions and bulk
	// windows-modal focus open a console: "native" (pop a native OS window, the
	// default) or "web" (open an in-browser terminal pane in the Terminals tab).
	// row-actions.js / palette.js / refresh.js read this off each poll to route
	// those actions; the dedicated "web term" / "web window" buttons ignore it
	// (always web). See Config.DefaultTerminal.
	DefaultTerminal string `json:"default_terminal"`
	// DefaultDirectoryPicker mirrors dashboard.default_directory_picker for
	// local connections. The client additionally forces web mode whenever its
	// hostname is not loopback.
	DefaultDirectoryPicker string `json:"default_directory_picker"`
	// ShowAgentHideButton mirrors config dashboard.show_agent_hide_button —
	// whether each agent row's "hide window" button (the slashed-eye beside
	// "focus", data-act="hide") is shown. Off by default: the button detaches
	// the agent's terminal window and is far less used than "focus", so the
	// row hides it to stay tight. refresh.js toggles body.show-agent-hide-btn
	// off this each poll; CSS drops the button unless the class is present.
	// See Config.ShowAgentHideButton.
	ShowAgentHideButton bool `json:"show_agent_hide_button"`
	// ShowGroupDescription mirrors config dashboard.show_group_description —
	// whether each group header's 📝 description chip is shown. Off by default:
	// group descriptions are a deprecated, display-only feature, so the chip is
	// hidden to keep headers tight. refresh.js toggles body.show-group-description
	// off this each poll; CSS drops the chip unless the class is present. See
	// Config.ShowGroupDescription.
	ShowGroupDescription bool `json:"show_group_description"`
	// CostTabVisible drives the Costs tab's auto-hide: true when there is
	// real pay-per-token spend to show OR a subscription account has opted
	// into the WHAT-IF view (config cost.show_on_subscription). When false
	// the dashboard hides the Costs nav button + section entirely — the
	// "don't show an empty Costs tab on a subscription" rule.
	CostTabVisible bool `json:"cost_tab_visible"`
	// CostTabWhatIf is true when the Costs tab is showing the hypothetical
	// subscription estimate (no real spend, but the opt-in is on) rather
	// than real spend — the front-end renders the WHAT-IF banner and fetches
	// /api/costs?whatif=1. Implies CostTabVisible.
	CostTabWhatIf bool `json:"cost_tab_whatif"`
	// RemoteAccess surfaces the optional network-exposed dashboard listener's
	// live state so the Config tab can guide setup: whether the cert/passphrase
	// material has been generated (run `tclaude remote-access setup` first) and
	// whether THIS agentd already has the listener running. config.json carries
	// the saved intent; this carries the live reality so the UI can flag a
	// "no material yet" foot-gun and a "restart agentd to apply" pending state.
	RemoteAccess dashboardRemoteAccess `json:"remote_access"`
}

// activityBotsView carries the resolved per-mode activity-bot style to the
// dashboard — each is "emoji" | "sprites" | "off" (config
// dashboard.activity_bots, defaulted by ActivityBotsRegular /
// ActivityBotsSlop / ActivityBotsWizard). The front-end picks Regular vs Slop
// vs Wizard off body.slop / body.wizard.
type activityBotsView struct {
	Regular string `json:"regular"`
	Slop    string `json:"slop"`
	Wizard  string `json:"wizard"`
}

// dashboardRemoteAccess is the snapshot view of the remote-access feature's
// runtime state — distinct from the config.json `remote_access` block the
// Config tab edits (delivered via /api/config). The dashboard reads this to
// render accurate status next to the toggle without a second round-trip.
type dashboardRemoteAccess struct {
	// MaterialExists is remoteaccess.Exists(): whether `tclaude remote-access
	// setup` has generated the CA/cert/passphrase material. Enabling the
	// listener without it is a no-op (agentd logs + skips), so the UI warns.
	MaterialExists bool `json:"material_exists"`
	// Running reports whether THIS agentd process started the remote listener
	// (true only after a successful startRemoteServer at boot). Lets the UI
	// distinguish "enabled & live" from "enabled but a restart is pending".
	Running bool `json:"running"`
	// RunningBind is the address the live listener is actually serving on,
	// "" when Running is false. The UI compares it to the edited bind to spot
	// an unsaved/pending bind change.
	RunningBind string `json:"running_bind"`
}

// dashboardHarness is the snapshot view of one spawnable harness — its
// identifier + human label, the model/effort/sandbox values its spawn
// menus offer, and the capability flags the per-row controls gate on. The
// dashboard never hard-codes a harness; it renders this list.
type dashboardHarness struct {
	// Name is the stable identifier ("claude", "codex") forwarded as the
	// spawn body's `harness` and matched against an agent's state.harness.
	Name string `json:"name"`
	// DisplayName is the human label ("Claude Code", "Codex").
	DisplayName string `json:"display_name"`
	// Models is the curated suggestion list for Model menus. It need not be an
	// allow-list: harness validation remains authoritative and every populated
	// dropdown includes a custom-ID entry. Empty means a harness has no
	// suggestions, so the dashboard falls back to a free-text model field.
	Models []string `json:"models"`
	// EffortLevels is the reasoning-effort scale for the Effort menu, in
	// ascending order. Both harnesses share tclaude's levels today.
	EffortLevels []string `json:"effort_levels"`
	// SandboxModes lists the launch-time OS-sandbox modes this harness
	// accepts for the spawn dialog's Sandbox selector (Codex: tclaude-agent /
	// workspace-write / read-only / danger-full-access; Claude Code:
	// inherit / on / off). Empty only for a harness with no sandbox catalog.
	SandboxModes []string `json:"sandbox_modes"`
	// DefaultSandbox is the recommended default mode the spawn dialog
	// pre-selects (Codex: the managed tclaude-agent profile; Claude Code:
	// inherit). "" when SandboxModes is empty.
	DefaultSandbox string `json:"default_sandbox"`
	// SandboxModeHelp maps each sandbox mode value to a one-line description
	// the dialog shows as a live hint for the selected option (notably its
	// agentd-socket reachability). {} (not null) when the harness has no
	// launch sandbox, so the JS lookup is always safe.
	SandboxModeHelp map[string]string `json:"sandbox_mode_help"`
	// ApprovalModes lists the launch-time approval/permission modes this
	// harness surfaces as a dropdown (Claude Code: inherit + its
	// --permission-mode values; Codex: its --ask-for-approval policies).
	// The ApprovalCatalog parallels SandboxModes.
	ApprovalModes []string `json:"approval_modes"`
	// DefaultApproval is the recommended approval mode the dialog pre-selects
	// (Claude Code: inherit). "" when ApprovalModes is empty.
	DefaultApproval string `json:"default_approval"`
	// ApprovalModeHelp maps each approval mode to a one-line hint (notably
	// whether it is safe for a detached agent). {} (not null) when the harness
	// surfaces no approval modes, so the JS lookup is always safe.
	ApprovalModeHelp map[string]string `json:"approval_mode_help"`
	// AskTimeoutModes lists the Claude Code AskUserQuestion idle-timeout values
	// this harness surfaces as a dropdown (inherit / never / 60s / 5m / 10m).
	// Empty for a harness with no AskUserQuestion dialog (Codex), so the dialog
	// hides the selector. The AskTimeoutCatalog parallel to SandboxModes.
	AskTimeoutModes []string `json:"ask_timeout_modes"`
	// DefaultAskTimeout is the recommended value the dialog pre-selects (Claude
	// Code: inherit). "" when AskTimeoutModes is empty.
	DefaultAskTimeout string `json:"default_ask_timeout"`
	// AskTimeoutModeHelp maps each value to a one-line hint. {} (not null) when
	// the harness surfaces no timeout, so the JS lookup is always safe.
	AskTimeoutModeHelp map[string]string `json:"ask_timeout_mode_help"`
	// CanRename / CanCompact mirror Harness.CanRename / CanCompact — the
	// deliverable-action predicates the per-row controls gate on. Note
	// CanRename is true for Codex (it renames via its ConvStore even
	// without an in-pane /rename), so the dashboard keeps Codex renameable.
	CanRename  bool `json:"can_rename"`
	CanCompact bool `json:"can_compact"`
	// CanSandbox reports whether the harness takes a launch sandbox flag —
	// the same condition as a non-empty SandboxModes, surfaced explicitly
	// so the dialog has a single boolean to gate the sandbox row on.
	CanSandbox bool `json:"can_sandbox"`
	// CanApproval reports whether the harness has a launch approval/permission
	// catalog. The dialog's approval row additionally gates on a non-empty
	// ApprovalModes, mirroring how the sandbox row gates on can_sandbox &&
	// sandbox_modes.length.
	CanApproval bool `json:"can_approval"`
	// CanAutoReview reports whether approval requests can be routed to a
	// harness-owned reviewer instead of a human. This is intentionally distinct
	// from CanApproval: Codex exposes both axes, while Claude Code's permission
	// catalog has no separate approvals_reviewer control.
	CanAutoReview bool `json:"can_auto_review"`
	// CanAskTimeout reports whether the harness has a launch AskUserQuestion
	// idle-timeout catalog (Claude Code). The dialog's timeout row gates on this
	// + a non-empty AskTimeoutModes, mirroring the sandbox row.
	CanAskTimeout bool `json:"can_ask_timeout"`
	// CanRemoteControl mirrors Harness.CanRemoteControl — true only for a
	// harness with a built-in Remote Access toggle (Claude Code), false for
	// one without it (Codex). The per-row remote-control toggle gates on
	// this exactly the way the rename control gates on CanRename (JOH-259).
	CanRemoteControl bool `json:"can_remote_control"`
}

// buildHarnessCatalog assembles the spawnable-harness catalog for the
// snapshot from the harness registry. Only spawnable harnesses (those with
// a Spawner + ModelCatalog) are listed — the spawn dialog is the only
// consumer, and a non-spawnable harness has nothing to offer it. Ordered
// by Names() (sorted) so the dialog's harness selector is stable.
func buildHarnessCatalog() []dashboardHarness {
	out := []dashboardHarness{}
	for _, name := range harness.Names() {
		h, err := harness.ResolveSpawnable(name)
		if err != nil {
			continue // not spawnable — skip
		}
		dh := dashboardHarness{
			Name:             h.Name,
			DisplayName:      h.DisplayName,
			Models:           h.Models.Models(),
			EffortLevels:     h.Models.EffortLevels(),
			CanRename:        h.CanRename(),
			CanCompact:       h.CanCompact(),
			CanSandbox:       h.SupportsSandbox(),
			CanApproval:      h.SupportsApproval(),
			CanAutoReview:    h.SupportsAutoReview(),
			CanAskTimeout:    h.SupportsAskTimeout(),
			CanRemoteControl: h.CanRemoteControl(),
		}
		if dh.Models == nil {
			dh.Models = []string{} // JSON [] not null, so JS .map() is safe
		}
		dh.SandboxModeHelp = map[string]string{}
		if h.SupportsSandbox() {
			dh.SandboxModes = h.Sandbox.Modes()
			dh.DefaultSandbox = h.Sandbox.DefaultMode()
			for _, m := range dh.SandboxModes {
				dh.SandboxModeHelp[m] = h.Sandbox.ModeHelp(m)
			}
		} else {
			dh.SandboxModes = []string{}
		}
		// Approval modes mirror the sandbox block. The dialog gates its row on a
		// non-empty ApprovalModes as well as CanApproval.
		dh.ApprovalModeHelp = map[string]string{}
		dh.ApprovalModes = []string{}
		if h.SupportsApproval() {
			if modes := h.Approval.Modes(); len(modes) > 0 {
				dh.ApprovalModes = modes
				dh.DefaultApproval = h.Approval.DefaultPolicy()
				for _, m := range modes {
					dh.ApprovalModeHelp[m] = h.Approval.ModeHelp(m)
				}
			}
		}
		// AskUserQuestion idle-timeout modes mirror the sandbox block: a
		// Claude-only catalog (never|60s|5m|10m + inherit), hidden for a harness
		// with no AskUserQuestion dialog (Codex).
		dh.AskTimeoutModeHelp = map[string]string{}
		if h.SupportsAskTimeout() {
			dh.AskTimeoutModes = h.AskTimeout.Modes()
			dh.DefaultAskTimeout = h.AskTimeout.DefaultMode()
			for _, m := range dh.AskTimeoutModes {
				dh.AskTimeoutModeHelp[m] = h.AskTimeout.ModeHelp(m)
			}
		} else {
			dh.AskTimeoutModes = []string{}
		}
		out = append(out, dh)
	}
	return out
}

// dashboardLink is the snapshot view of one agent_group_links row.
// Group names are pre-resolved so the renderer doesn't need to do a
// second lookup.
type dashboardLink struct {
	ID        int64  `json:"id"`
	From      string `json:"from"`
	To        string `json:"to"`
	Mode      string `json:"mode"`
	CreatedAt string `json:"created_at,omitempty"`
}

// dashboardCronJob is the snapshot view of one agent_cron_jobs row.
// Mirrors jobJSON in cron_handlers.go but adds a few resolved fields
// — owner/target labels and the most-recent run row — so the
// dashboard can render a self-contained table without a second
// fetch per row.
type dashboardCronJob struct {
	ID               int64  `json:"id"`
	Name             string `json:"name"`
	OwnerAgent       string `json:"owner_agent,omitempty"`
	OwnerConv        string `json:"owner_conv"`
	OwnerLabel       string `json:"owner_label"`
	TargetKind       string `json:"target_kind"`
	TargetAgent      string `json:"target_agent,omitempty"`
	TargetConv       string `json:"target_conv,omitempty"`
	TargetLabel      string `json:"target_label,omitempty"`
	GroupID          int64  `json:"group_id"`
	GroupName        string `json:"group_name,omitempty"`
	TargetRole       string `json:"target_role,omitempty"`
	IntervalSeconds  int64  `json:"interval_seconds"`
	CronExpr         string `json:"cron_expr,omitempty"`
	CronDesc         string `json:"cron_desc,omitempty"` // English rendering of CronExpr; best-effort, may be empty
	Subject          string `json:"subject,omitempty"`
	Body             string `json:"body"`
	Enabled          bool   `json:"enabled"`
	RunImmediately   bool   `json:"run_immediately"`
	QueueWhenOffline bool   `json:"queue_when_offline"`
	CreatedAt        string `json:"created_at,omitempty"`
	LastRunAt        string `json:"last_run_at,omitempty"`
	LastRunStatus    string `json:"last_run_status,omitempty"`
	NextDueAt        string `json:"next_due_at,omitempty"`
}

type snapshotPermissionsView struct {
	Defaults []string            `json:"defaults"`
	Grants   map[string][]string `json:"grants"`
	// Overrides is the full tri-state per-conv view — conv-id → slug →
	// "grant" | "deny" — that the permanent-permission editor reads to
	// pre-populate its modal. Grants (above) is the grant-only
	// projection, kept for the read-only Permissions tab.
	Overrides map[string]map[string]string `json:"overrides"`
}

type dashboardGroup struct {
	Name           string   `json:"name"`
	Descr          string   `json:"descr"`
	DefaultCwd     string   `json:"default_cwd"`     // pre-fills the spawn form's cwd; "" = none
	DefaultContext string   `json:"default_context"` // shared startup context injected into spawned agents; "" = none
	DefaultProfile string   `json:"default_profile"` // spawn profile whose launch fields fill blank spawn fields for this group's agents; "" = none (the spawn default's single source — the vestigial default_model was dropped, JOH-220)
	SandboxProfile string   `json:"sandbox_profile"` // filesystem/environment profile assigned to this group; "" = inherit global
	Permissions    []string `json:"permissions"`     // live additive grants held by current group members
	MaxMembers     int      `json:"max_members"`     // hard member cap; 0 = unlimited. A spawn that would exceed it is refused.
	NotifyEnabled  bool     `json:"notify_enabled"`  // group OS-notification switch; false mutes every member (per-agent 'on' still overrides)
	// RemoteControlPolicy is the group's remote-control policy that overrides a
	// spawn profile's remote-control default (JOH-262): "inherit" (defer to the
	// profile), "optin" (force Remote Access on) or "deny" (force it off).
	RemoteControlPolicy string `json:"remote_control_policy"`
	// Mission and SourceTemplate are deployment provenance (JOH-245): what
	// the group was deployed against and the template it was
	// instantiated/deployed from. Both "" for a group not created from a
	// template — the Task Forces framing on the Templates tab renders a group
	// as a deployed force only when SourceTemplate is set.
	Mission        string `json:"mission,omitempty"`
	SourceTemplate string `json:"source_template,omitempty"`
	// Parent is the NAME of the group this one is nested under (n-level
	// groups-in-groups, JOH-392); "" = top-level. Resolved from the row's
	// parent_id against the same snapshot's group set — name-keyed to match
	// how the client's sibling-order and collapse prefs already key. A
	// parent_id that doesn't resolve to a live group (should not happen — the
	// FK is ON DELETE SET NULL) is shipped as "" so the child renders
	// top-level rather than dangling.
	Parent string `json:"parent,omitempty"`
	// Process is the group's advisory process state (JOH-242): the current
	// phase, the ordered phase map, and the transition log. nil for a group
	// with no process (the phase chip + advance control render only when set).
	Process *processStateJSON `json:"process,omitempty"`
	// Waves is the group's staged-spawn choreography status (JOH-244): the
	// current wave + how many waves/agents are still pending. nil once the
	// choreography is complete (or for a single-wave deploy) — the "wave N/M
	// pending" chip renders only while set.
	Waves *waveStatusJSON `json:"waves,omitempty"`
	// Scribe marks the group as a daemon-created scribe's eponymous system
	// group (descr == scribeGroupDescr — the circle-scribe machinery, JOH-361).
	// The Groups tab always surfaces these while at least one member is online;
	// dormant scribe groups remain behind the "show offline scribes" toggle.
	// The flag is the discriminator, so the client need not match a name.
	Scribe  bool              `json:"scribe,omitempty"`
	Members []dashboardMember `json:"members"`
	Online  int               `json:"online"`
}

// dashboardMember.Owner mirrors the memberJSON convention from
// /v1/groups/{name}/members:
//   - true on a member row → that member is also a group owner
//     (rendered as a badge alongside the role).
//   - true on a row with Role=="owner" and no descr → a pure owner
//     who isn't a member (so the list stays comprehensive).
type dashboardMember struct {
	// AgentID is the member's stable actor key — the canonical, rotation-immune
	// ID the roster leads with; ConvID is the live generation behind it (still
	// the internal drag-and-drop / routing key).
	AgentID string `json:"agent_id,omitempty"`
	ConvID  string `json:"conv_id"`
	Title   string `json:"title"`
	// CreatedAt is the earliest known existence timestamp: actor birth
	// (agents.created_at) or, for a late-enrolled legacy actor, the older
	// conv_index.Created — see snapshotRowCache.createdFor. Emitted as UTC
	// RFC3339Nano, empty when unknown. Rendered as a relative
	// "Age" column and the default sort key (newest first).
	CreatedAt string `json:"created_at,omitempty"`
	Role      string `json:"role,omitempty"`
	Descr     string `json:"descr,omitempty"`
	// agentLocationView carries `branch` (current branch) plus the
	// startup/current directory split — see agent_location_view.go.
	agentLocationView
	// repoLinksView carries the GitHub web links for the branch cells
	// — dashboard-only enrichment, see branchlinks.go.
	repoLinksView
	// taskRefView carries the per-agent task-reference link (Task
	// column) — see taskref.go.
	taskRefView
	// tagsView carries the per-agent tag set (chips in the Description
	// column) — see tags.go.
	tagsView
	Online bool       `json:"online"`
	Owner  bool       `json:"owner,omitempty"`
	State  agentState `json:"state"`
	// Notify is the per-agent override ("on"/"off", "" = inherit);
	// NotifyEffective folds the agent + group levels together (the
	// global switch is separate — snapshot.notifications_enabled).
	Notify          string `json:"notify,omitempty"`
	NotifyEffective bool   `json:"notify_effective"`
}

type dashboardAgent struct {
	// AgentID is the agent's stable actor key — the canonical, rotation-immune
	// ID; ConvID is the live generation behind it (still the internal key).
	AgentID string `json:"agent_id,omitempty"`
	ConvID  string `json:"conv_id"`
	Title   string `json:"title"`
	// agentLocationView carries `branch` (current branch) plus the
	// startup/current directory split — see agent_location_view.go.
	agentLocationView
	// repoLinksView carries the GitHub web links for the branch cells
	// — dashboard-only enrichment, see branchlinks.go.
	repoLinksView
	// taskRefView carries the per-agent task-reference link (Task
	// column) — see taskref.go.
	taskRefView
	// tagsView carries the per-agent tag set (chips in the Description
	// column) — see tags.go.
	tagsView
	Online      bool                 `json:"online"`
	State       agentState           `json:"state"`
	Groups      []string             `json:"groups"`
	OwnedGroups []string             `json:"owned_groups"`          // subset of Groups the agent owns; UI tags these distinctly
	Effective   []string             `json:"effective"`             // perms = (defaults ∪ active-group grants ∪ per-conv grants) − per-conv denies
	ActiveSudo  []dashboardSudoEntry `json:"active_sudo,omitempty"` // current sudo grants (slug + id + remaining); empty when none
	// Notify is the per-agent override ("on"/"off", "" = inherit);
	// NotifyEffective folds the agent + group levels together (the
	// global switch is separate — snapshot.notifications_enabled).
	Notify          string `json:"notify,omitempty"`
	NotifyEffective bool   `json:"notify_effective"`
}

// dashboardConversation is the snapshot view of one non-enrolled
// conversation — a promotion candidate in the Agents tab's second
// list. Deliberately leaner than dashboardAgent: a plain conversation
// has no groups, permissions or sudo state to render.
type dashboardConversation struct {
	ConvID string     `json:"conv_id"`
	Title  string     `json:"title"`
	Online bool       `json:"online"`
	State  agentState `json:"state"`
	// Modified is the conv's last-activity RFC3339 stamp, so the
	// dashboard can show "how recent" without a second lookup.
	Modified string `json:"modified,omitempty"`
}

// dashboardRetiredAgent is the snapshot view of one retired agent —
// rendered in the Agents tab's "Retired" section with a reinstate
// button. Carries the retire audit fields so the human can see who
// demoted it and why.
type dashboardRetiredAgent struct {
	// AgentID is the retired actor's stable key — the canonical ID the
	// dashboard/CLI leads with; ConvID is the live generation behind it
	// (kept as the snapshot/hover).
	AgentID   string `json:"agent_id,omitempty"`
	ConvID    string `json:"conv_id"`
	Title     string `json:"title"`
	Online    bool   `json:"online"`
	RetiredAt string `json:"retired_at,omitempty"`
	// RetiredBy is the RAW audit value the retire recorded — a conv-id when an
	// agent performed the retire, or a literal ("human", "system:export-clone").
	// Kept for provenance (the JS surfaces it as the cell's hover title);
	// RetiredByDisplay is what the column actually renders.
	RetiredBy string `json:"retired_by,omitempty"`
	// RetiredByDisplay is the resolved, human-readable retirer (JOH-306): the
	// retirer's current name plus stable short agent_id ("name (agt_xxxxxxxx)"),
	// the bare short agent_id when the name can't be resolved, or the raw
	// RetiredBy literal when there is no stable companion at all.
	RetiredByDisplay string `json:"retired_by_display,omitempty"`
	RetireReason     string `json:"retire_reason,omitempty"`
}

// dashboardReplacedGen is the snapshot view of one REPLACED (predecessor)
// conversation generation — a past conv-id of a still-existing actor, left
// behind when a reincarnate / Claude Code /clear advanced the actor's live
// pointer (JOH-26). Rendered in the Groups tab's default-hidden "Replaced
// generations" virtual group so the operator can see (and prune) the
// generations the actor-level Retired tray no longer lists. Annotated with the
// owning actor so a row points back to the live agent, and with how/when this
// generation was superseded (the reason + timestamp of the generation that
// replaced it). Leaner than a roster row — a predecessor has no live identity
// state of its own.
type dashboardReplacedGen struct {
	ConvID string `json:"conv_id"` // this predecessor generation
	Title  string `json:"title"`   // the predecessor's own title, else the actor's
	// Reason / ReplacedAt describe how + when this generation was SUPERSEDED:
	// the rotation reason ("reincarnate" | "clear" | …) and RFC3339 timestamp
	// of the generation that replaced it.
	Reason     string `json:"reason,omitempty"`
	ReplacedAt string `json:"replaced_at,omitempty"`
	Online     bool   `json:"online"` // ~always false — a predecessor has no live pane
	// ActorConvID / ActorTitle point at the still-live (or retired) actor this
	// generation belongs to, so the row can link back to the current agent.
	// ActorAgentID is the actor's stable key — the canonical ID to link the
	// row back to the live agent (ActorConvID is its current generation,
	// kept as the snapshot/hover). NOTE: the row's own ConvID stays a bare
	// conv-id on purpose — it names this predecessor GENERATION, not an
	// actor (a KEEP-2 case), so it gets no agent_id companion.
	ActorAgentID string `json:"actor_agent_id,omitempty"`
	ActorConvID  string `json:"actor_conv_id"`
	ActorTitle   string `json:"actor_title"`
	ActorRetired bool   `json:"actor_retired,omitempty"` // the owning actor is itself retired
}

// dashboardPending is the snapshot view of one not-yet-enrolled
// dashboard spawn (a pending_spawns row). It carries what the dashboard
// needs to render the pending agent and drive its focus button: its stable
// AgentID, the spawn Label (which is the session-row id AND the focus key — a
// pending agent has no conv-id), its intended group/role/name/descr, whether
// its tmux pane is still alive, and where it is gated (Cwd). Leaner than
// dashboardAgent — a pending spawn has no groups/permissions/sudo to show.
type dashboardPending struct {
	// AgentID is reserved before the harness launches, so the pending row can
	// show the same stable identity as the enrolled-agent table before a
	// harness conversation id exists. Empty only for legacy pending rows.
	AgentID string `json:"agent_id,omitempty"`
	// Label is the spawn label = the session-row id. The focus button
	// keys on THIS (not a conv-id, which does not exist yet).
	Label string `json:"label"`
	// Group is the resolved name of the group the spawn will join (from
	// the row's group_id), or "" for an ungrouped spawn.
	Group string `json:"group,omitempty"`
	Role  string `json:"role,omitempty"`
	Name  string `json:"name,omitempty"`
	Descr string `json:"descr,omitempty"`
	// Online reports whether the spawn's tmux pane is still alive — a
	// pending row whose pane has died (operator closed it, or the spawn
	// crashed) is stale and can no longer be focused to clear the gate.
	Online bool `json:"online"`
	// Cwd is where the agent is gated (from its session row), so the
	// operator sees which untrusted dir to trust. Harness is the spawn's
	// harness ("codex" in practice — Claude Code fires its hook at launch
	// and never lands here). Both empty when no session row exists yet.
	Cwd     string `json:"cwd,omitempty"`
	Harness string `json:"harness,omitempty"`
	// CreatedAt is the RFC3339Nano spawn time (how long it has been
	// pending), used to sort newest-first and show age.
	CreatedAt string `json:"created_at,omitempty"`
}

// dashboardSudoEntry is the wire shape for one active sudo grant in
// the snapshot. Used both as agent[*].active_sudo[] (per-row "this
// agent currently holds these") and as the top-level Sudo[] (full
// list across all agents for the dedicated tab).
type dashboardSudoEntry struct {
	ID               int64  `json:"id"`
	AgentID          string `json:"agent_id,omitempty"` // stable actor key the grant is keyed on
	ConvID           string `json:"conv_id,omitempty"`  // omitted on agent[*].active_sudo (caller already knows)
	ConvTitle        string `json:"conv_title,omitempty"`
	Slug             string `json:"slug"`
	GrantedAt        string `json:"granted_at"`
	ExpiresAt        string `json:"expires_at"`
	GrantedBy        string `json:"granted_by,omitempty"`
	Reason           string `json:"reason,omitempty"`
	RemainingSeconds int64  `json:"remaining_seconds"`
}

// agentState mirrors what `tclaude session ls` shows: status from the
// hook callbacks (idle / working / awaiting_*), last hook timestamp,
// the agent's cwd, and subagent count. Empty string fields when no
// live session row exists for the conv.
type agentState struct {
	Status        string `json:"status,omitempty"`
	StatusDetail  string `json:"status_detail,omitempty"`
	SubagentCount int    `json:"subagent_count,omitempty"`
	LastHook      string `json:"last_hook,omitempty"`
	Cwd           string `json:"cwd,omitempty"`
	// Context-window usage, read from the same sessions row the hook
	// callbacks update — no new data source, one extra indexed read.
	// ContextPct is Claude Code's authoritative "how full" figure from
	// the statusline hook (0 = not reported yet). The token counts feed
	// the dashboard context-meter's tooltip; all zero means the
	// statusline hook hasn't fired for this session yet, which the UI
	// renders as a neutral / empty meter.
	ContextPct        float64 `json:"context_pct,omitempty"`
	TokensInput       int64   `json:"tokens_input,omitempty"`
	TokensOutput      int64   `json:"tokens_output,omitempty"`
	ContextWindowSize int64   `json:"context_window_size,omitempty"`
	// Model is the LLM model display name the agent is running on
	// ("Opus 4.8", "Sonnet 4.6", …), recorded by the statusline hook.
	// Empty until the statusbar has ticked at least once; the dashboard
	// renders it as the harness line under the per-row controls and in
	// the status-dot tooltip. Surfaced regardless of liveness — a frozen
	// model for an exited agent is still informative.
	Model string `json:"model,omitempty"`
	// EffortLevel is the reasoning-effort level the agent is running on
	// ("low"…"max"), recorded by the statusline hook on the same row as
	// Model. Empty until the statusbar has ticked, or when the model
	// lacks reasoning-effort support; the dashboard appends it to the
	// per-agent model line ("CC · O4.8 1M high") and omits it when empty.
	EffortLevel string `json:"effort_level,omitempty"`
	// CostUSD is the agent's cumulative API cost in USD, recorded by the
	// statusline hook on the same row — but only when the session runs
	// on API/enterprise pricing (no subscription rate-limit data). 0
	// means "no cost data" (subscription plan, or no tick yet) and the
	// dashboard renders no cost badge for it. Surfaced regardless of
	// liveness, like Model — what a dead agent cost is still informative.
	CostUSD float64 `json:"cost_usd,omitempty"`
	// VirtualCostUSD is the WHAT-IF sibling of CostUSD: the agent's
	// cumulative pay-per-token-EQUIVALENT cost on a subscription session. 0
	// on pay-per-token (CostUSD carries the real figure there) or before a
	// tick. The Groups tab shows it as the per-agent cost badge — flagged
	// hypothetical — only when the WHAT-IF view is active
	// (snapshot.cost_tab_whatif). Surfaced regardless of liveness, like CostUSD.
	VirtualCostUSD float64 `json:"virtual_cost_usd,omitempty"`
	// ExitReason is why a now-offline agent's session ended: a graceful
	// SessionEnd `reason`, a daemon-owned clean reason, or 'unexpected'
	// when a harness-specific reaper path has a positive abnormal-death
	// signal. Only populated for an offline agent; empty for a live one,
	// a normal Codex close with no explicit reason, or a row that exited
	// before the exit_reason column existed. The dashboard renders
	// 'unexpected' as "crashed" and everything else (incl. empty) as a
	// plain exit.
	ExitReason string `json:"exit_reason,omitempty"`
	// Harness is the coding tool this agent runs under ("claude", "codex"),
	// from the session row. Empty (a conv with no session row) renders as
	// the default (Claude Code). The dashboard badges it per row and gates
	// the rename control on the matching harness's can_rename (JOH-162).
	// Surfaced regardless of liveness — a dead Codex agent is still Codex.
	Harness string `json:"harness,omitempty"`
	// SandboxMode is the launch-time OS-sandbox mode the agent was spawned
	// under (Codex: read-only / workspace-write / danger-full-access; Claude
	// Code: on / off — its `inherit` default normalizes to "" and records
	// nothing). "" renders no sandbox badge. Surfaced regardless of liveness.
	SandboxMode string `json:"sandbox_mode,omitempty"`
	// RemoteControl is tclaude's best-known state of whether the harness's
	// built-in Remote Access is enabled for this agent (JOH-256). It is a
	// best-known flag — the harness exposes no readback, so the dashboard
	// reflects the recorded intent and reconciles on refresh. Surfaced
	// regardless of liveness; the per-row toggle lets the operator flip an
	// agent's remote access before stepping away (JOH-259). The CAPABILITY
	// gate (which harness can be remote-controlled at all) is not here — it
	// rides the harness catalog's can_remote_control, the same place the
	// rename control reads can_rename from.
	RemoteControl bool `json:"remote_control,omitempty"`
}

// stateForConvIn looks up the most-recent live tmux session row for
// this conv-id and returns its hook-tracked state. When no tmux session
// is alive the agent has exited: the hook-recorded Status is frozen at
// whatever it was when the process died (usually "idle" from the final
// Stop hook, since no SessionEnd-style hook fires on exit), so we
// report StatusExited rather than passing the stale value through —
// otherwise a dead agent masquerades as "idle" on the dashboard.
// LastHook is preserved either way so the UI can show when the agent
// was last active.
//
// For a LIVE agent the hook status flows through verbatim — including
// StatusError from a StopFailure hook. The exited override below is
// keyed on tmux liveness, not on the status string, so an errored but
// still-running agent keeps its "error" status (its CC process is
// alive; only its last turn failed).
//
// Snapshot-shaped: takes a pre-fetched alive set (the SAME map across
// every call in one HTTP request). Callers MUST fetch the set once via
// clcommon.Default.ListSessions at the top of the handler and reuse
// it; per-call fetching defeats the purpose.
func stateForConvIn(convID string, aliveSet map[string]struct{}) agentState {
	rows, err := db.FindSessionsByConvID(convID)
	if err != nil {
		return agentState{}
	}
	return stateForConvInSessions(rows, aliveSet)
}

// stateForConvInSessions is stateForConvIn over an already-fetched session
// slice (most-recent-first, as FindSessionsByConvID returns). The dashboard
// snapshot's per-request batch loader (TCL-368) resolves each conv's state
// through it so the conv's rows are read once per poll rather than per surface.
// Behaviour is identical to stateForConvIn — including the codex read-through
// (refreshCodexContextSnapshotOnRead) and the per-pick context / exit-reason
// point reads, which stay per-conv.
func stateForConvInSessions(rows []*db.SessionRow, aliveSet map[string]struct{}) agentState {
	return stateForConvInSessionsTimed(rows, aliveSet, nil)
}

func stateForConvInSessionsTimed(rows []*db.SessionRow, aliveSet map[string]struct{}, recordCodexTelemetry func(time.Duration)) agentState {
	if len(rows) == 0 {
		return agentState{}
	}
	pick := rows[0] // already sorted most-recent first
	alive := false
	for _, r := range rows {
		if r.TmuxSession == "" {
			continue
		}
		if _, ok := aliveSet[r.TmuxSession]; ok {
			pick = r
			alive = true
			break
		}
	}
	out := agentState{
		Status:       pick.Status,
		StatusDetail: pick.StatusDetail,
		Cwd:          pick.Cwd,
		// Harness + sandbox are launch properties of the row, surfaced
		// regardless of liveness (a dead Codex agent is still Codex). The
		// exited override below only touches Status/StatusDetail.
		Harness:     pick.Harness,
		SandboxMode: pick.SandboxMode,
		// RemoteControl is tclaude's best-known Remote Access flag for the
		// conv (JOH-256), surfaced regardless of liveness like the other
		// launch/row properties — the dashboard reflects the recorded intent
		// and reconciles on the next refresh (the harness has no readback).
		RemoteControl: pick.RemoteControl,
	}
	// Codex records collaboration-child lifecycle in its rollout even when an
	// explicit interrupt does not invoke the configured SubagentStop hook. The
	// same read-through scan that refreshes context telemetry reconstructs the
	// authoritative interrupted IDs and removes them from the shared hook ledger
	// below. Other lifecycle still comes from hooks: Codex rollouts do not carry a
	// terminal normal-completion activity event.
	codexInterruptedSubagents := refreshCodexContextSnapshotOnReadTimed(pick, alive, recordCodexTelemetry)
	// Sub-agents run INSIDE the harness process, so a dead session has
	// none by definition — a stale count on an exited row must not render
	// a "🤖+N" badge. For a live row, prefer the TTL-filtered ledger over
	// the raw cached count so a phantom entry (a sub-agent whose
	// SubagentStop was lost) stops being displayed as soon as it expires,
	// even if no hook has fired since to sweep it from storage. An empty
	// ledger with a non-zero count is a row last written by a pre-ledger
	// hook binary — surface its raw count rather than hiding real work.
	if alive {
		if set := db.ParseSubagentSet(pick.SubagentsJSON); set != nil {
			for id := range codexInterruptedSubagents {
				delete(set, id)
			}
			out.SubagentCount = set.LiveCount(time.Now())
		} else {
			out.SubagentCount = pick.SubagentCount
		}
		// Keep the status consistent with the TTL-filtered count: a row
		// can be stuck at main_agent_idle / "N subagents running" when
		// the ledger emptied without a SubagentStop (TTL expiry with no
		// later hook to re-settle it). The badge already reads 0 then —
		// showing a busy "N subagents running" next to it would be
		// self-contradicting, so settle the DISPLAY to idle; the stored
		// row converges on the session's next hook.
		if out.SubagentCount == 0 && out.Status == session.StatusMainAgentIdle {
			out.Status = session.StatusIdle
			out.StatusDetail = ""
		}
	}
	if !pick.LastHook.IsZero() {
		out.LastHook = pick.LastHook.Format(time.RFC3339)
	}
	// Context-window usage rides on the same sessions row — the
	// statusline hook (UpdateContextSnapshot) keeps it current. We
	// surface it regardless of liveness: a frozen context_pct for an
	// exited agent is genuinely informative ("it died at 80%"), unlike
	// a frozen "idle" status that would mislabel a dead agent.
	if snap, err := db.GetContextSnapshot(pick.ID); err == nil {
		out.ContextPct = snap.ContextPct
		out.TokensInput = snap.TokensInput
		out.TokensOutput = snap.TokensOutput
		out.ContextWindowSize = snap.ContextWindowSize
		out.Model = snap.Model
		out.EffortLevel = snap.EffortLevel
		out.CostUSD = snap.CostUSD
		out.VirtualCostUSD = snap.VirtualCostUSD
	}
	// No live tmux session — the agent's process is gone. Report it as
	// exited rather than letting the frozen hook status (typically
	// "idle") masquerade as a running state. StatusDetail is cleared so
	// stale "idle: Bash"-style leftovers don't leak into the snapshot.
	if !alive {
		out.Status = session.StatusExited
		out.StatusDetail = ""
		// Surface WHY it ended so the dashboard can tell a clean exit
		// from an unexpected death. pick is the most-recently-updated
		// row — the SessionEnd hook and the reaper both bump the row
		// they touch, so the latest row carries the authoritative
		// reason. An empty result (NULL exit_reason — a pre-migration
		// corpse, or a death the reaper has not swept yet) renders as a
		// plain exit, never as a crash.
		if reason, err := db.GetSessionExitReason(pick.ID); err == nil {
			out.ExitReason = reason
		} else {
			slog.Warn("dashboard: read exit_reason failed",
				"session", pick.ID, "error", err)
		}
	}
	return out
}

// handleDashboardSnapshot returns one JSON blob covering everything
// the page renders: groups + members, all known agents, the live
// permission state (defaults + per-conv grants), and the slug
// registry. Read-only.
func handleDashboardSnapshot(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	// This response is dynamic even when static_version remains unchanged: the
	// query parameter only lets us omit stable registry blobs, while agent
	// liveness and status still change on every poll. Never let a browser or
	// intermediary reuse an older response for the same versioned URL.
	w.Header().Set("Cache-Control", "no-store")
	// Wall-clock phase marks land in the /api/perf ring (perf.go,
	// TCL-374). Nil-safe: a direct call outside withPerfTiming (tests)
	// simply records nothing.
	span := perfSpanFrom(r)
	// One tmux ls for the whole snapshot. Every isConvOnlineIn /
	// stateForConvIn call below tests liveness via map lookup off this
	// set — replacing ~150 per-poll `has-session` subprocess spawns
	// with one. Routed through the short-TTL cache (TCL-370) so this
	// tick's other parallel poll handlers (/api/retired,
	// /api/conversations) share the same probe instead of each forking
	// their own `tmux ls`; the span mark below reads ~0 on a cache hit.
	// Errors / no-server collapse to an empty map (== "all offline"),
	// matching what per-row probes would have reported when the tmux
	// server is down.
	aliveSessions, _ := cachedLiveTmuxSessions()
	span.mark("tmux_ls")

	groups, _ := db.ListAgentGroups()
	sandboxProfiles, _ := db.ListSandboxProfiles()
	globalSandboxProfile, _ := db.GetGlobalSandboxProfile()
	allGrants, _ := db.ListAllAgentPermissions()
	allOverrides, _ := db.ListAllAgentPermissionOverrides()
	cfg, _ := config.Load()
	defaults := []string{}
	if cfg != nil && cfg.Agent != nil {
		defaults = append(defaults, cfg.Agent.DefaultPermissions...)
	}
	sort.Strings(defaults)

	// agentRows: union of (every group member) + (every conv-id with
	// explicit grants). Keyed by conv-id so members appearing in
	// multiple groups dedupe naturally.

	// Load each group's members + owners ONCE. ListAgentGroupMembers used to
	// run twice per group (the muted-notify pass below + the main group loop),
	// and ListAgentGroupOwners once in the group loop; keying both by group id
	// here lets inMutedGroup, the conv-set collect and the group loop all reuse
	// a single read.
	membersByGroup := make(map[int64][]*db.AgentGroupMember, len(groups))
	ownersByGroup := make(map[int64][]*db.AgentGroupOwner, len(groups))
	for _, g := range groups {
		members, _ := db.ListAgentGroupMembers(g.ID)
		membersByGroup[g.ID] = members
		owners, _ := db.ListAgentGroupOwners(g.ID)
		ownersByGroup[g.ID] = owners
	}

	// Notification-filter state: the per-agent overrides plus the set
	// of convs sitting in at least one muted (non-archived) group.
	// notifyEffective mirrors notify.AllowedForConv — agent pref wins,
	// else any muted group silences — so the bells the dashboard
	// renders agree with what the notify path will actually do.
	notifyPrefs, _ := db.ListConvNotifyPrefs()
	inMutedGroup := map[string]bool{}
	for _, g := range groups {
		if g.IsArchived() || g.NotifyEnabled {
			continue
		}
		for _, m := range membersByGroup[g.ID] {
			inMutedGroup[m.ConvID] = true
		}
	}
	notifyEffective := func(convID string) bool {
		switch notifyPrefs[convID] {
		case db.NotifyPrefOn:
			return true
		case db.NotifyPrefOff:
			return false
		}
		return !inMutedGroup[convID]
	}

	// Retired / superseded conv sets, loaded UP FRONT (TCL-369) so addAgent can
	// skip a retired/superseded conv before paying for its ~13-query row
	// resolution, rather than building it and discarding it in the output loop.
	// Both also remain the belt-and-braces roster filter further down.
	//
	// Retired ACTORS are demoted — they must never reach out.Agents. The
	// actor-level roster is keyed on agents.retired_at, which only a human
	// retire sets (a reincarnate / Claude Code /clear predecessor stays a
	// generation of its active actor, so it is NOT here).
	retiredAgents, _ := db.ListRetiredAgents()
	retiredSet := make(map[string]bool, len(retiredAgents))
	for _, e := range retiredAgents {
		retiredSet[e.CurrentConvID] = true
	}
	// Superseded conversations — the predecessors of a reincarnation chain —
	// are NOT agents: their identity moved to the chain head. The actor-level
	// roster already excludes them (a predecessor is never an active actor's
	// current conv), so supersededSet is a redundant read-time belt-and-braces
	// guard — symmetric with retiredSet.
	supersededSet := map[string]bool{}
	if successions, err := db.ListAgentConvSuccessions(); err == nil {
		for _, s := range successions {
			supersededSet[s.OldConvID] = true
		}
	}

	// Every active ACTOR — the canonical roster (JOH-26 PR3b reads the
	// agent-level roster, one row per live actor at its current conv). Unlike
	// the old "online ungrouped session" probe, this includes OFFLINE agents: a
	// conv that was an agent yesterday keeps showing after its tmux pane closed,
	// instead of silently vanishing. Plain conversations that were never
	// promoted are not here — they surface in out.Conversations.
	activeAgents, activeAgentsErr := db.ListActiveAgents()

	// Active sudo grants across every agent. One DB scan; bucketed per conv-id
	// below for O(1) per-agent Active rendering plus the top-level Sudo[] tab.
	sudoGrants, _ := db.ListAllActiveSudoGrants()

	// Collect the full conv set the snapshot will render — group members ∪
	// owners ∪ grant/override holders ∪ active agents ∪ sudo holders — then
	// batch-load every per-conv table once (TCL-368/TCL-367). rc.viewFor(convID)
	// returns a memoized row bundle so the member loop, addAgent and the owners
	// pass share ONE computation instead of ~13 point queries each, and the
	// location resolves from the batch without a per-poll .jsonl rescan.
	convSet := map[string]struct{}{}
	addConv := func(convID string) {
		if convID != "" {
			convSet[convID] = struct{}{}
		}
	}
	for _, members := range membersByGroup {
		for _, m := range members {
			addConv(m.ConvID)
		}
	}
	for _, owners := range ownersByGroup {
		for _, o := range owners {
			addConv(o.ConvID)
		}
	}
	for convID := range allGrants {
		addConv(convID)
	}
	for convID := range allOverrides {
		addConv(convID)
	}
	for _, e := range activeAgents {
		addConv(e.CurrentConvID)
	}
	for _, g := range sudoGrants {
		addConv(g.ConvID)
	}
	convIDs := make([]string, 0, len(convSet))
	for convID := range convSet {
		convIDs = append(convIDs, convID)
	}
	rc := newSnapshotRowCache(convIDs, aliveSessions)

	// Preload every agent's task-reference link once (a single query)
	// rather than a lookup per member/agent in this 2s-polled path.
	// Keyed by agent_id; callers pass the row's cached agent_id (from rc)
	// so no per-row AgentIDForConv query is needed.
	taskRefs, _ := db.ListAgentTaskRefs()
	taskRefFor := func(agentID string) taskRefView {
		return taskRefViewFor(taskRefs[agentID])
	}
	presentedPRs := preloadPresentedPRsForDashboard(time.Now())
	presentedPRsFor := func(agentID string) []presentedPRView {
		return presentedPRViews(presentedPRs[agentID])
	}
	// Same preload discipline as taskRefs: one ListAllAgentTags per snapshot,
	// keyed by agent_id, looked up per row (not a query per member/agent in
	// this 2s-polled path). The stored set is already sorted alphabetically.
	allTags, _ := db.ListAllAgentTags()
	tagsFor := func(agentID string) tagsView {
		return tagsView{Tags: allTags[agentID]}
	}
	span.mark("preload")

	agentRows := map[string]*dashboardAgent{}
	addAgent := func(convID string) *dashboardAgent {
		if existing, ok := agentRows[convID]; ok {
			return existing
		}
		// TCL-369: a retired/superseded conv never becomes an agent row, so
		// skip its resolution entirely instead of building it and dropping it
		// in the output loop. Callers that use the return value nil-check it.
		if retiredSet[convID] || supersededSet[convID] {
			return nil
		}
		b := rc.viewFor(convID)
		links := b.Links
		links.PresentedPRs = presentedPRsFor(b.AgentID)
		a := &dashboardAgent{
			AgentID:           b.AgentID,
			ConvID:            convID,
			Title:             b.Title,
			agentLocationView: b.Loc,
			repoLinksView:     links,
			taskRefView:       taskRefFor(b.AgentID),
			tagsView:          tagsFor(b.AgentID),
			Online:            b.Online,
			State:             b.State,
			// init non-nil so JSON serializes [] not null;
			// the dashboard's JS does .length / .map without a guard.
			Groups:          []string{},
			OwnedGroups:     []string{},
			Effective:       []string{},
			Notify:          notifyPrefs[convID],
			NotifyEffective: notifyEffective(convID),
		}
		agentRows[convID] = a
		return a
	}

	out := snapshotPayload{
		GeneratedAt:          time.Now().Format(time.RFC3339),
		Version:              buildversion.AppVersion(),
		PopupBase:            popupBaseURL,
		UserDefaultModel:     readUserDefaultModel(),
		Harnesses:            buildHarnessCatalog(),
		NotificationsEnabled: cfg != nil && cfg.Notifications != nil && cfg.Notifications.Enabled,
		SpawnNameNormalize:   cfg.SpawnNameNormalizeEnabled(),
		VegasInRegularMode:   cfg.ShowVegasInRegularMode(),
		HidePullLever:        cfg.HidePullLever(),
		ActivityBots: activityBotsView{
			Regular: cfg.ActivityBotsRegular(),
			Slop:    cfg.ActivityBotsSlop(),
			Wizard:  cfg.ActivityBotsWizard(),
		},
		HScrollFollow:            cfg.HScrollFollow(),
		GroupQuickOptions:        cfg.GroupQuickOptions(),
		DefaultTerminal:          cfg.DefaultTerminal(),
		DefaultDirectoryPicker:   cfg.DefaultDirectoryPicker(),
		ShowAgentHideButton:      cfg.ShowAgentHideButton(),
		ShowGroupDescription:     cfg.ShowGroupDescription(),
		ProcessesEnabled:         cfg.ProcessesEnabled(),
		AgentRosterAuthoritative: activeAgentsErr == nil,
		Permissions: snapshotPermissionsView{
			Defaults:  defaults,
			Grants:    map[string][]string{},
			Overrides: map[string]map[string]string{},
		},
		Slugs: append([]PermSlug{}, permissionRegistry...),
	}
	out.RetiredTotal = len(retiredAgents)
	sort.Slice(out.Slugs, func(i, j int) bool { return out.Slugs[i].Slug < out.Slugs[j].Slug })

	// Remote-access runtime state for the Config tab's guidance (JOH-227): has
	// setup generated the material, and is the listener live in this process.
	raRunning, raBind := remoteListenerStatus()
	out.RemoteAccess = dashboardRemoteAccess{
		MaterialExists: remoteaccess.Exists(),
		Running:        raRunning,
		RunningBind:    raBind,
	}

	// Initialise slices empty (not nil) so JSON serializes [] instead
	// of null — the dashboard's JS does .length on members directly,
	// which would crash on null.
	out.Groups = []dashboardGroup{}
	out.SandboxProfiles = []string{}
	for _, profile := range sandboxProfiles {
		out.SandboxProfiles = append(out.SandboxProfiles, profile.Name)
	}
	if globalSandboxProfile != nil {
		out.SandboxProfileDefault = globalSandboxProfile.Name
	}
	out.Agents = []dashboardAgent{}
	// id→name for resolving each group's parent_id to a parent NAME the
	// client tree keys off. Built from the same group set we're serializing,
	// so a parent_id with no live group simply doesn't resolve (child stays
	// top-level) — the FK's ON DELETE SET NULL should already prevent that.
	groupNameByID := make(map[int64]string, len(groups))
	groupGrantsByConv := map[string][]string{}
	for _, g := range groups {
		groupNameByID[g.ID] = g.Name
	}
	for _, g := range groups {
		groupPermissions, _ := db.ListAgentGroupPermissions(g.ID)
		dg := dashboardGroup{Name: g.Name, Descr: g.Descr, DefaultCwd: g.DefaultCwd, DefaultContext: g.DefaultContext, DefaultProfile: g.DefaultProfile, SandboxProfile: g.SandboxProfile, Permissions: groupPermissions, MaxMembers: g.MaxMembers, NotifyEnabled: g.NotifyEnabled, RemoteControlPolicy: remoteControlPolicyToWire(g.RemoteControl), Mission: g.Mission, SourceTemplate: g.SourceTemplate, Scribe: isScribeGroup(g), Members: []dashboardMember{}}
		if g.ParentGroupID != nil {
			dg.Parent = groupNameByID[*g.ParentGroupID]
		}
		// Advisory process state (JOH-242): attach the current phase + phase map
		// + transition log so the group view can render a phase chip + advance
		// control. nil for a group with no process.
		if st, trs, perr := loadGroupProcess(g.ID); perr == nil && st != nil {
			pv := processStateToJSON(st, trs)
			dg.Process = &pv
		}
		// Staged-spawn choreography status (JOH-244): a "wave N/M pending" chip
		// while later waves are still deferred. nil when the deploy is complete.
		if wv := loadWaveStatus(g.ID); wv != nil {
			dg.Waves = wv
		}
		members := membersByGroup[g.ID]
		if !g.IsArchived() {
			for _, m := range members {
				groupGrantsByConv[m.ConvID] = append(groupGrantsByConv[m.ConvID], groupPermissions...)
			}
		}
		// Pre-load the owner set so we can tag members who are also
		// owners. Mirrors handleGroupMembersList in handlers.go.
		ownerSet := map[string]bool{}
		for _, o := range ownersByGroup[g.ID] {
			ownerSet[o.ConvID] = true
		}
		memberSet := map[string]bool{}
		for _, m := range members {
			memberSet[m.ConvID] = true
			b := rc.viewFor(m.ConvID)
			links := b.Links
			links.PresentedPRs = presentedPRsFor(b.AgentID)
			dg.Members = append(dg.Members, dashboardMember{
				AgentID:           b.AgentID,
				ConvID:            m.ConvID,
				Title:             b.Title,
				CreatedAt:         b.Created,
				Role:              m.Role,
				Descr:             m.Descr,
				agentLocationView: b.Loc,
				repoLinksView:     links,
				taskRefView:       taskRefFor(b.AgentID),
				tagsView:          tagsFor(b.AgentID),
				Online:            b.Online,
				Owner:             ownerSet[m.ConvID],
				State:             b.State,
				Notify:            notifyPrefs[m.ConvID],
				NotifyEffective:   notifyEffective(m.ConvID),
			})
			if b.Online {
				dg.Online++
			}
			// addAgent returns nil for a retired/superseded member (TCL-369):
			// its member row above still renders, but it never joins the
			// Agents/Ungrouped roster.
			if a := addAgent(m.ConvID); a != nil {
				a.Groups = append(a.Groups, g.Name)
				if ownerSet[m.ConvID] {
					a.OwnedGroups = append(a.OwnedGroups, g.Name)
				}
			}
		}
		// Surface owners who aren't members so the list stays
		// comprehensive. Same shape as the CLI:
		// role="owner", no descr.
		for ownerConv := range ownerSet {
			if memberSet[ownerConv] {
				continue
			}
			b := rc.viewFor(ownerConv)
			ownerLinks := b.Links
			ownerLinks.PresentedPRs = presentedPRsFor(b.AgentID)
			dg.Members = append(dg.Members, dashboardMember{
				AgentID:           b.AgentID,
				ConvID:            ownerConv,
				Title:             b.Title,
				CreatedAt:         b.Created,
				Role:              "owner",
				agentLocationView: b.Loc,
				repoLinksView:     ownerLinks,
				taskRefView:       taskRefFor(b.AgentID),
				tagsView:          tagsFor(b.AgentID),
				Online:            b.Online,
				Owner:             true,
				State:             b.State,
				Notify:            notifyPrefs[ownerConv],
				NotifyEffective:   notifyEffective(ownerConv),
			})
			// Pure-owners are reachable via this group too — surface
			// the group on the agent's row in the Agents view so
			// "what groups can this conv see?" matches reality. nil for a
			// retired/superseded owner (TCL-369).
			if a := addAgent(ownerConv); a != nil {
				a.Groups = append(a.Groups, g.Name)
				a.OwnedGroups = append(a.OwnedGroups, g.Name)
			}
		}
		// Default ordering: newest-first by creation time (the Age
		// column; the JS column sort treats this as the natural order it
		// falls back to). Mirrors handleGroupMembersList.
		sortMembersByAge(dg.Members,
			func(m dashboardMember) string { return m.CreatedAt },
			func(m dashboardMember) string { return m.ConvID })
		out.Groups = append(out.Groups, dg)
	}
	codexTelemetryInGroups := rc.codexTelemetryDuration
	span.markExcluding("groups", codexTelemetryInGroups)
	for convID, slugs := range allGrants {
		addAgent(convID)
		copySlice := append([]string{}, slugs...)
		sort.Strings(copySlice)
		out.Permissions.Grants[convID] = copySlice
	}
	// Full tri-state overrides (grant AND deny) for the editor modal.
	// A conv with only a deny override is still an agent — surface it.
	for convID, slugEffects := range allOverrides {
		addAgent(convID)
		copyMap := make(map[string]string, len(slugEffects))
		for slug, effect := range slugEffects {
			copyMap[slug] = effect
		}
		out.Permissions.Overrides[convID] = copyMap
	}

	// Register every active actor's current conv on the roster. The set was
	// loaded up front (activeAgents) so its conv-ids could join the batch; here
	// we just fold them into agentRows. addAgent skips a retired/superseded conv
	// (TCL-369) — harmless here, its return value is unused.
	for _, e := range activeAgents {
		addAgent(e.CurrentConvID)
	}

	// Bucket the active sudo grants (loaded up front) per conv-id so the
	// per-agent Active rendering is O(1) inside the output loop. The same rows
	// feed the top-level Sudo[] for the dedicated tab.
	sudoByConv := map[string][]dashboardSudoEntry{}
	out.Sudo = []dashboardSudoEntry{}
	sudoNow := time.Now()
	for _, g := range sudoGrants {
		// Grantee name from the batch-loaded conv_index cache — custom
		// title > pending name > summary > first prompt, no .jsonl
		// rescan. The pending-name tier covers a just-spawned
		// grantee before its first index event; "(unknown)" means
		// nothing resolved, which this surface renders as blank.
		title := rc.titleFor(g.ConvID)
		if title == agent.UnknownTitle {
			title = ""
		}
		remaining := int64(0)
		if rem := g.ExpiresAt.Sub(sudoNow); rem > 0 {
			remaining = int64(rem.Seconds())
		}
		topEntry := dashboardSudoEntry{
			ID:               g.ID,
			AgentID:          g.AgentID,
			ConvID:           g.ConvID,
			ConvTitle:        title,
			Slug:             g.Slug,
			GrantedAt:        g.GrantedAt.Format(time.RFC3339Nano),
			ExpiresAt:        g.ExpiresAt.Format(time.RFC3339Nano),
			GrantedBy:        g.GrantedBy,
			Reason:           g.Reason,
			RemainingSeconds: remaining,
		}
		out.Sudo = append(out.Sudo, topEntry)
		// On the per-agent slice we omit ConvID — the agent row's
		// own ConvID already identifies who holds the grant. Saves
		// bytes on agents with many grants and keeps the JSON
		// readable in browser devtools.
		rowEntry := topEntry
		rowEntry.ConvID = ""
		rowEntry.AgentID = "" // the agent row already identifies the holder
		sudoByConv[g.ConvID] = append(sudoByConv[g.ConvID], rowEntry)
	}
	// All calls to rc.viewFor (group rows plus the grants/active-agent roster)
	// have completed, so this nested metric is the request's total Codex
	// telemetry cost rather than only the grouped subset.
	span.addDuration("codex_telemetry", rc.codexTelemetryDuration)
	span.markExcluding("roster", rc.codexTelemetryDuration-codexTelemetryInGroups)

	out.Ungrouped = []dashboardAgent{}
	for _, a := range agentRows {
		// Defensive: a retired conv must never appear on the roster,
		// even if a partially-applied retire left a stale group/grant
		// row that the passes above picked up.
		if retiredSet[a.ConvID] {
			continue
		}
		// Same for a superseded reincarnation predecessor — it is a
		// ghost of an agent that lives on under the chain head.
		if supersededSet[a.ConvID] {
			continue
		}
		// Effective = (defaults ∪ active-group grants ∪ grant-overrides)
		// − deny-overrides.
		// Defaults come from config; per-conv grant/deny overrides from
		// agent_permissions. A deny override subtracts a slug the
		// defaults would otherwise grant — mirroring resolvePermission.
		denied := map[string]bool{}
		for slug, effect := range out.Permissions.Overrides[a.ConvID] {
			if effect == db.PermEffectDeny {
				denied[slug] = true
			}
		}
		seen := map[string]bool{}
		merged := []string{}
		addEffective := func(s string) {
			if seen[s] || denied[s] {
				return
			}
			seen[s] = true
			merged = append(merged, s)
		}
		for _, s := range defaults {
			addEffective(s)
		}
		for _, s := range groupGrantsByConv[a.ConvID] {
			addEffective(s)
		}
		for _, s := range out.Permissions.Grants[a.ConvID] {
			addEffective(s)
		}
		sort.Strings(merged)
		a.Effective = merged
		sort.Strings(a.Groups)
		sort.Strings(a.OwnedGroups)
		if rows := sudoByConv[a.ConvID]; len(rows) > 0 {
			a.ActiveSudo = rows
		}
		out.Agents = append(out.Agents, *a)
		// An agent with no group memberships is "ungrouped" — surfaces
		// in the dedicated array so the dashboard can render the
		// virtual "Ungrouped" group (and feed the `+ add member`
		// overlay) without re-deriving the membership state. Effective
		// perms still come from the broader Agents row, so the
		// dashboard uses Ungrouped purely as a candidate-set hint.
		//
		// Online and offline alike: the virtual "Ungrouped" group is a
		// membership-management surface, so a freshly-promoted offline
		// conversation must show up there to be dragged into a group.
		// The `+ add member` overlay applies its own online filter, so
		// including offline rows here doesn't leak them into that
		// live-roster picker.
		if len(a.Groups) == 0 {
			out.Ungrouped = append(out.Ungrouped, *a)
		}
	}
	sort.Slice(out.Agents, func(i, j int) bool {
		return out.Agents[i].Title < out.Agents[j].Title
	})
	sort.Slice(out.Ungrouped, func(i, j int) bool {
		return out.Ungrouped[i].Title < out.Ungrouped[j].Title
	})
	span.mark("assemble")

	// The retired / conversations / replaced lists are no longer embedded in
	// the snapshot — the dashboard fetches them from their own paginated
	// endpoints (GET /api/retired, /api/conversations, /api/replaced). The
	// snapshot still loads the cheap retiredSet / supersededSet conv-id sets
	// above to guard the roster.
	out.Pending = collectPendingSnapshot(aliveSessions)
	out.Cron = collectCronSnapshot()
	// Badge-only: the Jobs tab's row window comes from /api/jobs, not the
	// snapshot. A count read error degrades to 0 (no badge), never a failed poll.
	if n, err := db.CountActiveExportJobs(); err == nil {
		out.ExportJobsActive = n
	}
	out.Links = collectLinksSnapshot()
	out.Usage = collectUsageSnapshot(cfg.ResolvedUsageIdleTimeout())
	// Costs-tab visibility: show when there is real pay-per-token spend to
	// display, OR a subscription account has opted into the WHAT-IF view
	// (cost.show_on_subscription). A subscription-only account with the opt-in
	// off hides the tab — it would only show an empty chart. WHAT-IF mode is
	// "visible, but no real spend" → the front-end renders the hypothetical
	// estimate behind a banner and fetches /api/costs?whatif=1. A DB error
	// degrades to "no real cost" (the opt-in still governs), matching how the
	// cost figures themselves degrade to 0.
	hasRealCost, costErr := db.HasAnyRealCost()
	if costErr != nil {
		slog.Debug("snapshot: HasAnyRealCost failed; treating as no real cost", "error", costErr)
	}
	showOnSub := cfg != nil && cfg.Cost != nil && cfg.Cost.ShowOnSubscription
	out.CostTabVisible = hasRealCost || showOnSub
	out.CostTabWhatIf = !hasRealCost && showOnSub
	out.Templates = collectTemplatesSnapshot()
	out.Profiles = collectProfilesSnapshot()
	if profile := globalDefaultProfile(); profile != nil {
		out.SpawnProfileDefault = profile.Name
	}
	out.Roles = collectRolesSnapshot()
	out.Messages, out.MessagesUnread = buildHumanMessagesSnapshot()
	out.AccessRequests = approvals.dashboardSnapshot()
	// Only the actionable (pending) ones drive the blink/banner/badge — the
	// list also carries recently-handled history, which must not count.
	out.AccessRequestsPending = approvals.pendingCount()
	var pluginsErr error
	out.Plugins, out.PluginsWarn, pluginsErr = collectPluginsSnapshot()
	if pluginsErr != nil {
		out.PluginsError = pluginsErr.Error()
	}
	out.PluginsCatalog = pluginCatalog()
	// Plugins-tab visibility mirrors the Costs-tab rule: show it when there's
	// something to manage (≥1 installed plugin), when plugins.json is broken
	// (so the human can see + fix the error), or when they've opted to always
	// keep it (config dashboard.always_show_plugins_tab — the escape hatch to
	// the install-from-catalog UI). Otherwise hide the empty tab.
	out.PluginsTabVisible = len(out.Plugins) > 0 || out.PluginsError != "" || cfg.ShowPluginsTabAlways()
	out.DebugTabVisible = cfg.ShowDebugTab()

	// Display-only cost compensation, applied as the final step over the
	// fully-assembled payload (cfg was loaded once at the top). The DB
	// rows feeding these figures stay raw — see config.CostConfig.
	applyCostDisplayFactor(&out, cfg.ResolvedCostFactor())
	span.mark("collectors")

	// The large registries below change only after an explicit management
	// action (or a binary upgrade for the built-in registries). Hash them into
	// the dynamic snapshot and omit their contents when the browser proves it
	// already has this exact version. We still collect them before hashing so a
	// mutation is visible on the very next 2-second tick.
	out.StaticVersion = snapshotStaticVersion(out)
	if r.URL.Query().Get("static_version") == out.StaticVersion {
		out.StaticUnchanged = true
		out.Slugs = nil
		out.Templates = nil
		out.Profiles = nil
		out.Roles = nil
		out.PluginsCatalog = nil
	}

	writeJSON(w, http.StatusOK, out)
}

// snapshotStaticVersion returns a stable content fingerprint for the registry
// blobs restored client-side on an unchanged poll. Struct field order makes the
// JSON deterministic; each collector already returns a deterministic slice.
func snapshotStaticVersion(out snapshotPayload) string {
	static := struct {
		Slugs          []PermSlug         `json:"slugs"`
		Templates      []templateJSON     `json:"templates"`
		Profiles       []spawnProfileJSON `json:"profiles"`
		Roles          []roleJSON         `json:"roles"`
		PluginsCatalog []Plugin           `json:"plugins_catalog"`
	}{out.Slugs, out.Templates, out.Profiles, out.Roles, out.PluginsCatalog}
	b, _ := json.Marshal(static) // all fields are JSON-native wire structs
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// applyCostDisplayFactor scales every cost figure in the snapshot by the
// configured display multiplier: the per-agent badge (Agents, Ungrouped,
// and each group member's State.CostUSD / State.VirtualCostUSD) plus the
// top-bar month-to-date / today readouts. The WHAT-IF per-agent figure
// (VirtualCostUSD) scales on the same factor as the real one, so the Groups
// tab's hypothetical badge tracks the Costs tab's WHAT-IF total (which
// collectCosts scales by the same factor). It is the snapshot twin of
// collectCosts's scaling, so the per-agent badge, the Costs tab and the
// top-bar headline all move together. A factor of 1 (the default / unset)
// is a no-op, so the common path is untouched. Display-only: the DB keeps
// raw values.
func applyCostDisplayFactor(out *snapshotPayload, factor float64) {
	if factor == 1 {
		return
	}
	out.Usage.TotalCostUSD *= factor
	out.Usage.TodayCostUSD *= factor
	scaleAgents := func(rows []dashboardAgent) {
		for i := range rows {
			rows[i].State.CostUSD *= factor
			rows[i].State.VirtualCostUSD *= factor
		}
	}
	scaleAgents(out.Agents)
	scaleAgents(out.Ungrouped)
	for gi := range out.Groups {
		members := out.Groups[gi].Members
		for mi := range members {
			members[mi].State.CostUSD *= factor
			members[mi].State.VirtualCostUSD *= factor
		}
	}
}

// resolveRetiredByDisplay renders the dashboard's retired "by" column from the
// retire audit fields (JOH-306). retiredBy is the RAW value (a conv-id when an
// agent performed the retire, or a literal like "human" / "system:export-clone");
// retiredByAgent is the stable agent_id companion (db.RetireAgentByID derives it,
// the v78 backfill fills historical rows). agent_id is the source of truth, so:
//
//   - With a companion, name the retirer by its CURRENT title plus stable short
//     agent_id — "name (agt_xxxxxxxx)", the roster style (PR3c) — which is
//     rotation-immune (always the retirer's live name, not a stale generation's).
//     When the name can't be resolved, fall back to the bare short agent_id.
//   - Without a companion (a human/system literal, or a pre-v78 row whose
//     retired_by didn't resolve), show the raw retiredBy value unchanged.
//
// It never returns a bare conv-id for an agent retirer — that was the bug.
func resolveRetiredByDisplay(retiredBy, retiredByAgent string) string {
	retiredBy = strings.TrimSpace(retiredBy)
	retiredByAgent = strings.TrimSpace(retiredByAgent)
	if retiredByAgent == "" {
		return retiredBy // literal, or unresolvable — show the raw audit value
	}
	shortID := agent.ShortAgentID(retiredByAgent, "")
	name := ""
	if a, _ := db.GetAgent(retiredByAgent); a != nil {
		name = agent.CachedTitle(a.CurrentConvID)
	}
	if name == "" || name == agent.UnknownTitle {
		return shortID // name gone — the stable agent_id is the durable fallback
	}
	return name + " (" + shortID + ")"
}

// collectPendingSnapshot turns the pending_spawns rows into the
// dashboard's "Pending" list — dashboard spawns whose conv-id has not
// materialised yet (a live tmux pane stuck behind a startup gate). Each
// carries the reserved stable agent id, the spawn label (the focus key —
// there is no conv-id), the resolved group name, the spawn descriptors, and
// — from the spawn's session row — whether its pane is still alive and where
// it is gated.
// Newest spawn first, so the agent the operator just clicked sits at the
// top. aliveSessions is the snapshot-shaped alive set the caller
// pre-fetched; this function never spawns its own tmux probe. Returns an
// empty slice (not nil) so JS .map() / .length are safe.
func collectPendingSnapshot(aliveSessions map[string]struct{}) []dashboardPending {
	out := []dashboardPending{}
	pendings, err := db.ListPendingSpawns()
	if err != nil {
		return out
	}
	names := loadGroupNames()
	for _, p := range pendings {
		dp := dashboardPending{
			AgentID:   p.AgentID,
			Label:     p.Label,
			Group:     names[p.GroupID],
			Role:      p.Role,
			Name:      p.Name,
			Descr:     p.Descr,
			CreatedAt: p.CreatedAt,
		}
		// The session row (keyed by label, since the conv-id is the very
		// thing that hasn't materialised) tells us whether the pane is
		// still alive and where the agent is gated. A pending row with no
		// session row, or a dead pane, renders as offline — stale, no
		// longer focusable.
		if sess, err := db.LoadSession(p.Label); err == nil && sess != nil {
			if sess.TmuxSession != "" {
				_, dp.Online = aliveSessions[sess.TmuxSession]
			}
			dp.Cwd = sess.Cwd
			dp.Harness = sess.Harness
		}
		out = append(out, dp)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt > out[j].CreatedAt // newest first
	})
	return out
}

// collectLinksSnapshot enumerates every inter-group link, resolved to
// group names. One DB hit for the group list (via loadGroupNames),
// one for the link list; per-row lookups are map indexed. Returns an
// empty slice (not nil) so JS can safely call .map() / .length
// without a guard.
func collectLinksSnapshot() []dashboardLink {
	out := []dashboardLink{}
	rows, err := db.ListAllAgentGroupLinks()
	if err != nil {
		return out
	}
	names := loadGroupNames()
	for _, l := range rows {
		out = append(out, dashboardLink{
			ID:        l.ID,
			From:      names[l.FromGroupID],
			To:        names[l.ToGroupID],
			Mode:      l.Mode,
			CreatedAt: l.CreatedAt.Format(time.RFC3339),
		})
	}
	return out
}

// collectCronSnapshot builds the wire-shape rows for the dashboard's
// Cron tab. Resolves owner/target conv-ids to display titles and
// computes the next-due timestamp so the UI doesn't need a clock-
// arithmetic helper. Returns an empty slice (not nil) so the page's
// .map() doesn't crash on null.
func collectCronSnapshot() []dashboardCronJob {
	out := []dashboardCronJob{}
	jobs, err := db.ListAgentCronJobs()
	if err != nil {
		return out
	}
	// Cache group-id → name across rows in the same snapshot to avoid
	// a per-row lookup.
	groupNames := map[int64]string{}
	for _, j := range jobs {
		out = append(out, cronJobToView(j, groupNames))
	}
	return out
}

// cronJobToView maps one agent_cron_jobs row to its dashboard view. groupNames
// caches group-id → name lookups across rows in the same collection pass.
// Shared by the snapshot's cron list (above) and the Jobs tab's unified
// /api/jobs listing (dashboard_jobs.go).
func cronJobToView(j *db.AgentCronJob, groupNames map[int64]string) dashboardCronJob {
	row := dashboardCronJob{
		ID:               j.ID,
		Name:             j.Name,
		OwnerAgent:       j.OwnerAgent,
		OwnerConv:        j.OwnerConv,
		OwnerLabel:       labelForConv(j.OwnerConv),
		TargetKind:       j.TargetKind,
		TargetAgent:      j.TargetAgent,
		TargetConv:       j.TargetConv,
		GroupID:          j.GroupID,
		TargetRole:       j.TargetRole,
		IntervalSeconds:  j.IntervalSeconds,
		CronExpr:         j.CronExpr,
		CronDesc:         cronexpr.Describe(j.CronExpr),
		Subject:          j.Subject,
		Body:             j.Body,
		Enabled:          j.Enabled,
		RunImmediately:   j.RunImmediately,
		QueueWhenOffline: j.QueueWhenOffline,
		LastRunStatus:    j.LastRunStatus,
	}
	if j.TargetConv != "" {
		row.TargetLabel = labelForConv(j.TargetConv)
	}
	// Resolve group_name ONLY for a group-target job. A conv-target
	// job routed through a shared group also carries a non-zero
	// group_id, but it is not a multicast — leaving group_name empty
	// keeps target_kind the sole, unambiguous discriminator the
	// dashboard renders off.
	if j.IsGroupTarget() && j.GroupID > 0 {
		name, ok := groupNames[j.GroupID]
		if !ok {
			if g, gerr := db.GetAgentGroupByID(j.GroupID); gerr == nil && g != nil {
				name = g.Name
			}
			groupNames[j.GroupID] = name
		}
		row.GroupName = name
	}
	if !j.CreatedAt.IsZero() {
		row.CreatedAt = j.CreatedAt.Format(time.RFC3339)
	}
	if !j.LastRunAt.IsZero() {
		row.LastRunAt = j.LastRunAt.Format(time.RFC3339)
	}
	// Next-due mirrors the scheduler's due check in db.ListDueAgentCronJobs:
	// both modes anchor never-run jobs at created_at, matching the scheduler.
	if j.CronExpr != "" {
		base := j.LastRunAt
		if base.IsZero() {
			base = j.CreatedAt
		}
		if next, err := cronexpr.Next(j.CronExpr, base); err == nil && !next.IsZero() {
			row.NextDueAt = next.Format(time.RFC3339)
		}
	} else {
		base := j.LastRunAt
		if base.IsZero() {
			base = j.CreatedAt
		}
		if !base.IsZero() && j.IntervalSeconds > 0 {
			next := base.Add(time.Duration(j.IntervalSeconds) * time.Second)
			row.NextDueAt = next.Format(time.RFC3339)
		}
	}
	return row
}

// labelForConv returns a short display label for a conv-id. Resolves
// the conv's name from the live conv_index cache (custom title >
// pending name > summary > first prompt — no .jsonl rescan), then
// falls back to the 8-char prefix when nothing resolves. Mirrors the
// rendering used in the Groups/Agents tabs.
func labelForConv(convID string) string {
	if convID == "" {
		return ""
	}
	if t := agent.CachedTitle(convID); t != "" && t != agent.UnknownTitle {
		return t
	}
	if len(convID) >= 8 {
		return convID[:8]
	}
	return convID
}
