package hub

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/federation/stream"
)

// Board streams reuse authenticated FIN framing from bundle transfer. The
// payload is already end-to-end encrypted before it reaches the storage hub.
func (h *Hub) serveBoardBlob(c *conn) {
	defer h.wg.Done()
	defer h.unregister(c)
	var frame proto.Frame
	if e := c.ws.ReadJSON(&frame); e != nil {
		return
	}
	req := frame.BoardRequest
	if frame.Type != proto.FrameBoardRequest || req == nil || req.Verify(c.pub, h.hubID, c.nonce, time.Now()) != nil {
		return
	}
	result := &proto.HubAdminResult{ID: req.ID, Status: 200}
	var p boardParams
	fail := func(e error) {
		a := wrapAdminError(e)
		result.Status = a.Status
		result.Code = a.Code
		result.Error = a.Message
		_ = c.write(&proto.Frame{Type: proto.FrameBoardResult, BoardResult: result}, 5*time.Second)
	}
	if json.Unmarshal(req.Payload, &p) != nil || (req.Method != "blobs.put" && req.Method != "blobs.get") {
		fail(adminErr(403, proto.CodeBoardOnly, "board streams accept blob put/get only"))
		return
	}
	retired, e := h.store.IdentityRetired(c.id)
	if e != nil || retired {
		fail(adminErr(403, "identity_retired", "identity unavailable"))
		return
	}
	if !c.limiter.allow(len(req.Payload), time.Now()) {
		fail(adminErr(429, "rate_limited", "board request rate limit exceeded"))
		return
	}
	if _, e = h.store.boardCall(c.id, c.pub, req, false); e != nil {
		fail(e)
		return
	}
	kp, e := stream.NewKeyPair()
	if e != nil {
		fail(e)
		return
	}
	keys, e := stream.DeriveKeys(kp, p.Key, req.ID, false)
	if e != nil {
		fail(adminErr(400, "stream_key", "invalid stream key"))
		return
	}
	if e = h.store.AuditAdmin(c.id, req.ID, "board."+req.Method+".start", 200, ""); e != nil {
		fail(e)
		return
	}
	result.Body, _ = json.Marshal(map[string]any{"key": kp.Pub})
	if e = c.write(&proto.Frame{Type: proto.FrameBoardResult, BoardResult: result}, 5*time.Second); e != nil {
		return
	}
	c.ws.SetReadLimit(proto.MaxStreamMessage + 4096)
	conn, e := stream.New(c.ws, keys)
	if e != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Minute))
	h.mu.Lock()
	st := h.streamsLocked()
	lim := st.limiters[c.id]
	if lim == nil {
		lim = newByteLimiter(h.config().StreamBytesPerSecond)
		st.limiters[c.id] = lim
	}
	h.mu.Unlock()
	transferCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	go func() {
		select {
		case <-c.done:
			cancel()
		case <-transferCtx.Done():
		}
	}()
	limited := &boardBandwidth{rw: conn, lim: lim, done: transferCtx.Done()}
	if req.Method == "blobs.put" {
		e = h.store.StoreBoardBlob(c.id, p.Board, p.Blob, p.Digest, p.Bytes, limited)
		status := 200
		code := ""
		if e != nil {
			a := wrapAdminError(e)
			status = a.Status
			code = a.Code
		}
		if auditErr := h.store.AuditAdmin(c.id, req.ID, "board.blobs.put.complete", status, code); auditErr != nil {
			e = auditErr
		}
		if e == nil {
			_, e = conn.Write([]byte("ok"))
			if e == nil {
				_ = conn.CloseWrite()
			}
		}
	} else {
		e = h.store.ReadBoardBlob(c.id, p.Board, p.Blob, limited)
		status := 200
		code := ""
		if e != nil {
			a := wrapAdminError(e)
			status = a.Status
			code = a.Code
		}
		if auditErr := h.store.AuditAdmin(c.id, req.ID, "board.blobs.get.complete", status, code); auditErr != nil {
			e = auditErr
		}
		if e == nil {
			_ = conn.CloseWrite()
		}
	}
}

type boardBandwidth struct {
	rw   io.ReadWriter
	lim  *byteLimiter
	done <-chan struct{}
}

func (b *boardBandwidth) Read(p []byte) (int, error) {
	if len(p) > 32<<10 {
		p = p[:32<<10]
	}
	n, e := b.rw.Read(p)
	if n > 0 && !b.lim.wait(n, b.done) {
		return 0, errors.New("board stream closed")
	}
	return n, e
}
func (b *boardBandwidth) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n := len(p)
		if n > 32<<10 {
			n = 32 << 10
		}
		if !b.lim.wait(n, b.done) {
			return written, errors.New("board stream closed")
		}
		m, e := b.rw.Write(p[:n])
		written += m
		if e != nil {
			return written, e
		}
		if m != n {
			return written, io.ErrShortWrite
		}
		p = p[n:]
	}
	return written, nil
}
