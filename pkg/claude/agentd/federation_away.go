package agentd

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/client"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

const PermApprovalsAnswer = "approvals.answer"

type fedAwayState struct {
	config.FederationAwayConfig
	// Epoch is fresh on every selection and runtime start. It never survives a
	// restart, so persisted notices cannot revive authority over new waiters.
	Epoch string `json:"-"`
}
type fedAwayDecision struct {
	runtime               *fedRuntime
	peer, epoch, envelope string
	outcome               approvalOutcome
	expires               time.Time
}
type fedAwayAnswer struct {
	Request  string `json:"request"`
	Epoch    string `json:"epoch"`
	Decision string `json:"decision"`
}

func (rt *fedRuntime) activeAwayLocked() *fedAwayState {
	if currentFederation() != rt {
		return nil
	}
	if rt.away != nil && !rt.away.Until.IsZero() && !time.Now().Before(rt.away.Until) {
		recordFederationAudit("federation.away.expired", "operator", "", "", "cover="+rt.away.Cover, 200)
		rt.away = nil
	}
	if rt.away == nil {
		return nil
	}
	if p, _ := db.GetFederationPeer(rt.away.Cover); p == nil {
		return nil
	}
	return rt.away
}
func (rt *fedRuntime) awayNoticeCurrent(peer, epoch string) bool {
	rt.awayMu.Lock()
	defer rt.awayMu.Unlock()
	a := rt.activeAwayLocked()
	return a != nil && a.Cover == peer && a.Epoch == epoch
}
func fedPeerAnswers(peer string) bool {
	if p, _ := db.GetFederationPeer(peer); p == nil {
		return false
	}
	if db.FederationPeerUnrestricted(peer) {
		return true
	}
	grants, err := db.ListEffectiveFederationPeerGrants(peer)
	if err != nil {
		return false
	}
	for _, g := range grants {
		if g.Slug == PermApprovalsAnswer && g.Scope == "" {
			return true
		}
	}
	return false
}

// Serialize revocation with consumption of delegated decisions. Rotating after
// the mutation also prevents an old ticket from reviving on a later regrant.
func lockAwayAuthorityMutation(peer string) func() {
	return lockAwayMutation(peer, false)
}

func lockAwayMutation(peer string, trustOnly bool) func() {
	fedLifecycleMu.Lock()
	rt := currentFederation()
	if rt == nil {
		return fedLifecycleMu.Unlock
	}
	rt.awayMu.Lock()
	// Read the old trust level only after serializing with other mutations.
	// A stale snapshot taken by the HTTP handler cannot bypass invalidation.
	before, beforeErr := db.GetFederationPeer(peer)
	return func() {
		rotate := true
		if trustOnly {
			after, afterErr := db.GetFederationPeer(peer)
			rotate = beforeErr != nil || afterErr != nil || before == nil || after == nil || before.TrustLevel != after.TrustLevel
		}
		if rotate && rt.away != nil && rt.away.Cover == peer {
			rt.away.Epoch = newApprovalID()
		}
		rt.awayMu.Unlock()
		fedLifecycleMu.Unlock()
	}
}

