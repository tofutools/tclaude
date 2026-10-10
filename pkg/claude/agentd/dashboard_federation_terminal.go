package agentd

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/federation/stream"
	"github.com/tofutools/tclaude/pkg/federation/terminal"
)

// Only authentication failures remain HTTP errors: browser WebSockets cannot
// inspect refused upgrades. Operational refusals become bounded control frames.
func handleDashboardFederationTerminal(w http.ResponseWriter, r *http.Request) {
	mode := r.URL.Query().Get("mode")
	refusal := &peerViewResponse{header: make(http.Header)}
	if mode != "watch" && mode != "interactive" {
		writeError(refusal, 400, "invalid_arg", "mode must be watch or interactive")
	} else {
		q := r.URL.Query()
		q.Set("target", q.Get("agent")+"@"+q.Get("peer"))
		if mode == "watch" {
			q.Set("read_only", "1")
		} else {
			q.Del("read_only")
		}
		copy := r.Clone(r.Context())
		u := *r.URL
		copy.URL = &u
		copy.URL.RawQuery = q.Encode()
		handleFederationTerminal(&dashboardTerminalResponse{peerViewResponse: refusal, real: w}, copy, true)
	}
	if refusal.status == 0 {
		return
	}
	ws, err := termWSUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	var body struct {
		Code  string
		Error string
	}
	_ = json.Unmarshal(refusal.body.Bytes(), &body)
	reason := "error"
	switch refusal.status {
	case 403, 400, 404:
		reason = "denied"
	case 429:
		reason = "limit"
	case 502, 503, 504:
		reason = "offline"
	}
	_ = ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_ = ws.WriteJSON(map[string]any{"type": "closed", "reason": reason, "message": body.Error})
	_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, reason), time.Now().Add(time.Second))
}

type dashboardTerminalResponse struct {
	*peerViewResponse
	real http.ResponseWriter
}

func serveDashboardFederationTerminal(w http.ResponseWriter, r *http.Request, rt *fedRuntime, v *fedTerminalView, conn *stream.Conn, peer *db.FederationPeer, p proto.SessionOpenPayload) {
	if v.cols < 1 || v.rows < 1 || v.cols > 1000 || v.rows > 1000 {
		writeError(w, 502, "upgrade_required", "peer does not report pinned terminal dimensions; upgrade the peer")
		return
	}
	ws, err := termWSUpgrader.Upgrade(w.(*dashboardTerminalResponse).real, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	ws.SetReadLimit(terminal.MaxPayload)
	var wsMu, streamMu sync.Mutex
	writeWS := func(kind int, body []byte) error {
		wsMu.Lock()
		defer wsMu.Unlock()
		_ = ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
		return ws.WriteMessage(kind, body)
	}
	control := func(body any) error {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		return writeWS(websocket.TextMessage, raw)
	}
	write := func(f terminal.Frame) error { streamMu.Lock(); defer streamMu.Unlock(); return terminal.Write(conn, f) }
	mode := "interactive"
	if p.ReadOnly {
		mode = "watch"
	}
	if control(map[string]any{"type": "hello", "mode": mode, "cols": v.cols, "rows": v.rows, "peer": peer.InstanceID, "agent": p.Agent}) != nil {
		return
	}
	fedTerminalAudit("sessions.attach.open", "", peer.InstanceID, p.Agent, p.Group, "viewer="+v.ID+" browser mode="+mode, 200)
	defer fedTerminalAudit("sessions.attach.close", "", peer.InstanceID, p.Agent, p.Group, "viewer="+v.ID+" browser", 200)
	authorized := func() string {
		trusted, _ := db.GetFederationPeer(peer.InstanceID)
		if trusted == nil {
			return "untrusted"
		}
		if !rt.sessionPeerOnline(peer.InstanceID) {
			return "offline"
		}
		cat, _, _ := fedCatalogFor(peer.InstanceID)
		if cat == nil {
			return "revoked"
		}
		for _, g := range cat.Groups {
			if g.Name != p.Group || !g.HasCap(fedTerminalCap(p.ReadOnly)) {
				continue
			}
			for _, s := range g.Sessions {
				if s.Agent == p.Agent && s.Session == p.Session {
					if s.Incarnation != p.Incarnation {
						return "reincarnated"
					}
					return ""
				}
			}
		}
		return "revoked"
	}
	input := terminal.NewWindow()
	defer input.Close()
	var workers sync.WaitGroup
	stopped := make(chan string, 2)
	workers.Add(2)
	// Cleanup unblocks both readers, and input backpressure, before waiting.
	defer func() { input.Close(); _ = ws.Close(); _ = conn.Close(); workers.Wait() }()
	go func() {
		defer workers.Done()
		for {
			frame, err := terminal.Read(conn)
			if err != nil {
				stopped <- "offline"
				return
			}
			if frame.Kind == terminal.Closed {
				reason := string(frame.Data)
				switch reason {
				case "exit", "reincarnated", "revoked", "untrusted", "kicked", "error":
				default:
					reason = "error"
				}
				stopped <- reason
				return
			}
			if reason := authorized(); reason != "" {
				stopped <- reason
				return
			}
			switch frame.Kind {
			case terminal.Output:
				if err = writeWS(websocket.BinaryMessage, frame.Data); err == nil && len(frame.Data) > 0 {
					err = write(terminal.Frame{Kind: terminal.Credit, Data: terminal.Number(len(frame.Data))})
				}
			case terminal.Credit:
				var n int
				n, err = terminal.ParseNumber(frame.Data)
				if err == nil {
					err = input.Grant(n)
				}
			case terminal.Resize:
				var c, rows int
				c, rows, err = terminal.ParseSize(frame.Data)
				if err == nil {
					err = control(map[string]any{"type": "size", "cols": c, "rows": rows})
				}
			default:
				err = terminal.ErrProtocol
			}
			if err != nil {
				stopped <- "error"
				return
			}
		}
	}()
	go func() {
		defer workers.Done()
		for {
			kind, body, err := ws.ReadMessage()
			if err != nil {
				stopped <- ""
				return
			}
			if kind == websocket.TextMessage {
				var resize struct{ Type string }
				if json.Unmarshal(body, &resize) == nil && resize.Type == "resize" {
					continue
				}
				stopped <- "error"
				return
			}
			if kind != websocket.BinaryMessage {
				stopped <- "error"
				return
			}
			if p.ReadOnly {
				continue
			}
			for len(body) > 0 {
				n, err := input.Take(min(len(body), 1024))
				if err != nil {
					stopped <- "error"
					return
				}
				if reason := authorized(); reason != "" {
					stopped <- reason
					return
				}
				if err = write(terminal.Frame{Kind: terminal.Input, Data: body[:n]}); err != nil {
					stopped <- "error"
					return
				}
				body = body[n:]
			}
		}
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	reason := ""
loop:
	for {
		select {
		case reason = <-stopped:
			break loop
		case <-v.ctx.Done():
			reason = "offline"
			break loop
		case <-r.Context().Done():
			break loop
		case <-ticker.C:
			reason = authorized()
			if reason != "" {
				break loop
			}
		}
	}
	if reason != "" {
		_ = control(map[string]any{"type": "closed", "reason": reason, "message": "Remote terminal closed: " + reason})
	}
	wsMu.Lock()
	_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, reason), time.Now().Add(time.Second))
	wsMu.Unlock()
}
