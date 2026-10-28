package agentd

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/routeadapter"
)

// HTTP previews still traverse the authorized TCP adapter. The proxy never
// dials the publisher target in the host namespace or follows HTTP redirects.
func serveHostRouteHTTP(ctx context.Context, listener net.Listener, dial func(context.Context, string, string) (net.Conn, error), target string) error {
	host, err := routeadapter.ValidatePublisherTarget(target)
	if err != nil {
		return err
	}
	transport := &http.Transport{
		DialContext: dial, MaxIdleConnsPerHost: 8, IdleConnTimeout: 30 * time.Second,
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme = "http"
			r.Out.URL.Host = host
			r.Out.Host = host
			r.Out.URL.RawQuery = r.In.URL.RawQuery
		},
		Transport:     transport,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			status := http.StatusBadGateway
			if errors.Is(err, routeadapter.ErrHostStreamLimit) {
				status = http.StatusServiceUnavailable
			}
			http.Error(w, http.StatusText(status), status)
		},
	}
	server := &http.Server{
		Handler: proxy, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	context.AfterFunc(ctx, func() {
		_ = server.Close()
		transport.CloseIdleConnections()
	})
	go func() { _ = server.Serve(listener) }()
	return nil
}
