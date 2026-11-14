package agentd_test

import (
	"encoding/json"
	"fmt"
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
	ws, conn, open, _ := dashboardTerminalOpenWith(t, fh, mode, "", proto.SessionAnswerPayload{})
	return ws, conn, open
}

type dashboardTerminalHello struct {
	Type, Mode, Peer, Agent string
	Cols, Rows              int
	Shared                  bool
	Others                  []string
	TakeOver                bool `json:"take_over"`
}

// dashboardTerminalOpenWith opens a browser terminal with extraQuery appended,
// the peer's catalog granting extraCaps beyond watch and attach, and the peer
// answering with answer's sharing fields.
func dashboardTerminalOpenWith(t *testing.T, fh *fedHarness, mode, extraQuery string, answerWith proto.SessionAnswerPayload, extraCaps ...string) (*websocket.Conn, *stream.Conn, proto.SessionOpenPayload, dashboardTerminalHello) {
	t.Helper()
	row := dashboardRemoteTerminalCatalog(t, fh, append([]string{proto.CapSessionsWatch, proto.CapSessionsAttach}, extraCaps...)...)
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
		ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/federation/terminal?peer="+fh.peer.id.ID()+"&agent="+row.Agent+"&mode="+mode+extraQuery, nil)
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
	require.True(t, open.ShareSize, "a browser view always asks to share the size")
	answerWith.Stream, answerWith.OK, answerWith.Key, answerWith.Cols, answerWith.Rows = open.Stream, true, kp.Pub, 120, 40
	answer := fh.peer.envelope(proto.KindSessionAnswer, proto.Endpoint{}, answerWith)
	answer.InReplyTo = env.ID
	fh.peer.send(answer)
	conn := fedPeerStream(t, fh.peer, open.Stream, kp, open.Key, false)
	t.Cleanup(func() { _ = conn.Close() })
	got := <-ready
	require.NoError(t, got.err)
	t.Cleanup(func() { _ = got.ws.Close() })
	require.NoError(t, got.ws.SetReadDeadline(time.Now().Add(5*time.Second)))
	var hello dashboardTerminalHello
	require.NoError(t, got.ws.ReadJSON(&hello))
	require.Equal(t, "hello", hello.Type)
	require.Equal(t, mode, hello.Mode)
	require.Equal(t, 120, hello.Cols)
	require.Equal(t, 40, hello.Rows)
	require.Equal(t, fh.peer.id.ID(), hello.Peer)
	return got.ws, conn, open, hello
}

