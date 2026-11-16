package agentd

import (
	"crypto/tls"
	"encoding/binary"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// startRemoteH2Listener serves a stub handler on the remote listener's real
// TLS and serve path (remoteListenerTLSConfig, serveRemoteListener): /proto
// answers the request's protocol, /ws upgrades with the terminals' upgrader
// and echoes one message.
func startRemoteH2Listener(t *testing.T) (addr string, client *tls.Config) {
	t.Helper()
	_, m := startRemoteTestServer(t) // the remote-access material in a temp home
	ln, err := tls.Listen("tcp", "127.0.0.1:0", remoteListenerTLSConfig(m))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/proto", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, r.Proto) })
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		ws, err := termWSUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		kind, msg, err := ws.ReadMessage()
		if err == nil {
			_ = ws.WriteMessage(kind, msg)
		}
	})
	srv := serveRemoteListener(ln, mux)
	t.Cleanup(func() { _ = srv.Close() })
	tr := clientWithIdentity(t, false, true).Transport.(*http.Transport)
	return ln.Addr().String(), tr.TLSClientConfig.Clone()
}

// Scenario: a browser on the remote listener gets HTTP/2 (one connection for
// a boot's hundreds of module requests), with the client certificate still
// required.
func TestRemoteListener_ServesHTTP2(t *testing.T) {
	addr, cfg := startRemoteH2Listener(t)
	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg, ForceAttemptHTTP2: true}}
	resp, err := c.Get("https://" + addr + "/proto")
	if err != nil {
		t.Fatalf("GET over h2: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.ProtoMajor != 2 || string(body) != "HTTP/2.0" {
		t.Fatalf("negotiated %s (handler saw %q), want HTTP/2.0", resp.Proto, body)
	}

	h1 := cfg.Clone()
	h1.NextProtos = []string{"http/1.1"}
	c1 := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: h1}}
	resp1, err := c1.Get("https://" + addr + "/proto")
	if err != nil {
		t.Fatalf("GET over http/1.1: %v", err)
	}
	defer resp1.Body.Close()
	if resp1.ProtoMajor != 1 {
		t.Fatalf("an http/1.1-only client got %s", resp1.Proto)
	}
}

// Scenario: a terminal's WebSocket still upgrades. A browser opens it on an
// HTTP/1.1 connection of its own (ALPN http/1.1), because the server offers
// no extended CONNECT over HTTP/2.
func TestRemoteListener_WebSocketStillUpgrades(t *testing.T) {
	addr, cfg := startRemoteH2Listener(t)
	cfg.NextProtos = []string{"http/1.1"}
	dialer := websocket.Dialer{TLSClientConfig: cfg, HandshakeTimeout: 10 * time.Second}
	ws, resp, err := dialer.Dial("wss://"+addr+"/ws", nil)
	if err != nil {
		t.Fatalf("websocket dial: %v (response %+v)", err, resp)
	}
	defer ws.Close()
	if err := ws.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, msg, err := ws.ReadMessage()
	if err != nil || string(msg) != "ping" {
		t.Fatalf("echo = %q, %v", msg, err)
	}
}

// The WebSocket path above relies on the server's HTTP/2 SETTINGS not
// offering ENABLE_CONNECT_PROTOCOL (0x8): with it, a browser would send its
// WebSockets over HTTP/2, where the terminals' upgrade cannot hijack.
func TestRemoteListener_HTTP2OffersNoExtendedConnect(t *testing.T) {
	addr, cfg := startRemoteH2Listener(t)
	cfg.NextProtos = []string{"h2"}
	conn, err := tls.Dial("tcp", addr, cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if got := conn.ConnectionState().NegotiatedProtocol; got != "h2" {
		t.Fatalf("ALPN chose %q, want h2", got)
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	// The client preface and an empty SETTINGS frame.
	if _, err := io.WriteString(conn, "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n\x00\x00\x00\x04\x00\x00\x00\x00\x00"); err != nil {
		t.Fatalf("preface: %v", err)
	}
	header := make([]byte, 9)
	if _, err := io.ReadFull(conn, header); err != nil {
		t.Fatalf("read frame header: %v", err)
	}
	length := int(header[0])<<16 | int(header[1])<<8 | int(header[2])
	if header[3] != 0x4 {
		t.Fatalf("the server's first frame is type %d, want SETTINGS", header[3])
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(conn, payload); err != nil {
		t.Fatalf("read settings: %v", err)
	}
	for i := 0; i+6 <= len(payload); i += 6 {
		if binary.BigEndian.Uint16(payload[i:]) == 0x8 {
			t.Fatalf("the server offers ENABLE_CONNECT_PROTOCOL: browsers would send WebSockets over HTTP/2")
		}
	}
}
