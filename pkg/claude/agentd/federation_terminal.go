package agentd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/federation/stream"
	"github.com/tofutools/tclaude/pkg/federation/terminal"
)

const fedTerminalOpenTimeout = 20 * time.Second

type fedTerminalAnswer struct {
	peer, request string
	answer        proto.SessionAnswerPayload
}
type fedTerminalState struct {
	views   map[string]*fedTerminalView
	waiters map[string]chan fedTerminalAnswer
	opens   map[string][]time.Time
	stopped bool
}
type fedTerminalView struct {
	ID         string    `json:"id"`
	Peer       string    `json:"peer"`
	Agent      string    `json:"agent"`
	Session    string    `json:"session"`
	Group      string    `json:"group"`
	ReadOnly   bool      `json:"read_only"`
	Started    time.Time `json:"started"`
	Incoming   bool      `json:"incoming"`
	ctx        context.Context
	cancel     context.CancelFunc
	mu         sync.Mutex
	stopped    bool
	cleanup    []func()
	done       chan struct{}
	fixedSize  bool
	cols, rows int
	reason     string
}

func (rt *fedRuntime) terminalsLocked() *fedTerminalState {
	if rt.terminals == nil {
		rt.terminals = &fedTerminalState{views: map[string]*fedTerminalView{}, waiters: map[string]chan fedTerminalAnswer{}, opens: map[string][]time.Time{}}
	}
	return rt.terminals
}
func (v *fedTerminalView) addCleanup(f func()) {
	v.mu.Lock()
	if v.stopped {
		v.mu.Unlock()
		f()
		return
	}
	v.cleanup = append(v.cleanup, f)
	v.mu.Unlock()
}
func (v *fedTerminalView) close() {
	v.mu.Lock()
	if v.stopped {
		done := v.done
		v.mu.Unlock()
		if done != nil {
			<-done
		}
		return
	}
	v.stopped = true
	cleanup := v.cleanup
	v.cleanup = nil
	v.cancel()
	v.mu.Unlock()
	if v.done != nil {
		defer close(v.done)
	}
	for i := len(cleanup) - 1; i >= 0; i-- {
		cleanup[i]()
	}
}
func (rt *fedRuntime) addTerminal(peer string, p proto.SessionOpenPayload, incoming bool) (*fedTerminalView, error) {
	rt.terminalsMu.Lock()
	defer rt.terminalsMu.Unlock()
	st := rt.terminalsLocked()
	if st.stopped || rt.ctx.Err() != nil {
		return nil, errors.New("federation stopped")
	}
	if st.views[p.Stream] != nil {
		return nil, errors.New("duplicate viewer id")
	}
	count := 0
	for _, v := range st.views {
		if v.Peer == peer {
			count++
		}
	}
	if len(st.views) >= 32 || count >= 8 {
		return nil, errors.New("remote viewer limit reached")
	}
	if !allowPerMinute(st.opens, peer, 30) {
		return nil, errors.New("remote attach rate limited")
	}
	ctx, cancel := context.WithCancel(rt.ctx)
	v := &fedTerminalView{ID: p.Stream, Peer: peer, Agent: p.Agent, Session: p.Session, Group: p.Group, ReadOnly: p.ReadOnly, Started: time.Now(), Incoming: incoming, fixedSize: p.FixedSize, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	st.views[v.ID] = v
	v.addCleanup(func() { rt.terminalsMu.Lock(); delete(rt.terminalsLocked().views, v.ID); rt.terminalsMu.Unlock() })
	return v, nil
}
func (rt *fedRuntime) stopTerminals() {
	rt.terminalsMu.Lock()
	st := rt.terminalsLocked()
	st.stopped = true
	views := []*fedTerminalView{}
	for _, v := range st.views {
		views = append(views, v)
	}
	rt.terminalsMu.Unlock()
	for _, v := range views {
		v.close()
	}
}

func (rt *fedRuntime) handleSessionAnswer(peer *db.FederationPeer, env *proto.Envelope) {
	var a proto.SessionAnswerPayload
	if env.DecodePayload(&a) != nil || !proto.ValidStreamID(a.Stream) {
		return
	}
	rt.terminalsMu.Lock()
	ch := rt.terminalsLocked().waiters[a.Stream]
	rt.terminalsMu.Unlock()
	if ch != nil {
		select {
		case ch <- fedTerminalAnswer{peer.InstanceID, env.InReplyTo, a}:
		default:
		}
	}
}
func (rt *fedRuntime) openTerminalStream(v *fedTerminalView, p proto.SessionOpenPayload) (*stream.Conn, error) {
	kp, err := stream.NewKeyPair()
	if err != nil {
		return nil, err
	}
	p.Key = kp.Pub
	ch := make(chan fedTerminalAnswer, 1)
	rt.terminalsMu.Lock()
	rt.terminalsLocked().waiters[p.Stream] = ch
	rt.terminalsMu.Unlock()
	defer func() { rt.terminalsMu.Lock(); delete(rt.terminalsLocked().waiters, p.Stream); rt.terminalsMu.Unlock() }()
	env, err := proto.NewEnvelope(rt.id, proto.KindSessionOpen, proto.Endpoint{Name: rt.name}, proto.Endpoint{Instance: v.Peer}, fedTerminalOpenTimeout+time.Minute, p)
	if err != nil {
		return nil, err
	}
	peer, _ := db.GetFederationPeer(v.Peer)
	if peer == nil {
		return nil, errors.New("peer no longer trusted")
	}
	// sendControl normally creates its own envelope ID. Here the answer must be
	// bound to this exact request, so send the already-signed/encrypted envelope.
	sealed, err := proto.Seal(rt.id, env, peer.PubKey)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(v.ctx, fedTerminalOpenTimeout)
	defer cancel()
	sent, err := rt.cl.Send(ctx, v.Peer, sealed)
	if err != nil {
		return nil, err
	}
	if sent.Status != proto.SendDelivered {
		return nil, errors.New("peer is offline")
	}
	for {
		select {
		case a := <-ch:
			if a.peer != v.Peer || a.request != env.ID {
				continue
			}
			if !a.answer.OK {
				return nil, &fedTerminalRefusal{reason: proto.StripControls(a.answer.Reason)}
			}
			v.cols, v.rows = a.answer.Cols, a.answer.Rows
			return rt.joinStream(ctx, v.Peer, p.Stream, kp, a.answer.Key, true)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// resolveFedTerminalTarget resolves identity and scope from discovery. A stale
// session can never silently become access to a newer runtime at the receiver.
func resolveFedTerminalTarget(address string, readOnly bool) (*db.FederationPeer, proto.SessionOpenPayload, error) {
	ref, sel, ok := strings.Cut(address, "@")
	if !ok {
		return nil, proto.SessionOpenPayload{}, errors.New("target must be agt_…@peer from federation sessions")
	}
	peer, err := resolveFederationPeer(sel)
	if err != nil {
		return nil, proto.SessionOpenPayload{}, err
	}
	cat, _, err := fedCatalogFor(peer.InstanceID)
	if err != nil || cat == nil {
		return nil, proto.SessionOpenPayload{}, errors.New("no peer catalog; list federation sessions first")
	}
	var matches []proto.SessionOpenPayload
	for _, g := range cat.Groups {
		if !g.HasCap(fedTerminalCap(readOnly)) {
			continue
		}
		for _, s := range g.Sessions {
			if s.Agent == ref {
				matches = append(matches, proto.SessionOpenPayload{Agent: s.Agent, Session: s.Session, Incarnation: s.Incarnation, Group: g.Name, Stream: proto.NewEnvelopeID(), ReadOnly: readOnly, Cols: 80, Rows: 24})
			}
		}
	}
	if len(matches) == 0 {
		return nil, proto.SessionOpenPayload{}, errors.New("session not shared with the requested watch/attach capability")
	}
	return peer, matches[0], nil
}

func fedTerminalAudit(verb, caller, peer, target, group, detail string, status int) {
	kind, label := db.AuditActorHuman, "human"
	if caller != "" {
		kind, label = db.AuditActorAgent, agent.TitleFor(caller)
	}
	_, _ = db.InsertAuditLog(db.AuditLogEntry{At: time.Now(), ActorKind: kind, ActorConv: caller, ActorLabel: label, Verb: verb, TargetLabel: target + "@" + peer, GroupName: group, Detail: detail, Status: status, Source: "federation"})
}
func handleFederationAttach(w http.ResponseWriter, r *http.Request) {
	handleFederationTerminal(w, r, false)
}

func handleFederationTerminal(w http.ResponseWriter, r *http.Request, browser bool) {
	caller, human, ok := authedCaller(w, r)
	if !ok {
		return
	}
	if !browser && r.Header.Get("Origin") != "" {
		writeError(w, 403, "origin", "browser origins are not supported")
		return
	}
	readOnly := r.URL.Query().Get("read_only") == "1"
	peer, p, err := resolveFedTerminalTarget(r.URL.Query().Get("target"), readOnly)
	if err != nil {
		writeError(w, 400, "invalid_arg", err.Error())
		return
	}
	// An agent may have grants on only one of several groups sharing this
	// agent. Pick a covering catalog group rather than relying on catalog order.
	if !human {
		cat, _, _ := fedCatalogFor(peer.InstanceID)
		allowed := false
		for _, g := range cat.Groups {
			if !g.HasCap(fedTerminalCap(readOnly)) {
				continue
			}
			for _, s := range g.Sessions {
				if s.Agent != p.Agent || s.Session != p.Session {
					continue
				}
				yes, _, err := permissionAllowsAction(r, caller, fedTerminalSlug(readOnly), ActionContext{RemotePeer: peer.InstanceID, RemoteGroup: g.Name})
				if err == nil && yes {
					p.Group = g.Name
					p.Incarnation = s.Incarnation
					allowed = true
					break
				}
			}
			if allowed {
				break
			}
		}
		if !allowed {
			writeError(w, 403, "permission_denied", "requires "+fedTerminalSlug(readOnly)+" scoped to peer="+peer.InstanceID+"/"+p.Group)
			return
		}
	}
	rt := currentFederation()
	if rt == nil || !rt.sessionPeerOnline(peer.InstanceID) {
		writeError(w, 503, "offline", "federation peer is offline")
		return
	}
	p.FixedSize = browser
	p.Cols, p.Rows = queryInt(r, "cols", 80), queryInt(r, "rows", 24)
	if p.Cols < 1 || p.Cols > 1000 || p.Rows < 1 || p.Rows > 1000 {
		writeError(w, 400, "invalid_arg", "terminal dimensions must be 1..1000")
		return
	}
	v, err := rt.addTerminal(peer.InstanceID, p, false)
	if err != nil {
		writeError(w, 429, "limit", err.Error())
		return
	}
	defer v.close()
	conn, err := rt.openTerminalStream(v, p)
	if err != nil {
		fedTerminalAudit("sessions.attach.open", caller, peer.InstanceID, p.Agent, p.Group, err.Error(), 502)
		status, code := 502, "attach_failed"
		var refused *fedTerminalRefusal
		if errors.As(err, &refused) {
			status, code = 403, "denied"
			if strings.Contains(refused.reason, "limit") {
				status, code = 429, "limit"
			}
		}
		writeError(w, status, code, err.Error())
		return
	}
	v.addCleanup(func() { _ = conn.Close() })
	if browser {
		serveDashboardFederationTerminal(w, r, rt, v, conn, peer, p)
		return
	}
	// No browser is allowed to turn an ambient operator credential into a
	// terminal bridge. This endpoint is the Unix-socket CLI transport only.
	if r.Header.Get("Origin") != "" {
		writeError(w, 403, "origin", "browser origins are not supported")
		return
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "" }}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(terminal.MaxPayload + 5)
	v.addCleanup(func() { _ = ws.Close() })
	fedTerminalAudit("sessions.attach.open", caller, peer.InstanceID, p.Agent, p.Group, "viewer="+v.ID+" read_only="+fmt.Sprint(readOnly), 200)
	defer fedTerminalAudit("sessions.attach.close", caller, peer.InstanceID, p.Agent, p.Group, "viewer="+v.ID, 200)
	authorized := func() bool {
		if v.ctx.Err() != nil || !rt.sessionPeerOnline(peer.InstanceID) {
			return false
		}
		trusted, _ := db.GetFederationPeer(peer.InstanceID)
		if trusted == nil {
			return false
		}
		if !human {
			allowed, _, err := permissionAllowsAction(r, caller, fedTerminalSlug(readOnly), ActionContext{RemotePeer: peer.InstanceID, RemoteGroup: p.Group})
			if err != nil || !allowed {
				return false
			}
		}
		cat, _, _ := fedCatalogFor(peer.InstanceID)
		if cat == nil {
			return false
		}
		for _, g := range cat.Groups {
			if g.Name != p.Group || !g.HasCap(fedTerminalCap(readOnly)) {
				continue
			}
			for _, s := range g.Sessions {
				if s.Agent == p.Agent && s.Session == p.Session && s.Incarnation == p.Incarnation {
					return true
				}
			}
		}
		return false
	}
	errors := make(chan error, 2)
	go func() {
		for {
			f, err := terminal.Read(conn)
			if err != nil {
				errors <- err
				return
			}
			if !authorized() {
				errors <- terminal.ErrProtocol
				return
			}
			raw, _ := terminal.Encode(f)
			if err := ws.WriteMessage(websocket.BinaryMessage, raw); err != nil {
				errors <- err
				return
			}
		}
	}()
	go func() {
		for {
			kind, raw, err := ws.ReadMessage()
			if err != nil {
				errors <- err
				return
			}
			if kind != websocket.BinaryMessage {
				errors <- terminal.ErrProtocol
				return
			}
			f, err := terminal.Decode(raw)
			if err != nil {
				errors <- err
				return
			}
			if readOnly && f.Kind == terminal.Input {
				errors <- terminal.ErrProtocol
				return
			}
			if f.Kind != terminal.Input && f.Kind != terminal.Resize && f.Kind != terminal.Credit {
				errors <- terminal.ErrProtocol
				return
			}
			if !authorized() {
				errors <- terminal.ErrProtocol
				return
			}
			if err := terminal.Write(conn, f); err != nil {
				errors <- err
				return
			}
		}
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-errors:
			return
		case <-v.ctx.Done():
			return
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if !authorized() {
				return
			}
		}
	}
}
func queryInt(r *http.Request, name string, fallback int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return fallback
	}
	var n int
	if _, err := fmt.Sscan(v, &n); err != nil {
		return 0
	}
	return n
}

func (rt *fedRuntime) acceptSessionOpen(peer *db.FederationPeer, env *proto.Envelope) {
	var p proto.SessionOpenPayload
	if env.DecodePayload(&p) != nil || !proto.ValidStreamID(p.Stream) || !proto.ValidAgentRef(p.Agent) || len(p.Key) != 32 {
		return
	}
	var v *fedTerminalView
	answer := func(ok bool, key []byte, reason string) {
		cols, rows := 0, 0
		if v != nil {
			cols, rows = v.cols, v.rows
		}
		rt.sendControl(peer.InstanceID, proto.KindSessionAnswer, env.ID, proto.SessionAnswerPayload{Stream: p.Stream, OK: ok, Key: key, Reason: reason, Cols: cols, Rows: rows})
	}
	if p.Cols < 1 || p.Rows < 1 || p.Cols > 1000 || p.Rows > 1000 {
		answer(false, nil, "invalid terminal dimensions")
		return
	}
	// Reserve admission before spawning or performing database/tmux probes.
	var err error
	v, err = rt.addTerminal(peer.InstanceID, p, true)
	if err != nil {
		answer(false, nil, err.Error())
		return
	}
	rt.wg.Add(1)
	go func() { defer rt.wg.Done(); rt.handleSessionOpen(peer, p, v, answer) }()
}
func (rt *fedRuntime) handleSessionOpen(peer *db.FederationPeer, p proto.SessionOpenPayload, v *fedTerminalView, answer func(bool, []byte, string)) {
	defer v.close()
	pin, err := resolveFedPane(peer.InstanceID, p)
	if err != nil {
		answer(false, nil, err.Error())
		recordFederationAudit("sessions.attach.open", peerDisplay(peer), "", p.Group, "refused: "+err.Error(), 403)
		return
	}
	if p.FixedSize {
		cols, rows, err := pin.size()
		if err != nil {
			answer(false, nil, "target terminal size unavailable")
			return
		}
		p.Cols, p.Rows = cols, rows
	}
	restore, err := setFedIndicator(pin, v.ID, peerDisplay(peer), p.ReadOnly)
	if err != nil {
		answer(false, nil, "could not install remote viewer indicator")
		return
	}
	v.addCleanup(restore)
	backend, err := openFedPaneTerminal(pin, p.Cols, p.Rows)
	if err != nil {
		answer(false, nil, err.Error())
		return
	}
	v.addCleanup(func() { _ = backend.Close() })
	kp, err := stream.NewKeyPair()
	if err != nil {
		answer(false, nil, "stream unavailable")
		return
	}
	if v.ctx.Err() != nil {
		answer(false, nil, "viewer closed")
		return
	}
	v.cols, v.rows = p.Cols, p.Rows
	answer(true, kp.Pub, "")
	conn, err := rt.joinStream(v.ctx, peer.InstanceID, p.Stream, kp, p.Key, false)
	if err != nil {
		return
	}
	v.addCleanup(func() { _ = conn.Close() })
	if !pin.authorized(peer.InstanceID, p) || !fedIndicatorPresent(pin.window) {
		return
	}
	recordFederationAudit("sessions.attach.open", peerDisplay(peer), pin.conv, p.Group, "viewer="+v.ID+" read_only="+fmt.Sprint(p.ReadOnly), 200)
	defer recordFederationAudit("sessions.attach.close", peerDisplay(peer), pin.conv, p.Group, "viewer="+v.ID, 200)
	rt.servePaneTerminal(v, conn, backend, func() bool {
		return v.ctx.Err() == nil && pin.authorized(peer.InstanceID, p) && fedIndicatorPresent(pin.window)
	})
}
func (rt *fedRuntime) servePaneTerminal(v *fedTerminalView, conn io.ReadWriteCloser, backend *fedPaneTerminal, authorized func() bool) {
	reason := "exit"
	window := terminal.NewWindow()
	v.addCleanup(window.Close)
	var writeMu sync.Mutex
	write := func(f terminal.Frame) error { writeMu.Lock(); defer writeMu.Unlock(); return terminal.Write(conn, f) }
	var workers sync.WaitGroup
	workers.Add(2)
	defer func() {
		v.mu.Lock()
		if v.reason != "" {
			reason = v.reason
		}
		v.mu.Unlock()
		if reason == "revoked" {
			reason = backend.pin.closureReason(v.Peer, proto.SessionOpenPayload{ReadOnly: v.ReadOnly})
		}
		writeTerminalClosed(conn, &writeMu, reason)
		v.close()
		workers.Wait()
	}()

	errs := make(chan error, 2)
	go func() {
		defer workers.Done()
		b := make([]byte, terminal.MaxPayload)
		for {
			n, err := window.Take(len(b))
			if err != nil {
				errs <- err
				return
			}
			read, err := backend.Read(b[:n])
			if read < n {
				_ = window.Grant(n - read)
			}
			if read > 0 {
				if !authorized() {
					errs <- terminal.ErrProtocol
					return
				}
				if err := write(terminal.Frame{Kind: terminal.Output, Data: b[:read]}); err != nil {
					errs <- err
					return
				}
			}
			if err != nil {
				errs <- err
				return
			}
		}
	}()
	go func() {
		defer workers.Done()
		for {
			f, err := terminal.Read(conn)
			if err != nil {
				errs <- err
				return
			}
			if !authorized() {
				errs <- errors.New("session permission or identity changed")
				return
			}
			switch f.Kind {
			case terminal.Credit:
				n, err := terminal.ParseNumber(f.Data)
				if err == nil {
					err = window.Grant(n)
				}
				if err != nil {
					errs <- err
					return
				}
			case terminal.Input:
				if v.ReadOnly || len(f.Data) > 1024 {
					errs <- terminal.ErrProtocol
					return
				}
				if err := backend.Input(f.Data); err != nil {
					errs <- err
					return
				}
				if len(f.Data) > 0 {
					if err := write(terminal.Frame{Kind: terminal.Credit, Data: terminal.Number(len(f.Data))}); err != nil {
						errs <- err
						return
					}
				}
			case terminal.Resize:
				c, r, err := terminal.ParseSize(f.Data)
				if err != nil {
					errs <- err
					return
				}
				if err := backend.Resize(c, r); err != nil {
					errs <- err
					return
				}
			default:
				errs <- terminal.ErrProtocol
				return
			}
		}
	}()
	inputTicker := time.NewTicker(30 * time.Millisecond)
	defer inputTicker.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case err := <-errs:
			if !authorized() {
				reason = "revoked"
			} else if err != io.EOF {
				reason = "error"
			}
			return
		case <-v.ctx.Done():
			return
		case <-inputTicker.C:
			if v.ReadOnly || !backend.InputPending() {
				continue
			}
			if !authorized() || backend.FlushInput() != nil {
				reason = "revoked"
				return
			}
		case <-ticker.C:
			if v.fixedSize {
				cols, rows, err := backend.pin.size()
				if err != nil {
					reason = "exit"
					return
				}
				if cols != v.cols || rows != v.rows {
					if !authorized() {
						reason = "revoked"
						return
					}
					if backend.Resize(cols, rows) != nil || write(terminal.Frame{Kind: terminal.Resize, Data: terminal.Size(cols, rows)}) != nil {
						reason = "error"
						return
					}
					v.cols, v.rows = cols, rows
				}
			}
			if !authorized() {
				reason = "revoked"
				return
			}
		}
	}
}

