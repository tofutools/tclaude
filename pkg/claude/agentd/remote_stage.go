package agentd

import (
	"net"
	"net/http"
	"path"
	"strings"
)

// The remote stage (tcl-balzk0): live pages for a browser that reaches the
// dashboard over remote access (remote_server.go), such as a phone. A page's
// loopback origin, f-<token>.ui.localhost, means nothing to another machine,
// so the remote listener serves each open page under a path of its own,
// /p/<remote token>/, with no new port or setting.
//
// The path is served ahead of the passphrase session: a page's frame is
// credentialless and sandboxed, so it sends no cookie. It is still behind the
// listener's client certificate (RequireAndVerifyClientCert at the TLS
// layer): without one, nothing reaches a handler. Every rule of the loopback
// stage applies (serveStagePage): cookies and credentials stripped, the
// stage's own CSP, documents only into a frame, no route to the dashboard.
//
// Pages share the dashboard's origin here, so isolation rests on:
//
//   - CSP path matching: a page's sources name only its own path, never
//     'self' (which would be the dashboard), so page A can neither load nor
//     fetch page B's documents, nor the dashboard's /api.
//   - Redirects held to the page's path (stageKeepRedirectInPrefix), as a
//     browser stops applying a source's path after a redirect.
//   - The dashboard's own checks on a request from a page: its session
//     cookie is SameSite=Strict and not sent from the opaque-origin frame,
//     and remoteSameOrigin refuses the frame's Origin: null.
//   - A request carrying the dashboard's origin is refused by the stage.
//
// A page the operator let load other hosts is not served here: its wider
// sources would reach every other page on this origin.
//
// A dev server emits absolute paths, so it must be started with --base
// /p/<remote token>/ (`tclaude agent show` prints it) and shown at that path;
// its requests keep the prefix. A static source's requests lose it.

const remoteStagePrefix = "/p/"

// remoteStagePath splits a remote listener path into a page's remote token
// and the page path: /p/<token> or /p/<token>/rest.
func remoteStagePath(p string) (token, rest string, ok bool) {
	after, ok := strings.CutPrefix(p, remoteStagePrefix)
	if !ok {
		return "", "", false
	}
	token, rest, _ = strings.Cut(after, "/")
	return token, "/" + rest, true
}

// remoteStageRoute serves /p/ on the remote listener and hands everything
// else to the dashboard.
func remoteStageRoute(dashboard http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, rest, ok := remoteStagePath(r.URL.Path)
		if !ok {
			dashboard.ServeHTTP(w, r)
			return
		}
		serveRemoteStage(w, r, token, rest, true)
	})
}

// serveRemoteStage serves a page by its remote path. certified says the
// listener demands the remote-access client certificate (the :8443
// listener); on the dashboard listener the remote token is the only key.
func serveRemoteStage(w http.ResponseWriter, r *http.Request, token, rest string, certified bool) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", stageRefusalCSP)
	if certified && (r.TLS == nil || len(r.TLS.PeerCertificates) == 0) {
		stageNotFound(w)
		return
	}
	// A path that leaves itself (dot segments) is not served: the prefix
	// would no longer say which page it is.
	if c := path.Clean(rest); c != rest && c+"/" != rest {
		stageNotFound(w)
		return
	}
	page := stagePages.lookupRemote(token)
	host, ok := remoteStageHostport(r.Host)
	if page == nil || !ok || page.otherHostsAllowed() {
		stageNotFound(w)
		return
	}
	serveStagePage(w, r, page, stageSite{
		host:   host,
		prefix: remoteStagePrefix + token,
		strip:  page.static.Load(),
	})
}

// remoteStageHostport is a request's Host when it is one a browser could
// have used for this listener: a DNS name or an IP literal, and a port.
func remoteStageHostport(hostport string) (string, bool) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		host, port = hostport, ""
	}
	host = strings.ToLower(host)
	if host == "" || len(host) > 253 {
		return "", false
	}
	for i := 0; i < len(port); i++ {
		if port[i] < '0' || port[i] > '9' || len(port) > 5 {
			return "", false
		}
	}
	if ip := net.ParseIP(host); ip != nil {
		// CSP has no IPv6-literal host source (Chrome rejects one), so a
		// page cannot be framed for a viewer at a bare IPv6 address.
		if ip.To4() == nil {
			return "", false
		}
		host = ip.String()
	} else {
		for i := 0; i < len(host); i++ {
			c := host[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '.' {
				return "", false
			}
		}
	}
	if port == "" {
		return host, true
	}
	return host + ":" + port, true
}

// remotePageURL is a page's frame address for a remote dashboard: a path,
// /p/<remote token> and the page's path (unless that already starts with it:
// a dev server shown at its --base), so the viewer's browser supplies its own
// scheme and host. "" for a page the remote stage does not serve.
func remotePageURL(p *livePage, hostport string) string {
	if _, ok := remoteStageHostport(hostport); !ok || p.stage == nil || p.stage.otherHostsAllowed() {
		return ""
	}
	prefix := remoteStagePrefix + p.stage.remoteToken
	pth := p.path
	if pth != prefix && !strings.HasPrefix(pth, prefix+"/") {
		pth = prefix + pth
	}
	return pth
}

// dashboardViewerIsRemote says whether a dashboard request comes from a
// browser that cannot resolve *.ui.localhost to this machine: one on the
// remote-access listener, or one that reached the dashboard listener by any
// name but a loopback one (a LAN address, a VPN name, a proxy's host). The
// choice is by Host, which only shapes the asker's own view.
func dashboardViewerIsRemote(r *http.Request) bool {
	if _, remote := r.Context().Value(remoteAuthedCtxKey{}).(remoteAuthSession); remote {
		return true
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(strings.TrimSuffix(strings.ToLower(host), "."), "[]")
	if host == "localhost" {
		return false
	}
	ip := net.ParseIP(host)
	return ip == nil || !ip.IsLoopback()
}
