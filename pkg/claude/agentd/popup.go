package agentd

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"runtime"
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
	id        string
	perm      string
	convID    string
	convTitle string
	method    string
	path      string
	createdAt time.Time
	timeout   time.Duration
	decision  chan bool // approve=true, deny=false
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

	select {
	case d := <-req.decision:
		return d
	case <-time.After(req.timeout):
		return false
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
		renderApprovalPage(w, req)
	case http.MethodPost:
		if len(parts) < 2 {
			http.Error(w, "missing decision", http.StatusBadRequest)
			return
		}
		decision := parts[1]
		var ok bool
		switch decision {
		case "approve":
			ok = true
		case "deny":
			ok = false
		default:
			http.Error(w, "decision must be approve or deny", http.StatusBadRequest)
			return
		}
		// Non-blocking send so a duplicate click after the channel was
		// already read can't deadlock — request goroutine has already
		// returned and removed the entry, but our local copy of req
		// still has a buffered chan.
		select {
		case req.decision <- ok:
		default:
		}
		renderApprovalDoneCallback(w, ok)
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
  body { font-family: system-ui, sans-serif; max-width: 640px; margin: 4em auto; padding: 0 1em; }
  h1 { margin-bottom: 0.2em; }
  .meta { color: #555; font-size: 0.9em; margin-bottom: 1.5em; }
  .meta dt { font-weight: bold; }
  .meta dd { margin-left: 0; margin-bottom: 0.4em; font-family: ui-monospace, monospace; }
  form { display: inline; }
  button { font-size: 1.1em; padding: 0.6em 1.4em; margin-right: 0.4em; cursor: pointer; }
  .approve { background: #2c7a39; color: white; border: 1px solid #1f5c2a; }
  .deny    { background: #b03a2e; color: white; border: 1px solid #862c22; }
  .hint    { color: #777; font-size: 0.85em; margin-top: 1em; }
</style>
</head>
<body>
<h1>Agent wants permission</h1>
<dl class="meta">
  <dt>Permission</dt><dd>%s</dd>
  <dt>Caller (conv-id)</dt><dd>%s</dd>
  <dt>Caller (title)</dt><dd>%s</dd>
  <dt>Endpoint</dt><dd>%s %s</dd>
  <dt>Timeout</dt><dd>%s (auto-deny if unattended)</dd>
</dl>
<form action="/approve/%s/approve" method="post">
  <button class="approve" autofocus>Approve</button>
</form>
<form action="/approve/%s/deny" method="post">
  <button class="deny">Deny</button>
</form>
<p class="hint">This popup was opened by <code>tclaude agentd</code>
on this machine. If you didn't expect it, click Deny.</p>
</body>
</html>
`

func renderApprovalPage(w http.ResponseWriter, req *approvalRequest) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	title := req.convTitle
	if title == "" {
		title = "(unnamed)"
	}
	fmt.Fprintf(w, approvalPageTemplate,
		html.EscapeString(req.perm),
		html.EscapeString(req.convID),
		html.EscapeString(title),
		html.EscapeString(req.method),
		html.EscapeString(req.path),
		html.EscapeString(req.timeout.String()),
		html.EscapeString(req.id),
		html.EscapeString(req.id),
	)
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
