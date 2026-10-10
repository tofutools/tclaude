package agentd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/federation/stream"
)

// Bundle-specific policy/validation stays out of the transport. Agent sharing
// registers another kind here with its own admission scope and importer.
type fedBundleKind struct {
	Type         bundletransfer.Type
	Validate     func([]byte) error
	ValidateFile func(*os.File) error
}

func federationBundleKind(name string) (fedBundleKind, bool) {
	if name == bundletransfer.Config.Name {
		return fedBundleKind{Type: bundletransfer.Config, Validate: validateOfferedConfig}, true
	}
	if name == bundletransfer.Agent.Name {
		return fedBundleKind{Type: agentTransferType(), ValidateFile: func(f *os.File) error {
			b, err := agentbundle.DecodeFile(f, agentTransferLimit(), "")
			if err == nil {
				b.Close()
			}
			return err
		}, Validate: func(raw []byte) error { _, err := agentbundle.Decode(raw); return err }}, true
	}
	return fedBundleKind{}, false
}

var fedBundleMu sync.Mutex // serializes terminal transitions with local apply
func fedBundleSpool() bundletransfer.Spool {
	return bundletransfer.Spool{Root: filepath.Join(config.DataDir(), "federation", "bundles")}
}
func fedBundleAdmitted(peer string, kind bundletransfer.Type, d bundletransfer.Descriptor) bool {
	if p, err := db.GetFederationPeer(peer); err != nil || p == nil {
		return false
	}
	if kind.GroupScoped {
		g, err := db.GetAgentGroupByName(d.Group)
		return err == nil && g != nil && (fedPeerAllows(peer, g.ID, kind.AdmissionSlug) || d.Teleport != nil && fedPeerAllows(peer, g.ID, PermAgentsTeleportReceive))
	}
	if db.FederationPeerUnrestricted(peer) {
		return true
	}
	grants, err := db.ListEffectiveFederationPeerGrants(peer)
	if err != nil {
		return false
	}
	for _, g := range grants {
		if g.Slug == kind.AdmissionSlug && g.Scope == "" {
			return true
		}
	}
	return false
}

