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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/wsl"
)

// startPopupServer binds 127.0.0.1:0 (random port) and runs a tiny
// HTTP server that handles /approve/{id}[/{decision}]. Returns the
// server (so the caller can Shutdown it) and the base URL ("" if
// the listener could not be created — caller should log and continue
// without the popup feature).
func startPopupServer() (*http.Server, string) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		slog.Warn("popup: failed to bind loopback listener; --ask-human will not work", "err", err)
		return nil, ""
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/approve/", handlePopupApprove)
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Warn("popup: server exited", "err", err)
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return srv, fmt.Sprintf("http://127.0.0.1:%d", addr.Port)
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

	// sessionToken is set on the first GET render and required on all
	// POSTs (approve/deny/extend) for this approval. Stored as an
	// HttpOnly, SameSite=Strict cookie. See handlePopupApprove for the
	// threat model (defense-in-depth against drive-by CSRF and
	// scraped-URL replay; does NOT close the same-user /proc-leakage
	// path — that's a known limitation, see the cookie comment below).
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

// requestHumanApproval blocks until the human approves, denies, or
// timeout fires. Returns true on approve, false on deny/timeout.
//
// Side effects: stores a pending entry in `approvals`, spawns a browser
// pointed at the popup URL. The popup HTTP server (mounted at
// http://127.0.0.1:<port>/approve/{id}) renders the page and writes
// back to the channel on user click.
func requestHumanApproval(req *approvalRequest, popupBaseURL string) bool {
	approvals.mu.Lock()
	approvals.pending[req.id] = req
	approvals.mu.Unlock()
	defer func() {
		approvals.mu.Lock()
		delete(approvals.pending, req.id)
		approvals.mu.Unlock()
	}()

	url := popupBaseURL + "/approve/" + req.id
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
		// On first render, mint a session token and set it as an
		// HttpOnly SameSite=Strict cookie. Subsequent POSTs must
		// echo it. Reused on refresh.
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
			renderApprovalDoneCallback(w, true)
		case "deny":
			select {
			case req.decision <- false:
			default:
			}
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
//  1. The HttpOnly session cookie set on first GET must be present
//     and match the stored token. Stops naive replay from a curl
//     attacker who scraped the URL but never opened the page.
//
//  2. The Origin (or Referer if Origin is missing) must point at
//     our own popup base URL. Stops drive-by CSRF from another tab.
//
// Caveats — this is NOT a complete fix. Specifically, a same-user
// process that reads /proc/<browser launcher pid>/cmdline can
// discover the URL, then issue a GET (which returns Set-Cookie),
// then re-use the cookie on POST. That attacker has the same
// privileges as the user already (they can also dial agentd.sock
// directly), so the popup boundary doesn't close any new gap. We
// treat same-user processes as in-scope-of-trust, document the
// residual risk, and queue a more robust scheme (native dialogs or
// a dashboard requiring a tray-icon click) as future work.
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
		cmd = exec.Command("cmd", "/c", "start", "", url)
	default:
		if wsl.IsWSL() {
			if cmdExe := findWindowsCmd(); cmdExe != "" {
				cmd = exec.Command(cmdExe, "/c", "start", "", url)
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
