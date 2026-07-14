package agentd

import (
	"fmt"
	"html"
	"net/http"
)

// dashboardLoginPageTemplate is the in-place sign-in page the dashboard
// serves whenever a GET / arrives without a usable session — no cookie,
// a stale cookie (the common case after an agentd restart mints a fresh
// session token), or an expired init-token link. It replaces the old
// dead-end plain-text 403.
//
// It offers two ways back in:
//
//   - The zero-friction, no-secret path: run `tclaude agent dashboard`
//     (or click the tray's "Open dashboard"), which mints a fresh
//     authenticated URL over the human-only channel. This is the
//     primary path and the only one that works when the daemon was
//     started backgrounded (no banner ⇒ no operator token to paste).
//   - A self-service field: paste the operator token (printed on the
//     agentd startup banner) and POST it to /dashboard/login. Useful
//     from a remote/forwarded browser where dropping to the CLI is
//     awkward. See handleDashboardLogin for the security rationale.
//
// The two %s placeholders are the optional error banner and the form action's
// HTML-escaped query suffix. Everything else is static.
const dashboardLoginPageTemplate = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>tclaude dashboard — sign in</title>
<style>
  body { font-family: system-ui, sans-serif; max-width: 560px; margin: 4em auto; padding: 0 1.2em; color: #222; }
  h1 { font-size: 1.4em; margin-bottom: 0.3em; }
  p { line-height: 1.5; color: #444; }
  code, .cmd { font-family: ui-monospace, monospace; }
  .cmd {
    display: block; background: #f4f4f4; border: 1px solid #ddd; border-radius: 6px;
    padding: 0.7em 1em; margin: 0.6em 0 0.2em; font-size: 0.95em; user-select: all;
    word-break: break-all;
  }
  .err {
    background: #fdecea; border: 1px solid #f5c2bd; color: #8a1c10;
    border-radius: 6px; padding: 0.7em 1em; margin: 0 0 1.2em; font-size: 0.95em;
  }
  .divider { display: flex; align-items: center; color: #999; font-size: 0.85em; margin: 2em 0 1em; }
  .divider::before, .divider::after { content: ""; flex: 1; border-top: 1px solid #e2e2e2; }
  .divider span { padding: 0 0.8em; }
  form { display: flex; gap: 0.5em; }
  input[type=password] {
    flex: 1; font-family: ui-monospace, monospace; font-size: 1em;
    padding: 0.55em 0.7em; border: 1px solid #bbb; border-radius: 6px;
  }
  button {
    font-size: 1em; padding: 0.55em 1.2em; cursor: pointer;
    background: #2c5fb3; color: #fff; border: 1px solid #214a8c; border-radius: 6px;
  }
  .hint { color: #777; font-size: 0.85em; margin-top: 0.6em; }
</style>
</head>
<body>
<h1>Dashboard sign-in needed</h1>
%s<p>This browser has no valid dashboard session. If you were here a moment ago,
the most likely cause is that <code>tclaude agentd</code> was restarted, which
issues a fresh session and signs out old tabs.</p>

<p><strong>Easiest:</strong> run this in a terminal on this machine —</p>
<code class="cmd">tclaude agent dashboard</code>
<p class="hint">…or click <strong>Open dashboard</strong> on the agentd tray icon.
Either way a freshly authenticated tab opens.</p>

<div class="divider"><span>or sign in here</span></div>

<p>Paste your <strong>operator token</strong> — the <code>tclo_…</code> value
printed on the <code>tclaude agentd</code> startup banner:</p>
<form action="/dashboard/login%s" method="post" autocomplete="off">
  <input type="password" name="token" placeholder="tclo_…" autofocus
         autocapitalize="off" autocorrect="off" spellcheck="false" aria-label="operator token">
  <button type="submit">Sign in</button>
</form>
<p class="hint">No operator token? It's only shown when agentd runs attached to a
terminal — use the <code>tclaude agent dashboard</code> command above instead.</p>
</body>
</html>
`

// renderDashboardLoginPage writes the sign-in page with the given HTTP
// status. errMsg, when non-empty, is HTML-escaped and shown in a banner
// above the explanation; pass "" for the plain (first-visit) page.
//
// The form carries a validated same-origin return target so deep app locations
// and standalone terminal popouts resume after re-authentication.
func renderDashboardLoginPage(w http.ResponseWriter, r *http.Request, status int, errMsg string) {
	banner := ""
	if errMsg != "" {
		banner = `<p class="err">` + html.EscapeString(errMsg) + "</p>\n"
	}
	formQuery := html.EscapeString(dashboardLoginFormQuery(r))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, dashboardLoginPageTemplate, banner, formQuery)
}
