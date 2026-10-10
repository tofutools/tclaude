package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/jobstream"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/federation/stream"
)

// Follow is observational: losing a stream never cancels or resubmits a job.
// The immutable request fingerprint binds each stream to a previously admitted
// job. The terminal receipt and verified completed artifact remain authoritative.
func (rt *fedRuntime) serveJobFollow(peer *db.FederationPeer, env *proto.Envelope) {
	var req bundletransfer.Request
	var output fedJobOutputRequest
	var decodeErr error
	if env.Kind == proto.KindJobOutput {
		decodeErr = env.DecodePayload(&output)
		req = output.Request
	} else {
		decodeErr = env.DecodePayload(&req)
	}
	if env.From.Agent != "" || env.To.Agent != "" || decodeErr != nil || !proto.ValidStreamID(req.Offer) || !proto.ValidStreamID(req.Stream) || len(req.Key) != 32 {
		return
	}
	a := bundletransfer.Answer{Request: req}
	refuse := func(reason string) {
		a.Reason = reason
		rt.sendControl(peer.InstanceID, proto.KindJobFollowAnswer, env.ID, a)
	}
	if !rt.allowInbound(peer.InstanceID) {
		refuse("follow rate exceeded")
		return
	}
	fresh, e := db.MarkFederationEnvelopeSeen(peer.InstanceID, "jobfollow:"+env.ID, time.Now().Add(fedControlTTL+time.Minute))
	if e != nil || !fresh {
		return
	}
	j, e := db.GetFederationJob(req.Offer)
	if e != nil || j.Direction != "in" || j.Peer != peer.InstanceID || j.Fingerprint != req.SHA256 || !j.ExpiresAt.After(time.Now()) {
		refuse("job unavailable")
		return
	}
	if env.Kind == proto.KindJobOutput && !output.valid(j) {
		refuse("invalid output cursor or limit")
		return
	}
	key := "job-follow-out/" + peer.InstanceID + "/" + req.Stream
	if !rt.reserveBundleTransfer(key) {
		refuse("follow busy")
		return
	}
	defer rt.releaseBundleTransfer(key)
	path, e := federationJobLivePath(j.ID)
	if e != nil {
		return
	}
	f, e := os.Open(path)
	if e != nil {
		refuse("live output unavailable; query completed logs")
		return
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil || !info.Mode().IsRegular() || info.Size() > jobstream.MaxEncodedBytes {
		refuse("invalid live output")
		return
	}
	kp, e := stream.NewKeyPair()
	if e != nil {
		return
	}
	a.OK = true
	a.Key = kp.Pub
	if !rt.sendControl(peer.InstanceID, proto.KindJobFollowAnswer, env.ID, a) {
		return
	}
	duration := 25 * time.Hour
	if env.Kind == proto.KindJobOutput {
		duration = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(rt.ctx, duration)
	defer cancel()
	conn, e := rt.joinStream(ctx, peer.InstanceID, req.Stream, kp, req.Key, false)
	if e != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	_ = conn.SetDeadline(time.Now().Add(duration))
	if env.Kind == proto.KindJobOutput {
		result, err := readFederationJobOutput(f, j, output.Cursor, output.MaxBytes)
		if err == nil {
			// Authority is rechecked immediately before exposing the bounded reply.
			if p, err := db.GetFederationPeer(j.Peer); err == nil && p != nil {
				if json.NewEncoder(conn).Encode(result) == nil {
					_ = conn.CloseWrite()
				}
			}
		}
		return
	}
	// The initiator sends no payload. Detect disconnect while the job is idle.
	go func() { var b [1]byte; _, _ = conn.Read(b[:]); cancel() }()
	if tailJobFrames(ctx, f, j, conn) == nil {
		_ = conn.CloseWrite()
	}
}

// A reader can observe EOF halfway through a frame while the broker appends.
// Copy bytes unchanged; only the receiver decodes complete bounded frames.
func tailJobFrames(ctx context.Context, f *os.File, j *db.FederationJob, out io.Writer) error {
	buf := make([]byte, 32<<10)
	total := 0
	for {
		n, e := f.Read(buf)
		if n > 0 {
			total += n
			if total > jobstream.MaxEncodedBytes {
				return errors.New("live output limit")
			}
			if _, err := out.Write(buf[:n]); err != nil {
				return err
			}
		}
		if e != nil && e != io.EOF {
			return e
		}
		if n > 0 {
			continue
		}
		p, err := db.GetFederationPeer(j.Peer)
		if err != nil || p == nil {
			return errors.New("peer untrusted")
		}
		current, err := db.GetFederationJob(j.ID)
		if err != nil {
			return err
		}
		if jobTerminal(current.State) {
			return nil
		}
		if current.State == "unknown" {
			return errors.New("job execution uncertain")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (rt *fedRuntime) openJobFollow(parent context.Context, j *db.FederationJob) (io.ReadCloser, error) {
	return rt.openJobFollowRequest(parent, j, nil)
}

func (rt *fedRuntime) openJobFollowRequest(parent context.Context, j *db.FederationJob, output *fedJobOutputRequest) (io.ReadCloser, error) {
	key := "job-follow-in/" + j.Peer + "/" + j.ID
	if !rt.reserveBundleTransfer(key) {
		return nil, errors.New("follow busy")
	}
	release := true
	defer func() {
		if release {
			rt.releaseBundleTransfer(key)
		}
	}()
	kp, e := stream.NewKeyPair()
	if e != nil {
		return nil, e
	}
	sid := proto.NewEnvelopeID()
	ch := make(chan bundletransfer.Answer, 1)
	rt.bundleMu.Lock()
	if rt.bundleWaiters == nil {
		rt.bundleWaiters = map[string]fedBundleWaiter{}
	}
	rt.bundleWaiters[sid] = fedBundleWaiter{Peer: j.Peer, Offer: j.ID, Digest: j.Fingerprint, Answer: ch}
	rt.bundleMu.Unlock()
	defer func() { rt.bundleMu.Lock(); delete(rt.bundleWaiters, sid); rt.bundleMu.Unlock() }()
	request := bundletransfer.Request{Offer: j.ID, Stream: sid, SHA256: j.Fingerprint, Key: kp.Pub}
	kind := proto.KindJobFollow
	var payload any = request
	if output != nil {
		output.Request = request
		payload = output
		kind = proto.KindJobOutput
	}
	if !rt.sendControl(j.Peer, kind, "", payload) {
		return nil, errors.New("job peer offline")
	}
	var a bundletransfer.Answer
	select {
	case a = <-ch:
	case <-parent.Done():
		return nil, parent.Err()
	case <-time.After(30 * time.Second):
		return nil, errors.New("follow unanswered")
	}
	if !a.OK {
		return nil, errors.New(a.Reason)
	}
	conn, e := rt.joinStream(parent, j.Peer, sid, kp, a.Key, true)
	if e != nil {
		return nil, e
	}
	stop := context.AfterFunc(parent, func() { _ = conn.Close() })
	release = false
	return &jobFollowReader{ReadCloser: conn, close: func() { stop(); rt.releaseBundleTransfer(key) }}, nil
}

type jobFollowReader struct {
	io.ReadCloser
	close func()
}

func (f *jobFollowReader) Close() error { e := f.ReadCloser.Close(); f.close(); return e }

func handleFederationJobFollow(w http.ResponseWriter, r *http.Request) {
	j, e := db.GetFederationJob(r.PathValue("id"))
	if e != nil {
		writeError(w, 404, "job", "job not found")
		return
	}
	if !authorizeJobAccess(w, r, j) {
		return
	}
	if j.Direction != "out" || jobTerminal(j.State) {
		writeError(w, 409, "job", "follow requires an active submitted job; use completed logs")
		return
	}
	rt := currentFederation()
	if rt == nil {
		writeError(w, 503, "offline", "federation offline")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Hour)
	defer cancel()
	conn, e := rt.openJobFollow(ctx, j)
	if e != nil {
		writeError(w, 409, "follow", e.Error())
		return
	}
	defer func() { _ = conn.Close() }()
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p, e := db.GetFederationPeer(j.Peer)
				if e != nil || p == nil || !authorizeJobAccess(discardJobResponse{}, r, j) {
					cancel()
					return
				}
			}
		}
	}()
	w.Header().Set("Content-Type", "application/vnd.tclaude.job-frames")
	// Validate each complete frame before exposing bytes to a local client.
	bounded := io.LimitReader(conn, jobstream.MaxEncodedBytes+1)
	enc := jobstream.NewEncoder(w)
	cursor := jobstream.Cursor{}
	for {
		f, err := jobstream.Read(bounded)
		if err != nil {
			return
		}
		p, err := db.GetFederationPeer(j.Peer)
		if err != nil || p == nil {
			return
		}
		if !authorizeJobAccess(discardJobResponse{}, r, j) {
			return
		}
		if err = cursor.Apply(f, enc.Channel(jobstream.Stdout), enc.Channel(jobstream.Stderr)); err != nil {
			return
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
}

type discardJobResponse struct{}

func (discardJobResponse) Header() http.Header         { return http.Header{} }
func (discardJobResponse) Write(p []byte) (int, error) { return len(p), nil }
func (discardJobResponse) WriteHeader(int)             {}
