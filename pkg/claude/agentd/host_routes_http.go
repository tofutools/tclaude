package agentd

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"golang.org/x/net/netutil"

	"github.com/tofutools/tclaude/pkg/claude/routeadapter"
)

// HTTP previews still traverse the authorized TCP adapter. The proxy never
// dials the publisher target in the host namespace or follows HTTP redirects.
func serveHostRouteHTTP(ctx context.Context, listener net.Listener, dial func(context.Context, string, string) (net.Conn, error), target, leaseID string) error {
	host, err := routeadapter.ValidatePublisherTarget(target)
	if err != nil {
		return err
	}
	transport := &http.Transport{
		DialContext: dial, MaxIdleConnsPerHost: 8, IdleConnTimeout: 30 * time.Second,
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			// Cookies are not scoped by port. Even manually opened loopback
			// URLs must never forward the operator's dashboard credential.
			cookies := r.Out.Cookies()
			r.Out.Header.Del("Cookie")
			for _, cookie := range cookies {
				if !hostPreviewDashboardCookie(cookie.Name) {
					r.Out.AddCookie(cookie)
				}
			}
			r.Out.URL.Scheme = "http"
			r.Out.URL.Host = host
			r.Out.Host = host
			// Rewrite sanitizes query parameters before this hook. Preserve the
			// original query: preview authorization never depends on its values.
			r.Out.URL.RawQuery = r.In.URL.RawQuery
		},
		Transport: transport,
		ModifyResponse: func(r *http.Response) error {
			cookies := r.Header.Values("Set-Cookie")
			r.Header.Del("Set-Cookie")
			for _, cookie := range cookies {
				name, _, _ := strings.Cut(cookie, "=")
				if !hostPreviewDashboardCookie(strings.TrimSpace(name)) {
					r.Header.Add("Set-Cookie", cookie)
				}
			}
			return nil
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			status := http.StatusBadGateway
			if errors.Is(err, routeadapter.ErrHostStreamLimit) {
				status = http.StatusServiceUnavailable
				w.Header().Set("Retry-After", "1")
			}
			http.Error(w, http.StatusText(status), status)
		},
	}
	endpointHost, endpointPort, _ := net.SplitHostPort(listener.Addr().String())
	previewHost := "route-" + strings.TrimPrefix(leaseID, hostRouteLeasePrefix) + ".localhost"
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hostname, port, err := net.SplitHostPort(r.Host)
		if err != nil && endpointPort == "80" && !strings.Contains(r.Host, ":") {
			hostname, port, err = r.Host, "80", nil
		}
		hostname = strings.ToLower(strings.TrimSuffix(hostname, "."))
		if err != nil || port != endpointPort || (hostname != endpointHost && hostname != "localhost" && hostname != previewHost) {
			http.Error(w, "invalid host", http.StatusMisdirectedRequest)
			return
		}
		proxy.ServeHTTP(w, r)
	})
	server := &http.Server{
		Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	context.AfterFunc(ctx, func() {
		_ = server.Close()
		transport.CloseIdleConnections()
	})
	go func() { _ = server.Serve(netutil.LimitListener(listener, 32)) }()
	return nil
}

// All installations on this browser host share cookie visibility across ports.
func hostPreviewDashboardCookie(name string) bool {
	return name == legacyDashboardCookieName || strings.HasPrefix(name, dashboardCookieNamePrefix)
}
