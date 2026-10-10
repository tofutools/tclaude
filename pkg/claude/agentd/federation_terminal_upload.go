package agentd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/federation/stream"
)

var terminalUploadType = bundletransfer.Type{Name: "terminal-image", MaxBytes: spawnAttachmentMaxTotalBytes + 1<<20}

const terminalUploadTimeout = 2 * time.Minute

type fedTerminalUpload struct {
	bundletransfer.Request
	Descriptor  bundletransfer.Descriptor `json:"descriptor"`
	Target      proto.SessionOpenPayload  `json:"target"`
	ContentType string                    `json:"content_type"`
}
type fedTerminalUploadReply struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

func remoteTerminalPath(raw string) (*url.URL, bool) {
	u, err := url.ParseRequestURI(raw)
	return u, err == nil && !u.IsAbs() && u.Host == "" && u.Fragment == "" && u.Path == "/api/federation/terminal"
}

func handleDashboardFederationTerminalAttachments(w http.ResponseWriter, r *http.Request) {
	u, ok := remoteTerminalPath(r.URL.Query().Get("terminal"))
	if !ok {
		writeError(w, 400, "terminal", "expected a same-origin remote terminal path")
		return
	}
	if u.Query().Get("mode") != "interactive" {
		writeError(w, 403, "permission_denied", "image paste requires interactive attach")
		return
	}
	peer, p, err := resolveFedTerminalTarget(u.Query().Get("agent")+"@"+u.Query().Get("peer"), false)
	if err != nil {
		writeError(w, 403, "permission_denied", err.Error())
		return
	}
	rt := currentFederation()
	if rt == nil || !rt.sessionPeerOnline(peer.InstanceID) {
		writeError(w, 503, "offline", "federation peer is offline")
		return
	}
	key := "terminal-upload-out/" + peer.InstanceID
	if !rt.reserveBundleTransfer(key) {
		writeError(w, 429, "limit", "terminal upload busy")
		return
	}
	defer rt.releaseBundleTransfer(key)
	if len(r.Header.Get("Content-Type")) > 512 {
		writeError(w, 400, "multipart", "invalid multipart content type")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), terminalUploadTimeout)
	defer cancel()
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(terminalUploadTimeout))
	stopBody := context.AfterFunc(ctx, func() { _ = r.Body.Close() })
	defer stopBody()
	// Keep large images off control frames and out of memory. Only this private
	// temporary file is sent; the peer never chooses a host-local source path.
	file, err := os.CreateTemp("", "tclaude-terminal-upload-")
	if err != nil {
		writeFedErr(w, err)
		return
	}
	defer os.Remove(file.Name())
	defer file.Close()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(r.Body, terminalUploadType.MaxBytes+1))
	if err != nil || n == 0 || n > terminalUploadType.MaxBytes {
		writeError(w, 413, "too_large", "terminal upload is incomplete or exceeds its cap")
		return
	}
	_, err = file.Seek(0, io.SeekStart)
	if err != nil {
		writeFedErr(w, err)
		return
	}
	d := bundletransfer.Descriptor{ID: proto.NewEnvelopeID(), Type: terminalUploadType.Name, Bytes: n, SHA256: hex.EncodeToString(hash.Sum(nil)), ExpiresAt: time.Now().Add(terminalUploadTimeout)}
	kp, err := stream.NewKeyPair()
	if err != nil {
		writeFedErr(w, err)
		return
	}
	sid := proto.NewEnvelopeID()
	ch := make(chan bundletransfer.Answer, 1)
	rt.bundleMu.Lock()
	if rt.bundleWaiters == nil {
		rt.bundleWaiters = map[string]fedBundleWaiter{}
	}
	rt.bundleWaiters[sid] = fedBundleWaiter{Peer: peer.InstanceID, Offer: d.ID, Digest: d.SHA256, Answer: ch}
	rt.bundleMu.Unlock()
	defer func() { rt.bundleMu.Lock(); delete(rt.bundleWaiters, sid); rt.bundleMu.Unlock() }()
	req := fedTerminalUpload{Request: bundletransfer.Request{Offer: d.ID, Stream: sid, SHA256: d.SHA256, Key: kp.Pub}, Descriptor: d, Target: p, ContentType: r.Header.Get("Content-Type")}
	if !rt.sendControl(peer.InstanceID, proto.KindTerminalUpload, "", req) {
		writeError(w, 503, "offline", "peer unavailable")
		return
	}
	var answer bundletransfer.Answer
	select {
	case answer = <-ch:
	case <-ctx.Done():
		writeError(w, 504, "timeout", "terminal upload timed out")
		return
	}
	if !answer.OK {
		writeError(w, 403, "permission_denied", proto.StripControls(answer.Reason))
		return
	}
	conn, err := rt.joinStream(ctx, peer.InstanceID, sid, kp, answer.Key, true)
	if err != nil {
		writeError(w, 502, "upload", err.Error())
		return
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	_ = conn.SetDeadline(time.Now().Add(terminalUploadTimeout))
	_, err = io.CopyN(conn, file, n)
	if err == nil {
		err = conn.CloseWrite()
	}
	if err != nil {
		writeError(w, 502, "upload", "image transfer failed")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(conn, 64<<10))
	var reply fedTerminalUploadReply
	if err != nil || len(raw) >= 64<<10 || json.Unmarshal(raw, &reply) != nil || reply.Status < 200 || reply.Status > 599 || !json.Valid(reply.Body) {
		writeError(w, 502, "upload", "invalid or incomplete upload reply")
		return
	}
	trusted, _ := db.GetFederationPeer(peer.InstanceID)
	if trusted == nil {
		writeError(w, 403, "permission_denied", "peer no longer trusted")
		return
	}
	fedTerminalAudit("sessions.attach.upload", "", peer.InstanceID, p.Agent, p.Group, "image paste", reply.Status)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.WriteHeader(reply.Status)
	_, _ = w.Write(reply.Body)
}

