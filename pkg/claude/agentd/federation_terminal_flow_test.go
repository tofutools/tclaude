package agentd_test

import (
	"fmt"
	"github.com/gorilla/websocket"
	"github.com/tofutools/tclaude/pkg/testharness"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/federation/stream"
	"github.com/tofutools/tclaude/pkg/federation/terminal"
)

// Only the external tmux subprocess is replaced. The daemon mux, envelope
// authentication, hub relay, stream encryption and terminal protocol are real.
type terminalTmux struct {
	clcommon.Tmux
	mu      sync.Mutex
	options map[string]string
	keys    []byte
	pane    string
	windows string
	version string
	attach  []string
	probes  int
}

func terminalEcho(s string) *exec.Cmd { return exec.Command("printf", "%s", s) }
func (m *terminalTmux) Command(args ...string) *exec.Cmd {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch args[0] {
	case "-V":
		return terminalEcho(m.version)
	case "display-message":
		if args[len(args)-1] == "#{pane_width} #{pane_height}" {
			return terminalEcho("120 40")
		}
		if strings.Contains(args[len(args)-1], "#{window_panes}") {
			m.probes++
			return terminalEcho(m.pane + "\t@1\t$1\t" + m.windows + "\t1\n")
		}
	case "resize-window":
		m.options["window-size"] = "manual"
		return exec.Command("true")
	case "show-options":
		name := args[len(args)-1]
		value, exists := m.options[name]
		if !exists {
			return terminalEcho("")
		}
		if args[2] == "-qv" {
			return terminalEcho(value + "\n")
		}
		return terminalEcho(name + " " + value + "\n")
	case "set-option":
		if args[1] == "-w" {
			m.options[args[4]] = args[5]
			return exec.Command("true")
		}
		if args[1] == "-wu" {
			delete(m.options, args[4])
			return exec.Command("true")
		}
	case "attach-session":
		m.attach = append([]string{}, args...)
		// An actual PTY child produces output and waits until the renderer closes.
		return exec.Command("sh", "-c", "printf 'remote renderer ready'; exec cat")
	case "send-keys":
		if len(args) > 5 && args[3] == "-l" && args[4] == "--" {
			m.keys = append(m.keys, []byte(strings.ReplaceAll(args[5], "\\;", ";"))...)
			return exec.Command("true")
		}
		names := map[string]string{"Enter": "\r", "Escape": "\x1b", "C-c": "\x03", "C-b": "\x02", "Up": "\x1b[A"}
		if len(args) == 4 {
			m.keys = append(m.keys, []byte(names[args[3]])...)
			return exec.Command("true")
		}

	}
	return m.Tmux.Command(args...)
}
func (m *terminalTmux) snapshot() (map[string]string, []byte, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	opts := map[string]string{}
	for k, v := range m.options {
		opts[k] = v
	}
	return opts, append([]byte{}, m.keys...), append([]string{}, m.attach...)
}
func terminalAnswer(t *testing.T, p *fedPeer, sid string) proto.SessionAnswerPayload {
	t.Helper()
	var a proto.SessionAnswerPayload
	fedEventually(t, "session answer", func() bool {
		for _, e := range p.envelopes(proto.KindSessionAnswer) {
			var next proto.SessionAnswerPayload
			if e.DecodePayload(&next) == nil && next.Stream == sid {
				a = next
				return true
			}
		}
		return false
	})
	return a
}
func terminalRead(t *testing.T, c *stream.Conn) terminal.Frame {
	t.Helper()
	type result struct {
		f   terminal.Frame
		err error
	}
	ch := make(chan result, 1)
	go func() { f, err := terminal.Read(c); ch <- result{f, err} }()
	select {
	case r := <-ch:
		require.NoError(t, r.err)
		return r.f
	case <-time.After(5 * time.Second):
		t.Fatal("terminal frame timeout")
		return terminal.Frame{}
	}
}
func terminalCatalogIncarnation(t *testing.T, fh *fedHarness, aid string) string {
	t.Helper()
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermSessionsRead, "scope": "group=team"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	incarnation := ""
	fedEventually(t, "session incarnation advertised", func() bool {
		for _, e := range fh.peer.envelopes(proto.KindCatalog) {
			var c proto.CatalogPayload
			if e.DecodePayload(&c) != nil {
				continue
			}
			for _, g := range c.Groups {
				for _, s := range g.Sessions {
					if s.Agent == aid && s.Incarnation != "" {
						incarnation = s.Incarnation
					}
				}
			}
		}
		return incarnation != ""
	})
	return incarnation
}

