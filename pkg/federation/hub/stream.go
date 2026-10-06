package hub

import (
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// Stream relay.
//
// Group routes need a byte stream between two instances. Both instances
// learn a random stream id from a sealed envelope exchange the hub cannot
// read, then each dials StreamPath naming that id and the instance it
// expects on the other end. The hub authenticates each dialer like a main
// connection, checks admission and shared-space visibility, pairs the two
// dialers, and from then on only forwards binary messages, which the
// instances encrypt end to end. It enforces per-instance concurrency and
// bandwidth caps and drops an instance's streams when it is revoked.

// streamSession is one dialer of a stream.
type streamSession struct {
	ws   *websocket.Conn
	id   string
	peer string
	sid  string
	wmu  sync.Mutex
	done chan struct{}
	once sync.Once
	// paired receives the other side once it arrives.
	paired chan *streamSession
	// ready closes once this side has been sent stream_ready; the other
	// side's forwarder holds binary data until then, so a dialer never
	// reads data where it expects the ready frame.
	ready chan struct{}
	// idle bounds one forwarded write: a receiver that stops reading for
	// this long (its route flow control holding it, or a dead peer) ends
	// the stream, just as silence on the read side does.
	idle time.Duration
}

func (s *streamSession) writeJSON(f *proto.Frame) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_ = s.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return s.ws.WriteJSON(f)
}

func (s *streamSession) writeBinary(p []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_ = s.ws.SetWriteDeadline(time.Now().Add(s.idle))
	return s.ws.WriteMessage(websocket.BinaryMessage, p)
}

// close never waits behind a forwarded write: flow control can hold one
// for up to the stream idle time, and revocation and shutdown close streams
// one after another. Closing the socket is what unblocks such a write.
func (s *streamSession) close() {
	s.once.Do(func() {
		close(s.done)
		if s.wmu.TryLock() {
			_ = s.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
			s.wmu.Unlock()
		}
		_ = s.ws.Close()
	})
}

// fail reports an error frame when nothing else is writing, and closes.
func (s *streamSession) fail(code, msg string) {
	if s.wmu.TryLock() {
		_ = s.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
		_ = s.ws.WriteJSON(&proto.Frame{Type: proto.FrameError, Code: code, Message: msg})
		s.wmu.Unlock()
	}
	s.close()
}

// streamState is the hub's stream bookkeeping, guarded by Hub.mu.
type streamState struct {
	pending  map[string]*streamSession          // stream id → first dialer
	sessions map[string]map[*streamSession]bool // instance → its live dialers
	limiters map[string]*byteLimiter
}

func (h *Hub) streamsLocked() *streamState {
	if h.streams == nil {
		h.streams = &streamState{
			pending: map[string]*streamSession{}, sessions: map[string]map[*streamSession]bool{},
			limiters: map[string]*byteLimiter{},
		}
	}
	return h.streams
}

func (h *Hub) serveStream(w http.ResponseWriter, r *http.Request) {
	ws, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(proto.MaxStreamMessage + 4<<10)
	s, err := h.streamHandshake(ws)
	if err != nil {
		h.log.Info("stream refused", "remote", r.RemoteAddr, "error", err)
		return
	}
	defer h.dropStream(s)

	// Pair with the other dialer, or wait for it.
	h.mu.Lock()
	st := h.streamsLocked()
	other := st.pending[s.sid]
	if other != nil {
		if other.id != s.peer || other.peer != s.id {
			h.mu.Unlock()
			s.fail(proto.CodeBadFrame, "stream id in use")
			return
		}
		delete(st.pending, s.sid)
	} else {
		st.pending[s.sid] = s
	}
	h.mu.Unlock()

	if other == nil {
		select {
		case other = <-s.paired:
		case <-time.After(h.cfg.StreamWait):
			h.mu.Lock()
			if st.pending[s.sid] == s {
				delete(st.pending, s.sid)
			}
			h.mu.Unlock()
			s.fail(proto.CodeStreamWait, "peer did not join the stream")
			return
		case <-s.done:
			return
		}
	} else {
		other.paired <- s
	}
	if err := s.writeJSON(&proto.Frame{Type: proto.FrameStreamReady, Stream: s.sid, Peer: s.peer}); err != nil {
		s.close()
		other.close()
		return
	}
	close(s.ready)
	// Each side forwards its own reads to the other; the first to stop
	// tears both down.
	h.forwardStream(s, other)
	other.close()
}

// forwardStream copies binary messages from src to dst, charging src's
// instance bandwidth budget.
func (h *Hub) forwardStream(src, dst *streamSession) {
	h.mu.Lock()
	lim := h.streamsLocked().limiters[src.id]
	h.mu.Unlock()
	select {
	case <-dst.ready:
	case <-dst.done:
		return
	case <-src.done:
		return
	}
	for {
		mt, p, err := src.ws.ReadMessage()
		if err != nil {
			return
		}
		_ = src.ws.SetReadDeadline(time.Now().Add(h.cfg.StreamIdle))
		if mt != websocket.BinaryMessage {
			src.fail(proto.CodeBadFrame, "only binary messages after stream_ready")
			return
		}
		if !lim.wait(len(p), src.done) {
			return
		}
		if err := dst.writeBinary(p); err != nil {
			return
		}
		// A write the receiver held up is not idleness on this side.
		_ = src.ws.SetReadDeadline(time.Now().Add(h.cfg.StreamIdle))
	}
}

