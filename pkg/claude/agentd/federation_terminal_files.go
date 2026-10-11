package agentd

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/tofutools/tclaude/pkg/federation/terminal"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/federation/stream"
	"golang.org/x/sys/unix"
)

type fedTerminalFileRequest struct {
	bundletransfer.Request
	Viewer string `json:"viewer"`
	Path   string `json:"path"`
	Head   bool   `json:"head"`
	List   bool   `json:"list,omitempty"`
}
type fedTerminalFileHeader struct {
	Status int    `json:"status"`
	Code   string `json:"code,omitempty"`
	Error  string `json:"error,omitempty"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256,omitempty"`
}

func writeTerminalFileHeader(w io.Writer, h fedTerminalFileHeader) error {
	raw, err := json.Marshal(h)
	if err != nil {
		return err
	}
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(raw)))
	if _, err = w.Write(n[:]); err != nil {
		return err
	}
	_, err = w.Write(raw)
	return err
}
func readTerminalFileHeader(r io.Reader) (fedTerminalFileHeader, error) {
	var h fedTerminalFileHeader
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return h, err
	}
	size := binary.BigEndian.Uint32(n[:])
	if size == 0 || size > 4096 {
		return h, errors.New("invalid file metadata")
	}
	raw := make([]byte, size)
	_, err := io.ReadFull(r, raw)
	if err != nil {
		return h, err
	}
	err = json.Unmarshal(raw, &h)
	if h.Bytes < 0 || h.Bytes > terminalFileMaxBytes || h.Status < 200 || h.Status > 599 {
		return h, errors.New("invalid file metadata")
	}
	return h, err
}
func (rt *fedRuntime) terminalFileViewer(id, peer string, incoming bool) (*fedTerminalView, error) {
	rt.terminalsMu.Lock()
	v := rt.terminalsLocked().views[id]
	rt.terminalsMu.Unlock()
	if v == nil || v.Peer != peer || v.Incoming != incoming || v.ctx.Err() != nil {
		return nil, terminalFileRefusal(403, "viewer_closed", "the pinned terminal viewer is no longer live")
	}
	return v, nil
}
func (rt *fedRuntime) terminalFileAuthorized(v *fedTerminalView) bool {
	current, err := rt.terminalFileViewer(v.ID, v.Peer, v.Incoming)
	if err != nil || current != v {
		return false
	}
	peer, _ := db.GetFederationPeer(v.Peer)
	if peer == nil {
		return false
	}
	if !v.Incoming {
		return rt.sessionPeerOnline(v.Peer)
	}
	v.mu.Lock()
	pin := v.pin
	v.mu.Unlock()
	return pin != nil && pin.authorized(v.Peer, v.target) && fedPeerAllows(v.Peer, pin.group, PermSessionsFilesRead)
}
func terminalFileObserve(ctx context.Context, cancel context.CancelFunc, rt *fedRuntime, v *fedTerminalView) func() {
	stop := context.AfterFunc(v.ctx, cancel)
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !rt.terminalFileAuthorized(v) {
					cancel()
					return
				}
			}
		}
	}()
	return func() { stop(); close(done); <-finished }
}
func fileErrorHeader(err error) fedTerminalFileHeader {
	var e *terminalFileError
	if errors.As(err, &e) {
		return fedTerminalFileHeader{Status: e.status, Code: e.code, Error: e.message}
	}
	return fedTerminalFileHeader{Status: 403, Code: "unsafe_path", Error: "file path unavailable"}
}
func (rt *fedRuntime) acceptTerminalFile(peer *db.FederationPeer, env *proto.Envelope) {
	var req fedTerminalFileRequest
	if env.From.Agent != "" || env.To.Agent != "" || env.DecodePayload(&req) != nil || !proto.ValidStreamID(req.Stream) || !proto.ValidStreamID(req.Viewer) || req.Offer != req.Viewer || req.SHA256 != "" || len(req.Key) != 32 || len(req.Path) > 4096 {
		return
	}
	fresh, err := db.MarkFederationEnvelopeSeen(peer.InstanceID, "terminalfile:"+env.ID, time.Now().Add(terminalUploadTimeout))
	if err != nil || !fresh {
		return
	}
	answer := bundletransfer.Answer{Request: req.Request}
	refuse := func(code string) {
		answer.Reason = code
		rt.sendControl(peer.InstanceID, proto.KindBundleAnswer, env.ID, answer)
		recordFederationAudit("sessions.files.read", peerDisplay(peer), "", "", fmt.Sprintf("viewer=%s path=%q refused=%s", req.Viewer, req.Path, code), 403)
	}
	v, err := rt.terminalFileViewer(req.Viewer, peer.InstanceID, true)
	if err != nil {
		refuse("viewer_closed")
		return
	}
	if !rt.terminalFileAuthorized(v) {
		refuse("not_shared")
		return
	}
	key := "terminal-file-out/" + peer.InstanceID
	if !rt.allowInbound(peer.InstanceID) || !rt.reserveBundleTransfer(key) {
		refuse("limit")
		return
	}
	rt.wg.Add(1)
	go func() {
		defer rt.wg.Done()
		defer rt.releaseBundleTransfer(key)
		rt.sendTerminalFile(peer, env, req, v, answer)
	}()
}
func (rt *fedRuntime) sendTerminalFile(peer *db.FederationPeer, env *proto.Envelope, req fedTerminalFileRequest, v *fedTerminalView, answer bundletransfer.Answer) {
	ctx, cancel := context.WithTimeout(v.ctx, terminalUploadTimeout)
	defer cancel()
	stop := terminalFileObserve(ctx, cancel, rt, v)
	defer stop()
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
	defer func() { _ = conn.CloseWrite(); _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(terminalUploadTimeout))
	closeOnCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer closeOnCancel()
	status := 500
	var auditedBytes int64
	defer func() {
		recordFederationAudit("sessions.files.read", peerDisplay(peer), v.Agent, v.Group, fmt.Sprintf("viewer=%s path=%q bytes=%d status=%d", v.ID, req.Path, auditedBytes, status), status)
	}()
	fail := func(err error) { h := fileErrorHeader(err); status = h.Status; _ = writeTerminalFileHeader(conn, h) }
	if !rt.terminalFileAuthorized(v) {
		fail(terminalFileRefusal(403, "not_shared", "file read authority revoked"))
		return
	}
	v.mu.Lock()
	rootErr := v.fileRootErr
	rootPath := v.fileRootPath
	var root *os.File
	if v.fileRoot != nil {
		fd, e := unix.Dup(int(v.fileRoot.Fd()))
		if e == nil {
			unix.CloseOnExec(fd)
			root = os.NewFile(uintptr(fd), rootPath)
		} else {
			rootErr = e
		}
	}
	v.mu.Unlock()
	if rootErr != nil {
		fail(rootErr)
		return
	}
	if root == nil {
		fail(terminalFileRefusal(403, "viewer_closed", "viewer root unavailable"))
		return
	}
	defer func() { _ = root.Close() }()
	var f *os.File
	if req.List {
		f, err = terminalDirectoryListing(root, rootPath, req.Path)
	} else {
		f, err = openTerminalFile(root, rootPath, req.Path)
	}
	if err != nil {
		fail(err)
		return
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		fail(err)
		return
	}
	if req.Head {
		auditedBytes = info.Size()
		status = 502
		if writeTerminalFileHeader(conn, fedTerminalFileHeader{Status: 200, Bytes: info.Size()}) == nil && conn.CloseWrite() == nil {
			status = 200
		}
		return
	}
	spool, err := os.CreateTemp("", "tclaude-terminal-file-")
	if err != nil {
		fail(err)
		return
	}
	defer func() { _ = spool.Close(); _ = os.Remove(spool.Name()) }()
	hash := sha256.New()
	buf := make([]byte, 64<<10)
	var size int64
	for {
		if ctx.Err() != nil {
			fail(terminalFileRefusal(403, "not_shared", "file read authority revoked"))
			return
		}
		n, e := f.Read(buf)
		size += int64(n)
		if size > terminalFileMaxBytes {
			fail(terminalFileRefusal(413, "file_too_large", "file exceeds the 32 MiB download cap"))
			return
		}
		if n > 0 {
			if _, err = io.MultiWriter(spool, hash).Write(buf[:n]); err != nil {
				fail(err)
				return
			}
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			fail(e)
			return
		}
	}
	if _, err = spool.Seek(0, io.SeekStart); err != nil {
		fail(err)
		return
	}
	if !rt.terminalFileAuthorized(v) {
		fail(terminalFileRefusal(403, "not_shared", "file read authority revoked"))
		return
	}
	auditedBytes = size
	status = 502
	if writeTerminalFileHeader(conn, fedTerminalFileHeader{Status: 200, Bytes: size, SHA256: hex.EncodeToString(hash.Sum(nil))}) != nil {
		return
	}
	_, err = io.CopyN(conn, spool, size)
	if err != nil || !rt.terminalFileAuthorized(v) {
		status = 403
		return
	}
	if conn.CloseWrite() == nil {
		status = 200
	}
}