func TestFederation_TerminalInputWatchKickAndPin(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	const conv = "fed-terminal-target"
	f.HaveGroup("team")
	f.HaveConvWithTitle(conv, "terminal-target")
	f.HaveMember("team", conv)
	f.HaveAliveSession(conv, "terminal-runtime", "tclaude-terminal-runtime", f.TestCwd("work"))
	aid, err := db.AgentIDForConv(conv)
	require.NoError(t, err)
	original := clcommon.Default
	mock := &terminalTmux{Tmux: original, options: map[string]string{"pane-border-format": "original"}, pane: "%1", windows: "1", version: "tmux 3.4"}
	clcommon.Default = mock
	t.Cleanup(func() { agentd.ResetFederationForTest(); clcommon.Default = original })
	incarnation := terminalCatalogIncarnation(t, fh, aid)
	grant := func(slug string) {
		rec := fedHuman(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug, "scope": "group=team"})
		require.Equal(t, 200, rec.Code, rec.Body.String())
	}
	open := func(readonly bool) (proto.SessionOpenPayload, *stream.KeyPair, proto.SessionAnswerPayload) {
		kp, err := stream.NewKeyPair()
		require.NoError(t, err)
		o := proto.SessionOpenPayload{Agent: aid, Session: "terminal-runtime", Incarnation: incarnation, Group: "team", Stream: proto.NewEnvelopeID(), Key: kp.Pub, ReadOnly: readonly, Cols: 80, Rows: 24}
		p.send(p.envelope(proto.KindSessionOpen, proto.Endpoint{}, o))
		return o, kp, terminalAnswer(t, p, o.Stream)
	}
	_, _, ans := open(false)
	require.False(t, ans.OK, "no implicit interactive access")
	grant(agentd.PermSessionsWatch)
	_, _, ans = open(false)
	require.False(t, ans.OK, "watch does not authorize typing")
	o, kp, ans := open(true)
	require.True(t, ans.OK, ans.Reason)
	c := fedPeerStream(t, p, o.Stream, kp, ans.Key, true)
	frame := terminalRead(t, c)
	require.Equal(t, terminal.Output, frame.Kind)
	require.Contains(t, string(frame.Data), "remote renderer ready")
	opts, keys, argv := mock.snapshot()
	require.Empty(t, keys)
	require.Contains(t, opts["pane-border-format"], "REMOTE WATCH")
	require.Equal(t, []string{"attach-session", "-f", "ignore-size", "-t", "=tclaude-terminal-runtime"}, argv)
	rec := fedHuman(t, f, http.MethodGet, "/v1/federation/viewers", nil)
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), o.Stream)
	rec = fedHuman(t, f, http.MethodPost, "/v1/federation/viewers/"+o.Stream+"/kick", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Equal(t, 404, fedHuman(t, f, http.MethodPost, "/v1/federation/viewers/"+o.Stream+"/kick", nil).Code, "successful kick waits for viewer removal")
	fedEventually(t, "indicator restored after kick", func() bool {
		opts, _, _ := mock.snapshot()
		return opts["pane-border-format"] == "original" && opts["@tclaude-federation-viewers"] == ""
	})
	require.Equal(t, 404, fedHuman(t, f, http.MethodPost, "/v1/federation/viewers/"+o.Stream+"/kick", nil).Code)
	grant(agentd.PermSessionsAttach)
	o, kp, ans = open(false)
	require.True(t, ans.OK, ans.Reason)
	c = fedPeerStream(t, p, o.Stream, kp, ans.Key, true)
	_ = terminalRead(t, c)
	typed := []byte("answer\r\x1b\x03\x1b[A\x02:kill-server\r")
	require.NoError(t, terminal.Write(c, terminal.Frame{Kind: terminal.Input, Data: typed}))
	credit := terminalRead(t, c)
	require.Equal(t, terminal.Credit, credit.Kind)
	n, err := terminal.ParseNumber(credit.Data)
	require.NoError(t, err)
	require.Equal(t, len(typed), n)
	_, keys, _ = mock.snapshot()
	require.Equal(t, typed, keys, "fixed key encoding preserves controls")
	// A replacement pane is never followed, even with the same session name.
	mock.mu.Lock()
	mock.pane = "%2"
	mock.mu.Unlock()
	fedEventually(t, "viewer closed after pane replacement", func() bool {
		return !strings.Contains(fedHuman(t, f, http.MethodGet, "/v1/federation/viewers", nil).Body.String(), o.Stream)
	})
	row, err := db.LoadSession("terminal-runtime")
	require.NoError(t, err)
	row.CreatedAt = row.CreatedAt.Add(time.Second)
	require.NoError(t, db.SaveSession(row))
	_, _, ans = open(false)
	require.False(t, ans.OK)
	require.Contains(t, ans.Reason, "incarnation changed")
	row.CreatedAt = row.CreatedAt.Add(-time.Second)
	require.NoError(t, db.SaveSession(row))
	mock.mu.Lock()
	mock.windows = "2"
	mock.mu.Unlock()
	_, _, ans = open(false)
	require.False(t, ans.OK)
	require.Contains(t, ans.Reason, "exactly one window")
	mock.mu.Lock()
	mock.windows = "1"
	mock.version = "tmux 3.1c"
	mock.mu.Unlock()
	_, _, ans = open(false)
	require.False(t, ans.OK)
	require.Contains(t, ans.Reason, "3.2 or newer")
	require.NoError(t, db.SetSessionExitLaunchGeneration("terminal-runtime", strings.Repeat("a", 32)))
	_, _, ans = open(false)
	require.False(t, ans.OK)
	require.Contains(t, ans.Reason, "incarnation changed")
}

