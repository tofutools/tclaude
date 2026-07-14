package agentd

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/remoteaccess"
)

// The optional network-exposed dashboard listener (tclaude-remote-control
// phase 3 / JOH-227). It is a SEPARATE HTTPS server from the loopback popup
// server — the loopback dashboard and its init-token → cookie flow are
// untouched. This listener enforces TWO factors before serving anything:
//
//  1. mTLS — at the TLS layer (RequireAndVerifyClientCert against the tclaude
//     remote-access CA, see remoteaccess.Material.TLSConfig). A connection
//     without a valid client cert never reaches an HTTP handler.
//  2. A passphrase login (/login) that mints a signed, restart-surviving
//     session cookie; remoteAuthMiddleware gates every other route on it.
//
// Once both pass, requests are tagged authed (remoteAuthedCtxKey) and
// delegated to the SAME dashboard handlers the loopback server uses — so the
// fleet view + every control action work remotely with no per-handler change.
// checkDashboardAuth / handleDashboardRoot honour that tag and skip the
// loopback cookie/Origin check for these pre-authed requests.

// remoteSessionTTL is how long a passphrase login stays valid. The cookie is
// HMAC-signed with the persisted key, so it survives agentd restarts — a phone
// logs in once and stays logged in for the window.
const remoteSessionTTL = 30 * 24 * time.Hour

// remoteCookieName is the remote listener's session cookie. Deliberately
// DISTINCT from the loopback dashboardCookieName: although the two can't be
// confused (loopback does an exact-token compare a signed token never matches;
// the remote path runs VerifyCookie which a raw loopback token fails), a
// separate name removes any "could these collide?" question and keeps the two
// auth schemes visibly independent.
const remoteCookieName = "tclaude_remote_session"

// maxLoginBodyBytes caps the /login POST body — defense-in-depth behind mTLS
// against a client cert holder posting a giant form.
const maxLoginBodyBytes = 64 << 10

// remoteAuthedCtxKey marks a request that remoteAuthMiddleware has fully
// authenticated (valid client cert + valid session cookie + same-origin).
type remoteAuthedCtxKey struct{}

// dashboardPreAuthed reports whether the remote listener already authenticated
// this request. checkDashboardAuth and handleDashboardRoot consult it so the
// shared handlers serve remote requests without the loopback cookie/Origin
// checks (which would never match a remote origin).
func dashboardPreAuthed(r *http.Request) bool {
	v, _ := r.Context().Value(remoteAuthedCtxKey{}).(bool)
	return v
}

// startRemoteServer loads the remote-access material and starts the mTLS
// dashboard listener on bind. Returns the server (for graceful shutdown) or an
// error if material is missing / the bind fails — the caller logs and keeps the
// daemon running without remote access.
func startRemoteServer(bind string) (*http.Server, error) {
	m, err := remoteaccess.Load()
	if err != nil {
		return nil, err
	}
	tlsCfg := m.TLSConfig()
	ln, err := tls.Listen("tcp", bind, tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("bind %s: %w", bind, err)
	}

	// The listener (tls.Listen) already terminates TLS, so srv.Serve over it
	// is correct and srv.TLSConfig is intentionally NOT set (it would be
	// unused — ServeTLS is the path that reads it).
	srv := &http.Server{
		Handler:           buildRemoteDashboardHandler(m),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Warn("remote-access: server exited", "err", err)
		}
	}()
	setRemoteListenerRunning(bind)
	return srv, nil
}

// buildRemoteDashboardHandler mounts the shared dashboard routes behind the
// remote authentication boundary, then audits authenticated command attempts.
// Keeping auditRequests inside remoteAuthMiddleware gives audit attribution the
// unforgeable dashboardPreAuthed marker and avoids recording rejected login or
// session probes as operator actions. The remote mux itself is otherwise raw,
// so each accepted command produces exactly one audit row.
func buildRemoteDashboardHandler(m *remoteaccess.Material) http.Handler {
	dashMux := http.NewServeMux()
	registerDashboardRoutes(dashMux)
	return remoteAuthMiddleware(m, auditRequests(dashMux))
}