func handleFederationViewers(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "list remote terminal viewers") {
		return
	}
	out := []*fedTerminalView{}
	rt := currentFederation()
	if rt != nil {
		rt.terminalsMu.Lock()
		for _, v := range rt.terminalsLocked().views {
			if v.Incoming && (r.URL.Query().Get("session") == "" || r.URL.Query().Get("session") == v.Agent || r.URL.Query().Get("session") == v.Session) {
				out = append(out, v)
			}
		}
		rt.terminalsMu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	writeJSON(w, 200, out)
}
func handleFederationKick(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "kick a remote terminal viewer") {
		return
	}
	rt := currentFederation()
	if rt == nil {
		writeError(w, 404, "not_found", "no such remote viewer")
		return
	}
	rt.terminalsMu.Lock()
	v := rt.terminalsLocked().views[r.PathValue("id")]
	rt.terminalsMu.Unlock()
	if v == nil || !v.Incoming {
		writeError(w, 404, "not_found", "no such incoming remote viewer")
		return
	}
	v.mu.Lock()
	v.reason = "kicked"
	v.mu.Unlock()
	v.cancel()
	fedTerminalAudit("sessions.attach.kick", "", v.Peer, v.Agent, v.Group, "viewer="+v.ID, 200)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

type fedTerminalRefusal struct{ reason string }

func (e *fedTerminalRefusal) Error() string { return "peer refused attach: " + e.reason }

// A stuck output writer must never retain a kicked viewer, PTY or indicator.
// Notification is best effort; abort interrupts both a prior blocked writer
// and the notification itself before normal viewer cleanup proceeds.
func writeTerminalClosed(conn io.ReadWriteCloser, writeMu *sync.Mutex, reason string) {
	timer := time.AfterFunc(200*time.Millisecond, func() { _ = conn.Close() })
	defer timer.Stop()
	writeMu.Lock()
	defer writeMu.Unlock()
	_ = terminal.Write(conn, terminal.Frame{Kind: terminal.Closed, Data: []byte(reason)})
}
