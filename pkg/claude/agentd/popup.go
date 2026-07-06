package agentd

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/notify"
	"github.com/tofutools/tclaude/pkg/claude/common/wsl"
)

// startPopupServer binds the dashboard HTTP listener (also the home of the
// human-approval "Access requests" surface) and serves the dashboard routes.
// bindHost is the host/interface to bind (defaultDashboardBind = loopback when
// empty; a non-loopback host exposes it on the network); port pins the bound
// TCP port, 0 means an OS-chosen random free port (the historical default).
//
// A bind failure is returned as an error, NOT swallowed: the dashboard is
// essential, and a requested fixed port that is already in use must surface at
// startup rather than silently degrade to a random port — that would break the
// bookmark / reverse-proxy / firewall rule the fixed port was set up for. The
// caller aborts startup on error. On success returns the server (so the caller
// can Shutdown it) and the locally-reachable base URL.
func startPopupServer(bindHost string, port int) (*http.Server, string, error) {
	if strings.TrimSpace(bindHost) == "" {
		bindHost = defaultDashboardBind
	}
	portStr := "0"
	if port > 0 {
		portStr = strconv.Itoa(port)
	}
	bindAddr := net.JoinHostPort(bindHost, portStr)
	ln, err := net.Listen("tcp", bindAddr)
	if err != nil {
		return nil, "", fmt.Errorf("bind dashboard listener %s: %w", bindAddr, err)
	}
	mux := http.NewServeMux()
	// The dashboard is the one human-facing surface on this listener — a
	// single stable URL we can hand to the tray icon's "Open dashboard"
	// action. Human-approval requests are surfaced INSIDE the dashboard now
	// (the Messages tab's "Access requests" folder, dashboard_access_requests.go),
	// so they ride the dashboard's cookie/Origin auth and work over the remote
	// listener too — no separate loopback-only /approve page. Token + cookie
	// auth pinned to popupBaseURL (or host-relative when bound non-loopback)
	// gates /api/*.
	initDashboardToken()
	registerDashboardRoutes(mux)
	srv := &http.Server{
		// auditRequests records dashboard commands (spawn, message,
		// lifecycle, access-request decisions, …) to the audit log;
		// non-command routes (/, /static, the snapshot poll) fall through
		// unmatched. See audit.go (JOH-268).
		Handler:           auditRequests(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Warn("popup: server exited", "err", err)
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	// popupBaseURL must be a locally-reachable URL: it drives the local
	// browser auto-launch, the tray "open dashboard" action, the deep links
	// agentd builds, and (on a loopback bind) the same-origin pin. A wildcard
	// bind (0.0.0.0 / ::) isn't itself dialable, so fall back to loopback; a
	// specific host is used verbatim (JoinHostPort brackets IPv6). When bound
	// non-loopback the browser typically reaches the dashboard through some
	// OTHER hostname (a proxy/LAN IP) whose origin the host-relative check
	// accepts — this base URL just needs to be one that works from this box.
	return srv, "http://" + net.JoinHostPort(dashboardURLHost(bindHost), strconv.Itoa(addr.Port)), nil
}

// dashboardURLHost maps a bind host to the host to put in the
// locally-reachable base URL. A wildcard listen address binds every
// interface but is not itself a dialable destination, so it becomes
// loopback; any specific host (loopback or not) is returned as-is.
func dashboardURLHost(bindHost string) string {
	switch strings.TrimSpace(bindHost) {
	case "", "0.0.0.0", "::", "[::]":
		return defaultDashboardBind
	}
	return bindHost
}

// approvalOutcome is the human's decision on a pending approval. It widens
// the old approve/deny boolean with a third choice — "always allow for this
// agent" — which approves the pending request AND persists an allow
// override so future calls skip the popup (JOH-367). Only slugs flagged
// AutoGrantable can produce outcomeApproveAlways; the popup gates both the
// button and its server-side handler on that.
type approvalOutcome int

const (
	outcomeDeny          approvalOutcome = iota // deny this request
	outcomeApprove                              // approve this one request only
	outcomeApproveAlways                        // approve AND persist an allow override for the agent
)

// approved reports whether the outcome lets the pending request proceed
// (approve or approve-always). Only outcomeDeny blocks it.
func (o approvalOutcome) approved() bool { return o != outcomeDeny }

// approvalRequest is one in-flight human-approval request. It is surfaced to
// the operator in the dashboard's "Access requests" folder (see
// dashboard_access_requests.go) and blocks the requesting agent until the
// operator decides or the timeout auto-denies.
type approvalRequest struct {
	id              string
	perm            string
	convID          string // requester
	convTitle       string // requester's display title
	method          string
	path            string
	rawQuery        string // URL query string (without the '?'), if any
	bodyPreview     string // request body, JSON-prettified when possible
	bodyLabel       string // <dt> label for the body row; "" falls back to "Body"
	targetGroup     string // populated for actions on a specific group
	targetConvID    string // populated for actions on a specific other conv
	targetConvTitle string // resolved display title for targetConvID
	autoGrantable   bool   // slug is eligible for the "always allow" button (JOH-367)
	createdAt       time.Time
	timeout         time.Duration
	decision        chan approvalOutcome // the human's choice: deny / approve / approve-always
	extend          chan time.Duration   // +N seconds — bounded extension so an unattended popup still eventually times out

	// mu guards the mutable field(s) below; the rest of approvalRequest is
	// set once at construction and read lock-free.
	mu sync.Mutex
	// deadline is the wall-clock instant the auto-deny timer will fire,
	// kept live so the dashboard countdown stays honest across "+extend"
	// clicks. Set by the waiter (realRequestHumanApproval) at start and on
	// each extend; read by the snapshot under mu. Zero until the waiter
	// runs — the snapshot then falls back to createdAt+timeout.
	deadline time.Time
}

// approvalRegistry holds pending approvals keyed by ID. Browser
// callbacks resolve the matching channel.
type approvalRegistry struct {
	mu      sync.Mutex
	pending map[string]*approvalRequest
}

// maxResolvedApprovals bounds the dashboard's recent-history query.
const maxResolvedApprovals = 25

var approvals = &approvalRegistry{pending: map[string]*approvalRequest{}}

// pendingCount returns the number of in-flight approval requests.
// Used by the tray icon's poller to decide green vs yellow.
func (a *approvalRegistry) pendingCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.pending)
}