func TestFederation_TerminalWatchRejectsInputAndRevocation(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	const conv = "fed-terminal-watch"
	f.HaveGroup("team")
	f.HaveConvWithTitle(conv, "watch")
	f.HaveMember("team", conv)
	f.HaveAliveSession(conv, "watch-runtime", "tclaude-watch-runtime", f.TestCwd("work"))
	aid, err := db.AgentIDForConv(conv)
	require.NoError(t, err)
	original := clcommon.Default
	mock := &terminalTmux{Tmux: original, options: map[string]string{}, pane: "%1", windows: "1", version: "tmux 3.4"}
	clcommon.Default = mock
	t.Cleanup(func() { agentd.ResetFederationForTest(); clcommon.Default = original })
	incarnation := terminalCatalogIncarnation(t, fh, aid)
	grant := map[string]any{"peer": "bob", "slug": agentd.PermSessionsWatch, "scope": "group=team"}
	require.Equal(t, 200, fedHuman(t, f, http.MethodPost, "/v1/federation/grants", grant).Code)
	for _, revoke := range []bool{false, true} {
		kp, err := stream.NewKeyPair()
		require.NoError(t, err)
		sid := proto.NewEnvelopeID()
		o := proto.SessionOpenPayload{Agent: aid, Session: "watch-runtime", Incarnation: incarnation, Group: "team", Stream: sid, Key: kp.Pub, ReadOnly: true, Cols: 80, Rows: 24}
		p.send(p.envelope(proto.KindSessionOpen, proto.Endpoint{}, o))
		ans := terminalAnswer(t, p, sid)
		require.True(t, ans.OK, ans.Reason)
		c := fedPeerStream(t, p, sid, kp, ans.Key, true)
		_ = terminalRead(t, c)
		if revoke {
			require.Equal(t, 200, fedHuman(t, f, http.MethodDelete, "/v1/federation/grants", grant).Code)
		} else {
			require.NoError(t, terminal.Write(c, terminal.Frame{Kind: terminal.Input, Data: []byte("yes\r")}))
		}
		fedEventually(t, fmt.Sprintf("watch closed revoke=%v", revoke), func() bool {
			return !strings.Contains(fedHuman(t, f, http.MethodGet, "/v1/federation/viewers", nil).Body.String(), sid)
		})
		_, keys, _ := mock.snapshot()
		require.Empty(t, keys)
	}
}