func handleFederationAway(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "select or inspect away coverage") {
		return
	}
	fedLifecycleMu.Lock()
	defer fedLifecycleMu.Unlock()
	rt := currentFederation()
	if rt == nil {
		writeError(w, 409, "disabled", "federation must be enabled")
		return
	}
	rt.awayMu.Lock()
	defer rt.awayMu.Unlock()
	if r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]any{"away": rt.activeAwayLocked()})
		return
	}
	var in struct {
		Cover string    `json:"cover"`
		Until time.Time `json:"until"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		writeError(w, 400, "json", err.Error())
		return
	}
	peer, err := resolveFederationPeerOpt(in.Cover, false)
	if err != nil {
		writeFedErr(w, err)
		return
	}
	if !in.Until.IsZero() && !in.Until.After(time.Now()) {
		writeError(w, 400, "until", "until must be in the future")
		return
	}
	a := &fedAwayState{FederationAwayConfig: config.FederationAwayConfig{Cover: peer.InstanceID, Since: time.Now().UTC(), Until: in.Until}, Epoch: newApprovalID()}
	if _, err = config.Update(func(cfg *config.Config, loadErr error) error {
		if loadErr != nil {
			return loadErr
		}
		if cfg.Federation == nil {
			cfg.Federation = &config.FederationConfig{}
		}
		cfg.Federation.Away = &a.FederationAwayConfig
		return nil
	}); err != nil {
		writeFedErr(w, err)
		return
	}
	rt.away = a
	rt.awayWaiting = nil
	go forwardPendingAwayApprovals()
	warnings := []string{}
	if !rt.sessionPeerOnline(peer.InstanceID) {
		warnings = append(warnings, "covering peer is offline; notices queue until it returns or coverage ends")
	}
	if !fedPeerAnswers(peer.InstanceID) {
		warnings = append(warnings, "cover lacks approvals.answer; daemon access requests cannot be answered directly")
	}
	groups, _ := db.ListAgentGroups()
	for _, g := range groups {
		if g.IsArchived() {
			continue
		}
		if !fedPeerAllows(peer.InstanceID, g.ID, PermSessionsRead) || !fedPeerAllows(peer.InstanceID, g.ID, PermSessionsAttach) {
			warnings = append(warnings, "cover needs sessions.read and sessions.attach for group "+g.Name+" to answer harness prompts")
		}
	}
	recordFederationAudit("federation.away", "operator", "", "", "cover="+peer.InstanceID, 200)
	writeJSON(w, 200, map[string]any{"away": a, "warnings": warnings})
}
func handleFederationReturn(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "end away coverage") {
		return
	}
	fedLifecycleMu.Lock()
	defer fedLifecycleMu.Unlock()
	rt := currentFederation()
	if rt != nil {
		rt.awayMu.Lock()
		defer rt.awayMu.Unlock()
	}
	_, err := config.Update(func(cfg *config.Config, loadErr error) error {
		if loadErr != nil {
			return loadErr
		}
		if cfg.Federation != nil {
			cfg.Federation.Away = nil
		}
		return nil
	})
	if err != nil {
		writeFedErr(w, err)
		return
	}
	if rt != nil {
		rt.away = nil
		rt.awayWaiting = nil
	}
	recordFederationAudit("federation.return", "operator", "", "", "coverage ended", 200)
	writeJSON(w, 200, map[string]any{"away": nil})
}

// Called with awayMu held, so return cannot race notice construction. Outbox
// delivery also rechecks the epoch and expiry before each attempt.
func (rt *fedRuntime) queueAwayLocked(a *fedAwayState, subject, body string, deadline time.Time) error {
	peer, err := db.GetFederationPeer(a.Cover)
	if err != nil {
		return err
	}
	if peer == nil {
		return fmt.Errorf("cover no longer trusted")
	}
	expires := time.Now().Add(24 * time.Hour)
	if !a.Until.IsZero() && a.Until.Before(expires) {
		expires = a.Until
	}
	if !deadline.IsZero() && deadline.Before(expires) {
		expires = deadline
	}
	if !expires.After(time.Now()) {
		return fmt.Errorf("request expired")
	}
	// Never export host paths as attachments. Context is bounded to the mail cap.
	body = fedAwayClip(body, proto.MaxMailBody-256)
	_, err = queueFederatedEnvelope(fedOutgoing{peer: peer, kind: proto.KindAwayNotice, toLabel: peerDisplay(peer), subject: auditClip(subject, 512), preview: preview(body), inReplyTo: a.Epoch, ttl: time.Until(expires), payload: proto.MailPayload{Subject: auditClip(subject, 512), Body: body}})
	if err != nil {
		slog.Warn("federation: away forwarding failed", "cover", a.Cover, "error", err)
	}
	if err == nil {
		recordFederationAudit("federation.away.forward", "operator", "", "", "cover="+peer.InstanceID+" "+subject, 200)
	}
	return err
}
func forwardAwayHumanMessage(conv, title, subject, body string) {
	rt := currentFederation()
	if rt == nil {
		return
	}
	rt.awayMu.Lock()
	defer rt.awayMu.Unlock()
	a := rt.activeAwayLocked()
	if a == nil {
		return
	}
	address := peerAgentID(conv) + "@" + rt.id.ID()
	text := fmt.Sprintf("Away coverage: %s\nSession: %s\nGo answer it: tclaude federation attach %s\nReply by messaging the agent through federation mail (requires its mail grant).\nAttachments remain on the originating instance.\n\n%s", title, address, address, body)
	_ = rt.queueAwayLocked(a, "Away: "+subject, text, time.Time{})
}
func forwardAwayApproval(req *approvalRequest) {
	// Peer grants are trust administration and belong only to the local operator.
	if req.peerAccess != nil {
		return
	}
	rt := currentFederation()
	if rt == nil {
		return
	}
	rt.awayMu.Lock()
	defer rt.awayMu.Unlock()
	a := rt.activeAwayLocked()
	if a == nil {
		return
	}
	req.mu.Lock()
	if req.delegatedEpoch == a.Epoch || req.originalDeadline.IsZero() || !time.Now().Before(req.originalDeadline) {
		req.mu.Unlock()
		return
	}
	req.delegatedEpoch = a.Epoch
	req.delegatedEnvelope = ""
	req.delegatedDeadline = req.originalDeadline
	deadline := req.delegatedDeadline
	req.mu.Unlock()
	ticket := req.id + "." + a.Epoch + "@" + rt.id.ID()
	text := fmt.Sprintf("Away access request from %s (%s)\nPermission: %s\nAction: %s %s\nRequest: %s\nOriginal deadline: %s\nOne-shot answer: tclaude federation answer %s --decision approve\nUse --decision deny to refuse. Requires approvals.answer; always/extend are unavailable.\n\n%s", req.convTitle, req.agentID, req.perm, req.method, req.path, ticket, deadline.UTC().Format(time.RFC3339), ticket, req.bodyPreview)
	_ = rt.queueAwayLocked(a, "Away approval: "+req.perm, text, deadline)
}

// Selecting or changing cover also surfaces requests already waiting locally.
// Snapshot the registry before taking awayMu: registration and resolution may
// continue, and receipt always rechecks that the exact request is still live.
func forwardPendingAwayApprovals() {
	approvals.mu.Lock()
	pending := make([]*approvalRequest, 0, len(approvals.pending))
	for _, req := range approvals.pending {
		pending = append(pending, req)
	}
	approvals.mu.Unlock()
	for _, req := range pending {
		forwardAwayApproval(req)
	}
}

func (rt *fedRuntime) observeAwayWaiting() {
	if rt.cl == nil || rt.cl.Status().State != client.StateConnected {
		return
	}
	rt.awayMu.Lock()
	defer rt.awayMu.Unlock()
	a := rt.activeAwayLocked()
	if a == nil {
		rt.awayWaiting = nil
		return
	}
	// Only read session state while away with a covering peer. No extra observer
	// is started for instances that do not use this feature.
	groups, _ := db.ListAgentGroups()
	current := map[string]string{}
	for _, g := range groups {
		if g.IsArchived() {
			continue
		}
		for _, s := range fedCatalogSessions(g.ID) {
			if s.WaitingReason != "permission" && s.WaitingReason != "question" {
				continue
			}
			key := s.Agent + "/" + s.Incarnation
			state := a.Epoch + "/" + s.State
			if current[key] != "" {
				continue
			}
			current[key] = state
			if rt.awayWaiting[key] == state {
				continue
			}
			address := s.Agent + "@" + rt.id.ID()
			body := fmt.Sprintf("Away coverage: %s is waiting for %s.\nSession: %s\nGo answer it: tclaude federation attach %s\nRequires sessions.read and sessions.attach for group %s.\nSession listing: tclaude federation sessions %s", s.Name, s.WaitingReason, address, address, g.Name, rt.id.ID())
			if err := rt.queueAwayLocked(a, "Away: "+s.Name+" waiting for "+s.WaitingReason, body, time.Time{}); err != nil {
				delete(current, key)
			}
		}
	}
	rt.awayWaiting = current
}
func (rt *fedRuntime) acceptAwayNotice(peer *db.FederationPeer, env *proto.Envelope) {
	refuse := func(code, reason string) {
		rt.sendControl(peer.InstanceID, proto.KindAck, env.ID, proto.AckPayload{Status: proto.AckRefused, Code: code, Reason: reason})
	}
	var mp proto.MailPayload
	if env.From.Agent != "" || env.To.Agent != "" || env.DecodePayload(&mp) != nil || len(mp.Body) > proto.MaxMailBody || len(mp.Subject) > 512 || len(mp.Attachments) > 0 || env.ExpiresAt.Sub(env.CreatedAt) > fedMaxInboundTTL {
		refuse(fedCodeMalformed, "invalid away notice")
		return
	}
	if !rt.allowInbound(peer.InstanceID) {
		refuse(fedCodeQueueFull, "away notice rate exceeded")
		return
	}
	rt.acceptOperatorMail(peer, env, "cover request", mp, refuse)
}
func handleFederationAwayAnswer(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "answer a delegated access request") {
		return
	}
	var in struct {
		Ticket   string `json:"ticket"`
		Decision string `json:"decision"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		writeError(w, 400, "json", err.Error())
		return
	}
	token, ref, ok := splitFederatedAddress(in.Ticket)
	parts := strings.Split(token, ".")
	if !ok || len(parts) != 2 || len(parts[0]) != 32 || len(parts[1]) != 32 || (in.Decision != "approve" && in.Decision != "deny") {
		writeError(w, 400, "decision", "exact request ticket and decision approve or deny required")
		return
	}
	peer, err := resolveFederationPeerOpt(ref, false)
	if err != nil {
		writeFedErr(w, err)
		return
	}
	row, err := queueFederatedEnvelope(fedOutgoing{peer: peer, kind: proto.KindAwayAnswer, toLabel: peerDisplay(peer), subject: "Delegated " + in.Decision, preview: token, ttl: time.Hour, payload: fedAwayAnswer{Request: parts[0], Epoch: parts[1], Decision: in.Decision}})
	if err != nil {
		writeFedErr(w, err)
		return
	}
	id, err := federationIdentity()
	if err == nil {
		recordFederationAudit("federation.away.answer.out", id.ID(), "", "", "decider="+id.ID()+" origin="+peer.InstanceID+" request="+parts[0]+" decision="+in.Decision+" queued", 200)
	}
	writeJSON(w, 200, map[string]any{"envelope_id": row.EnvelopeID, "state": row.State})
}
func (rt *fedRuntime) acceptAwayAnswer(peer *db.FederationPeer, env *proto.Envelope) {
	var in fedAwayAnswer
	if env.From.Agent != "" || env.To.Agent != "" || env.DecodePayload(&in) != nil || (in.Decision != "approve" && in.Decision != "deny") {
		rt.awayAnswerAck(peer.InstanceID, env.ID, false, "invalid one-shot answer")
		return
	}
	if seen, err := db.FederationEnvelopeSeen(peer.InstanceID, env.ID); err != nil {
		rt.awayAnswerAck(peer.InstanceID, env.ID, false, "receipt store unavailable")
		return
	} else if seen {
		rt.sendControl(peer.InstanceID, proto.KindAck, env.ID, proto.AckPayload{Status: proto.AckAccepted})
		return
	}
	approvals.mu.Lock()
	req := approvals.pending[in.Request]
	approvals.mu.Unlock()
	if req == nil {
		rt.awayAnswerAck(peer.InstanceID, env.ID, false, "request no longer pending")
		return
	}
	d := fedAwayDecision{runtime: rt, peer: peer.InstanceID, epoch: in.Epoch, envelope: env.ID, expires: env.ExpiresAt, outcome: outcomeDeny}
	if in.Decision == "approve" {
		d.outcome = outcomeApprove
	}
	rt.awayMu.Lock()
	valid := rt.awayDecisionAllowedLocked(req, d)
	if valid {
		req.mu.Lock()
		duplicate := req.delegatedEnvelope == env.ID
		if req.delegatedEnvelope != "" {
			valid = false
		}
		if valid {
			select {
			case req.delegated <- d:
				req.delegatedEnvelope = env.ID
			default:
				valid = false
			}
		}
		req.mu.Unlock()
		if duplicate {
			rt.awayMu.Unlock()
			return
		} // original consumption sends its ack.
	}
	rt.awayMu.Unlock()
	if !valid {
		rt.awayAnswerAck(peer.InstanceID, env.ID, false, "coverage, grant or request expired")
		return
	}
	// Ack only after the waiter consumes and rechecks the decision.
}
func (rt *fedRuntime) awayDecisionAllowedLocked(req *approvalRequest, d fedAwayDecision) bool {
	if req.peerAccess != nil {
		return false
	}
	if currentFederation() != rt {
		return false
	}
	a := rt.activeAwayLocked()
	if a == nil || a.Cover != d.peer || a.Epoch != d.epoch || !fedPeerAnswers(d.peer) {
		return false
	}
	req.mu.Lock()
	defer req.mu.Unlock()
	return req.delegatedEpoch == d.epoch && !req.delegatedDeadline.IsZero() && time.Now().Before(req.delegatedDeadline)
}
func (rt *fedRuntime) applyAwayDecision(req *approvalRequest, d fedAwayDecision) (bool, bool) {
	rt.awayMu.Lock()
	if !rt.awayDecisionAllowedLocked(req, d) {
		rt.awayMu.Unlock()
		rt.awayAnswerAck(d.peer, d.envelope, false, "coverage, grant or original deadline expired")
		return false, false
	}
	req.mu.Lock()
	req.delegatedDecider = d.peer
	req.delegatedEpoch = ""
	req.mu.Unlock()
	approved := applyApprovalOutcome(req, d.outcome)
	if _, err := db.MarkFederationEnvelopeSeen(d.peer, d.envelope, d.expires); err != nil {
		slog.Warn("federation: failed to persist delegated decision receipt", "error", err)
	}
	rt.awayMu.Unlock()
	rt.awayAnswerAck(d.peer, d.envelope, true, "decision applied")
	return approved, true
}
func (rt *fedRuntime) awayAnswerAck(peer, envelope string, accepted bool, reason string) {
	status, code, httpStatus := proto.AckRefused, "denied", 403
	if accepted {
		status, code, httpStatus = proto.AckAccepted, "", 200
	}
	recordFederationAudit("federation.away.answer.in", peer, "", "", "decider="+peer+" envelope="+envelope+" "+reason, httpStatus)
	rt.sendControl(peer, proto.KindAck, envelope, proto.AckPayload{Status: status, Code: code, Reason: reason})
}

// Preserve multiline questions and UTF-8 while bounding the encrypted mail.
func fedAwayClip(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	s = s[:limit-len("\n…[truncated]")]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "\n…[truncated]"
}