// TestDashboardFederationTerminalSharedSizeAndTakeOver: a view sharing the
// size forwards the browser's grid as Resize frames and hears the shared size
// and the other clients back; a take-over reopen says so to the peer, and only
// when the peer's catalog grants it.
func TestDashboardFederationTerminalSharedSizeAndTakeOver(t *testing.T) {
	t.Run("shared", func(t *testing.T) {
		fh := newFedHarness(t)
		t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
		ws, conn, open, hello := dashboardTerminalOpenWith(t, fh, "watch", "", proto.SessionAnswerPayload{SharedSize: true, Others: []string{"a local terminal", "\x1b[2Jsneaky"}})
		require.False(t, open.TakeOver)
		require.True(t, hello.Shared)
		require.False(t, hello.TakeOver, "without sessions_takeover the browser offers no take-over")
		require.Equal(t, []string{"a local terminal", "__2Jsneaky"}, hello.Others, "peer labels arrive in the safe-name charset")
		require.NoError(t, ws.WriteJSON(map[string]any{"type": "resize", "cols": 190, "rows": 52}))
		frame := terminalRead(t, conn)
		require.Equal(t, terminal.Resize, frame.Kind, "the browser's grid reaches the peer")
		c, r, err := terminal.ParseSize(frame.Data)
		require.NoError(t, err)
		require.Equal(t, [2]int{190, 52}, [2]int{c, r})
		require.NoError(t, ws.WriteJSON(map[string]any{"type": "resize", "cols": 5000, "rows": 52}))
		require.NoError(t, terminal.Write(conn, terminal.Frame{Kind: terminal.Output, Data: []byte("x")}))
		_, body, err := ws.ReadMessage()
		require.NoError(t, err)
		require.NoError(t, ws.WriteJSON(map[string]any{"type": "credit", "bytes": len(body)}))
		require.Equal(t, terminal.Credit, terminalRead(t, conn).Kind, "an out-of-range grid is dropped, not forwarded")

		require.NoError(t, terminal.Write(conn, terminal.Frame{Kind: terminal.Resize, Data: terminal.Size(150, 50)}))
		var size map[string]any
		require.NoError(t, ws.ReadJSON(&size))
		require.Equal(t, map[string]any{"type": "size", "cols": 150.0, "rows": 50.0}, size)
		status, err := json.Marshal(proto.SessionStatus{Others: []string{"another remote viewer"}})
		require.NoError(t, err)
		require.NoError(t, terminal.Write(conn, terminal.Frame{Kind: terminal.Status, Data: status}))
		var others map[string]any
		require.NoError(t, ws.ReadJSON(&others))
		require.Equal(t, map[string]any{"type": "others", "others": []any{"another remote viewer"}}, others)

		require.NoError(t, terminal.Write(conn, terminal.Frame{Kind: terminal.Closed, Data: []byte("taken_over")}))
		var closed map[string]any
		require.NoError(t, ws.ReadJSON(&closed))
		require.Equal(t, "taken_over", closed["reason"])
	})
	t.Run("take over with the grant", func(t *testing.T) {
		fh := newFedHarness(t)
		t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
		_, _, open, hello := dashboardTerminalOpenWith(t, fh, "interactive", "&take_over=1", proto.SessionAnswerPayload{SharedSize: true}, proto.CapSessionsTakeover)
		require.True(t, open.TakeOver, "a take-over reopen asks the peer to disconnect the others")
		require.True(t, hello.TakeOver)
	})
	t.Run("take over without the grant", func(t *testing.T) {
		fh := newFedHarness(t)
		t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
		row := dashboardRemoteTerminalCatalog(t, fh, proto.CapSessionsWatch, proto.CapSessionsAttach)
		server := httptest.NewServer(agentd.BuildDashboardHandlerForTest())
		t.Cleanup(server.Close)
		ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/federation/terminal?peer="+fh.peer.id.ID()+"&agent="+row.Agent+"&mode=interactive&take_over=1", nil)
		require.NoError(t, err)
		defer ws.Close()
		require.NoError(t, ws.SetReadDeadline(time.Now().Add(5*time.Second)))
		var closed map[string]any
		require.NoError(t, ws.ReadJSON(&closed))
		require.Equal(t, "denied", closed["reason"])
		require.Contains(t, closed["message"], "sessions.takeover")
		require.Empty(t, fh.peer.envelopes(proto.KindSessionOpen), "a refused take-over never reaches the peer")
	})
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
	// relaunched: the pane went away for a relaunch of the agent, which the
	// viewer follows (reincarnated); ended: it went away with no launch.
	for _, change := range []string{"kicked", "revoked", "reincarnated", "relaunched", "ended"} {
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
			case "relaunched":
				// The launch rewrites the row before its new pane is up.
				row, err := db.LoadSession("browser-pinned-runtime")
				require.NoError(t, err)
				row.CreatedAt = time.Now().Add(time.Second)
				require.NoError(t, db.SaveSession(row))
				f.MarkOffline("tclaude-browser-pinned")
			case "ended":
				f.MarkOffline("tclaude-browser-pinned")
			}
			want := map[string]string{"relaunched": "reincarnated", "ended": "exit"}[change]
			if want == "" {
				want = change
			}
			closed := terminalRead(t, conn)
			require.Equal(t, terminal.Closed, closed.Kind)
			require.Equal(t, want, string(closed.Data))
			fedEventually(t, "incoming viewer released", func() bool {
				return !strings.Contains(fedHuman(t, f, "GET", "/v1/federation/viewers", nil).Body.String(), open.Stream)
			})
		})
	}
}