func TestFederation_TerminalOutgoingScopeAndOrigin(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	const caller = "fed-terminal-reader"
	f.HaveConvWithTitle(caller, "viewer")
	row := proto.CatalogSession{Agent: "agt_remote00001", Session: "runtime", Incarnation: "remote-incarnation", Name: "remote"}
	p.send(p.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{Groups: []proto.CatalogGroup{{Name: "builders", Caps: []string{proto.CapSessions, proto.CapSessionsWatch, proto.CapSessionsAttach}, Sessions: []proto.CatalogSession{row}, SessionsAt: time.Now()}}}))
	fedEventually(t, "terminal catalog received", func() bool {
		return strings.Contains(fedHuman(t, f, http.MethodGet, "/v1/federation/sessions", nil).Body.String(), row.Agent)
	})
	request := func(origin string) *httptest.ResponseRecorder {
		req := testharness.JSONRequest(t, http.MethodGet, "/v1/federation/attach?target="+row.Agent+"@bob&read_only=1", nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		return testharness.Serve(f.Mux, agentd.AsAgentPeer(req, caller))
	}
	require.Equal(t, 403, request("").Code)
	require.NoError(t, db.GrantAgentPermission(caller, agentd.PermSessionsWatch, "test"))
	require.Equal(t, 403, request("").Code, "unscoped grant cannot cross restricted peer")
	require.NoError(t, db.GrantAgentPermissionWithScope(caller, agentd.PermSessionsWatch, `{"peer":["`+p.id.ID()+`/wrong"]}`, "test"))
	require.Equal(t, 403, request("").Code)
	require.NoError(t, db.GrantAgentPermissionWithScope(caller, agentd.PermSessionsWatch, `{"peer":["`+p.id.ID()+`/builders"]}`, "test"))
	require.Equal(t, 403, request("https://attacker.invalid").Code)
	require.Empty(t, p.envelopes(proto.KindSessionOpen), "denials must not open remote renderers")
	// A correctly scoped CLI upgrade can receive output and send control frames.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.Mux.ServeHTTP(w, agentd.AsAgentPeer(r, caller)) }))
	t.Cleanup(server.Close)
	type dialResult struct {
		c   *websocket.Conn
		err error
	}
	dialed := make(chan dialResult, 1)
	go func() {
		c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/federation/attach?target="+row.Agent+"@bob&read_only=1", nil)
		dialed <- dialResult{c, err}
	}()
	var env *proto.Envelope
	fedEventually(t, "authorized session open", func() bool {
		es := p.envelopes(proto.KindSessionOpen)
		if len(es) > 0 {
			env = es[len(es)-1]
			return true
		}
		return false
	})
	var o proto.SessionOpenPayload
	require.NoError(t, env.DecodePayload(&o))
	require.Equal(t, "builders", o.Group)
	kp, err := stream.NewKeyPair()
	require.NoError(t, err)
	answer := p.envelope(proto.KindSessionAnswer, proto.Endpoint{}, proto.SessionAnswerPayload{Stream: o.Stream, OK: true, Key: kp.Pub})
	answer.InReplyTo = env.ID
	p.send(answer)
	conn := fedPeerStream(t, p, o.Stream, kp, o.Key, false)
	var ws *websocket.Conn
	select {
	case r := <-dialed:
		require.NoError(t, r.err)
		ws = r.c
	case <-time.After(5 * time.Second):
		t.Fatal("websocket upgrade timeout")
	}
	t.Cleanup(func() { _ = ws.Close() })
	require.NoError(t, terminal.Write(conn, terminal.Frame{Kind: terminal.Output, Data: []byte("ready")}))
	_, raw, err := ws.ReadMessage()
	require.NoError(t, err)
	frame, err := terminal.Decode(raw)
	require.NoError(t, err)
	require.Equal(t, "ready", string(frame.Data))
	raw, err = terminal.Encode(terminal.Frame{Kind: terminal.Resize, Data: terminal.Size(100, 40)})
	require.NoError(t, err)
	require.NoError(t, ws.WriteMessage(websocket.BinaryMessage, raw))
	frame = terminalRead(t, conn)
	require.Equal(t, terminal.Resize, frame.Kind)
	// Local revocation blocks the very next output frame, without waiting for
	// the idle ticker or exhausting the peer's outstanding output credit.
	_, err = db.RevokeAgentPermission(caller, agentd.PermSessionsWatch)
	require.NoError(t, err)
	require.NoError(t, terminal.Write(conn, terminal.Frame{Kind: terminal.Output, Data: []byte("must not escape after revoke")}))
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, err = ws.ReadMessage()
	require.Error(t, err)
	entries, err := db.ListAuditLog(db.AuditLogFilter{Verb: "sessions.attach.open"})
	require.NoError(t, err)
	require.NotEmpty(t, entries)
}

func TestFederation_TerminalAdmissionBeforePaneProbes(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	const conv = "fed-terminal-limits"
	f.HaveGroup("team")
	f.HaveConvWithTitle(conv, "limited")
	f.HaveMember("team", conv)
	f.HaveAliveSession(conv, "limits-runtime", "tclaude-limits-runtime", f.TestCwd("work"))
	aid, err := db.AgentIDForConv(conv)
	require.NoError(t, err)
	original := clcommon.Default
	mock := &terminalTmux{Tmux: original, options: map[string]string{}, pane: "%1", windows: "1", version: "tmux 3.1"}
	clcommon.Default = mock
	t.Cleanup(func() { agentd.ResetFederationForTest(); clcommon.Default = original })
	incarnation := terminalCatalogIncarnation(t, fh, aid)
	rec := fedHuman(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermSessionsAttach, "scope": "group=team"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	kp, err := stream.NewKeyPair()
	require.NoError(t, err)
	for i := 0; i < 31; i++ {
		o := proto.SessionOpenPayload{Agent: aid, Session: "limits-runtime", Incarnation: incarnation, Group: "team", Stream: proto.NewEnvelopeID(), Key: kp.Pub, Cols: 80, Rows: 24}
		p.send(p.envelope(proto.KindSessionOpen, proto.Endpoint{}, o))
		ans := terminalAnswer(t, p, o.Stream)
		require.False(t, ans.OK)
		if i == 30 {
			require.Contains(t, ans.Reason, "rate limited")
		} else {
			require.Contains(t, ans.Reason, "3.2 or newer")
		}
	}
	mock.mu.Lock()
	probes := mock.probes
	mock.mu.Unlock()
	require.Equal(t, 30, probes, "rate-limited opens must not spawn a pane probe")
}
