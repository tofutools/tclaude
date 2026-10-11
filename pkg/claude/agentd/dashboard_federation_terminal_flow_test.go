package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/federation/stream"
	"github.com/tofutools/tclaude/pkg/federation/terminal"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func dashboardRemoteTerminalCatalog(t *testing.T, fh *fedHarness, caps ...string) proto.CatalogSession {
	t.Helper()
	row := proto.CatalogSession{Agent: "agt_remote00001", Session: "runtime", Incarnation: "pinned-runtime", Name: "remote"}
	caps = append(caps, proto.CapSessions)
	fh.peer.send(fh.peer.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{Groups: []proto.CatalogGroup{{Name: "builders", Caps: caps, Sessions: []proto.CatalogSession{row}, SessionsAt: time.Now()}}}))
	fedEventually(t, "catalog received", func() bool {
		return strings.Contains(fedHuman(t, fh.f, "GET", "/v1/federation/sessions", nil).Body.String(), row.Agent)
	})
	return row
}

func dashboardTerminalOpen(t *testing.T, fh *fedHarness, mode string) (*websocket.Conn, *stream.Conn, proto.SessionOpenPayload) {
	t.Helper()
	row := dashboardRemoteTerminalCatalog(t, fh, proto.CapSessionsWatch, proto.CapSessionsAttach)
	handler := agentd.BuildDashboardHandlerForTest()
	var handlers sync.WaitGroup
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlers.Add(1)
		defer handlers.Done()
		handler.ServeHTTP(w, r)
	}))
	// Server.Close does not join hijacked WebSocket handlers. Client/stream
	// cleanups registered below run first, unblocking these handlers; join them
	// so their deferred closing audits finish before the World closes SQLite.
	t.Cleanup(func() {
		server.Close()
		handlers.Wait()
	})
	type result struct {
		ws  *websocket.Conn
		err error
	}
	ready := make(chan result, 1)
	previous := len(fh.peer.envelopes(proto.KindSessionOpen))
	go func() {
		ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/federation/terminal?peer="+fh.peer.id.ID()+"&agent="+row.Agent+"&mode="+mode, nil)
		ready <- result{ws, err}
	}()
	var env *proto.Envelope
	fedEventually(t, "dashboard session open", func() bool {
		es := fh.peer.envelopes(proto.KindSessionOpen)
		if len(es) > previous {
			env = es[len(es)-1]
			return true
		}
		return false
	})
	var open proto.SessionOpenPayload
	require.NoError(t, env.DecodePayload(&open))
	require.True(t, open.FixedSize)
	require.Equal(t, mode == "watch", open.ReadOnly)
	kp, err := stream.NewKeyPair()
	require.NoError(t, err)
	answer := fh.peer.envelope(proto.KindSessionAnswer, proto.Endpoint{}, proto.SessionAnswerPayload{Stream: open.Stream, OK: true, Key: kp.Pub, Cols: 120, Rows: 40})
	answer.InReplyTo = env.ID
	fh.peer.send(answer)
	conn := fedPeerStream(t, fh.peer, open.Stream, kp, open.Key, false)
	t.Cleanup(func() { _ = conn.Close() })
	got := <-ready
	require.NoError(t, got.err)
	t.Cleanup(func() { _ = got.ws.Close() })
	require.NoError(t, got.ws.SetReadDeadline(time.Now().Add(5*time.Second)))
	var hello struct {
		Type, Mode, Peer, Agent string
		Cols, Rows              int
	}
	require.NoError(t, got.ws.ReadJSON(&hello))
	require.Equal(t, "hello", hello.Type)
	require.Equal(t, mode, hello.Mode)
	require.Equal(t, 120, hello.Cols)
	require.Equal(t, 40, hello.Rows)
	require.Equal(t, fh.peer.id.ID(), hello.Peer)
	return got.ws, conn, open
}

