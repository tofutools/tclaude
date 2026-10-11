package agentd_test

import (
	"encoding/json"
	"net"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/federation/hub"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// Both daemons run the real transport and durable outbox. Only the reconnect
// cap is shrunk (60s -> 20ms); the initial 1s retry and production outbox tick
// remain real. No flush helper or daemon restart is used to deliver the mail.
func TestFederation_ReconnectStartupAndHubOutageDrainsOutbox(t *testing.T) {
	st, err := hub.OpenStore(filepath.Join(testutil.CanonicalTempDir(t), "hub.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	var runningHub *hub.Hub
	var server *httptest.Server
	var activeListener *outageHubListener
	stop := func() {
		if runningHub != nil {
			// Abruptly sever hijacked WebSockets before graceful Close can send a
			// shutting_down refusal (which intentionally backs off for 30s).
			_ = activeListener.Close()
			activeListener.closeConnections()
			runningHub.Close()
			server.Close()
			runningHub = nil
			server = nil
		}
	}
	t.Cleanup(stop)
	start := func() {
		t.Helper()
		var err error
		runningHub, err = hub.New(st, hub.Config{})
		require.NoError(t, err)
		listener, err := net.Listen("tcp", address)
		require.NoError(t, err)
		server = httptest.NewUnstartedServer(runningHub.Handler())
		require.NoError(t, server.Listener.Close())
		activeListener = &outageHubListener{Listener: listener}
		server.Listener = activeListener
		server.Start()
	}
	a := startHistoryNode(t, "claude", "A", "TCLAUDE_TEST_FAST_RECONNECT=1")
	b := startHistoryNode(t, "claude", "B", "TCLAUDE_TEST_FAST_RECONNECT=1")
	nodes := []historyNode{a, b}
	for _, node := range nodes {
		require.NoError(t, st.Admit(node.Instance))
		code, raw := historyNodeRequest(t, node, "POST", "/v1/federation/config", "", map[string]any{"enabled": true, "hub_url": "ws://" + address, "name": "reconnect"})
		require.Equal(t, 200, code, string(raw))
	}
	status := func(node historyNode) fedStatusView {
		code, raw := historyNodeRequest(t, node, "GET", "/v1/federation/status", "", nil)
		require.Equal(t, 200, code, string(raw))
		var result fedStatusView
		require.NoError(t, json.Unmarshal(raw, &result))
		return result
	}
	for _, node := range nodes {
		fedEventually(t, "startup hub unreachable", func() bool { s := status(node); return s.Hub != nil && s.Hub.State == "disconnected" })
	}
	start()
	connected := func() {
		t.Helper()
		for i, node := range nodes {
			fedEventually(t, "node reconnects and sees other node", func() bool {
				s := status(node)
				if s.Hub == nil || s.Hub.State != "connected" {
					return false
				}
				for _, p := range s.Peers {
					if p.InstanceID == nodes[1-i].Instance && p.Online {
						return true
					}
				}
				return false
			})
		}
	}
	connected()
	for i, node := range nodes {
		peer := nodes[1-i]
		code, raw := historyNodeRequest(t, node, "POST", "/v1/federation/peers/trust", "", map[string]any{"instance": peer.Instance, "level": "unrestricted", "confirm_fingerprint": peer.Fingerprint, "label": "other"})
		require.Equal(t, 200, code, string(raw))
	}
	stop()
	for _, node := range nodes {
		fedEventually(t, "hub outage visible", func() bool { s := status(node); return s.Hub != nil && s.Hub.State == "disconnected" })
	}
	ids := map[string]string{}
	for _, node := range nodes {
		code, raw := historyNodeRequest(t, node, "POST", "/v1/federation/notify", "", map[string]any{"peer": "other", "subject": "outage", "body": "queued during hub outage"})
		require.Equal(t, 200, code, string(raw))
		var result struct {
			ID    string `json:"envelope_id"`
			State string `json:"state"`
		}
		require.NoError(t, json.Unmarshal(raw, &result))
		require.NotEmpty(t, result.ID)
		require.Equal(t, "queued", result.State)
		ids[node.Instance] = result.ID
	}
	// Startup failure has already reached the 20ms retry cap. Keep the hub
	// down for multiple capped retries, rather than sleeping for a real 60s.
	time.Sleep(150 * time.Millisecond)
	for _, node := range nodes {
		code, raw := historyNodeRequest(t, node, "GET", "/v1/federation/outbox", "", nil)
		require.Equal(t, 200, code, string(raw))
		var rows []struct {
			ID    string `json:"envelope_id"`
			State string `json:"state"`
		}
		require.NoError(t, json.Unmarshal(raw, &rows))
		require.Len(t, rows, 1)
		require.Equal(t, ids[node.Instance], rows[0].ID)
		require.Equal(t, "queued", rows[0].State)
	}
	start()
	connected()
	for _, node := range nodes {
		fedEventually(t, "queued outbox acknowledged", func() bool {
			code, raw := historyNodeRequest(t, node, "GET", "/v1/federation/outbox", "", nil)
			require.Equal(t, 200, code, string(raw))
			var rows []struct {
				ID    string `json:"envelope_id"`
				State string `json:"state"`
			}
			require.NoError(t, json.Unmarshal(raw, &rows))
			return len(rows) == 1 && rows[0].ID == ids[node.Instance] && rows[0].State == "accepted"
		})
	}
	for _, node := range nodes {
		code, raw := historyNodeRequest(t, node, "GET", "/v1/federation/inbox", "", nil)
		require.Equal(t, 200, code, string(raw))
		var inbox []fedInboxRow
		require.NoError(t, json.Unmarshal(raw, &inbox))
		require.Len(t, inbox, 1)
		require.Equal(t, "outage", inbox[0].Subject)
		require.Contains(t, inbox[0].Body, "queued during hub outage")
	}
	connected()
}

// httptest.CloseClientConnections excludes hijacked WebSockets. Track the real
// accepted sockets to simulate a crash, not the hub's graceful refusal path.
type outageHubListener struct {
	net.Listener
	mu          sync.Mutex
	connections []net.Conn
	closed      bool
}

func (l *outageHubListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.mu.Lock()
		if l.closed {
			_ = conn.Close()
		} else {
			l.connections = append(l.connections, conn)
		}
		l.mu.Unlock()
	}
	return conn, err
}
func (l *outageHubListener) closeConnections() {
	l.mu.Lock()
	l.closed = true
	connections := append([]net.Conn(nil), l.connections...)
	l.mu.Unlock()
	for _, conn := range connections {
		_ = conn.Close()
	}
}