// TestFederationTerminalSharedViewerSizesWindow: a viewer sharing the size of
// a session nobody else is attached to opens at the pinned size, and its
// Resize frames size the target window (less tmux's status line) and come
// back as the shared size. A take-over needs its own grant.
func TestFederationTerminalSharedViewerSizesWindow(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	const conv = "browser-owner-target"
	f.HaveGroup("team")
	f.HaveConvWithTitle(conv, "owner")
	f.HaveMember("team", conv)
	f.HaveAliveSession(conv, "browser-owner-runtime", "tclaude-browser-owner", f.TestCwd("work"))
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
	refused := proto.SessionOpenPayload{Agent: aid, Session: "browser-owner-runtime", Incarnation: incarnation, Group: "team", Stream: proto.NewEnvelopeID(), Key: kp.Pub, ReadOnly: true, FixedSize: true, ShareSize: true, TakeOver: true, Cols: 80, Rows: 24}
	p.send(p.envelope(proto.KindSessionOpen, proto.Endpoint{}, refused))
	answer := terminalAnswer(t, p, refused.Stream)
	require.False(t, answer.OK, "a watch grant alone does not allow a take-over")
	require.Contains(t, answer.Reason, agentd.PermSessionsTakeover)

	open := proto.SessionOpenPayload{Agent: aid, Session: "browser-owner-runtime", Incarnation: incarnation, Group: "team", Stream: proto.NewEnvelopeID(), Key: kp.Pub, ReadOnly: true, FixedSize: true, ShareSize: true, Cols: 80, Rows: 24}
	p.send(p.envelope(proto.KindSessionOpen, proto.Endpoint{}, open))
	answer = terminalAnswer(t, p, open.Stream)
	require.True(t, answer.OK, answer.Reason)
	require.True(t, answer.SharedSize)
	require.Empty(t, answer.Others, "nobody else is attached")
	require.Equal(t, [2]int{120, 40}, [2]int{answer.Cols, answer.Rows}, "a browser view opens at the pinned size")
	conn := fedPeerStream(t, p, open.Stream, kp, answer.Key, true)
	t.Cleanup(func() { _ = conn.Close() })
	require.Equal(t, terminal.Output, terminalRead(t, conn).Kind)
	require.NoError(t, terminal.Write(conn, terminal.Frame{Kind: terminal.Resize, Data: terminal.Size(190, 52)}))
	fedEventually(t, "the sole viewer sizes the window", func() bool {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		for _, r := range mock.resizes {
			if strings.Join(r, " ") == "resize-window -t @1 -x 190 -y 51" {
				return true
			}
		}
		return false
	})
	for {
		frame := terminalRead(t, conn)
		if frame.Kind != terminal.Resize {
			continue
		}
		c, r, err := terminal.ParseSize(frame.Data)
		require.NoError(t, err)
		require.Equal(t, [2]int{190, 52}, [2]int{c, r}, "the shared size comes back as the viewer's grid")
		break
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

// A browser opens a peer's shell only while the peer's catalog grants this
// node sessions.shell on the group; terminal caps alone do not reach it.
func TestDashboardFederationTerminalShellNeedsShellCap(t *testing.T) {
	for _, granted := range []bool{false, true} {
		t.Run(fmt.Sprintf("granted=%v", granted), func(t *testing.T) {
			fh := newFedHarness(t)
			t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
			caps := []string{proto.CapSessionsWatch, proto.CapSessionsAttach}
			if granted {
				caps = append(caps, proto.CapSessionsShell)
			}
			dashboardRemoteTerminalCatalog(t, fh, caps...)
			server := httptest.NewServer(agentd.BuildDashboardHandlerForTest())
			t.Cleanup(server.Close)
			ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/federation/terminal?peer="+fh.peer.id.ID()+"&group=builders&shell=group&mode=interactive", nil)
			require.NoError(t, err)
			defer ws.Close()
			if !granted {
				require.NoError(t, ws.SetReadDeadline(time.Now().Add(5*time.Second)))
				var closed map[string]any
				require.NoError(t, ws.ReadJSON(&closed))
				require.Equal(t, "denied", closed["reason"])
				require.Empty(t, fh.peer.envelopes(proto.KindSessionOpen))
				return
			}
			fedEventually(t, "shell open sent", func() bool {
				for _, e := range fh.peer.envelopes(proto.KindSessionOpen) {
					var p proto.SessionOpenPayload
					if e.DecodePayload(&p) == nil && p.Shell == proto.ShellGroup && p.Group == "builders" && p.Agent == "" {
						return true
					}
				}
				return false
			})
		})
	}
}

// TestDashboardFederationTerminalFollowsARelaunch: a browser terminal on a
// peer's agent follows the agent across a relaunch on the peer, as a local
// terminal does. When the catalog stops listing the pane while the group
// still shares terminals, the view closes "reincarnated" (followed), not
// "revoked". Its redial still reaches the peer, naming the agent and the group
// it was attached through: the peer's "starting" and "moved" answers read
// "starting" (the browser keeps waiting; moved also asks for a fresh catalog),
// and "no longer live" reads "not_running". Once the catalog lists the new
// launch, the redial names it.
func TestDashboardFederationTerminalFollowsARelaunch(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	ws, _, first := dashboardTerminalOpen(t, fh, "watch")
	caps := []string{proto.CapSessions, proto.CapSessionsWatch, proto.CapSessionsAttach}
	catalog := func(sessions ...proto.CatalogSession) {
		fh.peer.send(fh.peer.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{Groups: []proto.CatalogGroup{{Name: "builders", Caps: caps, Sessions: sessions, SessionsAt: time.Now()}}}))
	}
	// The relaunch takes the pane down: the peer's catalog lists none.
	catalog()
	var closed map[string]any
	require.NoError(t, ws.ReadJSON(&closed))
	require.Equal(t, "reincarnated", closed["reason"])
	_ = ws.Close()

	server := httptest.NewServer(agentd.BuildDashboardHandlerForTest())
	t.Cleanup(server.Close)
	redial := func(refusal string) (proto.SessionOpenPayload, string) {
		t.Helper()
		previous := len(fh.peer.envelopes(proto.KindSessionOpen))
		type result struct {
			reason string
			err    error
		}
		done := make(chan result, 1)
		go func() {
			ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/federation/terminal?peer="+fh.peer.id.ID()+"&agent="+first.Agent+"&mode=watch", nil)
			if err != nil {
				done <- result{err: err}
				return
			}
			defer ws.Close()
			_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
			var closed map[string]any
			err = ws.ReadJSON(&closed)
			reason, _ := closed["reason"].(string)
			done <- result{reason, err}
		}()
		var env *proto.Envelope
		fedEventually(t, "redial reaches the peer", func() bool {
			es := fh.peer.envelopes(proto.KindSessionOpen)
			if len(es) > previous {
				env = es[len(es)-1]
				return true
			}
			return false
		})
		var open proto.SessionOpenPayload
		require.NoError(t, env.DecodePayload(&open))
		answer := fh.peer.envelope(proto.KindSessionAnswer, proto.Endpoint{}, proto.SessionAnswerPayload{Stream: open.Stream, Reason: refusal})
		answer.InReplyTo = env.ID
		fh.peer.send(answer)
		got := <-done
		require.NoError(t, got.err)
		return open, got.reason
	}

	open, reason := redial("the agent is starting")
	require.Equal(t, "starting", reason)
	require.Equal(t, first.Agent, open.Agent)
	require.Equal(t, first.Group, open.Group, "the redial keeps the group it was attached through")
	require.Empty(t, open.Session, "no session is named while the catalog lists none")
	require.Empty(t, open.Incarnation)

	catalogRequests := len(fh.peer.envelopes(proto.KindCatalogReq))
	_, reason = redial("session incarnation changed; list sessions again")
	require.Equal(t, "starting", reason, "a newer launch than the catalog lists is waited for")
	fedEventually(t, "a fresh catalog is asked for", func() bool {
		return len(fh.peer.envelopes(proto.KindCatalogReq)) > catalogRequests
	})

	_, reason = redial("advertised session is no longer live; list sessions again")
	require.Equal(t, "not_running", reason)

	// The catalog lists the new launch: the redial names it.
	catalog(proto.CatalogSession{Agent: first.Agent, Session: "runtime-2", Incarnation: "relaunched-runtime", Name: "remote"})
	fedEventually(t, "new launch listed", func() bool {
		return strings.Contains(fedHuman(t, fh.f, "GET", "/v1/federation/sessions", nil).Body.String(), "relaunched-runtime")
	})
	open, _ = redial("the agent is starting")
	require.Equal(t, "runtime-2", open.Session)
	require.Equal(t, "relaunched-runtime", open.Incarnation)
}

// TestDashboardFederationTerminalOpensARelaunchingMember: opened afresh on a
// peer's agent whose pane is down (relaunching), a terminal still asks the
// peer when a group sharing the terminal lists the agent as a member, as a
// local terminal opened on a relaunching agent waits for it. An agent the
// catalog does not list at all is refused here, without asking the peer.
func TestDashboardFederationTerminalOpensARelaunchingMember(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	const member = "agt_remote00001"
	fh.peer.send(fh.peer.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{Groups: []proto.CatalogGroup{{
		Name: "builders", Caps: []string{proto.CapSessions, proto.CapSessionsWatch},
		Members: []proto.CatalogMember{{Agent: member, Name: "remote"}}, SessionsAt: time.Now(),
	}}}))
	fedEventually(t, "catalog received", func() bool {
		raw, _, _ := db.GetFederationCatalog(fh.peer.id.ID())
		return strings.Contains(raw, member)
	})
	server := httptest.NewServer(agentd.BuildDashboardHandlerForTest())
	t.Cleanup(server.Close)
	// The bridge upgrades only once the peer has answered, so a dial runs
	// beside the test and hands back the closure it reads.
	dial := func(agentID string) <-chan string {
		done := make(chan string, 1)
		go func() {
			ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/federation/terminal?peer="+fh.peer.id.ID()+"&agent="+agentID+"&mode=watch", nil)
			if err != nil {
				done <- "dial: " + err.Error()
				return
			}
			defer ws.Close()
			_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
			var closed map[string]any
			if err := ws.ReadJSON(&closed); err != nil {
				done <- "read: " + err.Error()
				return
			}
			reason, _ := closed["reason"].(string)
			done <- reason
		}()
		return done
	}

	require.Equal(t, "denied", <-dial("agt_stranger01"))
	require.Empty(t, fh.peer.envelopes(proto.KindSessionOpen), "an agent the catalog does not list is not asked about")

	closed := dial(member)
	var env *proto.Envelope
	fedEventually(t, "the open reaches the peer", func() bool {
		es := fh.peer.envelopes(proto.KindSessionOpen)
		if len(es) > 0 {
			env = es[0]
			return true
		}
		return false
	})
	var open proto.SessionOpenPayload
	require.NoError(t, env.DecodePayload(&open))
	require.Equal(t, member, open.Agent)
	require.Equal(t, "builders", open.Group)
	require.Empty(t, open.Session)
	answer := fh.peer.envelope(proto.KindSessionAnswer, proto.Endpoint{}, proto.SessionAnswerPayload{Stream: open.Stream, Reason: "the agent is starting"})
	answer.InReplyTo = env.ID
	fh.peer.send(answer)
	require.Equal(t, "starting", <-closed)
}
