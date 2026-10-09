package agent

import (
	"context"
	"io"
	"net/http"

	"github.com/gorilla/websocket"
)

// DialDaemonWebSocket uses the same Unix socket and caller identity as daemon
// HTTP requests. It never falls back to a TCP/browser endpoint.
func DialDaemonWebSocket(ctx context.Context, path string) (*websocket.Conn, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://agentd"+path, nil)
	if err != nil {
		return nil, err
	}
	attachCallerIdentity(req)
	dial := websocket.Dialer{NetDialContext: httpClient().Transport.(*http.Transport).DialContext}
	conn, response, err := dial.DialContext(ctx, "ws://agentd"+path, req.Header)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil && response != nil {
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 8192))
		return nil, decodeDaemonError(response.StatusCode, raw)
	}
	return conn, err
}