func (rt *fedRuntime) receiveTerminalFile(ctx context.Context, v *fedTerminalView, path string, head, list bool) (*os.File, fedTerminalFileHeader, error) {
	var h fedTerminalFileHeader
	if len(path) > 4096 || strings.ContainsRune(path, 0) {
		return nil, h, terminalFileRefusal(403, "unsafe_path", "invalid file path")
	}
	key := "terminal-file-in/" + v.Peer
	if !rt.reserveBundleTransfer(key) {
		return nil, h, terminalFileRefusal(429, "limit", "file download busy")
	}
	defer rt.releaseBundleTransfer(key)
	ctx, cancel := context.WithTimeout(ctx, terminalUploadTimeout)
	defer cancel()
	stop := terminalFileObserve(ctx, cancel, rt, v)
	defer stop()
	kp, err := stream.NewKeyPair()
	if err != nil {
		return nil, h, err
	}
	sid := proto.NewEnvelopeID()
	ch := make(chan bundletransfer.Answer, 1)
	rt.bundleMu.Lock()
	if rt.bundleWaiters == nil {
		rt.bundleWaiters = map[string]fedBundleWaiter{}
	}
	rt.bundleWaiters[sid] = fedBundleWaiter{Peer: v.Peer, Offer: v.ID, Answer: ch}
	rt.bundleMu.Unlock()
	defer func() { rt.bundleMu.Lock(); delete(rt.bundleWaiters, sid); rt.bundleMu.Unlock() }()
	req := fedTerminalFileRequest{Request: bundletransfer.Request{Offer: v.ID, Stream: sid, Key: kp.Pub}, Viewer: v.ID, Path: path, Head: head, List: list}
	if !rt.sendControl(v.Peer, proto.KindTerminalFile, "", req) {
		return nil, h, terminalFileRefusal(503, "peer_offline", "peer unavailable")
	}
	var answer bundletransfer.Answer
	select {
	case answer = <-ch:
	case <-ctx.Done():
		return nil, h, terminalFileRefusal(504, "timeout", "file download timed out")
	}
	if !answer.OK {
		return nil, h, terminalFileRefusal(403, answer.Reason, "remote file download refused: "+answer.Reason)
	}
	conn, err := rt.joinStream(ctx, v.Peer, sid, kp, answer.Key, true)
	if err != nil {
		return nil, h, err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(terminalUploadTimeout))
	closeOnCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer closeOnCancel()
	h, err = readTerminalFileHeader(conn)
	if err != nil {
		return nil, h, err
	}
	if h.Status != 200 {
		return nil, h, terminalFileRefusal(h.Status, h.Code, h.Error)
	}
	if head {
		if !rt.terminalFileAuthorized(v) {
			return nil, h, terminalFileRefusal(403, "viewer_closed", "terminal viewer is no longer authorized")
		}
		return nil, h, nil
	}
	f, err := os.CreateTemp("", "tclaude-terminal-download-")
	if err != nil {
		return nil, h, err
	}
	_ = os.Remove(f.Name()) // private descriptor only; no persistent downloaded copy.
	hash := sha256.New()
	_, err = io.CopyN(io.MultiWriter(f, hash), conn, h.Bytes)
	if err == nil {
		var extra [1]byte
		n, e := conn.Read(extra[:])
		if n != 0 || e != io.EOF {
			err = errors.New("incomplete or oversized file stream")
		}
	}
	if err == nil && hex.EncodeToString(hash.Sum(nil)) != h.SHA256 {
		err = errors.New("file checksum mismatch")
	}
	if err == nil && !rt.terminalFileAuthorized(v) {
		err = terminalFileRefusal(403, "viewer_closed", "terminal viewer is no longer authorized")
	}
	if err != nil {
		_ = f.Close()
		return nil, h, err
	}
	_, err = f.Seek(0, io.SeekStart)
	if err != nil {
		_ = f.Close()
		return nil, h, err
	}
	return f, h, nil
}
func handleDashboardFederationTerminalFile(w http.ResponseWriter, r *http.Request) {
	u, ok := remoteTerminalPath(r.URL.Query().Get("terminal"))
	if !ok {
		writeError(w, 400, "terminal", "expected a remote terminal path")
		return
	}
	rt := currentFederation()
	if rt == nil {
		writeError(w, 503, "peer_offline", "federation offline")
		return
	}
	v, err := rt.terminalFileViewer(r.URL.Query().Get("viewer"), u.Query().Get("peer"), false)
	if err == nil && (v.Agent != u.Query().Get("agent") || (u.Query().Get("mode") == "watch") != v.ReadOnly) {
		err = terminalFileRefusal(403, "viewer_closed", "terminal viewer does not match this path")
	}
	if err != nil {
		h := fileErrorHeader(err)
		writeError(w, h.Status, h.Code, h.Error)
		return
	}
	list := r.URL.Query().Get("list") == "true"
	f, h, err := rt.receiveTerminalFile(r.Context(), v, r.URL.Query().Get("path"), r.Method == "HEAD", list)
	status := h.Status
	if err != nil {
		h = fileErrorHeader(err)
		status = h.Status
	}
	fedTerminalAudit("sessions.files.read", "", v.Peer, v.Agent, v.Group, fmt.Sprintf("viewer=%s path=%q bytes=%d", v.ID, r.URL.Query().Get("path"), h.Bytes), status)
	if err != nil {
		writeError(w, h.Status, h.Code, h.Error)
		return
	}
	if f != nil {
		defer func() { _ = f.Close() }()
	}
	if list {
		w.Header().Set("Content-Type", "application/json")
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(r.URL.Query().Get("path"))}))
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Length", fmt.Sprint(h.Bytes))
	if r.Method != "HEAD" {
		_, _ = io.Copy(w, f)
	}
}