func (rt *fedRuntime) acceptTerminalUpload(peer *db.FederationPeer, env *proto.Envelope) {
	var req fedTerminalUpload
	if env.From.Agent != "" || env.To.Agent != "" || env.DecodePayload(&req) != nil || !proto.ValidStreamID(req.Stream) || len(req.Key) != 32 || req.Target.ReadOnly || req.Descriptor.Validate(terminalUploadType, time.Now()) != nil || len(req.Descriptor.Inline) != 0 || req.Offer != req.Descriptor.ID || req.SHA256 != req.Descriptor.SHA256 || len(req.ContentType) > 512 {
		return
	}
	answer := bundletransfer.Answer{Request: req.Request}
	refuse := func(reason string) {
		answer.Reason = reason
		rt.sendControl(peer.InstanceID, proto.KindBundleAnswer, env.ID, answer)
		recordFederationAudit("sessions.attach.upload", peerDisplay(peer), "", req.Target.Group, "refused: "+reason, 403)
	}
	if !rt.allowInbound(peer.InstanceID) {
		refuse("upload rate exceeded")
		return
	}
	fresh, err := db.MarkFederationEnvelopeSeen(peer.InstanceID, "terminalupload:"+env.ID, time.Now().Add(terminalUploadTimeout))
	if err != nil || !fresh {
		return
	}
	pin, err := resolveFedPane(peer.InstanceID, req.Target)
	if err != nil {
		refuse(err.Error())
		return
	}
	key := "terminal-upload-in/" + peer.InstanceID
	if !rt.reserveBundleTransfer(key) {
		refuse("terminal upload busy")
		return
	}
	rt.wg.Add(1)
	go func() {
		defer rt.wg.Done()
		defer rt.releaseBundleTransfer(key)
		rt.receiveTerminalUpload(peer, env, req, pin, answer)
	}()
}

func (rt *fedRuntime) receiveTerminalUpload(peer *db.FederationPeer, env *proto.Envelope, req fedTerminalUpload, pin *fedPanePin, answer bundletransfer.Answer) {
	ctx, cancel := context.WithDeadline(rt.ctx, req.Descriptor.ExpiresAt)
	defer cancel()
	kp, err := stream.NewKeyPair()
	if err != nil {
		return
	}
	answer.OK = true
	answer.Key = kp.Pub
	if !rt.sendControl(peer.InstanceID, proto.KindBundleAnswer, env.ID, answer) {
		return
	}
	conn, err := rt.joinStream(ctx, peer.InstanceID, req.Stream, kp, req.Key, false)
	if err != nil {
		return
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	_ = conn.SetDeadline(time.Now().Add(terminalUploadTimeout))
	// Check the exact same pinned attach authority while receiving, then again
	// before and after staging. Neither watch nor an old incarnation can upload.
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !pin.authorized(peer.InstanceID, req.Target) {
					cancel()
					return
				}
			}
		}
	}()
	d := req.Descriptor
	defer fedBundleSpool().Remove("in", peer.InstanceID, d.ID)
	if fedBundleSpool().Receive("in", peer.InstanceID, d, conn) != nil {
		return
	}
	if !pin.authorized(peer.InstanceID, req.Target) {
		return
	}
	file, err := fedBundleSpool().Open("in", peer.InstanceID, d)
	if err != nil {
		return
	}
	defer file.Close()
	base, create, status, err := terminalAttachmentBase("/api/spawn-focus-ws/"+url.PathEscape(pin.session), session.IsTmuxSessionAlive)
	rec := &peerViewResponse{header: make(http.Header)}
	if err != nil {
		writeError(rec, status, "terminal", err.Error())
	} else {
		r := &http.Request{Method: "POST", Header: make(http.Header), Body: io.NopCloser(&terminalUploadReader{ctx: ctx, r: file})}
		r.Header.Set("Content-Type", req.ContentType)
		storeDashboardAttachmentBatch(rec, r, base, create, true)
	}
	if rec.status == 200 && !pin.authorized(peer.InstanceID, req.Target) {
		var staged spawnAttachmentsResponse
		_ = json.Unmarshal(rec.body.Bytes(), &staged)
		if staged.Dir != "" {
			_ = removeDaemonStagedAttachmentBatch(staged.Dir)
		}
		return
	}
	recordFederationAudit("sessions.attach.upload", peerDisplay(peer), pin.conv, req.Target.Group, "image paste", rec.status)
	raw, err := json.Marshal(fedTerminalUploadReply{Status: rec.status, Body: rec.body.Bytes()})
	if err == nil {
		_, err = conn.Write(raw)
		if err == nil {
			_ = conn.CloseWrite()
		}
	}
}

type terminalUploadReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *terminalUploadReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