// Persisted group identity, rather than a mutable wire name, governs admission
// after receipt. The receiver's operator can still choose another granted group.
func fedBundleOfferAdmitted(o *db.FederationBundleOffer, kind bundletransfer.Type) bool {
	if !kind.GroupScoped {
		return fedBundleAdmitted(o.Peer, kind, o.Descriptor)
	}
	if p, err := db.GetFederationPeer(o.Peer); err != nil || p == nil {
		return false
	}
	return o.GroupID != 0 && (fedPeerAllows(o.Peer, o.GroupID, kind.AdmissionSlug) || o.Descriptor.Teleport != nil && fedPeerAllows(o.Peer, o.GroupID, PermAgentsTeleportReceive))
}
func (rt *fedRuntime) acceptBundleOffer(peer *db.FederationPeer, env *proto.Envelope) {
	refuse := func(code, msg string) {
		rt.sendControl(peer.InstanceID, proto.KindAck, env.ID, proto.AckPayload{Status: proto.AckRefused, Code: code, Reason: msg})
	}
	var d bundletransfer.Descriptor
	if env.DecodePayload(&d) != nil || env.To.Agent != "" {
		refuse(fedCodeMalformed, "bundle offers must target the remote operator")
		return
	}
	kind, ok := federationBundleKind(d.Type)
	if !ok {
		refuse(fedCodeMalformed, "unsupported bundle type")
		return
	}
	if d.Type == bundletransfer.Agent.Name && d.Bytes > kind.Type.MaxBytes {
		refuse(fedCodeMalformed, fmt.Sprintf("agent archive exceeds federation.agent_transfer_max_bytes=%d on the receiver; raise that node setting", kind.Type.MaxBytes))
		return
	}
	if err := d.Validate(kind.Type, time.Now()); err != nil || d.ID != env.ID || d.ExpiresAt.After(env.ExpiresAt.Add(time.Second)) {
		refuse(fedCodeMalformed, "invalid bundle offer descriptor")
		return
	}
	if !kind.Type.GroupScoped && env.From.Agent != "" {
		refuse(fedCodeMalformed, "config offers must come from the remote operator")
		return
	}
	if !rt.allowInbound(peer.InstanceID) {
		refuse(fedCodeRateLimited, "offer rate exceeded")
		return
	}
	fedBundleMu.Lock()
	defer fedBundleMu.Unlock()
	existing, err := db.GetFederationBundleOffer("in", peer.InstanceID, d.ID)
	if err != nil {
		refuse(fedCodeInternal, "offer store unavailable")
		return
	}
	if existing != nil {
		if !sameTeleportIntent(existing.Descriptor.Teleport, d.Teleport) || !sameMoveIntent(existing.Descriptor.Move, d.Move) || existing.Descriptor.SHA256 != d.SHA256 || existing.Descriptor.Bytes != d.Bytes || existing.Descriptor.Type != d.Type || existing.Descriptor.Group != d.Group || existing.SenderAgent != env.From.Agent || !existing.Descriptor.ExpiresAt.Equal(d.ExpiresAt) {
			refuse(fedCodeMalformed, "offer identity reused with different content")
			return
		}
		if (existing.State == "pending" || existing.State == "ready") && !fedBundleOfferAdmitted(existing, kind.Type) {
			refuse(fedCodeNotExported, "receive admission revoked")
			return
		}
		rt.sendControl(peer.InstanceID, proto.KindAck, env.ID, proto.AckPayload{Status: proto.AckAccepted})
		return
	}
	if !fedBundleAdmitted(peer.InstanceID, kind.Type, d) {
		refuse(fedCodeNotExported, "peer lacks "+kind.Type.AdmissionSlug)
		return
	}
	groupID := int64(0)
	if kind.Type.GroupScoped {
		g, err := db.GetAgentGroupByName(d.Group)
		if err != nil || g == nil {
			refuse(fedCodeNotExported, "receiving group unavailable")
			return
		}
		groupID = g.ID
	}
	teleport, err := incomingTeleportRecord(peer.InstanceID, env.From.Agent, d, groupID)
	if err != nil {
		refuse("teleport_refused", err.Error())
		return
	}
	if teleport != nil {
		limits := teleportLocalLimits()
		if teleport.Landing != nil {
			limits.Hour = min(limits.Hour, teleport.Landing.Limits.Hour)
			limits.Day = min(limits.Day, teleport.Landing.Limits.Day)
		}
		if err = db.RecordFederationTeleport(*teleport, limits); err != nil {
			refuse(fedCodeRateLimited, err.Error())
			return
		}
	}
	if len(d.Inline) > 0 {
		if err := kind.Validate(d.Inline); err != nil {
			refuse(fedCodeMalformed, "invalid bundle content")
			return
		}
	}
	o := db.FederationBundleOffer{Descriptor: d, Peer: peer.InstanceID, Direction: "in", State: "pending", GroupID: groupID, SenderAgent: env.From.Agent}
	if _, err := db.InsertFederationBundleOffer(o, kind.Type); err != nil {
		code := fedCodeInternal
		if errors.Is(err, db.ErrOfferQuota) {
			code = fedCodeQueueFull
		}
		refuse(code, err.Error())
		return
	}
	if len(d.Inline) > 0 {
		if err := fedBundleSpool().Receive("in", peer.InstanceID, d, bytes.NewReader(d.Inline)); err != nil {
			_ = db.DeleteFederationBundleOffer("in", peer.InstanceID, d.ID)
			refuse(fedCodeInternal, "could not store bundle")
			return
		}
		_ = db.SetFederationBundleOfferState("in", peer.InstanceID, d.ID, "ready", "")
	}
	if d.Move != nil && d.Move.DirectIfAllowed {
		_ = db.InsertFederationAgentMove(db.FederationAgentMove{Direction: "in", Peer: peer.InstanceID, ID: d.ID, State: "awaiting_acceptance", Disposition: "checking", SourceAgent: d.Move.SourceAgent, SourceConv: d.Move.SourceConv, SHA256: d.SHA256, Group: d.Group, ExpiresAt: d.ExpiresAt})
	}
	// The durable offer itself appears in federation inbox and offers listings;
	// no prompt/content from it is delivered to an agent.
	recordFederationAudit("federation.bundle.in", peerDisplay(peer), "", "", d.Type+" offer "+d.ID, 200)
	if teleport != nil {
		recordFederationAudit("teleport.receive", peer.InstanceID, teleport.Intent.SourceAgent, d.Group, fmt.Sprintf("offer=%s chain=%s hop=%d credentials=%s state=%s", d.ID, teleport.Intent.Chain, len(teleport.Intent.Hops), teleport.Credentials, teleport.State), 200)
	}
	rt.sendControl(peer.InstanceID, proto.KindAck, env.ID, proto.AckPayload{Status: proto.AckAccepted})
}

