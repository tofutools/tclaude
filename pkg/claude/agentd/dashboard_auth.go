package agentd

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// dashboardSessionToken is generated once per agentd process and gates every
// /api/* request. On a clean shutdown its SHA-256 digest (never the replayable
// cookie value) is kept briefly in SQLite. The next daemon accepts that old
// cookie only during dashboardSessionGracePeriod and immediately replaces it
// with this process's fresh token, so a connected browser crosses a restart
// without turning the session cookie into a durable credential.
//
// Empty until initDashboardToken runs in startPopupServer.
var dashboardSessionToken string
var dashboardSessionMintedAt time.Time

const dashboardSessionGracePeriod = 30 * time.Minute

// dashboardGraceSessionHashes is restored once at startup and then read-only.
// Values are absolute expiries so a long-running daemon never accepts a grace
// cookie past its bounded handoff window even if its in-memory map remains.
var dashboardGraceSessionHashes = map[string]time.Time{}

// dashboardSessionNow is indirected for focused expiry tests.
var dashboardSessionNow = time.Now

const (
	legacyDashboardCookieName = "tclaude_dashboard_session"
	dashboardCookieNamePrefix = legacyDashboardCookieName + "_"
)

// dashboardCookieName is stable across restarts of this agentd, but distinct
// for another tclaude installation on the same browser host. HTTP cookies are
// scoped by host + path, never by port: without this namespace, opening a
// second loopback dashboard on another port overwrites the first dashboard's
// credential and its next poll is forced through re-authentication.
//
// runServe replaces the deterministic availability fallback with a persisted
// random installation namespace before the listener starts. The old
// unnamespaced cookie remains accepted below only as a one-way upgrade path.
var dashboardCookieName = dashboardCookieNameForDataDir(config.DataDir())

func dashboardCookieNameForDataDir(dataDir string) string {
	digest := sha256.Sum256([]byte(dataDir))
	return dashboardCookieNameForNamespace(hex.EncodeToString(digest[:16]))
}

func dashboardCookieNameForNamespace(namespace string) string {
	return dashboardCookieNamePrefix + namespace
}

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
	dashboardSessionMintedAt = dashboardSessionNow()
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

// dashboardRequestSessionMatch finds a usable cookie for this agentd. Prefer
// the namespaced cookie, then accept the pre-namespace wire name so a browser
// authenticated by the previous release can cross one more clean restart.
//
// present distinguishes "no dashboard credential" from "one was sent but is
// stale"; both authenticate identically, but the root handler uses it to keep
// its explanatory branches accurate. A valid legacy cookie always requests a
// refresh even when its value happens to equal the current process token:
// migration is complete only once the browser stores the namespaced name.
func dashboardRequestSessionMatch(r *http.Request) (present, valid, refresh bool) {
	var legacy *http.Cookie
	for _, cookie := range r.Cookies() {
		switch cookie.Name {
		case dashboardCookieName:
			present = true
			if ok, rotate := dashboardSessionCookieMatch(cookie.Value); ok {
				return true, true, rotate
			}
		case legacyDashboardCookieName:
			present = true
			legacy = cookie
		}
	}
	if legacy != nil {
		if ok, _ := dashboardSessionCookieMatch(legacy.Value); ok {
			return true, true, true
		}
	}
	return present, false, false
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
	if !isDashboardAppPath(u.Path) && !isDashboardTerminalsPath(u.Path) {
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
	if _, _, refresh := dashboardRequestSessionMatch(r); refresh {
		setDashboardSessionCookie(w)
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
	present, valid, _ := dashboardRequestSessionMatch(r)
	if !present {
		return false, true, http.StatusForbidden, "missing or invalid dashboard cookie; load / first"
	}
	if !valid {
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

func dashboardAuthSessionForRequest(r *http.Request) dashboardAuthSession {
	if session, ok := r.Context().Value(remoteAuthedCtxKey{}).(remoteAuthSession); ok {
		return dashboardAuthSession{
			MintedAt:  session.MintedAt.Format(time.RFC3339),
			ExpiresAt: session.ExpiresAt.Format(time.RFC3339),
		}
	}
	mintedAt := dashboardSessionMintedAt
	if mintedAt.IsZero() {
		// Direct handler tests and embedders may install a session token
		// without running the daemon bootstrap. Keep the wire valid there.
		mintedAt = dashboardSessionNow()
	}
	return dashboardAuthSession{MintedAt: mintedAt.Format(time.RFC3339)}
}