// recordResolved persists a decided request to the bounded recent-history store.
// Called by the approval waiter at each terminal outcome (human decision or
// timeout) so the dashboard can show what was chosen.
func (a *approvalRegistry) recordResolved(req *approvalRequest, outcome string) {
	if err := db.UpsertAccessRequest(accessRequestDB(req, outcome, time.Now())); err != nil {
		slog.Warn("access request: failed to persist resolved request",
			"id", req.id, "perm", req.perm, "outcome", outcome, "err", err)
	}
}

// outcomeLabel maps a decided outcome to its history label. The timeout path
// records "timed out" directly (it isn't an approvalOutcome).
func outcomeLabel(o approvalOutcome) string {
	switch o {
	case outcomeApprove:
		return "approved"
	case outcomeApproveAlways:
		return "always"
	case outcomeDeny:
		return "declined"
	}
	return "declined"
}

// pendingApprovalSummary is a tray-friendly slice of one pending row.
// Keeps only the fields the tray menu needs so callers don't hold
// references to *approvalRequest (which is mutex-protected and would
// race if read off the registry).
type pendingApprovalSummary struct {
	ID        string
	Perm      string
	ConvTitle string
	ConvID    string
	CreatedAt time.Time
}

// snapshotPendingApprovals returns a snapshot of every in-flight
// approval, sorted oldest-first (so the longest-waiting popup is at
// the top of the tray menu — the human's eye lands on what's been
// blocked longest). Safe to call from any goroutine; takes the
// registry mutex briefly.
func (a *approvalRegistry) snapshot() []pendingApprovalSummary {
	a.mu.Lock()
	out := make([]pendingApprovalSummary, 0, len(a.pending))
	for _, req := range a.pending {
		out = append(out, pendingApprovalSummary{
			ID:        req.id,
			Perm:      req.perm,
			ConvTitle: req.convTitle,
			ConvID:    req.convID,
			CreatedAt: req.createdAt,
		})
	}
	a.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// RequestHumanApprovalImpl is the indirection point for
// requestHumanApproval so flow tests can stub the popup decision
// without spawning a browser. Production assigns realRequestHumanApproval
// (the inline body below); tests replace it via t.Cleanup-restored
// assignment.
var RequestHumanApprovalImpl = realRequestHumanApproval

var (
	approvalBrowserOpener = openBrowser
	accessRequestNotify   = notify.SendAccessRequest
)

// requestHumanApproval blocks until the human approves, denies, or
// timeout fires. Returns true on approve, false on deny/timeout.
//
// Side effects: stores a pending entry in `approvals`, spawns a browser
// pointed at the popup URL. The popup HTTP server (mounted at
// http://127.0.0.1:<port>/approve/{id}) renders the page and writes
// back to the channel on user click.
func requestHumanApproval(req *approvalRequest, popupBaseURL string) bool {
	return RequestHumanApprovalImpl(req, popupBaseURL)
}

func realRequestHumanApproval(req *approvalRequest, popupBaseURL string) bool {
	approvals.mu.Lock()
	approvals.pending[req.id] = req
	approvals.mu.Unlock()
	defer func() {
		approvals.mu.Lock()
		delete(approvals.pending, req.id)
		approvals.mu.Unlock()
	}()

	// Record the request itself the moment it is raised — an
	// agent-attributed "approval.request" row that pairs with the operator's
	// later "approval.approve"/"approval.deny" decision (JOH-392). Without
	// it the trail showed decisions with no matching request, and a request
	// that TIMED OUT (whose auto-deny is intentionally not a recorded
	// decision) left no approval-verb trace at all.
	recordApprovalRequest(req)

	cfg, cfgErr := config.Load()
	if cfgErr != nil {
		slog.Warn("popup: failed to load config for access-request alerting", "err", cfgErr)
	}
	if cfgErr == nil && cfg.AccessRequestSystemNotification() {
		accessRequestNotify(notifyHumanSenderSessionID(req.convID), req.convTitle, req.targetGroup, req.perm, req.path)
	}
	if cfgErr == nil && cfg.AccessRequestAutoOpenBrowser() {
		// Optional compatibility path for operators who still want the old
		// auto-raise behavior. By default the dashboard's Messages badge and
		// access-request banner are the attention surface.
		url := popupBaseURL + "/?init_token=" + mintInitToken(initScopeDashboard)
		go func() {
			if err := approvalBrowserOpener(url); err != nil {
				slog.Warn("popup: failed to open browser", "err", err, "url", url)
			}
		}()
		slog.Info("popup: auto-opening dashboard for access request",
			"id", req.id, "perm", req.perm, "conv", req.convID, "url", url)
	}
	slog.Info("popup: awaiting human decision",
		"id", req.id, "perm", req.perm, "conv", req.convID,
		"path", req.path, "timeout", req.timeout)

	// timer fires the auto-deny. "+N" extensions reset it so the human
	// can buy more time mid-review without leaving the popup unattended
	// indefinitely.
	timer := time.NewTimer(req.timeout)
	defer timer.Stop()
	req.setDeadline(time.Now().Add(req.timeout))
	if err := db.UpsertAccessRequest(accessRequestDB(req, db.AccessRequestStatusPending, time.Time{})); err != nil {
		slog.Warn("access request: failed to persist pending request",
			"id", req.id, "perm", req.perm, "conv", req.convID, "err", err)
	}
	for {
		select {
		case d := <-req.decision:
			approved := applyApprovalOutcome(req, d)
			approvals.recordResolved(req, outcomeLabel(d))
			return approved
		case d := <-req.extend:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(d)
			req.setDeadline(time.Now().Add(d))
			slog.Info("popup: timeout extended",
				"id", req.id, "perm", req.perm, "by", d)
		case <-timer.C:
			slog.Info("popup: timeout fired (auto-deny)",
				"id", req.id, "perm", req.perm)
			approvals.recordResolved(req, "timed out")
			return false
		}
	}
}

// newApprovalID returns a 32-hex-char random token. Callers should
// treat IDs as opaque; the popup URL is the only place they appear.
func newApprovalID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Random failure is exceptional. Fall back to a time-based ID;
		// it's still unguessable enough for our same-user threat model.
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// applyApprovalOutcome runs the side-effects of a consumed decision and
// reports whether the pending request may proceed. It audits the decision
// and, for outcomeApproveAlways, persists the allow override. Called from
// the approval WAITER at the moment it consumes the decision off
// req.decision — so the side-effects run for exactly the decision that took
// effect, exactly once (a double-submitted POST whose send is dropped never
// reaches here). Both the real waiter and the test stub route through this,
// so the stub exercises the same audit + persist path as production.
func applyApprovalOutcome(req *approvalRequest, outcome approvalOutcome) bool {
	recordApprovalDecision(req, outcome)
	if outcome == outcomeApproveAlways {
		persistAlwaysAllowGrant(req)
	}
	return outcome.approved()
}

// persistAlwaysAllowGrant writes the popup-origin allow override for the
// deciding agent (JOH-367). The override is keyed on the agent's stable
// identity (db.SetAgentPermissionOverride resolves conv → agent_id), so it
// follows the agent through /clear conv-rotation and reincarnation — the
// operator's "always allow for THIS agent" intent, not "this one conv".
//
// Defense in depth: re-checks IsAutoGrantableSlug even though the POST
// handler already gated on req.autoGrantable, so a malformed caller can
// never persist an ineligible slug. Best-effort — a DB failure is logged
// but does NOT fail the approval the human just granted (the one-off action
// still proceeds; only the persistence was lost).
func persistAlwaysAllowGrant(req *approvalRequest) {
	if !IsAutoGrantableSlug(req.perm) {
		slog.Warn("popup: refusing to persist always-allow for ineligible slug",
			"perm", req.perm, "conv", req.convID)
		return
	}
	if req.convID == "" {
		return
	}
	if err := db.SetAgentPermissionOverride(req.convID, req.perm, db.PermEffectGrant, "human:popup-always"); err != nil {
		slog.Warn("popup: failed to persist always-allow grant",
			"perm", req.perm, "conv", req.convID, "err", err)
	}
}

// recordApprovalRequest writes an audit row for the moment an agent RAISES
// a human-approval request (the `--ask-human` / X-Tclaude-Ask-Human escape
// hatch). It is the agent-attributed counterpart to recordApprovalDecision:
// this row names the requester as the actor, the eventual decision row names
// the operator. Recording the request closes two gaps (JOH-392): decisions
// no longer appear with no matching request, and a request that later TIMES
// OUT — whose auto-deny is deliberately not a recorded decision — still
// leaves an approval-verb trace of what was asked.
//
// The actor is always an agent: humans bypass the permission gates before
// ever reaching a popup. Source is derived from the original request path
// (the surface the agent's call arrived on), so the request row is tagged
// cli/dashboard like the underlying command rather than popup (which is the
// human's decision surface). Status is 200 — the request was successfully
// placed; its outcome is carried by the separate decision/command rows.
// Best-effort: a logging failure is warned and swallowed so it can never
// affect the request the agent just made.
func recordApprovalRequest(req *approvalRequest) {
	label := strings.TrimSpace(req.convTitle)
	if label == "" {
		label = short8(req.convID)
	}
	// Same fallback for the target (the cross-agent path sets targetConvID):
	// never leave a resolvable conv with a blank label, matching
	// resolveAuditTarget's convention.
	targetLabel := strings.TrimSpace(req.targetConvTitle)
	if targetLabel == "" && req.targetConvID != "" {
		targetLabel = short8(req.targetConvID)
	}
	detail := strings.TrimSpace(req.perm)
	if action := strings.TrimSpace(req.method + " " + req.path); action != "" {
		if detail != "" {
			detail += " — " + action
		} else {
			detail = action
		}
	}
	if _, err := db.InsertAuditLog(db.AuditLogEntry{
		ActorKind:   db.AuditActorAgent,
		ActorConv:   req.convID,
		ActorLabel:  label,
		Verb:        "approval.request",
		TargetConv:  req.targetConvID,
		TargetLabel: targetLabel,
		GroupName:   req.targetGroup,
		Detail:      auditClip(detail, 120),
		Method:      req.method,
		Path:        req.path,
		Status:      http.StatusOK,
		Source:      auditSourceForPath(req.path),
	}); err != nil {
		slog.Warn("audit: failed to record approval request", "perm", req.perm, "err", err)
	}
}

// auditSourceForPath maps a request path to the audit source of the surface
// it arrived on: /v1/* is the CLI, /api/* is the dashboard. Anything else
// falls back to popup (the approval subsystem's own surface).
func auditSourceForPath(path string) string {
	switch {
	case strings.HasPrefix(path, "/v1/"):
		return db.AuditSourceCLI
	case strings.HasPrefix(path, "/api/"):
		return db.AuditSourceDashboard
	default:
		return db.AuditSourcePopup
	}
}

// recordApprovalDecision writes an audit row for a human decision on a
// pending permission request. The popup server isn't under /v1 or /api, so
// the auditRequests middleware never matches it — and the approval context
// (which agent, which permission) lives in the in-memory request, not the
// HTTP body — so we record here directly. It is called from the approval
// WAITER (via applyApprovalOutcome) at the moment it consumes the decision
// off req.decision, so it records exactly the decision that took effect,
// exactly once. Best-effort: a logging failure is warned and swallowed so
// it can never affect the decision the human just made.
//
// The popup is human-only (loopback + single-use init token + per-approval
// session cookie), so the actor is always the operator; the target is the
// agent whose request was decided. A timeout auto-deny and `extend` are not
// human decisions and are intentionally not recorded.
func recordApprovalDecision(req *approvalRequest, outcome approvalOutcome) {
	verb, word := "approval.deny", "deny"
	switch outcome {
	case outcomeApprove:
		verb, word = "approval.approve", "approve"
	case outcomeApproveAlways:
		verb, word = "approval.approve-always", "always"
	case outcomeDeny:
		// keep the deny defaults
	}
	detail := strings.TrimSpace(req.perm)
	if action := strings.TrimSpace(req.method + " " + req.path); action != "" {
		if detail != "" {
			detail += " — " + action
		} else {
			detail = action
		}
	}
	if _, err := db.InsertAuditLog(db.AuditLogEntry{
		ActorKind:   db.AuditActorHuman,
		ActorLabel:  "operator",
		Verb:        verb,
		TargetConv:  req.convID,
		TargetLabel: req.convTitle,
		GroupName:   req.targetGroup,
		Detail:      auditClip(detail, 120),
		Method:      http.MethodPost,
		Path:        "/approve/" + req.id + "/" + word,
		Status:      http.StatusOK,
		Source:      db.AuditSourcePopup,
	}); err != nil {
		slog.Warn("audit: failed to record approval decision", "verb", verb, "err", err)
	}
}

// snapshotRequestBody reads the request body, builds a bounded preview
// string for the popup (JSON-prettified when it parses), and replaces
// r.Body with a fresh reader holding the SAME bytes so the downstream
// handler still receives its full request. Returns the preview ("" if no
// body).
//
// The preview is capped at maxBodyPreview and marked "…[truncated]" when
// it overflows — that only affects what the human is shown. The RESTORED
// body is preserved up to maxRestoreBody (well above the largest legit
// mutating body — a 256 KiB clipboard payload is ≈1.5 MiB on the wire),
// so the popup never silently shortens what the handler decodes. A body
// past maxRestoreBody is restored truncated, but the handler's own
// MaxBytesReader then rejects it with the same 400 it would return with no
// popup — never a silent mis-decode. (Restoring only the 64 KiB preview,
// as this did before, truncated a large clipboard body AFTER the human had
// approved it: an approve-then-fail.)
func snapshotRequestBody(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	const (
		maxBodyPreview = 64 * 1024       // rendered in the popup
		maxRestoreBody = 2 * 1024 * 1024 // preserved for the handler
	)
	buf, err := io.ReadAll(io.LimitReader(r.Body, maxRestoreBody))
	_ = r.Body.Close()
	if err != nil {
		r.Body = io.NopCloser(bytes.NewReader(nil))
		return ""
	}
	// Restore the full body we read, so the handler decodes exactly what the
	// client sent (up to the restore bound).
	r.Body = io.NopCloser(bytes.NewReader(buf))
	if len(buf) == 0 {
		return ""
	}
	// Build the preview from the leading bytes only.
	preview := buf
	truncated := false
	if len(preview) > maxBodyPreview {
		preview = preview[:maxBodyPreview]
		truncated = true
	}
	// Prettify JSON if it parses; otherwise show raw.
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, preview, "", "  "); err == nil {
		out := pretty.String()
		if truncated {
			out += "\n…[truncated]"
		}
		return out
	}
	out := string(preview)
	if truncated {
		out += "\n…[truncated]"
	}
	return out
}

