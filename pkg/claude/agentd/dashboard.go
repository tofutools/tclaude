package agentd

import (
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/tofutools/tclaude/pkg/claude/agentd/dashboard"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// The dashboard single-page UI lives under the embedded dashboard/
// directory: dashboard.html, dashboard.css, and the ES-module JS set
// under dashboard/js/. agentd serves dashboard.html at "/" and the CSS
// and JS as static assets under /static/ (see registerDashboardRoutes).
// The dashboard shell, feature islands, and retained imperative integrations
// load source JS as native ES modules.
// Preact, HTM, and Signals are vendored under dashboard/third_party/preact/, and
// markdown-it (the Markdown viewer's parser) under dashboard/third_party/markdown-it/;
// an import map gives application modules normal package specifiers. Islands are
// dynamically loaded so a missing optional feature module cannot prevent the
// static entry graph from linking.
//
// The tree is embedded by the dashboard package (see its doc for why);
// dashboardAssetsFS addresses its files as "dashboard.html", "js/...".
var dashboardAssetsFS fs.FS = dashboard.FS

// dashboardIndexHTML is dashboard.html, read once at init — the page
// handleDashboardRoot serves at "/". injectBootPreload splices in the
// modulepreload block + module-total for the boot progress bar, so the entry
// import graph is fetched in parallel rather than as a discover-one-at-a-time
// waterfall.
var dashboardIndexHTML = injectAssetsVersion(injectBootPreload(mustReadFS(dashboardAssetsFS, "dashboard.html"), dashboardAssetsFS))

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
	// Vendored bundles keep their upstream filenames, and markdown-it's browser
	// ES module ships as .mjs — a type the host table often does not carry.
	_ = mime.AddExtensionType(".mjs", "text/javascript")
	_ = mime.AddExtensionType(".css", "text/css")
	_ = mime.AddExtensionType(".svg", "image/svg+xml")
	_ = mime.AddExtensionType(".map", "application/json")
}

// daemonInstanceID identifies THIS agentd process. It is generated once at
// startup, so it necessarily differs in the next process — including a restart
// of the same build, which Version cannot distinguish.
//
// It exists for clients holding something that dies with the daemon: a browser
// terminal's PTY WebSocket. Such a client polls /api/instance while it is down
// and reattaches when the id it sees is no longer the one it connected under.
// It is a restart marker and nothing more: not a credential, carrying no state,
// and useless to anyone who cannot already authenticate to the dashboard.
var daemonInstanceID = uuid.NewString()

// handleDashboardInstance answers the restart probe above. It is deliberately
// the cheapest endpoint in the daemon — a disconnected terminal polls it once a
// second — so it touches no database, builds no view, and returns one field.
func handleDashboardInstance(w http.ResponseWriter, r *http.Request) {
	// Auth first: an unauthenticated caller learns nothing about this route,
	// not even that it exists.
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"instance_id": daemonInstanceID})
}

