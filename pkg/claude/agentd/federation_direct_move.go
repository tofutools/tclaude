package agentd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

var fedDirectMoveMu sync.Mutex

// Automatic moves use the same import/launch authority seam as automatic
// teleports. The receiver owns this authority; the sender only requests it.
func directMoveAuthority(o *db.FederationBundleOffer) (*teleportLandingAuthority, error) {
	p, err := db.GetFederationPeer(o.Peer)
	if err != nil || p == nil || !fedPeerAllows(o.Peer, o.GroupID, PermAgentsReceive) {
		return nil, errors.New("receiving permission is unavailable")
	}
	g, err := db.GetAgentGroupByID(o.GroupID)
	if err != nil || g == nil || g.IsArchived() {
		return nil, errors.New("receiving group is unavailable")
	}
	t := &db.FederationTeleport{Peer: o.Peer, TargetAgent: db.NewAgentID(), Landing: &db.FederationTeleportLanding{Group: g.Name, GroupID: g.ID}}
	if p.TrustLevel != "unrestricted" {
		landing, profile, worker, mode, err := resolveTeleportLanding(o.Peer, g.ID, "local")
		if err != nil {
			return nil, err
		}
		t.Landing, t.Profile, t.WorkerDefaults, t.Credentials = landing, profile, worker, mode
	}
	return &teleportLandingAuthority{record: t, direct: o}, nil
}

func (a *teleportLandingAuthority) checkDirectMove() error {
	o := a.direct
	if o == nil || o.Descriptor.Move == nil || !o.Descriptor.Move.DirectIfAllowed || o.Descriptor.Teleport != nil || !o.Descriptor.ExpiresAt.After(time.Now()) {
		return errors.New("direct move is unavailable")
	}
	live, err := db.GetFederationBundleOffer("in", o.Peer, o.Descriptor.ID)
	if err != nil || live == nil || (live.State != "ready" && (live.State != "applied" || live.ImportAgent != a.record.TargetAgent)) || live.Descriptor.SHA256 != o.Descriptor.SHA256 || !sameMoveIntent(live.Descriptor.Move, o.Descriptor.Move) {
		return errors.New("move offer changed")
	}
	current, err := directMoveAuthority(o)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current.record.Landing, a.record.Landing) || !reflect.DeepEqual(current.record.Profile, a.record.Profile) || !reflect.DeepEqual(current.record.WorkerDefaults, a.record.WorkerDefaults) || current.record.Credentials != a.record.Credentials {
		return errors.New("receiving authority changed")
	}
	if a.record.Profile != nil {
		return directMoveCapacity(o, a.record.Landing.MaxLive)
	}
	return nil
}

func (a *teleportLandingAuthority) prepareDirectMoveBundle(b *agentbundle.Bundle) error {
	if err := a.check(); err != nil {
		return err
	}
	if b.Manifest.History == nil || b.Manifest.History.SourceConvID != a.direct.Descriptor.Move.SourceConv {
		return errors.New("move requires the offered source generation's native history")
	}
	if a.record.Profile != nil {
		profile := profileToJSON(a.record.Profile)
		if profile.Harness == "" {
			profile.Harness = "claude"
		}
		if profile.Harness != b.Manifest.Agent.Harness {
			return errors.New("receiving profile is incompatible with transferred history")
		}
		b.Manifest.Agent.Profile, _ = json.Marshal(profile)
		b.Manifest.Placeholders = nil
		if err := applyTeleportModelCredentials(b, a.record.Credentials); err != nil {
			return err
		}
	}
	return nil
}

func reconcileFederationDirectMoves() {
	if !fedDirectMoveMu.TryLock() {
		return
	}
	defer fedDirectMoveMu.Unlock()
	rt := currentFederation()
	if rt == nil {
		return
	}
	offers, err := db.ListFederationBundleOffers("in")
	if err != nil {
		return
	}
	for _, o := range offers {
		if o.Descriptor.Move == nil || !o.Descriptor.Move.DirectIfAllowed || o.Descriptor.Teleport != nil || o.ImportAgent != "" || o.State != "pending" && o.State != "ready" {
			continue
		}
		if o.LastError != "" {
			if strings.HasPrefix(o.LastError, "Awaiting receiver acceptance:") {
				queueDirectMovePending(&o)
			} else {
				fedBundleMu.Lock()
				settleDirectMovePending(&o, errors.New(o.LastError), http.StatusConflict)
				fedBundleMu.Unlock()
			}
			continue
		}
		processDirectMove(rt, &o)
	}
}