func (h *Hub) streamHandshake(ws *websocket.Conn) (*streamSession, error) {
	nonce := randHex(16)
	deadline := time.Now().Add(h.cfg.HelloTimeout)
	_ = ws.SetWriteDeadline(deadline)
	if err := ws.WriteJSON(&proto.Frame{Type: proto.FrameChallenge, HubID: h.hubID, Nonce: nonce, Proto: proto.ProtocolVersion}); err != nil {
		_ = ws.Close()
		return nil, err
	}
	_ = ws.SetReadDeadline(deadline)
	var hello proto.Frame
	if err := ws.ReadJSON(&hello); err != nil {
		_ = ws.Close()
		return nil, err
	}
	refuse := func(code, msg string) (*streamSession, error) {
		_ = ws.WriteJSON(&proto.Frame{Type: proto.FrameError, Code: code, Message: msg})
		_ = ws.Close()
		return nil, &refusal{code, msg}
	}
	switch {
	case hello.Type != proto.FrameHello:
		return refuse(proto.CodeBadFrame, "expected hello")
	case hello.Proto != proto.ProtocolVersion:
		return refuse(proto.CodeBadVersion, "unsupported protocol version")
	case !proto.VerifyHello(&hello, h.hubID, nonce):
		return refuse(proto.CodeBadAuth, "hello signature does not verify")
	case !proto.ValidStreamID(hello.Stream) || !proto.ValidInstanceID(hello.Peer) || hello.Peer == hello.InstanceID:
		return refuse(proto.CodeBadFrame, "stream hello needs a stream id and a peer instance")
	}
	id := hello.InstanceID
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.policy.admitted[id] {
		return refuse(proto.CodeNotAdmitted, "instance "+id+" is not admitted to this hub")
	}
	if !h.policy.visible(id, hello.Peer) {
		return refuse(proto.CodeNotVisible, "peer is not visible to this instance")
	}
	st := h.streamsLocked()
	if len(st.sessions[id]) >= h.cfg.MaxStreams {
		return refuse(proto.CodeStreamLimit, "too many concurrent streams for this instance")
	}
	s := &streamSession{ws: ws, id: id, peer: hello.Peer, sid: hello.Stream, done: make(chan struct{}), paired: make(chan *streamSession, 1), ready: make(chan struct{}), idle: h.cfg.StreamIdle}
	if st.sessions[id] == nil {
		st.sessions[id] = map[*streamSession]bool{}
	}
	st.sessions[id][s] = true
	if st.limiters[id] == nil {
		st.limiters[id] = newByteLimiter(h.cfg.StreamBytesPerSecond)
	}
	_ = ws.SetReadDeadline(time.Now().Add(h.cfg.StreamIdle))
	ws.SetPongHandler(func(string) error { _ = ws.SetReadDeadline(time.Now().Add(h.cfg.StreamIdle)); return nil })
	go s.keepalive()
	return s, nil
}

func (s *streamSession) keepalive() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			s.wmu.Lock()
			err := s.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
			s.wmu.Unlock()
			if err != nil {
				s.close()
				return
			}
		}
	}
}

func (h *Hub) dropStream(s *streamSession) {
	s.close()
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.streamsLocked()
	delete(st.sessions[s.id], s)
	if len(st.sessions[s.id]) == 0 {
		delete(st.sessions, s.id)
	}
	if st.pending[s.sid] == s {
		delete(st.pending, s.sid)
	}
}

// streamsToDropLocked returns the stream sessions of instances keep rejects.
func (h *Hub) streamsToDropLocked(keep func(id string) bool) []*streamSession {
	var out []*streamSession
	if h.streams == nil {
		return nil
	}
	for id, set := range h.streams.sessions {
		if keep(id) {
			continue
		}
		for s := range set {
			out = append(out, s)
		}
	}
	return out
}

// StreamCount returns how many stream dialers are connected.
func (h *Hub) StreamCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	if h.streams != nil {
		for _, set := range h.streams.sessions {
			n += len(set)
		}
	}
	return n
}

// byteLimiter is a blocking token bucket over bytes per second: a stream
// that exceeds its instance's budget is slowed down, not dropped.
type byteLimiter struct {
	mu     sync.Mutex
	rate   float64
	tokens float64
	last   time.Time
}

func newByteLimiter(perSecond int) *byteLimiter {
	return &byteLimiter{rate: float64(perSecond), tokens: float64(perSecond), last: time.Now()}
}

// wait blocks until n bytes fit, or returns false when done closes.
func (l *byteLimiter) wait(n int, done <-chan struct{}) bool {
	for {
		l.mu.Lock()
		now := time.Now()
		l.tokens += now.Sub(l.last).Seconds() * l.rate
		if l.tokens > l.rate {
			l.tokens = l.rate
		}
		l.last = now
		if l.tokens >= float64(n) || (n > int(l.rate) && l.tokens >= l.rate) {
			l.tokens -= float64(n)
			l.mu.Unlock()
			return true
		}
		need := (float64(n) - l.tokens) / l.rate
		l.mu.Unlock()
		select {
		case <-done:
			return false
		case <-time.After(time.Duration(need * float64(time.Second))):
		}
	}
}