func TestDashboardFederationTerminalWatchAndInteractive(t *testing.T) {
	for _, mode := range []string{"watch", "interactive"} {
		t.Run(mode, func(t *testing.T) {
			fh := newFedHarness(t)
			t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
			ws, conn, _ := dashboardTerminalOpen(t, fh, mode)
			require.NoError(t, ws.WriteJSON(map[string]any{"type": "resize", "cols": 200, "rows": 80}))
			require.NoError(t, ws.WriteMessage(websocket.BinaryMessage, []byte("typed\r")))
			if mode == "interactive" {
				frame := terminalRead(t, conn)
				require.Equal(t, terminal.Input, frame.Kind)
				require.Equal(t, "typed\r", string(frame.Data))
				require.NoError(t, terminal.Write(conn, terminal.Frame{Kind: terminal.Credit, Data: terminal.Number(len(frame.Data))}))
			}
			require.NoError(t, terminal.Write(conn, terminal.Frame{Kind: terminal.Output, Data: []byte("pane bytes")}))
			kind, body, err := ws.ReadMessage()
			require.NoError(t, err)
			require.Equal(t, websocket.BinaryMessage, kind)
			require.Equal(t, "pane bytes", string(body))
			require.NoError(t, ws.WriteJSON(map[string]any{"type": "credit", "bytes": len(body)}))
			frame := terminalRead(t, conn)
			require.Equal(t, terminal.Credit, frame.Kind, "watch input and browser resize never reach target")
			n, err := terminal.ParseNumber(frame.Data)
			require.NoError(t, err)
			require.Equal(t, len(body), n)
			require.NoError(t, terminal.Write(conn, terminal.Frame{Kind: terminal.Resize, Data: terminal.Size(90, 30)}))
			var size map[string]any
			require.NoError(t, ws.ReadJSON(&size))
			require.Equal(t, "size", size["type"])
			require.NoError(t, terminal.Write(conn, terminal.Frame{Kind: terminal.Closed, Data: []byte("kicked")}))
			var closed map[string]any
			require.NoError(t, ws.ReadJSON(&closed))
			require.Equal(t, "closed", closed["type"])
			require.Equal(t, "kicked", closed["reason"])
		})
	}
}

func TestDashboardFederationTerminalRefusalAndAuthority(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	row := dashboardRemoteTerminalCatalog(t, fh, proto.CapSessionsWatch)
	server := httptest.NewServer(agentd.BuildDashboardHandlerForTest())
	t.Cleanup(server.Close)
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/federation/terminal?peer="+fh.peer.id.ID()+"&agent="+row.Agent+"&mode=interactive", nil)
	require.NoError(t, err)
	defer ws.Close()
	require.NoError(t, ws.SetReadDeadline(time.Now().Add(5*time.Second)))
	var closed map[string]any
	require.NoError(t, ws.ReadJSON(&closed))
	require.Equal(t, "denied", closed["reason"])
	require.Empty(t, fh.peer.envelopes(proto.KindSessionOpen))
	for _, path := range []string{"/api/federation/terminal?mode=watch", "/api/federation/sessions"} {
		rec := testharness.Serve(agentd.PeerViewHandler(fh.peer.id.ID()), testharness.JSONRequest(t, "GET", path, nil))
		require.Equal(t, 403, rec.Code)
		mux := http.NewServeMux()
		agentd.RegisterDashboardRoutesForTest(mux)
		rec = testharness.Serve(mux, testharness.JSONRequest(t, "GET", path, nil))
		require.Equal(t, 403, rec.Code)
	}
	rec := testharness.Serve(agentd.BuildDashboardHandlerForTest(), testharness.JSONRequest(t, "GET", "/api/federation/sessions?peer="+fh.peer.id.ID(), nil))
	require.Equal(t, 200, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	require.Equal(t, true, rows[0]["watch"])
	require.Equal(t, false, rows[0]["attach"])
	require.Equal(t, fh.peer.id.ID(), rows[0]["instance"])
}

func TestDashboardFederationTerminalRevocationAndIncarnation(t *testing.T) {
	for _, change := range []string{"revoked", "reincarnated"} {
		t.Run(change, func(t *testing.T) {
			fh := newFedHarness(t)
			t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
			ws, _, _ := dashboardTerminalOpen(t, fh, "watch")
			row := proto.CatalogSession{Agent: "agt_remote00001", Session: "runtime", Incarnation: "pinned-runtime", Name: "remote"}
			caps := []string{proto.CapSessions}
			if change == "reincarnated" {
				row.Incarnation = "replacement-runtime"
				caps = append(caps, proto.CapSessionsWatch)
			}
			fh.peer.send(fh.peer.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{Groups: []proto.CatalogGroup{{Name: "builders", Caps: caps, Sessions: []proto.CatalogSession{row}, SessionsAt: time.Now()}}}))
			var closed map[string]any
			require.NoError(t, ws.ReadJSON(&closed))
			require.Equal(t, change, closed["reason"])
		})
	}
}

