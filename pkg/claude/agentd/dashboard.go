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
	mux.HandleFunc("/api/snapshot", handleDashboardSnapshot)
	mux.HandleFunc("/api/costs", handleDashboardCosts)
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
	if dashboardSessionToken == "" {
		http.Error(w, "dashboard token not initialised", http.StatusServiceUnavailable)
		return
	}

	// Exchange path: a valid init token is swapped for the session
	// cookie, then we 303 to the bare path so the one-shot token
	// drops out of the URL.
	if tok := r.URL.Query().Get("init_token"); tok != "" {
		if !consumeInitToken(tok, initScopeDashboard) {
			http.Error(w, "invalid or expired init token — reopen the dashboard with `tclaude agent dashboard`", http.StatusForbidden)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     dashboardCookieName,
			Value:    dashboardSessionToken,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
		})
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

	// No token, no cookie — refuse. The dashboard is reachable only
	// through `tclaude agent dashboard` or the agentd tray icon, both
	// of which mint an init token over a human-authenticated channel.
	http.Error(w, "dashboard requires an auth token — open it with `tclaude agent dashboard` or the agentd tray icon", http.StatusForbidden)
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
	if dashboardSessionToken == "" {
		http.Error(w, "dashboard not initialised", http.StatusServiceUnavailable)
		return false
	}
	c, err := r.Cookie(dashboardCookieName)
	if err != nil || c.Value != dashboardSessionToken {
		http.Error(w, "missing or invalid dashboard cookie; load / first", http.StatusForbidden)
		return false
	}
	origin := r.Header.Get("Origin")
	referer := r.Header.Get("Referer")
	if origin == "" && referer == "" {
		http.Error(w, "missing Origin and Referer", http.StatusForbidden)
		return false
	}
	if origin != "" && !strings.HasPrefix(origin, popupBaseURL) {
		http.Error(w, "Origin mismatch", http.StatusForbidden)
		return false
	}
	if origin == "" && !strings.HasPrefix(referer, popupBaseURL) {
		http.Error(w, "Referer mismatch", http.StatusForbidden)
		return false
	}
	return true
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
	Retired     []dashboardRetiredAgent `json:"retired"`
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
	PopupBase      string            `json:"popup_base"` // for tray-shareable display
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
	Name           string            `json:"name"`
	Descr          string            `json:"descr"`
	DefaultCwd     string            `json:"default_cwd"`     // pre-fills the spawn form's cwd; "" = none
	DefaultContext string            `json:"default_context"` // shared startup context injected into spawned agents; "" = none
	MaxMembers     int               `json:"max_members"`     // hard member cap; 0 = unlimited. A spawn that would exceed it is refused.
	Members        []dashboardMember `json:"members"`
	Online         int               `json:"online"`
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
	Role   string `json:"role,omitempty"`
	Descr  string `json:"descr,omitempty"`
	// agentLocationView carries `branch` (current branch) plus the
	// startup/current directory split — see agent_location_view.go.
	agentLocationView
	// repoLinksView carries the GitHub web links for the branch cells
	// — dashboard-only enrichment, see branchlinks.go.
	repoLinksView
	Online bool       `json:"online"`
	Owner  bool       `json:"owner,omitempty"`
	State  agentState `json:"state"`
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
	// ExitReason is why a now-offline agent's session ended: a graceful
	// SessionEnd `reason`, or 'unexpected' when the process died with no
	// clean shutdown (reaper-stamped). Only populated for an offline
	// agent; empty for a live one, or for a row that exited before the
	// exit_reason column existed. The dashboard renders 'unexpected' as
	// "crashed" and everything else (incl. empty) as a plain exit.
	ExitReason string `json:"exit_reason,omitempty"`
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
			Groups:      []string{},
			OwnedGroups: []string{},
			Effective:   []string{},
		}
		agentRows[convID] = a
		return a
	}

	out := snapshotPayload{
		GeneratedAt: time.Now().Format(time.RFC3339),
		PopupBase:   popupBaseURL,
		Permissions: snapshotPermissionsView{
			Defaults:  defaults,
			Grants:    map[string][]string{},
			Overrides: map[string]map[string]string{},
		},
		Slugs: append([]PermSlug{}, permissionRegistry...),
	}
	sort.Slice(out.Slugs, func(i, j int) bool { return out.Slugs[i].Slug < out.Slugs[j].Slug })

	// Initialise slices empty (not nil) so JSON serializes [] instead
	// of null — the dashboard's JS does .length on members directly,
	// which would crash on null.
	out.Groups = []dashboardGroup{}
	out.Agents = []dashboardAgent{}
	for _, g := range groups {
		dg := dashboardGroup{Name: g.Name, Descr: g.Descr, DefaultCwd: g.DefaultCwd, DefaultContext: g.DefaultContext, MaxMembers: g.MaxMembers, Members: []dashboardMember{}}
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
				Role:              m.Role,
				Descr:             m.Descr,
				agentLocationView: loc,
				repoLinksView:     branchLinksFor(m.ConvID, loc),
				Online:            online,
				Owner:             ownerSet[m.ConvID],
				State:             stateForConvIn(m.ConvID, aliveSessions),
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
				Role:              "owner",
				agentLocationView: ownerLoc,
				repoLinksView:     branchLinksFor(ownerConv, ownerLoc),
				Online:            online,
				Owner:             true,
				State:             stateForConvIn(ownerConv, aliveSessions),
			})
			// Pure-owners are reachable via this group too — surface
			// the group on the agent's row in the Agents view so
			// "what groups can this conv see?" matches reality.
			a := addAgent(ownerConv)
			a.Groups = append(a.Groups, g.Name)
			a.OwnedGroups = append(a.OwnedGroups, g.Name)
		}
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

	// Every active enrolled agent — the canonical roster. Unlike the
	// old "online ungrouped session" probe, this includes OFFLINE
	// agents: a conv that was an agent yesterday keeps showing after
	// its tmux pane closed, instead of silently vanishing. Plain
	// conversations that were never promoted are not here — they
	// surface in out.Conversations as promotion candidates.
	activeAgents, _ := db.ListActiveAgents()
	for _, e := range activeAgents {
		addAgent(e.ConvID)
	}

	// Retired agents are demoted — they must never reach out.Agents.
	// Retire revokes group membership and grants, so a retired conv
	// normally can't arrive via the group/grant passes above anyway;
	// retiredSet is the belt-and-braces guard for a partially-applied
	// retire, and feeds the dedicated out.Retired list below.
	retiredAgents, _ := db.ListRetiredAgents()
	retiredSet := make(map[string]bool, len(retiredAgents))
	for _, e := range retiredAgents {
		retiredSet[e.ConvID] = true
	}

	// Superseded conversations — the predecessors of a reincarnation
	// chain — are NOT agents: their identity moved to the chain head.
	// The v29→v30 enrollment backfill used to mis-enroll them as active
	// agents (migrateV30toV31 cleans existing DBs; the fixed backfill
	// prevents new ones). supersededSet is the read-time belt-and-
	// braces guard — symmetric with retiredSet — so a ghost predecessor
	// never reaches out.Agents / out.Ungrouped even if a partially
	// applied reincarnate left a stale enrollment row behind.
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

	out.Conversations = collectConversationsSnapshot(activeAgents, retiredAgents, aliveSessions)
	out.Retired = collectRetiredSnapshot(retiredAgents, aliveSessions)
	out.Cron = collectCronSnapshot()
	out.Links = collectLinksSnapshot()
	out.Usage = collectUsageSnapshot()
	out.Templates = collectTemplatesSnapshot()
	out.Messages, out.MessagesUnread = buildHumanMessagesSnapshot()
	var pluginsErr error
	out.Plugins, out.PluginsWarn, pluginsErr = collectPluginsSnapshot()
	if pluginsErr != nil {
		out.PluginsError = pluginsErr.Error()
	}
	out.PluginsCatalog = pluginCatalog()

	writeJSON(w, http.StatusOK, out)
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
func collectConversationsSnapshot(active, retired []*db.AgentEnrollment, aliveSessions map[string]struct{}) []dashboardConversation {
	out := []dashboardConversation{}
	enrolled := make(map[string]bool, len(active)+len(retired))
	for _, e := range active {
		enrolled[e.ConvID] = true
	}
	for _, e := range retired {
		enrolled[e.ConvID] = true
	}
	rows, err := db.ListRecentConvIndex(conversationsScanLimit)
	if err != nil {
		return out
	}
	for _, row := range rows {
		if len(out) >= conversationsListMax {
			break
		}
		if row.ConvID == "" || enrolled[row.ConvID] {
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
func collectRetiredSnapshot(retired []*db.AgentEnrollment, aliveSessions map[string]struct{}) []dashboardRetiredAgent {
	out := make([]dashboardRetiredAgent, 0, len(retired))
	for _, e := range retired {
		retiredAt := ""
		if !e.RetiredAt.IsZero() {
			retiredAt = e.RetiredAt.Format(time.RFC3339)
		}
		out = append(out, dashboardRetiredAgent{
			ConvID:       e.ConvID,
			Title:        agent.CachedTitle(e.ConvID),
			Online:       isConvOnlineIn(e.ConvID, aliveSessions),
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
