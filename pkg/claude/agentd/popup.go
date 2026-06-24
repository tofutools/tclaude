package agentd

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
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

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/wsl"
)

// startPopupServer binds a loopback HTTP listener and runs a tiny
// server that handles /approve/{id}[/{decision}] plus the dashboard
// routes (both are human-only and share the one stable URL handed to
// the tray icon / `tclaude agent dashboard`). port pins the bound TCP
// port; 0 means an OS-chosen random free port (the historical default).
//
// A bind failure is returned as an error, NOT swallowed: the dashboard
// + approval popup are essential, and a requested fixed port that is
// already in use must surface at startup rather than silently degrade to
// a random port — that would break the bookmark / reverse-proxy / firewall
// rule the fixed port was set up for. The caller aborts startup on error.
// On success returns the server (so the caller can Shutdown it) and the
// base URL.
func startPopupServer(port int) (*http.Server, string, error) {
	bindAddr := "127.0.0.1:0"
	if port > 0 {
		bindAddr = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	}
	ln, err := net.Listen("tcp", bindAddr)
	if err != nil {
		return nil, "", fmt.Errorf("bind loopback listener %s: %w", bindAddr, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/approve/", handlePopupApprove)
	// Dashboard rides on the same loopback listener — both views are
	// human-only, both want one stable URL we can hand to the tray
	// icon's "Open dashboard" action. Token + cookie auth pinned to
	// popupBaseURL gates /api/* the same way popup approval does.
	initDashboardToken()
	registerDashboardRoutes(mux)
	srv := &http.Server{
		// auditRequests records dashboard commands (spawn, message,
		// lifecycle, …) to the audit log; non-command routes (/, /static,
		// /approve, the snapshot poll) fall through unmatched. See audit.go
		// (JOH-268).
		Handler:           auditRequests(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Warn("popup: server exited", "err", err)
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return srv, fmt.Sprintf("http://127.0.0.1:%d", addr.Port), nil
}

// ApprovalRequest is what the popup UI shows the human. Fields are
// embedded into the HTML page; all values must be HTML-escaped before
// rendering.
type approvalRequest struct {
	id              string
	perm            string
	convID          string // requester
	convTitle       string // requester's display title
	method          string
	path            string
	rawQuery        string // URL query string (without the '?'), if any
	bodyPreview     string // request body, JSON-prettified when possible
	targetGroup     string // populated for actions on a specific group
	targetConvID    string // populated for actions on a specific other conv
	targetConvTitle string // resolved display title for targetConvID
	createdAt       time.Time
	timeout         time.Duration
	decision        chan bool          // approve=true, deny=false
	extend          chan time.Duration // +N seconds — bounded extension so an unattended popup still eventually times out

	// sessionToken is minted when a valid init token is exchanged at
	// the GET handler, and is required on all POSTs (approve/deny/
	// extend) for this approval. Stored as an HttpOnly, SameSite=Strict
	// cookie. See handlePopupApprove and checkPopupAuth for the threat
	// model.
	mu           sync.Mutex
	sessionToken string
}

// approvalRegistry holds pending approvals keyed by ID. Browser
// callbacks resolve the matching channel.
type approvalRegistry struct {
	mu      sync.Mutex
	pending map[string]*approvalRequest
}

var approvals = &approvalRegistry{pending: map[string]*approvalRequest{}}

// pendingCount returns the number of in-flight approval requests.
// Used by the tray icon's poller to decide green vs yellow.
func (a *approvalRegistry) pendingCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.pending)
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

	// Embed a one-shot init token bound to this approval; the popup's
	// GET exchanges it for the session cookie. The human's browser,
	// launched right below, consumes it — see inittoken.go for the
	// residual /proc-scrape note.
	url := popupBaseURL + "/approve/" + req.id + "?init_token=" + mintInitToken(initScopeApprove(req.id))
	go func() {
		if err := openBrowser(url); err != nil {
			slog.Warn("popup: failed to open browser", "err", err, "url", url)
		}
	}()
	slog.Info("popup: awaiting human decision",
		"id", req.id, "perm", req.perm, "conv", req.convID,
		"path", req.path, "timeout", req.timeout, "url", url)

	// timer fires the auto-deny. "+N" extensions reset it so the human
	// can buy more time mid-review without leaving the popup unattended
	// indefinitely.
	timer := time.NewTimer(req.timeout)
	defer timer.Stop()
	for {
		select {
		case d := <-req.decision:
			return d
		case d := <-req.extend:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(d)
			slog.Info("popup: timeout extended",
				"id", req.id, "perm", req.perm, "by", d)
		case <-timer.C:
			slog.Info("popup: timeout fired (auto-deny)",
				"id", req.id, "perm", req.perm)
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

// handlePopupApprove serves both the GET (render the page) and POST
// (record the decision) sides of /approve/{id}[/{decision}]. Mounted on
// the loopback HTTP server, never on the unix socket.
func handlePopupApprove(w http.ResponseWriter, r *http.Request) {
	// Refuse anything that isn't loopback. http.ListenAndServe on
	// 127.0.0.1 already restricts the listening addr, but a
	// belt-and-braces check on RemoteAddr keeps the door shut even if
	// the listener gets reused later.
	if !strings.HasPrefix(r.RemoteAddr, "127.0.0.1:") && !strings.HasPrefix(r.RemoteAddr, "[::1]:") {
		http.Error(w, "forbidden: loopback only", http.StatusForbidden)
		return
	}

	rest := strings.TrimPrefix(r.URL.Path, "/approve/")
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	if id == "" {
		http.Error(w, "missing approval id", http.StatusBadRequest)
		return
	}
	approvals.mu.Lock()
	req, ok := approvals.pending[id]
	approvals.mu.Unlock()
	if !ok {
		http.Error(w, "no such approval (already decided, expired, or unknown)", http.StatusNotFound)
		return
	}

	switch r.Method {
	case http.MethodGet:
		// The session cookie is handed out only in exchange for a
		// valid single-use init token — the same token-exchange the
		// dashboard uses. tclaude agentd embeds the token in the URL
		// it launches; the tray re-mints one on demand. A bare GET
		// with no token and no cookie is refused, so a process that
		// merely scrapes the approval id cannot mint itself a cookie.
		if tok := r.URL.Query().Get("init_token"); tok != "" {
			if !consumeInitToken(tok, initScopeApprove(req.id)) {
				http.Error(w, "invalid or expired init token; reopen this approval from the agentd tray icon", http.StatusForbidden)
				return
			}
			req.mu.Lock()
			if req.sessionToken == "" {
				req.sessionToken = newApprovalID() // reuse the random gen; same entropy
			}
			token := req.sessionToken
			req.mu.Unlock()
			http.SetCookie(w, &http.Cookie{
				Name:     "tclaude_popup_" + req.id,
				Value:    token,
				Path:     "/approve/" + req.id,
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
			})
			// Redirect to the bare path so the one-shot token drops
			// out of the address bar and browser history.
			http.Redirect(w, r, "/approve/"+req.id, http.StatusSeeOther)
			return
		}
		// No init token: render only for an already-exchanged cookie
		// (a refresh of the page the human already opened).
		req.mu.Lock()
		expected := req.sessionToken
		req.mu.Unlock()
		if expected == "" {
			http.Error(w, "open this approval via the link tclaude agentd launched, or from the agentd tray icon", http.StatusForbidden)
			return
		}
		if c, err := r.Cookie("tclaude_popup_" + req.id); err != nil || c.Value != expected {
			http.Error(w, "missing or invalid popup session cookie", http.StatusForbidden)
			return
		}
		renderApprovalPage(w, req)
	case http.MethodPost:
		if !checkPopupAuth(w, r, req) {
			return
		}
		if len(parts) < 2 {
			http.Error(w, "missing decision", http.StatusBadRequest)
			return
		}
		decision := parts[1]
		switch decision {
		case "approve":
			select {
			case req.decision <- true:
			default:
			}
			recordApprovalDecision(r, req, true)
			renderApprovalDoneCallback(w, true)
		case "deny":
			select {
			case req.decision <- false:
			default:
			}
			recordApprovalDecision(r, req, false)
			renderApprovalDoneCallback(w, false)
		case "extend":
			// Resets the auto-deny timer; bounded so an unattended
			// popup still eventually times out. Default +5 minutes;
			// caller can pass ?secs=N (capped at 300 to match the
			// daemon's overall AskHuman ceiling).
			extendBy := 5 * time.Minute
			if v := r.URL.Query().Get("secs"); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					if n > 300 {
						n = 300
					}
					extendBy = time.Duration(n) * time.Second
				}
			}
			select {
			case req.extend <- extendBy:
			default:
				// Already an extend in flight; idempotent no-op.
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprintf(w, "+%s\n", extendBy)
		default:
			http.Error(w, "decision must be approve, deny, or extend", http.StatusBadRequest)
			return
		}
	default:
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
	}
}

// recordApprovalDecision writes an audit row for a human approve/deny of a
// pending permission request. The popup server isn't under /v1 or /api, so
// the auditRequests middleware never matches it — and the approval context
// (which agent, which permission) lives in the in-memory request, not the
// HTTP body — so we record here directly while `req` is in hand rather than
// retrofitting the middleware. Best-effort: a logging failure is warned and
// swallowed so it can never affect the decision the human just made.
//
// The popup is human-only (loopback + single-use init token + per-approval
// session cookie), so the actor is always the operator; the target is the
// agent whose request was decided. `extend` is not a final decision and is
// intentionally not recorded.
func recordApprovalDecision(r *http.Request, req *approvalRequest, approved bool) {
	verb := "approval.deny"
	if approved {
		verb = "approval.approve"
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
		Method:      r.Method,
		Path:        r.URL.Path,
		Status:      http.StatusOK,
		Source:      db.AuditSourcePopup,
	}); err != nil {
		slog.Warn("audit: failed to record approval decision", "verb", verb, "err", err)
	}
}

const approvalPageTemplate = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>tclaude agent approval</title>
<style>
  body { font-family: system-ui, sans-serif; max-width: 720px; margin: 4em auto; padding: 0 1em; }
  h1 { margin-bottom: 0.2em; }
  h2 { font-size: 1em; margin-top: 1.4em; margin-bottom: 0.4em; color: #444; }
  .meta { color: #555; font-size: 0.9em; margin-bottom: 1.5em; }
  .meta dt { font-weight: bold; }
  .meta dd { margin-left: 0; margin-bottom: 0.6em; }
  .name { font-weight: 600; color: #222; }
  .id   { font-family: ui-monospace, monospace; color: #777; font-size: 0.85em; display: block; }
  .mono { font-family: ui-monospace, monospace; }
  pre.body {
    background: #f4f4f4; border: 1px solid #ddd; border-radius: 4px;
    padding: 0.8em 1em; font-family: ui-monospace, monospace; font-size: 0.85em;
    white-space: pre-wrap; word-break: break-word; max-height: 22em; overflow-y: auto;
  }
  form { display: inline; }
  button { font-size: 1.1em; padding: 0.6em 1.4em; margin-right: 0.4em; cursor: pointer; }
  .approve { background: #2c7a39; color: white; border: 1px solid #1f5c2a; }
  .deny    { background: #b03a2e; color: white; border: 1px solid #862c22; }
  .extend  { background: #4d6fb3; color: white; border: 1px solid #345088; }
  .extend:disabled { background: #aaa; border-color: #888; cursor: default; }
  .hint    { color: #777; font-size: 0.85em; margin-top: 1em; }
  .countdown { font-family: ui-monospace, monospace; font-weight: bold; color: #b03a2e; }
  .countdown.paused { color: #2c7a39; }
</style>
</head>
<body>
<h1>Agent wants permission</h1>
<dl class="meta">
  <dt>Permission</dt><dd class="mono">%s</dd>
  <dt>Requester</dt><dd><span class="name">%s</span><span class="id">%s</span></dd>%s%s
  <dt>Endpoint</dt><dd class="mono">%s %s</dd>%s%s
  <dt>Timeout</dt><dd>auto-deny in <span id="countdown" class="countdown">%ds</span></dd>
</dl>
<form action="/approve/%s/approve" method="post">
  <button class="approve" autofocus>Approve</button>
</form>
<form action="/approve/%s/deny" method="post">
  <button class="deny">Deny</button>
</form>
<button id="extend-btn" class="extend" type="button" data-id="%s">+5min</button>
<p class="hint">This popup was opened by <code>tclaude agentd</code>
on this machine. If you didn't expect it, click Deny. Use <strong>+5min</strong>
to push the auto-deny back if you need more time to read.</p>
<script>
(function() {
  const id = document.getElementById('extend-btn').dataset.id;
  const cd = document.getElementById('countdown');
  let remaining = parseInt(cd.textContent, 10);
  let lastTick = Date.now();
  function render() {
    if (remaining <= 0) { cd.textContent = 'TIMED OUT'; return; }
    cd.textContent = remaining + 's';
  }
  setInterval(function() {
    const now = Date.now();
    if (now - lastTick >= 1000) {
      remaining -= Math.floor((now - lastTick) / 1000);
      lastTick = now;
      render();
    }
  }, 200);
  document.getElementById('extend-btn').addEventListener('click', function() {
    const btn = this;
    btn.disabled = true;
    btn.textContent = 'extending…';
    fetch('/approve/' + id + '/extend?secs=300', {method: 'POST'})
      .then(function(r) { return r.text(); })
      .then(function() {
        remaining += 300;
        cd.classList.add('paused');
        render();
        btn.textContent = '+5min';
        btn.disabled = false;
      })
      .catch(function() {
        btn.textContent = 'extend failed';
      });
  });
})();
</script>
</body>
</html>
`

func renderApprovalPage(w http.ResponseWriter, req *approvalRequest) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	requesterTitle := req.convTitle
	if requesterTitle == "" {
		requesterTitle = "(unnamed)"
	}

	groupRow := ""
	if req.targetGroup != "" {
		groupRow = "\n  <dt>Group</dt><dd>" + html.EscapeString(req.targetGroup) + "</dd>"
	}

	targetRow := ""
	if req.targetConvID != "" {
		t := req.targetConvTitle
		if t == "" {
			t = "(unknown)"
		}
		targetRow = "\n  <dt>Target agent</dt><dd>" +
			"<span class=\"name\">" + html.EscapeString(t) + "</span>" +
			"<span class=\"id\">" + html.EscapeString(req.targetConvID) + "</span></dd>"
	}

	queryRow := ""
	if req.rawQuery != "" {
		queryRow = "\n  <dt>Query</dt><dd>" + html.EscapeString(req.rawQuery) + "</dd>"
	}

	bodyRow := ""
	if req.bodyPreview != "" {
		bodyRow = "\n  <dt>Body</dt><dd><pre class=\"body\">" + html.EscapeString(req.bodyPreview) + "</pre></dd>"
	}

	fmt.Fprintf(w, approvalPageTemplate,
		html.EscapeString(req.perm),
		html.EscapeString(requesterTitle),
		html.EscapeString(req.convID),
		groupRow,
		targetRow,
		html.EscapeString(req.method),
		html.EscapeString(req.path),
		queryRow,
		bodyRow,
		int(req.timeout.Seconds()),
		html.EscapeString(req.id),
		html.EscapeString(req.id),
		html.EscapeString(req.id),
	)
}

// checkPopupAuth gates POSTs to the approval endpoints with two
// cheap defense-in-depth checks:
//
//  1. The HttpOnly session cookie must be present and match the
//     stored token. The cookie is handed out only in exchange for a
//     single-use init token (see handlePopupApprove's GET), so a
//     process that scraped the approval URL but lost the race to the
//     human's browser cannot have it.
//
//  2. The Origin (or Referer if Origin is missing) must point at
//     our own popup base URL. Stops drive-by CSRF from another tab.
//
// Residual: a same-user process that reads the browser launcher's
// argv off /proc can still race the human's browser for the
// single-use init token. Winning that race means beating a browser
// the daemon launches immediately, and losing burns the token.
// Closing it entirely means stopping a process from reading another
// process's argv — a sandbox responsibility, not tclaude's.
func checkPopupAuth(w http.ResponseWriter, r *http.Request, req *approvalRequest) bool {
	// Cookie check.
	c, err := r.Cookie("tclaude_popup_" + req.id)
	if err != nil || c.Value == "" {
		http.Error(w, "missing popup session cookie; load the popup page first", http.StatusForbidden)
		return false
	}
	req.mu.Lock()
	expected := req.sessionToken
	req.mu.Unlock()
	if expected == "" || c.Value != expected {
		http.Error(w, "popup session cookie does not match", http.StatusForbidden)
		return false
	}

	// Origin / Referer check. Browser fetch() sends Origin; classic
	// form posts only send Referer. Accept either as long as it
	// points at our own popup base.
	origin := r.Header.Get("Origin")
	referer := r.Header.Get("Referer")
	if origin == "" && referer == "" {
		http.Error(w, "missing Origin and Referer", http.StatusForbidden)
		return false
	}
	// popupBaseURL is empty only in tests that stand up the mux without a
	// bound listener; the per-approval session cookie is the gate there
	// and the origin pin is disabled (mirrors checkDashboardAuth).
	if popupBaseURL != "" {
		if origin != "" && !originMatchesBase(origin, popupBaseURL) {
			http.Error(w, "Origin mismatch", http.StatusForbidden)
			return false
		}
		if origin == "" && !originMatchesBase(referer, popupBaseURL) {
			http.Error(w, "Referer mismatch", http.StatusForbidden)
			return false
		}
	}
	return true
}

// snapshotRequestBody reads the request body (up to maxBodyPreview
// bytes), JSON-prettifies it when it parses, and replaces r.Body with
// a fresh reader so the downstream handler still receives the same
// bytes. Returns the preview string ("" if no body).
//
// Bodies above the cap are truncated and marked. The handler will
// also see the truncated bytes — fine for our use, since the agent
// CLI never sends >64KiB to mutating endpoints today.
func snapshotRequestBody(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	const maxBodyPreview = 64 * 1024
	buf, err := io.ReadAll(io.LimitReader(r.Body, maxBodyPreview+1))
	_ = r.Body.Close()
	if err != nil {
		r.Body = io.NopCloser(bytes.NewReader(nil))
		return ""
	}
	truncated := false
	if len(buf) > maxBodyPreview {
		buf = buf[:maxBodyPreview]
		truncated = true
	}
	r.Body = io.NopCloser(bytes.NewReader(buf))
	if len(buf) == 0 {
		return ""
	}
	// Prettify JSON if it parses; otherwise show raw.
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, buf, "", "  "); err == nil {
		out := pretty.String()
		if truncated {
			out += "\n…[truncated]"
		}
		return out
	}
	out := string(buf)
	if truncated {
		out += "\n…[truncated]"
	}
	return out
}

func renderApprovalDoneCallback(w http.ResponseWriter, approved bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	verb := "Denied"
	if approved {
		verb = "Approved"
	}
	fmt.Fprintf(w, `<!doctype html>
<html><head><meta charset="utf-8"><title>%s</title>
<style>body{font-family:system-ui;max-width:640px;margin:4em auto;text-align:center;}</style>
</head><body><h1>%s</h1>
<p>You can close this tab.</p></body></html>`, verb, verb)
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