// openBrowser launches the platform's default browser pointed at url.
// Best-effort: returns the underlying error so callers can log it, but
// the request flow does not depend on browser launch (the human can
// always paste the URL manually).
//
// On WSL we try harder than plain xdg-open: routing through Windows
// avoids the libsecret/gnome-keyring prompts that fire when xdg-open
// happens to resolve to a Linux browser inside the WSL distro. Order
// is cmd.exe /c start → wslview → xdg-open.
//
//   - cmd.exe is the most direct interop: if /mnt/c/.../cmd.exe is
//     reachable, the URL hands off to the Windows host browser with
//     zero extra dependencies.
//   - wslview (from the `wslu` package) does the same thing but its
//     own self-check is broken on recent WSL2 kernels that load the
//     binfmt entry as `WSLInterop-late` instead of `WSLInterop`, so
//     it bails before opening anything. We still try it as a fallback
//     in case cmd.exe isn't on /mnt/c/ (custom mount layouts).
//   - xdg-open is the final fallback (and may still hit a Linux
//     browser → keyring prompt; we accept that on hosts where neither
//     of the above works).
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", escapeForCmdExe(url))
	default:
		if wsl.IsWSL() {
			if cmdExe := findWindowsCmd(); cmdExe != "" {
				cmd = exec.Command(cmdExe, "/c", "start", "", escapeForCmdExe(url))
				break
			}
			if path, err := exec.LookPath("wslview"); err == nil {
				cmd = exec.Command(path, url)
				break
			}
		}
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