// registerDashboardRoutes wires the dashboard onto the popup-server
// mux. We share the listener since both views are loopback-only and
// the human only ever wants one process serving them.
func registerDashboardRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/", handleDashboardRoot)
	mux.HandleFunc("/terminals", handleDashboardTerminals)
	// The Terminals tab addresses a single terminal by agent id
	// (/terminals/<agent-id>), so the route is a subtree, not one exact path.
	// Without this the deep link would fall through to the "/" catch-all and
	// 404, because `terminals` is deliberately not in dashboardAppTabs.
	mux.HandleFunc("/terminals/", handleDashboardTerminals)
	mux.HandleFunc("/dashboard/login", handleDashboardLogin)
	mux.HandleFunc("/api/tui/attach-ws/", handleTUIAttachWS)
	mux.Handle("/api/tui/", http.StripPrefix(tuiHTTPPrefix, buildTUIHTTPHandler()))
	mux.HandleFunc("/api/auth/session", handleDashboardAuthSession)
	mux.HandleFunc("/api/instance", handleDashboardInstance)
	mux.HandleFunc("GET /api/node-summary", handleDashboardNodeSummary)
	mux.HandleFunc("/api/snapshot", withGzip(withPerfTiming("/api/snapshot", handleDashboardSnapshot)))
	mux.HandleFunc("/api/perf", withGzip(handleDashboardPerf))
	mux.HandleFunc("/api/perf/reset", handleDashboardPerfReset)
	registerProfileRoutes(mux)
	mux.HandleFunc("/api/costs", withGzip(handleDashboardCosts))
	mux.HandleFunc("/api/costs/ignore", handleDashboardCostsIgnore)
	mux.HandleFunc("/api/usage-history", withGzip(handleDashboardUsageHistory))
	mux.HandleFunc("/api/usage-history/point", handleDashboardUsageHistoryPoint)
	mux.HandleFunc("/api/audit", handleDashboardAudit)
	mux.HandleFunc("/api/logs", handleDashboardLogs)
	mux.HandleFunc("/api/logs/export", handleDashboardLogsExport)
	mux.HandleFunc("/api/spawn-timing", handleDashboardSpawnTiming)
	// The Processes tab consumes the same versioned REST surface as other
	// clients. Dashboard auth wraps it before the dynamic feature gate.
	mux.HandleFunc("GET /v1/process/templates", dashboardProcessRoute(handleProcessTemplates))
	mux.HandleFunc("POST /v1/process/templates", dashboardProcessRoute(handleProcessTemplateCreate))
	mux.HandleFunc("GET /v1/process/template-heads", dashboardProcessRoute(handleProcessTemplateHeads))
	mux.HandleFunc("GET /v1/process/templates/{id}", dashboardProcessRoute(handleProcessTemplate))
	mux.HandleFunc("POST /v1/process/templates/{id}", dashboardProcessRoute(handleProcessTemplate))
	mux.HandleFunc("DELETE /v1/process/templates/{id}", dashboardProcessRoute(handleProcessTemplate))
	mux.HandleFunc("POST /v1/process/templates/{id}/listing", dashboardProcessRoute(handleProcessTemplateListing))
	mux.HandleFunc("POST /v1/process/groups/{group}/processes", dashboardProcessRoute(handleProcessGroupPin))
	mux.HandleFunc("DELETE /v1/process/groups/{group}/processes/{templateId}", dashboardProcessRoute(handleProcessGroupUnpin))
	mux.HandleFunc("POST /v1/process/validate", dashboardProcessRoute(handleProcessValidate))
	mux.HandleFunc("POST /v1/process/resolve-preview", dashboardProcessRoute(handleProcessResolvePreview))
	// Share a process as a .pwf file, and review then import one (docs/pwf.md).
	mux.HandleFunc("GET /v1/process/templates/{id}/export", dashboardProcessRoute(handleProcessTemplateExport))
	mux.HandleFunc("POST /v1/process/import", dashboardProcessRoute(handleProcessImport))
	mux.HandleFunc("GET /v1/process/context", dashboardProcessRoute(handleProcessContext))
	// The Groups tab's Task board sections (taskboard_context.go).
	mux.HandleFunc("GET /v1/taskboard/context", asDashboardHuman(handleTaskboardContext))
	// Groups › Processes "+ start process" (G3) starts a group-scoped run.
	mux.HandleFunc("POST /v1/process/runs", dashboardProcessRoute(handleProcessRuns))
	// The single-run focus (G5) reads one run's durable view and evidence and
	// records a human gate's verdict.
	mux.HandleFunc("GET /v1/process/runs", dashboardProcessRoute(handleProcessRuns))
	mux.HandleFunc("GET /v1/process/runs/{id}", dashboardProcessRoute(handleProcessRun))
	mux.HandleFunc("GET /v1/process/runs/{id}/events", dashboardProcessRoute(handleProcessRunEvents))
	mux.HandleFunc("POST /v1/process/runs/{id}/decide", dashboardProcessRoute(handleProcessRunDecide))
	// The Terminals overlay promotes an agent-scoped run to a group (T3) and
	// hands a node's attempt to another performer (T2/G5 reassign).
	mux.HandleFunc("POST /v1/process/runs/{id}/rescope", dashboardProcessRoute(handleProcessRunRescope))
	mux.HandleFunc("POST /v1/process/runs/{id}/nodes/{node}/assign", dashboardProcessRoute(handleProcessRunAssign))
	// The Pause / Resume control (T2/T3/G5) holds and releases a run.
	mux.HandleFunc("POST /v1/process/runs/{id}/pause", dashboardProcessRoute(handleProcessRunPause))
	mux.HandleFunc("POST /v1/process/runs/{id}/resume", dashboardProcessRoute(handleProcessRunResume))
	// The Blocked chip's retry / skip / cancel resolves a parked branch.
	mux.HandleFunc("POST /v1/process/runs/{id}/resolve-blocked", dashboardProcessRoute(handleProcessRunResolveBlocked))
	mux.Handle("/static/", handleDashboardStatic())
	registerDashboardFederationRoutes(mux)
	registerDashboardPAFRoutes(mux)
	registerNodeUpdateRoutes(mux)
	registerNodeUpdateScriptRoutes(mux)
	registerNodeRunRoutes(mux)
	registerHarnessAvailabilityRoutes(mux)
	registerHarnessOperationsRoutes(mux)
	registerDashboardPeerRoutes(mux)
	registerDashboardPeerAttachmentRoutes(mux)
	registerDashboardPeerDownloadRoutes(mux)
	registerDashboardPeerUploadRoutes(mux)
	registerDashboardEditRoutes(mux)
	registerDashboardHostRoutes(mux)
}