type fedBundleWaiter struct {
	Peer, Offer, Digest string
	Answer              chan bundletransfer.Answer
}

func (rt *fedRuntime) acceptBundleAnswer(peer *db.FederationPeer, env *proto.Envelope) {
	var a bundletransfer.Answer
	if env.From.Agent != "" || env.To.Agent != "" || env.DecodePayload(&a) != nil {
		return
	}
	rt.bundleMu.Lock()
	w, ok := rt.bundleWaiters[a.Stream]
	rt.bundleMu.Unlock()
	if !ok || w.Peer != peer.InstanceID || w.Offer != a.Offer || w.Digest != a.SHA256 {
		return
	}
	select {
	case w.Answer <- a:
	default:
	}
}
func (rt *fedRuntime) reserveBundleTransfer(key string) bool {
	rt.bundleMu.Lock()
	defer rt.bundleMu.Unlock()
	if rt.bundleActive == nil {
		rt.bundleActive = map[string]bool{}
	}
	if rt.bundleActive[key] || len(rt.bundleActive) >= 4 {
		return false
	}
	rt.bundleActive[key] = true
	return true
}
func (rt *fedRuntime) releaseBundleTransfer(key string) {
	rt.bundleMu.Lock()
	delete(rt.bundleActive, key)
	rt.bundleMu.Unlock()
}
func (rt *fedRuntime) fetchBundle(ctx context.Context, o *db.FederationBundleOffer) error {
	key := "in/" + o.Peer + "/" + o.Descriptor.ID
	if !rt.reserveBundleTransfer(key) {
		return errors.New("bundle transfer already active or transfer limit reached")
	}
	defer rt.releaseBundleTransfer(key)
	ctx, cancel := context.WithDeadline(ctx, o.Descriptor.ExpiresAt)
	defer cancel()
	var catalog proto.CatalogPayload
	rawCatalog, _, _ := db.GetFederationCatalog(o.Peer)
	_ = json.Unmarshal([]byte(rawCatalog), &catalog)
	var err error
	if o.Descriptor.Type == bundletransfer.Agent.Name && catalog.AgentBundleChunks {
		var offset int64
		offset, err = fedBundleSpool().PartialBytes(o.Peer, o.Descriptor)
		if err != nil {
			return err
		}
		for offset < o.Descriptor.Bytes {
			kind, known := federationBundleKind(o.Descriptor.Type)
			if !known || !fedBundleOfferAdmitted(o, kind.Type) {
				return errors.New("bundle admission revoked")
			}
			length := min(bundletransfer.ChunkBytes, o.Descriptor.Bytes-offset)
			for {
				err = rt.fetchBundlePart(ctx, o, true, offset, length)
				if err == nil {
					break
				}
				var refusal bundleFetchRefusal
				if !errors.As(err, &refusal) || refusal.reason != "transfer busy" && refusal.reason != "transfer request rate exceeded" {
					return err
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Second):
				}
			}
			offset += length
			rt.sendControl(o.Peer, proto.KindBundleResult, "", bundletransfer.Result{Offer: o.Descriptor.ID, State: "progress", SHA256: o.Descriptor.SHA256, BytesDone: offset, BytesTotal: o.Descriptor.Bytes})
		}
		err = fedBundleSpool().FinishChunks(o.Peer, o.Descriptor)
	} else {
		err = rt.fetchBundlePart(ctx, o, false, 0, 0)
	}
	if err != nil {
		return err
	}
	kind, ok := federationBundleKind(o.Descriptor.Type)
	if !ok {
		return errors.New("unknown bundle type")
	}
	if kind.ValidateFile != nil {
		f, openErr := fedBundleSpool().Open("in", o.Peer, o.Descriptor)
		if openErr != nil {
			return openErr
		}
		err = kind.ValidateFile(f)
		_ = f.Close()
	} else {
		var raw []byte
		raw, err = fedBundleSpool().Read("in", o.Peer, o.Descriptor)
		if err == nil {
			err = kind.Validate(raw)
		}
	}
	if err != nil {
		_ = fedBundleSpool().Remove("in", o.Peer, o.Descriptor.ID)
		return err
	}
	fedBundleMu.Lock()
	defer fedBundleMu.Unlock()
	current, err := db.GetFederationBundleOffer("in", o.Peer, o.Descriptor.ID)
	if err != nil {
		return err
	}
	if current == nil || current.State != "pending" || !current.Descriptor.ExpiresAt.After(time.Now()) || !fedBundleOfferAdmitted(current, kind.Type) {
		_ = fedBundleSpool().Remove("in", o.Peer, o.Descriptor.ID)
		return errors.New("offer expired, declined or admission revoked during transfer")
	}
	return db.SetFederationBundleOfferState("in", o.Peer, o.Descriptor.ID, "ready", "")
}
func (rt *fedRuntime) fetchBundlePart(ctx context.Context, o *db.FederationBundleOffer, chunked bool, offset, length int64) error {
	ctx, cancel := context.WithDeadline(ctx, o.Descriptor.ExpiresAt)
	defer cancel()
	stop := context.AfterFunc(rt.ctx, cancel)
	defer stop()
	kp, err := stream.NewKeyPair()
	if err != nil {
		return err
	}
	sid := proto.NewEnvelopeID()
	ch := make(chan bundletransfer.Answer, 1)
	rt.bundleMu.Lock()
	if rt.bundleWaiters == nil {
		rt.bundleWaiters = map[string]fedBundleWaiter{}
	}
	rt.bundleWaiters[sid] = fedBundleWaiter{Peer: o.Peer, Offer: o.Descriptor.ID, Digest: o.Descriptor.SHA256, Answer: ch}
	rt.bundleMu.Unlock()
	defer func() { rt.bundleMu.Lock(); delete(rt.bundleWaiters, sid); rt.bundleMu.Unlock() }()
	req := bundletransfer.Request{Chunked: chunked, Offset: offset, Bytes: length, Offer: o.Descriptor.ID, Stream: sid, SHA256: o.Descriptor.SHA256, Key: kp.Pub}
	if !rt.sendControl(o.Peer, proto.KindBundleFetch, "", req) {
		return errors.New("sender is offline or transfer request was not delivered")
	}
	var a bundletransfer.Answer
	select {
	case a = <-ch:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(30 * time.Second):
		return errors.New("sender did not answer bundle fetch")
	}
	if a.Chunked != chunked || a.Offset != offset || a.Bytes != length {
		return errors.New("bundle answer range mismatch")
	}
	if !a.OK {
		return bundleFetchRefusal{proto.StripControls(a.Reason)}
	}
	conn, err := rt.joinStream(ctx, o.Peer, sid, kp, a.Key, true)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	closeOnCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer closeOnCancel()
	reader := bundleIdleReader{conn}
	if chunked {
		return fedBundleSpool().ReceiveChunk(o.Peer, o.Descriptor, offset, length, a.ChunkSHA256, reader)
	}
	return fedBundleSpool().Receive("in", o.Peer, o.Descriptor, reader)
}
func (rt *fedRuntime) serveBundleFetch(peer *db.FederationPeer, env *proto.Envelope) {
	var req bundletransfer.Request
	if env.From.Agent != "" || env.To.Agent != "" || env.DecodePayload(&req) != nil || !proto.ValidStreamID(req.Offer) || !proto.ValidStreamID(req.Stream) || len(req.Key) != 32 {
		return
	}
	a := bundletransfer.Answer{Request: req}
	refuse := func(reason string) {
		a.Reason = reason
		rt.sendControl(peer.InstanceID, proto.KindBundleAnswer, env.ID, a)
	}
	if !rt.allowInbound(peer.InstanceID) {
		refuse("transfer request rate exceeded")
		return
	}
	if fresh, err := db.MarkFederationEnvelopeSeen(peer.InstanceID, "bundlefetch:"+env.ID, time.Now().Add(fedControlTTL+time.Minute)); err != nil || !fresh {
		return
	}
	o, err := db.GetFederationBundleOffer("out", peer.InstanceID, req.Offer)
	if err != nil || o == nil || o.Descriptor.SHA256 != req.SHA256 || !o.Descriptor.ExpiresAt.After(time.Now()) || (o.State != "pending" && o.State != "ready") {
		refuse("offer unavailable")
		return
	}
	key := "out/" + peer.InstanceID + "/" + req.Offer
	if !rt.reserveBundleTransfer(key) {
		refuse("transfer busy")
		return
	}
	defer rt.releaseBundleTransfer(key)
	f, err := fedBundleSpool().Open("out", peer.InstanceID, o.Descriptor)
	if err != nil {
		refuse("offer payload unavailable")
		return
	}
	defer f.Close()
	length := o.Descriptor.Bytes
	if req.Chunked {
		if o.Descriptor.Type != bundletransfer.Agent.Name || req.Offset < 0 || req.Offset%bundletransfer.ChunkBytes != 0 || req.Bytes <= 0 || req.Bytes > bundletransfer.ChunkBytes || req.Offset > o.Descriptor.Bytes-req.Bytes || req.Bytes != min(bundletransfer.ChunkBytes, o.Descriptor.Bytes-req.Offset) {
			refuse("invalid bundle chunk range")
			return
		}
		length = req.Bytes
		if _, err = f.Seek(req.Offset, io.SeekStart); err != nil {
			refuse("chunk unavailable")
			return
		}
		digest := sha256.New()
		if _, err = io.CopyN(digest, f, length); err != nil {
			refuse("chunk unavailable")
			return
		}
		a.ChunkSHA256 = hex.EncodeToString(digest.Sum(nil))
		if _, err = f.Seek(req.Offset, io.SeekStart); err != nil {
			refuse("chunk unavailable")
			return
		}
	} else if req.Offset != 0 || req.Bytes != 0 {
		refuse("invalid bundle range")
		return
	}
	kp, err := stream.NewKeyPair()
	if err != nil {
		return
	}
	a.OK = true
	a.Key = kp.Pub
	if !rt.sendControl(peer.InstanceID, proto.KindBundleAnswer, env.ID, a) {
		return
	}
	ctx, cancel := context.WithDeadline(rt.ctx, o.Descriptor.ExpiresAt)
	defer cancel()
	conn, err := rt.joinStream(ctx, peer.InstanceID, req.Stream, kp, req.Key, false)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if _, err = io.CopyN(bundleIdleWriter{conn}, f, length); err == nil {
		_ = conn.CloseWrite()
	}
}
func (rt *fedRuntime) acceptBundleResult(peer *db.FederationPeer, env *proto.Envelope) {
	var res bundletransfer.Result
	if env.From.Agent != "" || env.To.Agent != "" || env.DecodePayload(&res) != nil || (res.State != "progress" && res.State != "applied" && res.State != "declined" && (res.State != "pending" || res.Disposition != "pending_acceptance")) {
		return
	}
	o, err := db.GetFederationBundleOffer("out", peer.InstanceID, res.Offer)
	if err != nil || o == nil {
		return
	}
	fedBundleMu.Lock()
	defer fedBundleMu.Unlock()
	if res.State == "progress" {
		if res.SHA256 != o.Descriptor.SHA256 || res.BytesTotal != o.Descriptor.Bytes || res.BytesDone < 0 || res.BytesDone > res.BytesTotal || o.Descriptor.Move == nil {
			return
		}
		_ = db.UpdateFederationMoveTransfer("out", o.Peer, res.Offer, res.BytesDone, res.BytesTotal)
		rt.sendControl(peer.InstanceID, proto.KindAck, env.ID, proto.AckPayload{Status: proto.AckAccepted})
		return
	}
	if res.State == "pending" {
		if o.Descriptor.Move == nil || !o.Descriptor.Move.DirectIfAllowed {
			return
		}
		if m, err := db.GetFederationAgentMove("out", peer.InstanceID, res.Offer); err == nil && m != nil && m.State == "awaiting_confirmation" && m.Disposition == "checking" {
			m.Disposition = "pending_acceptance"
			_, _ = db.TransitionFederationAgentMove(*m, "awaiting_confirmation")
		}
		rt.sendControl(peer.InstanceID, proto.KindAck, env.ID, proto.AckPayload{Status: proto.AckAccepted})
		return
	}
	_ = db.SetFederationBundleOfferState("out", peer.InstanceID, res.Offer, res.State, "")
	_ = fedBundleSpool().Remove("out", peer.InstanceID, res.Offer)
	rt.sendControl(peer.InstanceID, proto.KindAck, env.ID, proto.AckPayload{Status: proto.AckAccepted})
}
func reconcileFederationBundleOffers() {
	fedBundleMu.Lock()
	defer fedBundleMu.Unlock()
	offers, err := db.ListFederationBundleOffers("")
	if err != nil {
		return
	}
	now := time.Now()
	fedBundleSpool().PruneTemporary(now)
	for _, o := range offers {
		if o.Direction == "in" && o.Descriptor.Move != nil && o.Descriptor.Move.DirectIfAllowed && (o.State == "ready" || o.State == "pending") && o.ImportAgent == "" && strings.HasPrefix(o.LastError, "Awaiting receiver acceptance:") {
			queueDirectMovePending(&o)
		}
		if o.Direction == "in" && o.State == "ready" && o.ImportAgent != "" {
			if o.ImportLabel == "" {
				// No subprocess boundary was reached, including across a daemon restart.
				if released, err := db.ReleaseUnlaunchedFederationBundleImport(o.Peer, o.Descriptor.ID, o.ImportAgent); err == nil && released {
					_ = db.DeleteFederationAgentMove("in", o.Peer, o.Descriptor.ID)
					cleanupReleasedFederationLanding(&o)
				}
			} else if a, err := db.GetAgent(o.ImportAgent); err == nil && a != nil && a.Active() && a.CurrentConvID != "" {
				if s, err := db.LoadSession(o.ImportLabel); err == nil && s != nil && s.ConvID == a.CurrentConvID && s.TmuxSession != "" && session.IsTmuxSessionAlive(s.TmuxSession) {
					if err := db.SetFederationBundleOfferState("in", o.Peer, o.Descriptor.ID, "applied", ""); err == nil {
						o.State = "applied"
					}
				}
			}
		}
		if o.Direction == "out" {
			if row, _ := db.GetFederationOutbox(o.Descriptor.ID); row != nil {
				_ = db.EraseSettledFederationBundlePayload(o.Descriptor.ID)
				if row.State == db.FedOutboxRefused && (o.State == "pending" || o.State == "ready") {
					_ = db.SetFederationBundleOfferState("out", o.Peer, o.Descriptor.ID, "declined", row.LastError)
					o.State = "declined"
				}
			}
		}
		if o.Direction == "in" && (o.State == "applied" || o.State == "declined") && !o.ResultQueued {
			queueBundleResult(&o, o.State)
		}

		terminal := o.State == "applied" || o.State == "declined" || o.State == "expired"
		if !o.Descriptor.ExpiresAt.After(now) && !terminal {
			_ = db.SetFederationBundleOfferState(o.Direction, o.Peer, o.Descriptor.ID, "expired", "")
			terminal = true
		}
		if terminal {
			_ = fedBundleSpool().Remove(o.Direction, o.Peer, o.Descriptor.ID)
		}
	}
}

type bundleDeadlineReader interface {
	io.Reader
	SetReadDeadline(time.Time) error
}
type bundleIdleReader struct{ bundleDeadlineReader }

func (r bundleIdleReader) Read(p []byte) (int, error) {
	_ = r.SetReadDeadline(time.Now().Add(5 * time.Minute))
	return r.bundleDeadlineReader.Read(p)
}

type bundleDeadlineWriter interface {
	io.Writer
	SetWriteDeadline(time.Time) error
}
type bundleIdleWriter struct{ bundleDeadlineWriter }

func (w bundleIdleWriter) Write(p []byte) (int, error) {
	_ = w.SetWriteDeadline(time.Now().Add(5 * time.Minute))
	return w.bundleDeadlineWriter.Write(p)
}

type bundleFetchRefusal struct{ reason string }

func (e bundleFetchRefusal) Error() string { return "sender refused: " + e.reason }