func processDirectMove(rt *fedRuntime, o *db.FederationBundleOffer) {
	if _, err := directMoveAuthority(o); err != nil {
		fedBundleMu.Lock()
		defer fedBundleMu.Unlock()
		settleDirectMovePending(o, err, http.StatusForbidden)
		return
	}
	if o.State == "pending" {
		if err := rt.fetchBundle(rt.ctx, o); err != nil {
			fedBundleMu.Lock()
			settleDirectMovePending(o, err, http.StatusBadGateway)
			fedBundleMu.Unlock()
			return
		}
		live, err := db.GetFederationBundleOffer("in", o.Peer, o.Descriptor.ID)
		if err != nil || live == nil {
			return
		}
		o = live
	}
	fedBundleMu.Lock()
	defer fedBundleMu.Unlock()
	live, err := db.GetFederationBundleOffer("in", o.Peer, o.Descriptor.ID)
	if err != nil || live == nil || live.State != "ready" || live.ImportAgent != "" || live.LastError != "" {
		return
	}
	o = live
	authority, err := directMoveAuthority(o)
	if err == nil {
		err = authority.check()
	}
	code := http.StatusConflict
	if err == nil {
		req := httptest.NewRequest(http.MethodPost, "/internal/direct-move", nil)
		req = req.WithContext(context.WithValue(rt.ctx, teleportLandingContextKey{}, authority))
		rec := httptest.NewRecorder()
		input := &fedBundleImportRequest{Group: authority.record.Landing.Group, configBundleRequest: configBundleRequest{Apply: true}}
		if authority.record.Profile == nil {
			input.Cwd, input.Landing = o.Descriptor.Move.Cwd, o.Descriptor.Move.Landing
		}
		importFederationAgentOffer(rec, req, o, input)
		code = rec.Code
		if code == http.StatusOK {
			recordFederationAudit("move.direct.land", o.Peer, o.ImportAgent, o.Descriptor.Group, "offer="+o.Descriptor.ID, code)
			return
		}
		err = fmt.Errorf("automatic landing unavailable: %s", rec.Body.String())
	}
	// A reserved uncertain launch remains inspectable and must never be retried.
	if o.ImportAgent != "" {
		return
	}
	settleDirectMovePending(o, err, code)
}

func settleDirectMovePending(o *db.FederationBundleOffer, err error, code int) {
	live, readErr := db.GetFederationBundleOffer("in", o.Peer, o.Descriptor.ID)
	if readErr != nil || live == nil || live.ImportAgent != "" || live.State != "ready" && live.State != "pending" {
		return
	}
	o = live
	reason := "Awaiting receiver acceptance: " + err.Error()
	_ = db.SetFederationBundleOfferState("in", o.Peer, o.Descriptor.ID, o.State, reason)
	if m, e := db.GetFederationAgentMove("in", o.Peer, o.Descriptor.ID); e == nil && m != nil && m.State == "awaiting_acceptance" {
		m.Disposition = "pending_acceptance"
		_, _ = db.TransitionFederationAgentMove(*m, m.State)
	}
	if m, e := db.GetFederationAgentMove("in", o.Peer, o.Descriptor.ID); e == nil && m == nil {
		_ = db.InsertFederationAgentMove(db.FederationAgentMove{Direction: "in", Peer: o.Peer, ID: o.Descriptor.ID, State: "awaiting_acceptance", Disposition: "pending_acceptance", SourceAgent: o.Descriptor.Move.SourceAgent, SourceConv: o.Descriptor.Move.SourceConv, SHA256: o.Descriptor.SHA256, Group: o.Descriptor.Group, ExpiresAt: o.Descriptor.ExpiresAt})
	}
	queueDirectMovePending(o)
	recordFederationAudit("move.direct.pending", o.Peer, "", o.Descriptor.Group, "offer="+o.Descriptor.ID+" "+reason, code)
}

func queueDirectMovePending(o *db.FederationBundleOffer) {
	p, err := db.GetFederationPeer(o.Peer)
	if err != nil || p == nil {
		return
	}
	sum := sha256.Sum256([]byte("direct-move-pending/" + o.Peer + "/" + o.Descriptor.ID))
	id := hex.EncodeToString(sum[:16])
	if row, err := db.GetFederationOutbox(id); err != nil || row != nil {
		return
	}
	_, _ = queueFederatedEnvelope(fedOutgoing{envelopeID: id, peer: p, kind: proto.KindBundleResult, inReplyTo: o.Descriptor.ID, toLabel: peerDisplay(p), subject: "move pending acceptance", preview: o.Descriptor.ID, ttl: time.Until(o.Descriptor.ExpiresAt), payload: bundletransfer.Result{Offer: o.Descriptor.ID, State: "pending", Disposition: "pending_acceptance"}})
}

func directMoveCapacity(o *db.FederationBundleOffer, limit int) error {
	offers, err := db.ListFederationBundleOffers("in")
	if err != nil {
		return err
	}
	alive, err := session.LiveTmuxSessions()
	if err != nil {
		return err
	}
	pending, err := db.ListPendingSpawns()
	if err != nil {
		return err
	}
	reserved := map[string]bool{}
	for _, p := range pending {
		reserved[p.AgentID] = true
	}
	currentPeer, err := db.ResolveFederationIdentitySuccessor(o.Peer)
	if err != nil {
		return err
	}
	used := 0
	for _, other := range offers {
		otherPeer, err := db.ResolveFederationIdentitySuccessor(other.Peer)
		if err != nil {
			return err
		}
		if otherPeer != currentPeer || other.GroupID != o.GroupID || other.Descriptor.ID == o.Descriptor.ID || other.ImportAgent == "" {
			continue
		}
		a, err := db.GetAgent(other.ImportAgent)
		if err != nil {
			return err
		}
		if reserved[other.ImportAgent] {
			used++
			continue
		}
		if a == nil || !a.Active() {
			continue
		}
		sessionRow, err := db.LoadSession(other.ImportLabel)
		if err != nil {
			return err
		}
		if sessionRow != nil {
			if _, ok := alive[sessionRow.TmuxSession]; ok {
				used++
			}
		}
	}
	if used >= limit {
		return errors.New("receiving policy worker limit reached")
	}
	return nil
}