// escapeForCmdExe escapes cmd.exe metacharacters (`^&<>|`) by prefixing
// each with `^`. Without this `cmd /c start "" URL` splits the command
// line at `&`, dropping the rest of the URL — exactly what happens to
// `http://…?init_token=X&slop=1` on WSL and native Windows, where the
// browser ends up at `…?init_token=X` and the slop theme never
// activates. wslview and xdg-open don't parse the URL through a shell,
// so they get the raw string unchanged.
//
// Order matters: `^` must be in the replacer table so an existing `^`
// in the URL doesn't get reinterpreted as an escape lead-in. The
// stdlib NewReplacer processes the input left-to-right without
// re-scanning its own output, so `^&` → `^^^&` (literal `^` then
// literal `&`) — correct.
func escapeForCmdExe(s string) string {
	return cmdExeEscaper.Replace(s)
}

var cmdExeEscaper = strings.NewReplacer(
	"^", "^^",
	"&", "^&",
	"<", "^<",
	">", "^>",
	"|", "^|",
)

// findWindowsCmd locates cmd.exe on a mounted Windows drive when running
// under WSL. Returns "" if not found.
func findWindowsCmd() string {
	for _, p := range []string{
		"/mnt/c/Windows/System32/cmd.exe",
		"/mnt/c/Windows/SysWOW64/cmd.exe",
	} {
		if _, err := exec.LookPath(p); err == nil {
			return p
		}
	}
	return ""
}