func dashboardProcessRoute(next http.HandlerFunc) http.HandlerFunc {
	// The socket listener applies idempotency outside its process routes. The
	// dashboard has a separate mux, so apply the same durable boundary here,
	// after dashboard auth has stamped the request as the human operator. That
	// peer identity is part of the idempotency fingerprint.
	guarded := idempotencyRequests(next)
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkDashboardAuth(w, r) {
			return
		}
		guarded.ServeHTTP(w, asDashboardHumanPeer(r))
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
// or /automations must serve the same index HTML so the client router
// (js/nav-history.js) can restore the view on reload or a bookmarked deep link.
// Only the FIRST path segment is validated here; the client normalizes any
// deeper subtab/selection segments (/access/sudo,
// /automations/processes/templates/<template-id> for an open process editor;
// /processes/... is the pre-nesting alias the client heals in place).
//
// Kept in sync with ROUTABLE_TABS in js/nav-history.js. Terminals is
// deliberately absent because it is served by its OWN handler
// (handleDashboardTerminals, registered for both /terminals and the
// /terminals/<agent-id> subtree), not because it is unroutable — it is a
// bookmarkable location like any other tab. Vegas is a conditional soundtrack
// tab that is not URL-routed at all. "costs" is a retired tab kept so old
// /costs bookmarks still load the SPA, whose router heals them to /usage/costs.
var dashboardAppTabs = map[string]bool{
	"groups": true, "automations": true, "jobs": true, "processes": true, "plugins": true, "access": true,
	"messages": true, "usage": true, "costs": true, "diagnostics": true, "config": true,
	"map": true, "fleet": true, "fleet-admin": true, "boards": true,
	// A process template's Collab tab, /collab/processes/<template-id>.
	"collab": true,
	// Retired tabs the client heals to their Diagnostics subtab (/audit →
	// /diagnostics/audit, /logs → /diagnostics/logs, /debug →
	// /diagnostics/performance), and Diagnostics' former name (/activity/<subtab>
	// → /diagnostics/<subtab>), kept so old bookmarks still load.
	"audit": true, "logs": true, "debug": true, "activity": true,
	// Doctor was briefly a top-level tab before becoming /diagnostics/doctor.
	"doctor": true,
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

// isDashboardTerminalsPath reports whether a path belongs to the Terminals
// route: the tab itself (/terminals) or a single terminal addressed by agent id
// (/terminals/<agent-id>). Both are served by handleDashboardTerminals, so both
// are legitimate post-login return targets. Only the prefix is validated — the
// client normalizes the id, and an unknown one degrades to the bare tab.
func isDashboardTerminalsPath(p string) bool {
	return p == "/terminals" || strings.HasPrefix(p, "/terminals/")
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
	if present, valid, refresh := dashboardRequestSessionMatch(r); present {
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
func autoLaunchDashboard(theme string, out io.Writer) {
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
		fmt.Fprintln(out, "  failed to open your browser, open this URL manually:")
		fmt.Fprintln(out, url)
		return
	}
	fmt.Fprintln(out, "  opening dashboard in your browser…")
}