func TestFederationTerminalPinnedBrowserSizeAndClosedReasons(t *testing.T) {
	for _, change := range []string{"kicked", "revoked", "reincarnated"} {
		t.Run(change, func(t *testing.T) {
			fh := newFedHarness(t)
			f, p := fh.f, fh.peer
			const conv = "browser-pinned-target"
			f.HaveGroup("team")
			f.HaveConvWithTitle(conv, "pinned")
			f.HaveMember("team", conv)
			f.HaveAliveSession(conv, "browser-pinned-runtime", "tclaude-browser-pinned", f.TestCwd("work"))
			aid, err := db.AgentIDForConv(conv)
			require.NoError(t, err)
			original := clcommon.Default
			mock := &terminalTmux{Tmux: original, options: map[string]string{}, pane: "%1", windows: "1", version: "tmux 3.4"}
			clcommon.Default = mock
			t.Cleanup(func() { agentd.ResetFederationForTest(); clcommon.Default = original })
			incarnation := terminalCatalogIncarnation(t, fh, aid)
			grant := fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermSessionsWatch, "scope": "group=team"})
			require.Equal(t, 200, grant.Code)
			kp, err := stream.NewKeyPair()
			require.NoError(t, err)
			open := proto.SessionOpenPayload{Agent: aid, Session: "browser-pinned-runtime", Incarnation: incarnation, Group: "team", Stream: proto.NewEnvelopeID(), Key: kp.Pub, ReadOnly: true, FixedSize: true, Cols: 80, Rows: 24}
			p.send(p.envelope(proto.KindSessionOpen, proto.Endpoint{}, open))
			answer := terminalAnswer(t, p, open.Stream)
			require.True(t, answer.OK, answer.Reason)
			require.Equal(t, 120, answer.Cols)
			require.Equal(t, 40, answer.Rows)
			conn := fedPeerStream(t, p, open.Stream, kp, answer.Key, true)
			t.Cleanup(func() { _ = conn.Close() })
			require.Equal(t, terminal.Output, terminalRead(t, conn).Kind)
			switch change {
			case "kicked":
				rec := fedHuman(t, f, "POST", "/v1/federation/viewers/"+open.Stream+"/kick", nil)
				require.Equal(t, 200, rec.Code)
			case "revoked":
				rec := fedHuman(t, f, "DELETE", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermSessionsWatch, "scope": "group=team"})
				require.Equal(t, 200, rec.Code)
			case "reincarnated":
				mock.mu.Lock()
				mock.pane = "%2"
				mock.mu.Unlock()
			}
			closed := terminalRead(t, conn)
			require.Equal(t, terminal.Closed, closed.Kind)
			require.Equal(t, change, string(closed.Data))
			fedEventually(t, "incoming viewer released", func() bool {
				return !strings.Contains(fedHuman(t, f, "GET", "/v1/federation/viewers", nil).Body.String(), open.Stream)
			})
		})
	}
}

func TestDashboardFederationTerminalViewerLimit(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	for range 8 {
		dashboardTerminalOpen(t, fh, "watch")
	}
	server := httptest.NewServer(agentd.BuildDashboardHandlerForTest())
	t.Cleanup(server.Close)
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/federation/terminal?peer="+fh.peer.id.ID()+"&agent=agt_remote00001&mode=watch", nil)
	require.NoError(t, err)
	defer ws.Close()
	require.NoError(t, ws.SetReadDeadline(time.Now().Add(5*time.Second)))
	var closed map[string]any
	require.NoError(t, ws.ReadJSON(&closed))
	require.Equal(t, "limit", closed["reason"])
	require.Len(t, fh.peer.envelopes(proto.KindSessionOpen), 8)
}

func TestDashboardFederationTerminalRejectsForgedCredit(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	ws, _, _ := dashboardTerminalOpen(t, fh, "watch")
	require.NoError(t, ws.WriteJSON(map[string]any{"type": "credit", "bytes": 1}))
	var closed map[string]any
	require.NoError(t, ws.ReadJSON(&closed))
	require.Equal(t, "error", closed["reason"])
}