// Running-listener state — recorded when startRemoteServer succeeds so the
// dashboard snapshot can tell the Config tab whether THIS agentd process
// actually has the remote listener up (and on which bind). config.json reflects
// the saved intent; this reflects the live reality, so the UI can distinguish
// "enabled & live on X" from "enabled but a restart is still pending". It is
// never cleared at runtime: the listener is started once at daemon startup and
// only stops when the whole process exits, after which no snapshot is served.
var (
	remoteListenerMu      sync.Mutex
	remoteListenerRunning bool
	remoteListenerBind    string
)

func setRemoteListenerRunning(bind string) {
	remoteListenerMu.Lock()
	defer remoteListenerMu.Unlock()
	remoteListenerRunning = true
	remoteListenerBind = bind
}

// remoteListenerStatus reports whether the remote listener is live in this
// process and the bind it is serving on. Used by the dashboard snapshot.
func remoteListenerStatus() (running bool, bind string) {
	remoteListenerMu.Lock()
	defer remoteListenerMu.Unlock()
	return remoteListenerRunning, remoteListenerBind
}

// remoteAuthMiddleware enforces the second factor (the passphrase session
// cookie) and same-origin, then delegates to the dashboard mux with the
// request tagged authed. /login is the only route reachable without the cookie
// (it's still behind mTLS).
func remoteAuthMiddleware(m *remoteaccess.Material, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			handleRemoteLogin(w, r, m)
			return
		}
		if !remoteSessionValid(r, m) {
			// An unauthenticated page navigation goes to the login form;
			// anything else (an /api fetch) gets a clean 401.
			if r.Method == http.MethodGet && (r.URL.Path == "/" || r.URL.Path == "/dashboard") {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			http.Error(w, "remote session required; log in at /login", http.StatusUnauthorized)
			return
		}
		if !remoteSameOrigin(r) {
			http.Error(w, "Origin mismatch", http.StatusForbidden)
			return
		}
		ctx := context.WithValue(r.Context(), remoteAuthedCtxKey{}, true)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// remoteSessionValid reports whether the request carries a valid (signed,
// unexpired) remote session cookie.
func remoteSessionValid(r *http.Request, m *remoteaccess.Material) bool {
	c, err := r.Cookie(remoteCookieName)
	if err != nil {
		return false
	}
	_, ok := remoteaccess.VerifyCookie(m.CookieKey(), c.Value)
	return ok
}

// remoteSameOrigin is the CSRF defense-in-depth behind the SameSite=Strict
// cookie: when an Origin header is present it must target the same host the
// request hit (r.Host). A request with no Origin (a top-level navigation)
// relies on the SameSite=Strict cookie, which a cross-site context never sends.
// Host-relative rather than pinned to a fixed base URL, because the phone may
// dial any of the server cert's SANs (LAN IP, tailnet name, …).
func remoteSameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == r.Host
}