// CLI downloads establish the same target viewer and pin as a terminal attach;
// they do not create a separate file-authority path on the receiving node.
func handleFederationFileGet(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "download a remote session file") {
		return
	}
	peer, p, err := resolveFedTerminalTarget(r.URL.Query().Get("target"), true)
	if err != nil {
		peer, p, err = resolveFedTerminalTarget(r.URL.Query().Get("target"), false)
	}
	if err != nil {
		writeError(w, 403, "not_shared", err.Error())
		return
	}
	rt := currentFederation()
	if rt == nil || !rt.sessionPeerOnline(peer.InstanceID) {
		writeError(w, 503, "peer_offline", "peer offline")
		return
	}
	p.FixedSize = true
	v, err := rt.addTerminal(peer.InstanceID, p, false)
	if err != nil {
		writeError(w, 429, "limit", err.Error())
		return
	}
	defer v.close()
	conn, err := rt.openTerminalStream(v, p)
	if err != nil {
		writeError(w, 403, "not_shared", err.Error())
		return
	}
	v.addCleanup(func() { _ = conn.Close() })
	// Drain bounded terminal output and return its credit, without sending input.
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer v.cancel()
		for {
			frame, err := terminal.Read(conn)
			if err != nil {
				return
			}
			switch frame.Kind {
			case terminal.Output:
				if terminal.Write(conn, terminal.Frame{Kind: terminal.Credit, Data: terminal.Number(len(frame.Data))}) != nil {
					return
				}
			case terminal.Closed:
				v.cancel()
				return
			}
		}
	}()
	defer func() { v.cancel(); _ = conn.Close(); <-done }()
	copy := r.Clone(r.Context())
	u := *r.URL
	copy.URL = &u
	query := u.Query()
	query.Set("viewer", v.ID)
	query.Set("terminal", "/api/federation/terminal?"+url.Values{"peer": {peer.InstanceID}, "agent": {p.Agent}, "mode": {map[bool]string{true: "watch", false: "interactive"}[p.ReadOnly]}}.Encode())
	copy.URL.RawQuery = query.Encode()
	handleDashboardFederationTerminalFile(w, copy)
}
