package agentd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
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
	ID           string    `json:"id"`
	Peer         string    `json:"peer"`
	Agent        string    `json:"agent"`
	Session      string    `json:"session"`
	Group        string    `json:"group"`
	ReadOnly     bool      `json:"read_only"`
	Started      time.Time `json:"started"`
	Incoming     bool      `json:"incoming"`
	ctx          context.Context
	cancel       context.CancelFunc
	mu           sync.Mutex
	stopped      bool
	cleanup      []func()
	done         chan struct{}
	fixedSize    bool
	cols, rows   int
	reason       string
	target       proto.SessionOpenPayload
	pin          *fedPanePin
	fileRoot     *os.File
	fileRootPath string
	fileRootErr  error
	files        bool
	// Requesting side: the peer shares the window size with this view (it
	// reports its size and gets the shared grid); others: what else was
	// attached at open; canTakeOver: the peer grants sessions.takeover.
	shared      bool
	others      []string
	canTakeOver bool
	// Target side, for a ShareSize viewer (federation_terminal_share.go).
	sharing                bool
	wantCols, wantRows     int
	sharedCols, sharedRows int
	liveOthers, sentOthers []string
	sizeCh                 chan struct{}
	sizer                  *fedWindowSizer
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
	v.target = p
	st.views[v.ID] = v
	v.addCleanup(func() {
		rt.terminalsMu.Lock()
		if rt.terminalsLocked().views[v.ID] == v {
			delete(rt.terminalsLocked().views, v.ID)
		}
		rt.terminalsMu.Unlock()
	})
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
			v.files = a.answer.Files
			v.shared = p.ShareSize && a.answer.SharedSize
			v.others = sanitizeFedOthers(a.answer.Others)
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
	if !strings.HasPrefix(ref, "agt_") {
		return nil, proto.SessionOpenPayload{}, errors.New("unknown session; use the agt_…@peer address from federation sessions")
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
	for _, g := range fedTerminalGroups(cat) {
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
		// Groups granting take-over come first; an agent still needs its own
		// sessions.takeover for one to be offered (below).
		for _, g := range fedTerminalGroups(cat) {
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
	// The browser bridge always shares the size. The CLI asks with
	// share_size=1, which also opts it into a JSON hello frame and the Resize
	// and Status frames an older CLI would reject.
	p.ShareSize = browser || r.URL.Query().Get("share_size") == "1"
	p.TakeOver = p.ShareSize && r.URL.Query().Get("take_over") == "1"
	p.Cols, p.Rows = queryInt(r, "cols", 80), queryInt(r, "rows", 24)
	if p.Cols < 1 || p.Cols > 1000 || p.Rows < 1 || p.Rows > 1000 {
		writeError(w, 400, "invalid_arg", "terminal dimensions must be 1..1000")
		return
	}
	// Take-over needs the peer's sessions.takeover grant (the target checks it
	// again) and, for an agent, the matching local permission.
	canTakeOver := fedCatalogGroupHasCap(peer.InstanceID, p.Group, proto.CapSessionsTakeover)
	if canTakeOver && !human {
		yes, _, err := permissionAllowsAction(r, caller, PermSessionsTakeover, ActionContext{RemotePeer: peer.InstanceID, RemoteGroup: p.Group})
		canTakeOver = err == nil && yes
	}
	if p.TakeOver && !canTakeOver {
		writeError(w, 403, "permission_denied", "take-over requires "+PermSessionsTakeover+" scoped to peer="+peer.InstanceID+"/"+p.Group)
		return
	}
	v, err := rt.addTerminal(peer.InstanceID, p, false)
	if err != nil {
		writeError(w, 429, "limit", err.Error())
		return
	}
	v.canTakeOver = canTakeOver
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
	// Only a CLI that asked to share the size reads this one text frame; the
	// rest of the stream stays binary terminal frames.
	if p.ShareSize {
		if ws.WriteJSON(map[string]any{"type": "hello", "shared": v.shared, "others": v.others, "take_over": v.canTakeOver}) != nil {
			return
		}
	}
	fedTerminalAudit("sessions.attach.open", caller, peer.InstanceID, p.Agent, p.Group, "viewer="+v.ID+" read_only="+fmt.Sprint(readOnly), 200)
	defer fedTerminalAudit("sessions.attach.close", caller, peer.InstanceID, p.Agent, p.Group, "viewer="+v.ID, 200)
	check := func() bool {
		if !rt.sessionPeerOnline(peer.InstanceID) {
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
	// Frames rely on the cached verdict; the ticker below refreshes it.
	gate := newFedAuthGateBool(check)
	authorized := func() bool { return v.ctx.Err() == nil && gate.allowed() == "" }
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
			if gate.refresh() != "" {
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
		a := proto.SessionAnswerPayload{Stream: p.Stream, OK: ok, Key: key, Reason: reason}
		if v != nil {
			v.mu.Lock()
			a.Cols, a.Rows, a.Files = v.cols, v.rows, v.files
			if ok {
				a.SharedSize, a.Others = v.sharing, v.others
			}
			v.mu.Unlock()
		}
		rt.sendControl(peer.InstanceID, proto.KindSessionAnswer, env.ID, a)
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
	root, rootPath, rootErr := openTerminalFileRoot(pin.cwd)
	v.mu.Lock()
	v.pin, v.fileRoot, v.fileRootPath, v.fileRootErr = pin, root, rootPath, rootErr
	v.files = rootErr == nil && fedPeerAllows(peer.InstanceID, pin.group, PermSessionsFilesRead)
	// Sharing is published with the pin: other viewers' sizers find v by it.
	if p.ShareSize {
		v.sharing, v.sizeCh = true, make(chan struct{}, 1)
	}
	v.mu.Unlock()
	if root != nil {
		v.addCleanup(func() { v.mu.Lock(); _ = root.Close(); v.fileRoot = nil; v.mu.Unlock() })
	}
	// Take-over needs its own grant; this side's check is authoritative.
	if p.TakeOver && !fedPeerAllows(peer.InstanceID, pin.group, PermSessionsTakeover) {
		answer(false, nil, "take-over requires the "+PermSessionsTakeover+" grant")
		recordFederationAudit("sessions.attach.open", peerDisplay(peer), "", p.Group, "refused: take-over not granted", 403)
		return
	}
	// A take-over's displacement waits until this open can no longer fail on
	// this side; the answer already reports what else is attached.
	displace := false
	if p.ShareSize {
		var others []string
		displace, others = rt.planFedTakeOver(pin, v, p.TakeOver)
		v.mu.Lock()
		v.others, v.sentOthers, v.liveOthers = others, others, others
		v.mu.Unlock()
		// A CLI reports its real size at open; a browser reports once fitted.
		if !p.FixedSize {
			v.setWant(p.Cols, p.Rows)
		}
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
	v.mu.Lock()
	v.cols, v.rows = p.Cols, p.Rows
	v.mu.Unlock()
	answer(true, kp.Pub, "")
	conn, err := rt.joinStream(v.ctx, peer.InstanceID, p.Stream, kp, p.Key, false)
	if err != nil {
		return
	}
	v.addCleanup(func() { _ = conn.Close() })
	if !pin.authorized(peer.InstanceID, p) || !fedIndicatorPresent(pin.window) {
		return
	}
	// The viewer is live: only now displace the others for a take-over.
	if displace {
		rt.displaceFedClients(pin, v)
	}
	// A sharing viewer takes part in the window's shared size from now on.
	if v.sharing {
		sizer := rt.joinFedWindowSizer(pin)
		v.mu.Lock()
		v.sizer = sizer
		v.mu.Unlock()
		v.addCleanup(sizer.leave)
	}
	recordFederationAudit("sessions.attach.open", peerDisplay(peer), pin.conv, p.Group, "viewer="+v.ID+" read_only="+fmt.Sprint(p.ReadOnly), 200)
	defer recordFederationAudit("sessions.attach.close", peerDisplay(peer), pin.conv, p.Group, "viewer="+v.ID, 200)
	rt.servePaneTerminal(v, conn, backend, func() bool {
		return pin.authorized(peer.InstanceID, p) && fedIndicatorPresent(pin.window)
	})
}

// servePaneTerminal re-runs check (several tmux execs and database reads)
// once a second; output chunks and keystrokes rely on the cached verdict.
func (rt *fedRuntime) servePaneTerminal(v *fedTerminalView, conn io.ReadWriteCloser, backend *fedPaneTerminal, check func() bool) {
	reason := "exit"
	gate := newFedAuthGateBool(check)
	authorized := func() bool { return v.ctx.Err() == nil && gate.allowed() == "" }
	fresh := func() bool { return v.ctx.Err() == nil && gate.refresh() == "" }
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
	// rendererEnded marks the renderer's tmux client going away by itself:
	// while the session lives on, that is a detach — another terminal took it.
	var rendererEnded atomic.Bool
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
				rendererEnded.Store(true)
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
				// A sharing view reports the size it could show; the window and
				// its renderer follow the shared size (sharedUpdate below).
				if v.sharing {
					v.setWant(c, r)
					v.sizer.kick()
					continue
				}
				// A pinned browser view renders at the target's size.
				if v.fixedSize {
					continue
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
			switch {
			case !fresh():
				reason = "revoked"
			case rendererEnded.Load() && session.IsTmuxSessionAlive(backend.pin.tmux):
				reason = fedTerminalTakenOver
			case rendererEnded.Load():
				reason = "exit"
			case err != io.EOF:
				reason = "error"
			}
			return
		case <-v.ctx.Done():
			return
		case <-v.sizeCh:
			// The shared size or the other clients changed: render the grid at
			// that size and tell the viewer both.
			cols, rows, status := v.sharedUpdate()
			if cols > 0 && (cols != v.cols || rows != v.rows) {
				if !authorized() {
					reason = "revoked"
					return
				}
				// The frame goes first so a CLI clears its screen before
				// tmux's redraw at the new size arrives.
				if write(terminal.Frame{Kind: terminal.Resize, Data: terminal.Size(cols, rows)}) != nil || backend.Resize(cols, rows) != nil {
					reason = "error"
					return
				}
				v.mu.Lock()
				v.cols, v.rows = cols, rows
				v.mu.Unlock()
			}
			if status != nil && write(terminal.Frame{Kind: terminal.Status, Data: status}) != nil {
				reason = "error"
				return
			}
		case <-inputTicker.C:
			if v.ReadOnly || !backend.InputPending() {
				continue
			}
			if !authorized() || backend.FlushInput() != nil {
				reason = "revoked"
				return
			}
		case <-ticker.C:
			// A sharing view re-checks the shared size: local clients come and
			// go (and resize) without telling us. A pinned view follows the
			// target's size.
			if v.sharing {
				v.sizer.kick()
			} else if v.fixedSize {
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
			if !fresh() {
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
	// A normal kick replies after cleanup. A wedged worker must not hold the
	// operator's HTTP request indefinitely; detach its registry entry and
	// force-close in the background if the bounded wait is exhausted.
	rt.waitKickedTerminal(r.Context(), v)
	fedTerminalAudit("sessions.attach.kick", "", v.Peer, v.Agent, v.Group, "viewer="+v.ID, 200)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (rt *fedRuntime) waitKickedTerminal(ctx context.Context, v *fedTerminalView) {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-v.done:
		return
	case <-ctx.Done():
	case <-timer.C:
	}
	rt.terminalsMu.Lock()
	if rt.terminalsLocked().views[v.ID] == v {
		delete(rt.terminalsLocked().views, v.ID)
	}
	rt.terminalsMu.Unlock()
	goBackground(v.close)
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
