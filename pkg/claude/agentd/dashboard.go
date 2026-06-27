package agentd

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/convindex"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/remoteaccess"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// The dashboard single-page UI lives under the embedded dashboard/
// directory: dashboard.html, dashboard.css, and the ES-module JS set
// under dashboard/js/. agentd serves dashboard.html at "/" and the CSS
// and JS as static assets under /static/ (see registerDashboardRoutes).
// The browser loads the JS as native ES modules — <script type="module">
// — so there is no bundler and no build step.
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
}

// dashboardSessionToken is generated once per agentd process and gates
// every /api/* request. It's set as an HttpOnly + SameSite=Strict
// cookie on the first GET of /, and checked on subsequent /api/*
// fetches. Same threat model as the popup: defense-in-depth against
// drive-by browser tabs and scraped-URL replay; a same-user process
// with /proc access can still snoop, which we accept as the
// inherent same-user trust boundary on this machine.
//
// Empty until initDashboardToken runs in startPopupServer.
var dashboardSessionToken string

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

// registerDashboardRoutes wires the dashboard onto the popup-server
// mux. We share the listener since both views are loopback-only and
// the human only ever wants one process serving them.
func registerDashboardRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/", handleDashboardRoot)
	mux.HandleFunc("/dashboard/login", handleDashboardLogin)
	mux.HandleFunc("/api/snapshot", handleDashboardSnapshot)
	mux.HandleFunc("/api/costs", handleDashboardCosts)
	mux.HandleFunc("/api/audit", handleDashboardAudit)
	mux.Handle("/static/", handleDashboardStatic())
	registerDashboardEditRoutes(mux)
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
func handleDashboardRoot(w http.ResponseWriter, r *http.Request) {
	// `/` is a catch-all in net/http; reject anything we don't know
	// so /favicon.ico etc. don't silently render the dashboard HTML.
	if r.URL.Path != "/" && r.URL.Path != "/dashboard" {
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
			renderDashboardLoginPage(w, http.StatusForbidden,
				"That dashboard link has expired or was already used.",
				r.URL.Query().Get("slop") == "1")
			return
		}
		setDashboardSessionCookie(w)
		// Preserve the slop param across the redirect — it's purely a
		// client-side theme switch the dashboard JS reads from the URL,
		// so it needs to survive the bare-path bounce. Anything else
		// (init_token included) is intentionally dropped.
		redirectTarget := r.URL.Path
		if r.URL.Query().Get("slop") == "1" {
			redirectTarget += "?slop=1"
		}
		http.Redirect(w, r, redirectTarget, http.StatusSeeOther)
		return
	}

	// Already authenticated: an existing valid cookie (refresh / new
	// tab in the same browser) just gets the page.
	if c, err := r.Cookie(dashboardCookieName); err == nil && c.Value == dashboardSessionToken {
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
	renderDashboardLoginPage(w, http.StatusForbidden, "", r.URL.Query().Get("slop") == "1")
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
	slop := r.URL.Query().Get("slop") == "1"
	if !checkLoginOrigin(r) {
		renderDashboardLoginPage(w, http.StatusForbidden,
			"Request blocked: it didn't come from the dashboard page.", slop)
		return
	}
	if !operatorTokenMatches(r.FormValue("token")) {
		// Uniform message for "blank", "wrong", and "no token was ever
		// minted" — never disclose which, and never echo the input back.
		renderDashboardLoginPage(w, http.StatusForbidden,
			"That operator token wasn't accepted. Copy it from the agentd startup banner (it begins with `tclo_`).", slop)
		return
	}
	setDashboardSessionCookie(w)
	// 303 to the bare path: a refresh now rides the cookie, and the
	// posted token never lingers in history or an access log.
	redirectTarget := "/"
	if slop {
		redirectTarget += "?slop=1"
	}
	http.Redirect(w, r, redirectTarget, http.StatusSeeOther)
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
	if r.URL.Query().Get("slop") == "1" {
		url += "&slop=1"
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
// slop=true tags the URL with ?slop=1 so the dashboard JS swaps in the
// 🎰 slop machine theme. Purely cosmetic — the data and routes are
// identical; the param survives the auth redirect (see handleDashboardRoot)
// so the bare-path URL ends up as /?slop=1 in the address bar.
//
// Best-effort — a missing loopback listener or a failed browser launch
// is logged and otherwise ignored; the daemon keeps running and the
// human can still run `tclaude agent dashboard`.
func autoLaunchDashboard(slop bool) {
	if popupBaseURL == "" {
		slog.Warn("auto-launch-dashboard: no loopback URL bound; dashboard unavailable in this process")
		return
	}
	url := popupBaseURL + "/?init_token=" + mintInitToken(initScopeDashboard)
	if slop {
		url += "&slop=1"
	}
	if err := dashboardBrowserOpener(url); err != nil {
		slog.Warn("auto-launch-dashboard: failed to open browser", "error", err, "url", url)
		return
	}
	fmt.Println("  opening dashboard in your browser…")
}

// checkDashboardAuth mirrors checkPopupAuth: cookie value match +
// Origin/Referer pinned to the popup base URL. Refuses every
// /api/* call when the dashboard token isn't set (cryptographic
// randomness failed at startup).
func checkDashboardAuth(w http.ResponseWriter, r *http.Request) bool {
	// The remote (mTLS + passphrase) listener authenticates at its own
	// boundary (remoteAuthMiddleware) and tags the request; such requests
	// have already cleared a STRONGER bar than the loopback cookie, so accept
	// them without the loopback cookie/Origin checks (which pin to the
	// loopback base URL and would never match a remote origin).
	if dashboardPreAuthed(r) {
		return true
	}
	ok, code, msg := dashboardAuthResult(r)
	if !ok {
		http.Error(w, msg, code)
		return false
	}
	return true
}

// dashboardAuthResult is the pure predicate behind checkDashboardAuth:
// it runs the cookie + Origin/Referer checks and returns whether the
// request is an authenticated dashboard caller, plus the HTTP code +
// message to use on failure. Factored out so the audit middleware can
// ask "is this the authenticated operator?" without writing a response —
// attribution keys on a *valid session*, never on the response status (a
// post-auth policy 403, e.g. a blocklisted sudo grant, is still the
// operator and must not be downgraded to "unauthenticated"). See JOH-268.
func dashboardAuthResult(r *http.Request) (ok bool, code int, msg string) {
	if dashboardSessionToken == "" {
		return false, http.StatusServiceUnavailable, "dashboard not initialised"
	}
	c, err := r.Cookie(dashboardCookieName)
	if err != nil || c.Value != dashboardSessionToken {
		return false, http.StatusForbidden, "missing or invalid dashboard cookie; load / first"
	}
	origin := r.Header.Get("Origin")
	referer := r.Header.Get("Referer")
	if origin == "" && referer == "" {
		return false, http.StatusForbidden, "missing Origin and Referer"
	}
	// popupBaseURL is always bound in production when these routes are
	// registered; it is empty only in tests that register the mux without
	// a loopback listener, where the session cookie is the gate and the
	// origin pin is disabled (mirrors checkLoginOrigin's early return).
	if popupBaseURL != "" {
		if origin != "" && !originMatchesBase(origin, popupBaseURL) {
			return false, http.StatusForbidden, "Origin mismatch"
		}
		if origin == "" && !originMatchesBase(referer, popupBaseURL) {
			return false, http.StatusForbidden, "Referer mismatch"
		}
	}
	return true, http.StatusOK, ""
}

// snapshotPayload is the wire shape for /api/snapshot. One round-trip
// gives the page everything it needs to render every tab; the page
// re-fetches on a 2s timer.
type snapshotPayload struct {
	GeneratedAt string           `json:"generated_at"`
	Groups      []dashboardGroup `json:"groups"`
	Agents      []dashboardAgent `json:"agents"`
	// Ungrouped: every active agent that is NOT a member of any group,
	// online or offline alike. Surfaces fresh-spawned agents, loose
	// convs and freshly-promoted offline conversations so the
	// dashboard's virtual "Ungrouped" group + the `+ add member`
	// overlay can show them as drag/add sources without a second
	// round-trip. (The overlay applies its own online filter on top.)
	// Same wire shape as Agents — empty when no loose convs exist.
	Ungrouped []dashboardAgent `json:"ungrouped"`
	// Conversations: recent non-enrolled conversations — i.e. convs
	// that are NOT agents. The Agents tab renders them in a second list
	// with a "promote" button so a plain conversation can be upgraded
	// into an agent. Recency-capped (newest first) so the snapshot
	// never carries the whole conv history.
	Conversations []dashboardConversation `json:"conversations"`
	// Retired: agents that were explicitly demoted (retire). Their
	// conversation data is intact; the dashboard offers a "reinstate"
	// button. Kept separate from Agents so a retired agent never shows
	// on the live roster.
	Retired []dashboardRetiredAgent `json:"retired"`
	// Replaced: every superseded predecessor conversation generation of a
	// still-existing actor (a reincarnate / Claude Code /clear left it behind
	// when it advanced the actor's live pointer — JOH-26). Powers the Groups
	// tab's default-hidden "Replaced generations" virtual group. Each row is
	// annotated with its owning actor + how/when it was replaced. Empty slice
	// (not nil) so JS .map() / .length are safe.
	Replaced []dashboardReplacedGen `json:"replaced"`
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
	// Messages are the human-facing notifications agents have sent via
	// `tclaude agent notify-human`, newest first — the Messages tab.
	// MessagesUnread is the count of unread ones, driving the tab badge.
	Messages       []dashboardHumanMessage `json:"messages"`
	MessagesUnread int                     `json:"messages_unread"`
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
	// ActivityBots mirrors config dashboard.activity_bots — the per-mode
	// STYLE of the deduped "activity bot" indicator in group headers + the
	// top bar. Each field is "emoji" | "sprites" | "off"; the front-end
	// (render.js) emits both a regular-mode and a slop-mode row and CSS
	// shows the one for the current mode. Defaults: regular emoji, slop
	// sprites. See ActivityBotsRegular / ActivityBotsSlop.
	ActivityBots activityBotsView `json:"activity_bots"`
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
// ActivityBotsSlop). The front-end picks Regular vs Slop off body.slop.
type activityBotsView struct {
	Regular string `json:"regular"`
	Slop    string `json:"slop"`
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
	// Models is the curated model list for the spawn dialog's Model menu.
	// Empty (e.g. Codex, whose model set changes per release and is
	// validated server-side) means "no curated list" — the dialog offers a
	// free-text model entry for that harness instead of a <select>.
	Models []string `json:"models"`
	// EffortLevels is the reasoning-effort scale for the Effort menu, in
	// ascending order. Both harnesses share tclaude's levels today.
	EffortLevels []string `json:"effort_levels"`
	// SandboxModes lists the launch-time OS-sandbox modes this harness
	// accepts (Codex: read-only / workspace-write / danger-full-access).
	// Empty for a harness whose sandbox is configured out of band (Claude
	// Code) — the dialog hides the sandbox selector for it.
	SandboxModes []string `json:"sandbox_modes"`
	// DefaultSandbox is the secure default mode the spawn dialog
	// pre-selects (Codex: the managed tclaude-agent profile). "" when
	// SandboxModes is empty.
	DefaultSandbox string `json:"default_sandbox"`
	// SandboxModeHelp maps each sandbox mode value to a one-line description
	// the dialog shows as a live hint for the selected option (notably its
	// agentd-socket reachability). {} (not null) when the harness has no
	// launch sandbox, so the JS lookup is always safe.
	SandboxModeHelp map[string]string `json:"sandbox_mode_help"`
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
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	OwnerConv       string `json:"owner_conv"`
	OwnerLabel      string `json:"owner_label"`
	TargetKind      string `json:"target_kind"`
	TargetConv      string `json:"target_conv,omitempty"`
	TargetLabel     string `json:"target_label,omitempty"`
	GroupID         int64  `json:"group_id"`
	GroupName       string `json:"group_name,omitempty"`
	IntervalSeconds int64  `json:"interval_seconds"`
	Subject         string `json:"subject,omitempty"`
	Body            string `json:"body"`
	Enabled         bool   `json:"enabled"`
	CreatedAt       string `json:"created_at,omitempty"`
	LastRunAt       string `json:"last_run_at,omitempty"`
	LastRunStatus   string `json:"last_run_status,omitempty"`
	NextDueAt       string `json:"next_due_at,omitempty"`
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
	Name           string `json:"name"`
	Descr          string `json:"descr"`
	DefaultCwd     string `json:"default_cwd"`     // pre-fills the spawn form's cwd; "" = none
	DefaultContext string `json:"default_context"` // shared startup context injected into spawned agents; "" = none
	DefaultProfile string `json:"default_profile"` // spawn profile whose launch fields fill blank spawn fields for this group's agents; "" = none (the spawn default's single source — the vestigial default_model was dropped, JOH-220)
	MaxMembers     int    `json:"max_members"`     // hard member cap; 0 = unlimited. A spawn that would exceed it is refused.
	NotifyEnabled  bool   `json:"notify_enabled"`  // group OS-notification switch; false mutes every member (per-agent 'on' still overrides)
	// RemoteControlPolicy is the group's remote-control policy that overrides a
	// spawn profile's remote-control default (JOH-262): "inherit" (defer to the
	// profile), "optin" (force Remote Access on) or "deny" (force it off).
	RemoteControlPolicy string            `json:"remote_control_policy"`
	Members             []dashboardMember `json:"members"`
	Online              int               `json:"online"`
}

// dashboardMember.Owner mirrors the memberJSON convention from
// /v1/groups/{name}/members:
//   - true on a member row → that member is also a group owner
//     (rendered as a badge alongside the role).
//   - true on a row with Role=="owner" and no descr → a pure owner
//     who isn't a member (so the list stays comprehensive).
type dashboardMember struct {
	ConvID string `json:"conv_id"`
	Title  string `json:"title"`
	// CreatedAt is the conversation's creation timestamp (RFC3339 — the
	// first .jsonl event's time), empty when unknown. Rendered as a
	// relative "Age" column and the default sort key (newest first).
	CreatedAt string `json:"created_at,omitempty"`
	Role      string `json:"role,omitempty"`
	Descr     string `json:"descr,omitempty"`
	// agentLocationView carries `branch` (current branch) plus the
	// startup/current directory split — see agent_location_view.go.
	agentLocationView
	// repoLinksView carries the GitHub web links for the branch cells
	// — dashboard-only enrichment, see branchlinks.go.
	repoLinksView
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
	ConvID string `json:"conv_id"`
	Title  string `json:"title"`
	// agentLocationView carries `branch` (current branch) plus the
	// startup/current directory split — see agent_location_view.go.
	agentLocationView
	// repoLinksView carries the GitHub web links for the branch cells
	// — dashboard-only enrichment, see branchlinks.go.
	repoLinksView
	Online      bool                 `json:"online"`
	State       agentState           `json:"state"`
	Groups      []string             `json:"groups"`
	OwnedGroups []string             `json:"owned_groups"`          // subset of Groups the agent owns; UI tags these distinctly
	Effective   []string             `json:"effective"`             // perms = union(defaults, per-conv grants)
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
	ConvID       string `json:"conv_id"`
	Title        string `json:"title"`
	Online       bool   `json:"online"`
	RetiredAt    string `json:"retired_at,omitempty"`
	RetiredBy    string `json:"retired_by,omitempty"`
	RetireReason string `json:"retire_reason,omitempty"`
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
	ActorConvID  string `json:"actor_conv_id"`
	ActorTitle   string `json:"actor_title"`
	ActorRetired bool   `json:"actor_retired,omitempty"` // the owning actor is itself retired
}

// dashboardPending is the snapshot view of one not-yet-enrolled
// dashboard spawn (a pending_spawns row). It carries what the dashboard
// needs to render the pending agent and drive its focus button: the
// spawn Label (which is the session-row id AND the focus key — a pending
// agent has no conv-id), its intended group/role/name/descr, whether its
// tmux pane is still alive, and where it is gated (Cwd). Leaner than
// dashboardAgent — a pending spawn has no groups/permissions/sudo to show.
type dashboardPending struct {
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
	ConvID           string `json:"conv_id,omitempty"` // omitted on agent[*].active_sudo (caller already knows)
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
	// under (Codex: read-only / workspace-write / danger-full-access), or
	// "" for a harness with no launch sandbox (Claude Code) — the dashboard
	// renders no sandbox badge for "". Surfaced regardless of liveness.
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
	if err != nil || len(rows) == 0 {
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
		Status:        pick.Status,
		StatusDetail:  pick.StatusDetail,
		SubagentCount: pick.SubagentCount,
		Cwd:           pick.Cwd,
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
	if !pick.LastHook.IsZero() {
		out.LastHook = pick.LastHook.Format(time.RFC3339)
	}
	// Context-window usage rides on the same sessions row — the
	// statusline hook (UpdateContextSnapshot) keeps it current. We
	// surface it regardless of liveness: a frozen context_pct for an
	// exited agent is genuinely informative ("it died at 80%"), unlike
	// a frozen "idle" status that would mislabel a dead agent.
	refreshCodexContextSnapshotOnRead(pick, alive)
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
	// One tmux ls for the whole snapshot. Every isConvOnlineIn /
	// stateForConvIn call below tests liveness via map lookup off this
	// set — replacing ~150 per-poll `has-session` subprocess spawns
	// with one. Errors / no-server collapse to an empty map (== "all
	// offline"), matching what per-row probes would have reported when
	// the tmux server is down.
	aliveSessions, _ := session.LiveTmuxSessions()

	groups, _ := db.ListAgentGroups()
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
		if members, err := db.ListAgentGroupMembers(g.ID); err == nil {
			for _, m := range members {
				inMutedGroup[m.ConvID] = true
			}
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

	agentRows := map[string]*dashboardAgent{}
	addAgent := func(convID string) *dashboardAgent {
		if existing, ok := agentRows[convID]; ok {
			return existing
		}
		loc := locationView(convID)
		a := &dashboardAgent{
			ConvID:            convID,
			Title:             agent.CachedTitle(convID),
			agentLocationView: loc,
			repoLinksView:     branchLinksFor(convID, loc),
			Online:            isConvOnlineIn(convID, aliveSessions),
			State:             stateForConvIn(convID, aliveSessions),
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
		PopupBase:            popupBaseURL,
		UserDefaultModel:     readUserDefaultModel(),
		Harnesses:            buildHarnessCatalog(),
		NotificationsEnabled: cfg != nil && cfg.Notifications != nil && cfg.Notifications.Enabled,
		SpawnNameNormalize:   cfg.SpawnNameNormalizeEnabled(),
		VegasInRegularMode:   cfg.ShowVegasInRegularMode(),
		ActivityBots: activityBotsView{
			Regular: cfg.ActivityBotsRegular(),
			Slop:    cfg.ActivityBotsSlop(),
		},
		Permissions: snapshotPermissionsView{
			Defaults:  defaults,
			Grants:    map[string][]string{},
			Overrides: map[string]map[string]string{},
		},
		Slugs: append([]PermSlug{}, permissionRegistry...),
	}
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
	out.Agents = []dashboardAgent{}
	for _, g := range groups {
		dg := dashboardGroup{Name: g.Name, Descr: g.Descr, DefaultCwd: g.DefaultCwd, DefaultContext: g.DefaultContext, DefaultProfile: g.DefaultProfile, MaxMembers: g.MaxMembers, NotifyEnabled: g.NotifyEnabled, RemoteControlPolicy: remoteControlPolicyToWire(g.RemoteControl), Members: []dashboardMember{}}
		members, _ := db.ListAgentGroupMembers(g.ID)
		// Pre-load the owner set so we can tag members who are also
		// owners. Mirrors handleGroupMembersList in handlers.go.
		ownerSet := map[string]bool{}
		if owners, err := db.ListAgentGroupOwners(g.ID); err == nil {
			for _, o := range owners {
				ownerSet[o.ConvID] = true
			}
		}
		memberSet := map[string]bool{}
		for _, m := range members {
			memberSet[m.ConvID] = true
			online := isConvOnlineIn(m.ConvID, aliveSessions)
			loc := locationView(m.ConvID)
			dg.Members = append(dg.Members, dashboardMember{
				ConvID:            m.ConvID,
				Title:             agent.CachedTitle(m.ConvID),
				CreatedAt:         agent.CachedCreated(m.ConvID),
				Role:              m.Role,
				Descr:             m.Descr,
				agentLocationView: loc,
				repoLinksView:     branchLinksFor(m.ConvID, loc),
				Online:            online,
				Owner:             ownerSet[m.ConvID],
				State:             stateForConvIn(m.ConvID, aliveSessions),
				Notify:            notifyPrefs[m.ConvID],
				NotifyEffective:   notifyEffective(m.ConvID),
			})
			if online {
				dg.Online++
			}
			a := addAgent(m.ConvID)
			a.Groups = append(a.Groups, g.Name)
			if ownerSet[m.ConvID] {
				a.OwnedGroups = append(a.OwnedGroups, g.Name)
			}
		}
		// Surface owners who aren't members so the list stays
		// comprehensive. Same shape as the CLI:
		// role="owner", no descr.
		for ownerConv := range ownerSet {
			if memberSet[ownerConv] {
				continue
			}
			online := isConvOnlineIn(ownerConv, aliveSessions)
			ownerLoc := locationView(ownerConv)
			dg.Members = append(dg.Members, dashboardMember{
				ConvID:            ownerConv,
				Title:             agent.CachedTitle(ownerConv),
				CreatedAt:         agent.CachedCreated(ownerConv),
				Role:              "owner",
				agentLocationView: ownerLoc,
				repoLinksView:     branchLinksFor(ownerConv, ownerLoc),
				Online:            online,
				Owner:             true,
				State:             stateForConvIn(ownerConv, aliveSessions),
				Notify:            notifyPrefs[ownerConv],
				NotifyEffective:   notifyEffective(ownerConv),
			})
			// Pure-owners are reachable via this group too — surface
			// the group on the agent's row in the Agents view so
			// "what groups can this conv see?" matches reality.
			a := addAgent(ownerConv)
			a.Groups = append(a.Groups, g.Name)
			a.OwnedGroups = append(a.OwnedGroups, g.Name)
		}
		// Default ordering: newest-first by creation time (the Age
		// column; the JS column sort treats this as the natural order it
		// falls back to). Mirrors handleGroupMembersList.
		sortMembersByAge(dg.Members,
			func(m dashboardMember) string { return m.CreatedAt },
			func(m dashboardMember) string { return m.ConvID })
		out.Groups = append(out.Groups, dg)
	}
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

	// Every active ACTOR — the canonical roster (JOH-26 PR3b reads the
	// agent-level roster, one row per live actor at its current conv). Unlike
	// the old "online ungrouped session" probe, this includes OFFLINE agents: a
	// conv that was an agent yesterday keeps showing after its tmux pane closed,
	// instead of silently vanishing. Plain conversations that were never
	// promoted are not here — they surface in out.Conversations.
	activeAgents, _ := db.ListActiveAgents()
	for _, e := range activeAgents {
		addAgent(e.CurrentConvID)
	}

	// Retired ACTORS are demoted — they must never reach out.Agents. The
	// actor-level roster is keyed on agents.retired_at, which only a human
	// retire sets (a reincarnate / Claude Code /clear predecessor stays a
	// generation of its active actor, so it is NOT here). retiredSet is the
	// belt-and-braces guard for a partially-applied retire, and feeds the
	// dedicated out.Retired list below.
	retiredAgents, _ := db.ListRetiredAgents()
	retiredSet := make(map[string]bool, len(retiredAgents))
	for _, e := range retiredAgents {
		retiredSet[e.CurrentConvID] = true
	}

	// Superseded conversations — the predecessors of a reincarnation chain —
	// are NOT agents: their identity moved to the chain head. The actor-level
	// roster already excludes them (a predecessor is never an active actor's
	// current conv), so supersededSet is now a redundant read-time belt-and-
	// braces guard — symmetric with retiredSet — kept so a ghost predecessor
	// never reaches out.Agents / out.Ungrouped even under a partially-applied
	// rotation.
	supersededSet := map[string]bool{}
	if successions, err := db.ListAgentConvSuccessions(); err == nil {
		for _, s := range successions {
			supersededSet[s.OldConvID] = true
		}
	}

	// Active sudo grants across every agent. One DB scan, then we
	// bucket per conv-id so the per-agent Active rendering is O(1)
	// inside the agent loop. The same rows feed the top-level Sudo[]
	// for the dedicated tab.
	sudoByConv := map[string][]dashboardSudoEntry{}
	out.Sudo = []dashboardSudoEntry{}
	if grants, err := db.ListAllActiveSudoGrants(); err == nil {
		now := time.Now()
		for _, g := range grants {
			// Grantee name from the live conv_index cache — custom
			// title > pending name > summary > first prompt, no .jsonl
			// rescan. The pending-name tier covers a just-spawned
			// grantee before its first index event; "(unknown)" means
			// nothing resolved, which this surface renders as blank.
			title := agent.CachedTitle(g.ConvID)
			if title == agent.UnknownTitle {
				title = ""
			}
			remaining := int64(0)
			if rem := g.ExpiresAt.Sub(now); rem > 0 {
				remaining = int64(rem.Seconds())
			}
			topEntry := dashboardSudoEntry{
				ID:               g.ID,
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
			sudoByConv[g.ConvID] = append(sudoByConv[g.ConvID], rowEntry)
		}
	}

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
		// Effective = (defaults ∪ grant-overrides) − deny-overrides.
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

	out.Conversations = collectConversationsSnapshot(aliveSessions)
	out.Retired = collectRetiredSnapshot(retiredAgents, aliveSessions)
	out.Replaced = collectReplacedGenerationsSnapshot(activeAgents, retiredAgents, aliveSessions)
	out.Pending = collectPendingSnapshot(aliveSessions)
	out.Cron = collectCronSnapshot()
	out.Links = collectLinksSnapshot()
	out.Usage = collectUsageSnapshot()
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
	out.Messages, out.MessagesUnread = buildHumanMessagesSnapshot()
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

	// Display-only cost compensation, applied as the final step over the
	// fully-assembled payload (cfg was loaded once at the top). The DB
	// rows feeding these figures stay raw — see config.CostConfig.
	applyCostDisplayFactor(&out, cfg.ResolvedCostFactor())

	writeJSON(w, http.StatusOK, out)
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

// Conversation-list sizing. conversationsScanLimit caps how many
// recent conv_index rows the snapshot scans; conversationsListMax caps
// how many non-agent conversations it actually emits. The gap absorbs
// the agents mixed into the recent set — without it, a burst of agent
// activity could starve the promotion list. The dashboard's filter box
// searches within the emitted set.
const (
	conversationsScanLimit = 200
	conversationsListMax   = 75
)

// collectConversationsSnapshot builds the Agents tab's second list:
// recent conversations that are NOT agents, offered with a promote
// button. enrolled (active ∪ retired) conv-ids are excluded — they are
// already agents (or deliberately demoted ones) and have their own
// lists. aliveSessions is the snapshot-shaped alive set the caller
// pre-fetched; this function never spawns its own tmux probe.
// Returns an empty slice (not nil) so the JS .map() is safe.
func collectConversationsSnapshot(aliveSessions map[string]struct{}) []dashboardConversation {
	out := []dashboardConversation{}
	// Exclude EVERY agent generation — active, retired, and superseded
	// predecessors alike — from the plain-conversations list. The actor rosters
	// expose only each actor's CURRENT conv, so without the full
	// agent_conversations set a predecessor generation would leak in as a bogus
	// promotion candidate (JOH-26 PR3b). On a read error, return empty rather
	// than risk surfacing agents as plain conversations.
	agentConvs, err := db.ListAgentConvIDs()
	if err != nil {
		return out
	}
	rows, err := db.ListRecentConvIndex(conversationsScanLimit)
	if err != nil {
		return out
	}
	for _, row := range rows {
		if len(out) >= conversationsListMax {
			break
		}
		if row.ConvID == "" {
			continue
		}
		if _, isAgent := agentConvs[row.ConvID]; isAgent {
			continue
		}
		// Plain conversations are non-agents — never /rename'd — so
		// their title is a summary or a raw first prompt. Render it
		// straight from the cached row via convindex.FormatConvTitle —
		// the same formatter the CLI's `conv ls` uses, so the dashboard
		// stops leaking uncleaned first-prompt text (system tags,
		// newlines) — WITHOUT the per-row os.Stat + reparse that
		// agent.FreshConvTitle would do. The conv_index monitor
		// (fsnotify.go) keeps these rows fresh, so the cached row is
		// trustworthy; this poll no longer has to refresh it.
		out = append(out, dashboardConversation{
			ConvID:   row.ConvID,
			Title:    convindex.FormatConvTitle(row.CustomTitle, row.Summary, row.FirstPrompt),
			Online:   isConvOnlineIn(row.ConvID, aliveSessions),
			State:    stateForConvIn(row.ConvID, aliveSessions),
			Modified: row.Modified,
		})
	}
	return out
}

// collectRetiredSnapshot turns the retired-enrollment rows into the
// Agents tab's "Retired" section — agents demoted to plain
// conversations, each reinstatable. Newest retirement first. Returns
// an empty slice (not nil) so the JS .map() is safe. aliveSessions is
// the snapshot-shaped alive set the caller pre-fetched; this function
// never spawns its own tmux probe.
func collectRetiredSnapshot(retired []*db.Agent, aliveSessions map[string]struct{}) []dashboardRetiredAgent {
	out := make([]dashboardRetiredAgent, 0, len(retired))
	for _, e := range retired {
		retiredAt := ""
		if !e.RetiredAt.IsZero() {
			retiredAt = e.RetiredAt.Format(time.RFC3339)
		}
		out = append(out, dashboardRetiredAgent{
			ConvID:       e.CurrentConvID,
			Title:        agent.CachedTitle(e.CurrentConvID),
			Online:       isConvOnlineIn(e.CurrentConvID, aliveSessions),
			RetiredAt:    retiredAt,
			RetiredBy:    e.RetiredBy,
			RetireReason: e.RetireReason,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].RetiredAt > out[j].RetiredAt // newest first
	})
	return out
}

// collectReplacedGenerationsSnapshot builds the "Replaced generations" list:
// every superseded predecessor conversation generation of a still-existing
// actor (active OR retired). For each actor it walks db.GenerationsForAgent
// (the rotation chain, oldest link first), skips the live head
// (agents.current_conv_id — shown on the roster, not here), and emits one row
// per predecessor. A predecessor's "replaced via / at" is taken from the NEXT
// link in the chain — the generation that superseded it — since within one
// actor the generations form a linear reincarnate/clear chain ordered by
// linked_at. aliveSessions is the pre-fetched alive set (no per-row tmux
// probe). Returns an empty slice (not nil) so JS .map() / .length are safe.
//
// No overlap with the Retired tray: that lists a retired actor's CURRENT conv;
// this lists its (and every active actor's) PAST generations.
func collectReplacedGenerationsSnapshot(active, retired []*db.Agent, aliveSessions map[string]struct{}) []dashboardReplacedGen {
	out := []dashboardReplacedGen{}
	emit := func(actors []*db.Agent, actorRetired bool) {
		for _, a := range actors {
			gens, err := db.GenerationsForAgent(a.AgentID)
			if err != nil || len(gens) < 2 {
				continue // only the head (or none) — no predecessors to show
			}
			actorTitle := agent.CachedTitle(a.CurrentConvID)
			for i, g := range gens {
				if g.ConvID == a.CurrentConvID {
					continue // the live head — shown on the roster, not here
				}
				// The generation that REPLACED this one is the next link in the
				// chain (ordered by linked_at); its reason + linked_at are how
				// and when this generation was superseded. Fall back to the
				// generation's own birth metadata if it is somehow the last
				// link yet not the head (defensive — shouldn't happen).
				reason, replacedAt := g.Reason, g.LinkedAt
				if i+1 < len(gens) {
					reason = gens[i+1].Reason
					replacedAt = gens[i+1].LinkedAt
				}
				title := agent.CachedTitle(g.ConvID)
				if title == "" {
					title = actorTitle
				}
				ra := ""
				if !replacedAt.IsZero() {
					ra = replacedAt.Format(time.RFC3339)
				}
				out = append(out, dashboardReplacedGen{
					ConvID:       g.ConvID,
					Title:        title,
					Reason:       reason,
					ReplacedAt:   ra,
					Online:       isConvOnlineIn(g.ConvID, aliveSessions),
					ActorConvID:  a.CurrentConvID,
					ActorTitle:   actorTitle,
					ActorRetired: actorRetired,
				})
			}
		}
	}
	emit(active, false)
	emit(retired, true)
	// Group a single actor's generations together (by the live actor's title),
	// newest replacement first within each actor.
	sort.Slice(out, func(i, j int) bool {
		if out[i].ActorTitle != out[j].ActorTitle {
			return out[i].ActorTitle < out[j].ActorTitle
		}
		return out[i].ReplacedAt > out[j].ReplacedAt
	})
	return out
}

// collectPendingSnapshot turns the pending_spawns rows into the
// dashboard's "Pending" list — dashboard spawns whose conv-id has not
// materialised yet (a live tmux pane stuck behind a startup gate). Each
// carries the spawn label (the focus key — there is no conv-id), the
// resolved group name, the spawn descriptors, and — from the spawn's
// session row — whether its pane is still alive and where it is gated.
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
		row := dashboardCronJob{
			ID:              j.ID,
			Name:            j.Name,
			OwnerConv:       j.OwnerConv,
			OwnerLabel:      labelForConv(j.OwnerConv),
			TargetKind:      j.TargetKind,
			TargetConv:      j.TargetConv,
			GroupID:         j.GroupID,
			IntervalSeconds: j.IntervalSeconds,
			Subject:         j.Subject,
			Body:            j.Body,
			Enabled:         j.Enabled,
			LastRunStatus:   j.LastRunStatus,
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
			next := j.LastRunAt.Add(time.Duration(j.IntervalSeconds) * time.Second)
			row.NextDueAt = next.Format(time.RFC3339)
		}
		out = append(out, row)
	}
	return out
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
