package hub

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// Board connections have a distinct registry slot and never enter the fleet
// directory. A fleet-admitted key on this endpoint is still board-scoped.
func (c *conn) registryKey() string {
	if c.boardOnly {
		return "board:" + c.id + ":" + c.nonce
	}
	return c.id
}
func (h *Hub) serveBoardWS(w http.ResponseWriter, r *http.Request) {
	ws, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(proto.MaxAdminPayload + 16384)
	nonce := randHex(16)
	deadline := time.Now().Add(h.config().HelloTimeout)
	_ = ws.SetWriteDeadline(deadline)
	_ = ws.SetReadDeadline(deadline)
	if err = ws.WriteJSON(&proto.Frame{Type: proto.FrameChallenge, HubID: h.hubID, Nonce: nonce, Proto: proto.ProtocolVersion}); err != nil {
		_ = ws.Close()
		return
	}
	var hello proto.Frame
	refuse := func(code, message string) {
		_ = ws.WriteJSON(&proto.Frame{Type: proto.FrameError, Code: code, Message: message})
		_ = ws.Close()
	}
	if err = ws.ReadJSON(&hello); err != nil {
		_ = ws.Close()
		return
	}
	if hello.Type != proto.FrameHello || hello.Proto != proto.ProtocolVersion || !proto.VerifyHello(&hello, h.hubID, nonce) {
		refuse(proto.CodeBadAuth, "invalid board hello")
		return
	}
	if hello.Invite != "" || len(hello.RotationChain) > 0 {
		refuse(proto.CodeBoardOnly, "board connections do not accept fleet invitations or identity actions")
		return
	}
	if hello.BoardToken != "" {
		if _, err = h.store.redeemBoardInvite(hello.BoardToken, hello.InstanceID, hello.PubKey, time.Now()); err != nil {
			refuse("board_invite", "board invitation refused")
			return
		}
	}
	fleet, err := h.store.Get(hello.InstanceID)
	if err != nil {
		refuse(proto.CodeNotAdmitted, "admission lookup failed")
		return
	}
	retired, err := h.store.IdentityRetired(hello.InstanceID)
	if err != nil || retired {
		refuse(proto.CodeNotAdmitted, "identity retired or conflicted")
		return
	}
	if !h.store.hasBoardMembership(hello.InstanceID) && (fleet == nil || fleet.Revoked) {
		refuse(proto.CodeNotAdmitted, "board membership required")
		return
	}
	generation, err := h.store.AdminGeneration()
	if err != nil {
		refuse(proto.CodeNotAdmitted, "hub state unavailable")
		return
	}
	if err = ws.WriteJSON(&proto.Frame{Type: proto.FrameWelcome, HubID: h.hubID, InstanceID: hello.InstanceID, AdminGeneration: generation}); err != nil {
		_ = ws.Close()
		return
	}
	_ = ws.SetReadDeadline(time.Now().Add(h.config().ConnectionIdle))
	_ = ws.SetWriteDeadline(time.Time{})
	ws.SetPongHandler(func(string) error { _ = ws.SetReadDeadline(time.Now().Add(h.config().ConnectionIdle)); return nil })
	h.mu.Lock()
	lim := h.limiters[hello.InstanceID]
	if lim == nil {
		lim = newBucketPair(h.config().FramesPerMinute, h.config().BytesPerMinute)
		h.limiters[hello.InstanceID] = lim
	}
	h.mu.Unlock()
	c := &conn{hub: h, ws: ws, id: hello.InstanceID, pub: hello.PubKey, nonce: nonce, boardOnly: true, boardStream: r.URL.Path == proto.BoardStreamPath, out: make(chan *proto.Frame, 256), done: make(chan struct{}), limiter: lim}
	if !h.register(c) {
		c.fail(proto.CodeShuttingDown, "hub shutting down")
		return
	}
	if c.boardStream {
		h.serveBoardBlob(c)
		return
	}
	go func() { defer h.wg.Done(); c.writeLoop() }()
	h.readLoop(c)
	h.unregister(c)
}

func (h *Hub) boardRequest(c *conn, f *proto.Frame, size int) {
	req := f.BoardRequest
	if req == nil || !proto.ValidStreamID(req.ID) {
		c.send(&proto.Frame{Type: proto.FrameError, Code: proto.CodeBadFrame, Message: "invalid board request"})
		return
	}
	result := &proto.HubAdminResult{ID: req.ID, Status: 200, Generation: req.Generation}
	fail := func(err error) {
		e := wrapAdminError(err)
		result.Status, result.Code, result.Error = e.Status, e.Code, e.Message
	}
	if !c.boardOnly {
		fail(adminErr(403, proto.CodeBoardOnly, "use the board connection endpoint"))
	} else if !c.limiter.allow(size, time.Now()) {
		fail(adminErr(429, "rate_limited", "board rate limit exceeded"))
	} else if req.Verify(c.pub, h.hubID, c.nonce, time.Now()) != nil {
		fail(adminErr(403, "bad_auth", "board request signature or validity check failed"))
	} else {
		retired, e := h.store.IdentityRetired(c.id)
		fleet, err := h.store.Get(c.id)
		if e != nil || retired {
			fail(adminErr(403, "identity_retired", "identity retired or conflicted"))
		} else if err != nil {
			fail(err)
		} else {
			body, err := h.store.boardCall(c.id, c.pub, req, fleet != nil && !fleet.Revoked)
			if err != nil {
				fail(err)
			} else {
				result.Body, err = json.Marshal(body)
				if err != nil {
					fail(err)
				} else if len(result.Body) > proto.MaxAdminResult {
					fail(adminErr(413, "result_limit", "board result exceeds limit"))
					result.Body = nil
				}
			}
		}
	}
	// Never audit token packages, keys, titles or unrecognized method strings.
	operation := "board.unknown"
	if boardMethodKnown(req.Method) {
		operation = "board." + req.Method
	}
	if err := h.store.AuditAdmin(c.id, req.ID, operation, result.Status, result.Code); err != nil {
		fail(adminErr(500, "audit", "board audit persistence failed"))
	}
	_ = c.write(&proto.Frame{Type: proto.FrameBoardResult, BoardResult: result}, 5*time.Second)
}
func boardMethodKnown(method string) bool {
	switch method {
	case "blobs.put", "blobs.get", "items.publish", "items.list", "items.versions", "items.get", "pins.set", "pins.list", "boards.list", "boards.create", "boards.get", "members.list", "members.set", "members.remove", "invites.create", "invites.list", "invites.revoke", "keys.get", "keys.rotate", "keys.join", "keys.install":
		return true
	}
	return false
}

// A board-scoped connection accepts exactly one request frame. Every new frame
// is denied unless deliberately added here; RPC methods are independently
// default-deny. Direct entry guards protect against future dispatch refactors.
func boardFrameAllowed(frame string) bool { return frame == proto.FrameBoardRequest }