// handleRemoteLogin renders the passphrase form (GET) and verifies it (POST),
// minting the signed session cookie on success. mTLS already gated this route,
// so only a holder of a valid client cert can even reach it.
func handleRemoteLogin(w http.ResponseWriter, r *http.Request, m *remoteaccess.Material) {
	switch r.Method {
	case http.MethodGet:
		if remoteSessionValid(r, m) {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		writeLoginPage(w, "", http.StatusOK)
	case http.MethodPost:
		if !remoteLoginAllowed() {
			writeLoginPage(w, "Too many attempts — wait a moment and try again.", http.StatusTooManyRequests)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxLoginBodyBytes)
		_ = r.ParseForm()
		if !m.VerifyPassphrase(r.PostFormValue("passphrase")) {
			remoteLoginFailed()
			writeLoginPage(w, "Incorrect passphrase.", http.StatusUnauthorized)
			return
		}
		remoteLoginSucceeded()
		http.SetCookie(w, &http.Cookie{
			Name:     remoteCookieName,
			Value:    remoteaccess.SignCookie(m.CookieKey(), "human", remoteSessionTTL),
			Path:     "/",
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteStrictMode,
			MaxAge:   int(remoteSessionTTL / time.Second),
		})
		http.Redirect(w, r, "/", http.StatusSeeOther)
	default:
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
	}
}

// Login rate limiter — a coarse global lockout that slows passphrase guessing.
// mTLS already requires a valid client cert to reach /login at all, so this is
// a secondary control; a global (not per-IP) counter is sufficient for the
// single-operator model and can't be evaded by source-address spoofing.
var (
	loginLimiterMu  sync.Mutex
	loginFailCount  int
	loginBlockUntil time.Time
)

const (
	loginMaxFails    = 5
	loginLockoutTime = 30 * time.Second
)

func remoteLoginAllowed() bool {
	loginLimiterMu.Lock()
	defer loginLimiterMu.Unlock()
	return time.Now().After(loginBlockUntil)
}

func remoteLoginFailed() {
	loginLimiterMu.Lock()
	defer loginLimiterMu.Unlock()
	loginFailCount++
	if loginFailCount >= loginMaxFails {
		loginBlockUntil = time.Now().Add(loginLockoutTime)
		loginFailCount = 0
	}
}

func remoteLoginSucceeded() {
	loginLimiterMu.Lock()
	defer loginLimiterMu.Unlock()
	loginFailCount = 0
	loginBlockUntil = time.Time{}
}

// writeLoginPage renders the minimal mobile-friendly passphrase form. errMsg is
// shown (already plain text, no untrusted interpolation) when non-empty.
func writeLoginPage(w http.ResponseWriter, errMsg string, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	errBlock := ""
	if errMsg != "" {
		errBlock = `<p class="err">` + errMsg + `</p>`
	}
	// strings.Replace, not fmt.Fprintf: the CSS contains literal `%` (width:
	// 100%) which a format string would misread as a verb.
	_, _ = w.Write([]byte(strings.Replace(loginPageTemplate, "{{ERR}}", errBlock, 1)))
}

// loginPageTemplate is a self-contained (no external assets) mobile-first login
// page. {{ERR}} is replaced with the optional error block. The only dynamic
// value is that server-controlled error string, so there is no untrusted
// interpolation.
const loginPageTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<meta name="color-scheme" content="dark light">
<title>tclaude — remote access</title>
<style>
  :root { color-scheme: dark light; }
  * { box-sizing: border-box; }
  body { margin: 0; min-height: 100vh; display: grid; place-items: center;
         font: 16px/1.4 system-ui, sans-serif; background: #0f1115; color: #e6e6e6; }
  form { width: min(92vw, 360px); padding: 28px; background: #181b22;
         border: 1px solid #2a2f3a; border-radius: 14px; }
  h1 { font-size: 18px; margin: 0 0 4px; }
  p.sub { margin: 0 0 20px; color: #9aa3b2; font-size: 13px; }
  label { display: block; font-size: 13px; color: #9aa3b2; margin-bottom: 6px; }
  input { width: 100%; padding: 14px; font-size: 16px; border-radius: 10px;
          border: 1px solid #2a2f3a; background: #0f1115; color: #e6e6e6; }
  button { width: 100%; margin-top: 16px; padding: 14px; font-size: 16px; font-weight: 600;
           border: 0; border-radius: 10px; background: #4b6bfb; color: #fff; }
  button:active { background: #3a55d8; }
  p.err { color: #ff6b6b; font-size: 13px; margin: 14px 0 0; }
</style>
</head>
<body>
<form method="POST" action="/login">
  <h1>📱 tclaude remote access</h1>
  <p class="sub">Enter the remote-access passphrase to continue.</p>
  <label for="passphrase">Passphrase</label>
  <input id="passphrase" name="passphrase" type="password" autocomplete="current-password"
         autofocus inputmode="text" autocapitalize="off" autocorrect="off">
  <button type="submit">Unlock</button>
  {{ERR}}
</form>
</body>
</html>
`
