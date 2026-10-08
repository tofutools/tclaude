package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/client"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

const teleportLeaseKind = proto.KindTeleportLease

var teleportLeaseMu sync.Mutex

type teleportLeaseFrame struct {
	Op       string `json:"op"`
	Offer    string `json:"offer"`
	Agent    string `json:"agent"`
	Epoch    int64  `json:"epoch"`
	Sequence int64  `json:"sequence,omitempty"`
	ReturnID string `json:"return_id,omitempty"`
	Findings string `json:"findings,omitempty"`
	Policy   string `json:"policy,omitempty"`
}
type teleportLeaseObservation struct {
	LastLive time.Time
	Sent     time.Time
}
type teleportLeaseObservations struct {
	Pulse time.Time
	Rows  map[string]teleportLeaseObservation
}

func teleportBackupPolicy() (config.TeleportBackupConfig, error) {
	c, err := config.Load()
	if err != nil {
		return config.TeleportBackupConfig{}, err
	}
	var p config.TeleportBackupConfig
	if c.Federation != nil && c.Federation.Teleport != nil {
		p = c.Federation.Teleport.Backup
	}
	p = p.Effective()
	return p, p.Validate()
}
func reserveTeleportBackup(peer, offer, agent, conv string, expires time.Time) error {
	p, err := teleportBackupPolicy()
	if err != nil {
		return err
	}
	return db.ReserveFederationTeleportLease(db.FederationTeleportLease{Direction: "out", Peer: peer, Offer: offer, SourceAgent: agent, SourceConv: conv, Epoch: 1, State: "reserved", ExpiresAt: expires, RenewSeconds: p.RenewSeconds, LeaseSeconds: p.LeaseSeconds, GraceSeconds: p.GraceSeconds}, p.DormantMax)
}
func pausedTeleportDescription(agent string) string {
	l, err := db.TeleportBackupForAgent(agent)
	if err != nil || l == nil || l.State == "reserved" {
		return ""
	}
	return fmt.Sprintf("paused (teleported to %s/%s)", l.Peer, l.TargetAgent)
}
func teleportResumeBlocked(conv string) error {
	id, err := db.AgentIDForConv(conv)
	if err != nil {
		return err
	}
	if id == "" {
		return nil
	}
	l, err := db.TeleportBackupForAgent(id)
	if err != nil {
		return err
	}
	if l != nil && l.State != "reserved" {
		return errors.New("agent is a paused teleport backup; use agent teleport recover after the lease observation period")
	}
	remote, e := db.TeleportIncomingLeaseForAgent(id)
	if e != nil {
		return e
	}
	if remote != nil && remote.State != "active" && remote.State != "clone" {
		return errors.New("roaming teleport lease has ended or is returning; ordinary resume is refused")
	}
	return nil
}

// Caller holds the move reconciler lock. Lease state is committed before the
// verified stop; a crash retries the same pause and never retires the actor.
func pauseConfirmedTeleport(m db.FederationAgentMove) bool {
	t, err := db.GetFederationTeleport("out", m.Peer, m.ID)
	if err != nil || t == nil || !t.Intent.KeepPausedBackup {
		return false
	}
	teleportLeaseMu.Lock()
	defer teleportLeaseMu.Unlock()
	l, err := db.GetFederationTeleportLease("out", m.Peer, m.ID)
	if err != nil || l == nil {
		return true
	}
	if l.State == "reserved" {
		if err = moveAuthority(m); err != nil {
			m.State = "blocked"
			m.LastError = err.Error()
			_, _ = db.TransitionFederationAgentMove(m, "confirmed")
			return true
		}
		l.State = "pausing"
		l.TargetAgent = m.TargetAgent
		won, e := db.TransitionFederationTeleportLease(*l, "")
		if e != nil || !won {
			return true
		}
		l.Revision++
	}
	if l.State != "pausing" && l.State != "paused" {
		return true
	}
	if l.State == "pausing" {
		if !prepareTeleportShutdown(l, m.SourceConv) {
			return true
		}
		res, outcome := stopOneConvAndWait(m.SourceConv, false, db.AgentExitActionStop, "", 0)
		if res.Action == "error" || outcome == softExitStuck || outcome == softExitUnattempted || teleportShutdownAlive(*l) {
			l.LastError = res.Detail
			if l.LastError == "" {
				l.LastError = "pinned pane process still alive; waiting for verified exit"
			}
			_, _ = db.TransitionFederationTeleportLease(*l, "")
			return true
		}
		l.State = "paused"
		l.LastError = ""
		l.LastRenewed = time.Now()
		l.ExpiresAt = l.LastRenewed.Add(time.Duration(l.LeaseSeconds) * time.Second)
		if won, e := db.TransitionFederationTeleportLease(*l, ""); e != nil || !won {
			return true
		}
		recordFederationAudit("teleport.pause", m.Peer, m.SourceAgent, m.Group, "offer="+m.ID, 200)
	}
	old := m.State
	m.State = "paused"
	m.LastError = ""
	_, _ = db.TransitionFederationAgentMove(m, old)
	return true
}
func ensureIncomingTeleportLease(m db.FederationAgentMove) error {
	t, err := db.GetFederationTeleport("in", m.Peer, m.ID)
	if err != nil || t == nil || !t.Intent.KeepPausedBackup {
		return err
	}
	teleportLeaseMu.Lock()
	defer teleportLeaseMu.Unlock()
	l, err := db.GetFederationTeleportLease("in", m.Peer, m.ID)
	if err != nil {
		return err
	}
	if l != nil {
		if l.TargetAgent != m.TargetAgent || l.State != "active" {
			return errors.New("teleport lease already settled")
		}
		return nil
	}
	return db.ReserveFederationTeleportLease(db.FederationTeleportLease{Direction: "in", Peer: m.Peer, Offer: m.ID, SourceAgent: m.SourceAgent, SourceConv: m.SourceConv, TargetAgent: m.TargetAgent, Epoch: 1, State: "active", RenewSeconds: t.Intent.BackupRenewSeconds}, 1)
}
func (rt *fedRuntime) sendTeleportLease(l db.FederationTeleportLease, f teleportLeaseFrame) {
	f.Offer = l.Offer
	f.Agent = l.TargetAgent
	rt.sendControl(l.Peer, teleportLeaseKind, "", f)
}

// Observation time is deliberately runtime-local. Restart, disconnect, and a
// long scheduling/suspend gap restart the full online wait. Offline wall time
// must never turn opening a laptop into immediate failover.
func (rt *fedRuntime) observeTeleportOnline(now time.Time) bool {
	return rt.teleportLeases.observe(now, rt.cl.Status().State == client.StateConnected)
}
func (o *teleportLeaseObservations) observe(now time.Time, connected bool) bool {
	if !connected {
		*o = teleportLeaseObservations{}
		return false
	}
	// Strip the monotonic reading: Go's monotonic clock may exclude suspend.
	// Wall time detects a closed laptop and restarts the observation window.
	now = now.UTC()
	if o.Pulse.IsZero() || now.Sub(o.Pulse) > 5*time.Second || now.Before(o.Pulse) {
		o.Rows = map[string]teleportLeaseObservation{}
	}
	o.Pulse = now
	return true
}
func (rt *fedRuntime) teleportObservation(l db.FederationTeleportLease, now time.Time) teleportLeaseObservation {
	o := rt.teleportLeases.Rows[l.Offer]
	if o.LastLive.IsZero() {
		o.LastLive = now
	}
	return o
}
func teleportLeaseWait(l db.FederationTeleportLease) time.Duration {
	return time.Duration(l.LeaseSeconds+l.GraceSeconds) * time.Second
}
func (rt *fedRuntime) reconcileTeleportLeases() {
	if !teleportLeaseMu.TryLock() {
		return
	}
	defer teleportLeaseMu.Unlock()
	if currentFederation() != rt {
		return
	}
	now := time.Now()
	if !rt.observeTeleportOnline(now) {
		return
	}
	p, err := teleportBackupPolicy()
	if err != nil {
		return
	}
	rows, err := db.ListActiveFederationTeleportLeases()
	if err != nil {
		return
	}
	for _, l := range rows {
		if l.Direction == "out" {
			if l.State != "released" && l.State != "recovered" {
				a, e := db.GetAgent(l.SourceAgent)
				if e != nil {
					continue
				}
				if a == nil || !a.Active() || a.CurrentConvID != l.SourceConv {
					l.State = "released"
					l.Epoch++
					_, _ = db.TransitionFederationTeleportLease(l, "")
					continue
				}
			}
			if l.State == "reserved" {
				m, e := db.GetFederationAgentMove("out", l.Peer, l.Offer)
				if e == nil && (!l.ExpiresAt.After(now) || m != nil && (m.State == "declined" || m.State == "expired" || m.State == "abandoned" || m.State == "blocked")) {
					l.State = "released"
					l.Epoch++
					_, _ = db.TransitionFederationTeleportLease(l, "")
				}
				continue
			}
			if l.State == "recovering" {
				rt.resumeTeleportBackup(&l)
				continue
			}
			if l.State != "paused" && l.State != "recovery_needed" {
				continue
			}
			o := rt.teleportObservation(l, now)
			rt.teleportLeases.Rows[l.Offer] = o
			if now.Sub(o.LastLive) < teleportLeaseWait(l) {
				continue
			}
			if p.Recovery == "manual" {
				if l.State != "recovery_needed" {
					l.State = "recovery_needed"
					_, _ = db.TransitionFederationTeleportLease(l, teleportLeaseLostBriefing(l)+"\nManual recovery required: agent teleport recover "+l.SourceAgent)
					recordFederationAudit("teleport.recovery_needed", l.Peer, l.SourceAgent, "", "offer="+l.Offer, 200)
				}
			} else {
				rt.beginTeleportRecovery(&l, teleportLeaseLostBriefing(l))
			}
			continue
		}
		if l.State == "superseding" {
			rt.stopSupersededTeleport(&l)
			continue
		}
		if l.State == "returning" {
			rt.stopReturningTeleport(&l)
		}
		if l.State != "active" && l.State != "stopped" {
			continue
		}
		o := rt.teleportObservation(l, now)
		if !o.Sent.IsZero() && now.Sub(o.Sent) < time.Duration(l.RenewSeconds)*time.Second {
			continue
		}
		o.Sent = now
		rt.teleportLeases.Rows[l.Offer] = o
		if l.State == "stopped" {
			rt.sendTeleportLease(l, teleportLeaseFrame{Op: "return", Epoch: l.Epoch, ReturnID: l.ReturnID, Findings: l.Findings})
			continue
		}
		a, e := db.GetAgent(l.TargetAgent)
		if e != nil {
			continue
		}
		op := "renew"
		if a == nil || !a.Active() || !isConvOnline(a.CurrentConvID) {
			op = "gone"
		}
		l.Sequence++
		if won, e := db.TransitionFederationTeleportLease(l, ""); e != nil || !won {
			continue
		}
		rt.sendTeleportLease(l, teleportLeaseFrame{Op: op, Epoch: l.Epoch, Sequence: l.Sequence})
	}
}
func teleportLeaseLostBriefing(l db.FederationTeleportLease) string {
	return fmt.Sprintf("Teleport lease lost for %s/%s (offer %s). Resuming this backup. The remote copy may still be alive in a network partition. Do not redo destructive or one-time work without checking the remote and current state first.", l.Peer, l.TargetAgent, l.Offer)
}
func (rt *fedRuntime) beginTeleportRecovery(l *db.FederationTeleportLease, briefing string) {
	l.State = "recovering"
	l.Epoch++
	l.LastError = ""
	won, err := db.TransitionFederationTeleportLease(*l, briefing)
	if err != nil || !won {
		return
	}
	l.Revision++
	recordFederationAudit("teleport.recover", l.Peer, l.SourceAgent, "", fmt.Sprintf("offer=%s epoch=%d", l.Offer, l.Epoch), 200)
	rt.resumeTeleportBackup(l)
}
func (rt *fedRuntime) resumeTeleportBackup(l *db.FederationTeleportLease) {
	a, err := db.GetAgent(l.SourceAgent)
	if err != nil || a == nil || !a.Active() || a.CurrentConvID != l.SourceConv {
		l.LastError = "backup identity was retired or changed"
		_, _ = db.TransitionFederationTeleportLease(*l, "")
		return
	}
	lock := resumeLaunchLock(l.SourceConv)
	lock.Lock()
	// The private boolean bypass is only used after the durable recovery CAS.
	res := resumeOneConvUnderLaunchLockForTeleport(l.SourceConv, false, nil, true)
	lock.Unlock()
	if res.Action != "resumed" && res.Action != "skipped:already_online" {
		l.LastError = res.Action + ": " + res.Detail
		_, _ = db.TransitionFederationTeleportLease(*l, "")
		return
	}
	l.State = "recovered"
	l.LastError = ""
	if won, e := db.TransitionFederationTeleportLease(*l, ""); e != nil || !won {
		return
	}
	rt.sendTeleportLease(*l, teleportLeaseFrame{Op: "superseded", Epoch: l.Epoch, Policy: teleportSupersededPolicy()})
	db.NotifyStatusChanged()
}
func teleportSupersededPolicy() string {
	p, err := teleportBackupPolicy()
	if err != nil {
		return "stop"
	}
	return p.Superseded
}
func (rt *fedRuntime) acceptTeleportLease(peer *db.FederationPeer, env *proto.Envelope) {
	var f teleportLeaseFrame
	if env.From.Agent != "" || env.To.Agent != "" || env.DecodePayload(&f) != nil || !proto.ValidStreamID(f.Offer) || !proto.ValidAgentRef(f.Agent) || f.Epoch < 1 || f.Sequence < 0 || len(f.Findings) > 16<<10 || len(f.ReturnID) > 128 {
		return
	}
	p, err := teleportBackupPolicy()
	if err != nil {
		return
	}
	// These controls are retried freshly; old queued controls never count as a
	// new observation or settle a new epoch.
	if time.Since(env.CreatedAt) > time.Duration(p.RenewSeconds*2+15)*time.Second || env.CreatedAt.After(time.Now().Add(5*time.Second)) {
		return
	}
	teleportLeaseMu.Lock()
	defer teleportLeaseMu.Unlock()
	if currentFederation() != rt {
		return
	}
	switch f.Op {
	case "renew", "gone", "return":
		l, e := db.GetFederationTeleportLease("out", peer.InstanceID, f.Offer)
		if e != nil || l == nil || l.TargetAgent != f.Agent && (l.TargetAgent != "" || l.State != "released") {
			return
		}
		if l.TargetAgent == "" && l.State == "released" {
			l.TargetAgent = f.Agent
			if won, e := db.TransitionFederationTeleportLease(*l, ""); e != nil || !won {
				return
			}
			l.Revision++
		}
		if f.Op == "return" && f.Epoch <= l.Epoch && (l.State == "recovered" || l.State == "recovering") && proto.ValidStreamID(f.ReturnID) {
			if l.ReturnID == "" {
				l.ReturnID = f.ReturnID
				l.Findings = f.Findings
				won, e := db.TransitionFederationTeleportLease(*l, "Late teleport findings from "+l.Peer+"/"+l.TargetAgent+" (backup already recovering or recovered; no second resume):\n\n"+f.Findings)
				if e != nil || !won {
					return
				}
				l.Revision++
				recordFederationAudit("teleport.report.late", l.Peer, l.TargetAgent, "", "offer="+l.Offer+" return="+f.ReturnID, 200)
			}
			rt.sendTeleportLease(*l, teleportLeaseFrame{Op: "superseded", Epoch: l.Epoch, Policy: teleportSupersededPolicy()})
			return
		}
		if l.Epoch != f.Epoch || l.State == "recovered" || l.State == "recovering" || l.State == "released" {
			rt.sendTeleportLease(*l, teleportLeaseFrame{Op: "superseded", Epoch: l.Epoch, Policy: teleportSupersededPolicy()})
			return
		}
		if l.State != "paused" && l.State != "recovery_needed" {
			return
		}
		if f.Op == "return" {
			if !proto.ValidStreamID(f.ReturnID) {
				return
			}
			l.ReturnID = f.ReturnID
			l.Findings = f.Findings
			recordFederationAudit("teleport.report", l.Peer, l.TargetAgent, "", "offer="+l.Offer+" return="+f.ReturnID, 200)
			rt.beginTeleportRecovery(l, "Teleport return from "+l.Peer+"/"+l.TargetAgent+". The remote confirmed it stopped.\n\nFindings:\n"+f.Findings)
			return
		}
		if f.Op == "renew" {
			m, e := db.GetFederationAgentMove("out", l.Peer, l.Offer)
			if e != nil || m == nil {
				return
			}
			allowed, _, e := permissionAllowsAction(httpRequestAsAgent(l.SourceConv), l.SourceConv, PermSelfTeleport, ActionContext{RemotePeer: l.Peer, RemoteGroup: m.Group})
			if e != nil || !allowed {
				return
			}
		}
		if f.Sequence <= l.Sequence {
			return
		}
		l.Sequence = f.Sequence
		if f.Op == "renew" {
			l.LastRenewed = time.Now()
			l.ExpiresAt = l.LastRenewed.Add(time.Duration(l.LeaseSeconds) * time.Second)
			l.State = "paused"
			if rt.observeTeleportOnline(time.Now()) {
				o := rt.teleportObservation(*l, time.Now())
				o.LastLive = time.Now()
				rt.teleportLeases.Rows[l.Offer] = o
			}
		}
		if won, e := db.TransitionFederationTeleportLease(*l, ""); e == nil && won {
			rt.sendTeleportLease(*l, teleportLeaseFrame{Op: "ack", Epoch: l.Epoch, Sequence: f.Sequence})
		}
	case "ack": // informational; missing acknowledgements never stop the remote
	case "superseded":
		l, e := db.GetFederationTeleportLease("in", peer.InstanceID, f.Offer)
		if e != nil || l == nil || l.TargetAgent != f.Agent || f.Epoch <= l.Epoch {
			return
		}
		// A fresh response from the pinned origin may only end this exact lease.
		l.Epoch = f.Epoch
		if f.Policy == "clone" && l.State == "active" {
			l.State = "clone"
			if won, e := db.TransitionFederationTeleportLease(*l, ""); e == nil && won {
				a, _ := db.GetAgent(l.TargetAgent)
				if a != nil {
					_, _ = db.InsertAgentMessage(&db.AgentMessage{ToConv: a.CurrentConvID, Subject: "Teleport superseded; retained as clone", Body: "The origin resumed its backup. You are now an independent clone; coordinate before repeating destructive or one-time work."})
				}
				recordFederationAudit("teleport.clone", l.Peer, l.TargetAgent, "", "offer="+l.Offer, 200)
			}
			return
		}
		l.State = "superseding"
		if won, e := db.TransitionFederationTeleportLease(*l, ""); e == nil && won {
			l.Revision++
			rt.stopSupersededTeleport(l)
		}
	}
}
func (rt *fedRuntime) stopSupersededTeleport(l *db.FederationTeleportLease) {
	a, err := db.GetAgent(l.TargetAgent)
	if err != nil {
		return
	}
	if a != nil && a.Active() {
		if !prepareTeleportShutdown(l, a.CurrentConvID) {
			return
		}
		res, out := stopOneConvAndWait(a.CurrentConvID, false, db.AgentExitActionStop, "", 0)
		if res.Action == "error" || out == softExitStuck || out == softExitUnattempted || teleportShutdownAlive(*l) {
			l.LastError = res.Detail
			if l.LastError == "" {
				l.LastError = "pinned pane process still alive; waiting for verified exit"
			}
			_, _ = db.TransitionFederationTeleportLease(*l, "")
			return
		}
	}
	l.State = "superseded"
	l.LastError = ""
	_, _ = db.TransitionFederationTeleportLease(*l, "")
	recordFederationAudit("teleport.superseded", l.Peer, l.TargetAgent, "", "offer="+l.Offer, 200)
}
func (rt *fedRuntime) stopReturningTeleport(l *db.FederationTeleportLease) {
	a, err := db.GetAgent(l.TargetAgent)
	if err != nil || a == nil {
		return
	}
	if !prepareTeleportShutdown(l, a.CurrentConvID) {
		return
	}
	res, out := stopOneConvAndWait(a.CurrentConvID, false, db.AgentExitActionStop, "", 0)
	if res.Action == "error" || out == softExitStuck || out == softExitUnattempted || teleportShutdownAlive(*l) {
		l.LastError = res.Detail
		if l.LastError == "" {
			l.LastError = "pinned pane process still alive; waiting for verified exit"
		}
		_, _ = db.TransitionFederationTeleportLease(*l, "")
		return
	}
	l.State = "stopped"
	l.LastError = ""
	if won, e := db.TransitionFederationTeleportLease(*l, ""); e == nil && won {
		l.Revision++
	}
}
func handleTeleportReport(w http.ResponseWriter, r *http.Request) {
	caller, ok := requireAgent(w, r)
	if !ok {
		return
	}
	var in struct {
		Findings string `json:"findings"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&in) != nil || len(in.Findings) > 16<<10 {
		writeError(w, 400, "invalid_arg", "findings exceed 16 KiB or invalid JSON")
		return
	}
	beginTeleportReport(w, r, caller, in.Findings)
}
func beginTeleportReport(w http.ResponseWriter, r *http.Request, caller, findings string) {
	a, err := db.GetAgentByConv(caller)
	if err != nil || a == nil || !a.Active() || a.CurrentConvID != caller {
		writeError(w, 409, "source", "current active agent required")
		return
	}
	t, err := db.FederationTeleportForAgent(a.AgentID)
	if err != nil || t == nil || !t.Intent.KeepPausedBackup {
		writeError(w, 409, "no_backup", "no paused teleport backup")
		return
	}
	// Reporting is confined to the caller's already-admitted lease. It cannot
	// select a peer or launch a new remote identity, so it needs no new travel grant.
	teleportLeaseMu.Lock()
	defer teleportLeaseMu.Unlock()
	l, err := db.GetFederationTeleportLease("in", t.Peer, t.Offer)
	if err != nil || l == nil || l.TargetAgent != a.AgentID || l.State != "active" && l.State != "returning" && l.State != "stopped" {
		writeError(w, 409, "lease", "lease is not active")
		return
	}
	if l.State == "active" {
		l.ReturnID = proto.NewEnvelopeID()
		l.Findings = findings
		l.State = "returning"
		won, e := db.TransitionFederationTeleportLease(*l, "")
		if e != nil || !won {
			writeError(w, 409, "lease", "lease changed")
			return
		}
		recordFederationAudit("teleport.report", l.Peer, l.TargetAgent, "", "offer="+l.Offer+" return="+l.ReturnID, 200)
	} else if l.Findings != findings {
		writeError(w, 409, "report_pending", "a different report is already pending")
		return
	}
	writeJSON(w, 202, map[string]any{"state": l.State, "return_id": l.ReturnID, "offer": l.Offer})
}
func handleTeleportRecover(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Agent string `json:"agent"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in) != nil || !proto.ValidAgentRef(in.Agent) {
		writeError(w, 400, "invalid_arg", "source stable agent ID required")
		return
	}
	a, err := db.GetAgent(in.Agent)
	if err != nil || a == nil {
		writeError(w, 404, "agent", "backup agent not found")
		return
	}
	if _, ok := requireCrossAgentPermission(w, r, PermAgentResume, a.CurrentConvID); !ok {
		return
	}
	rt := currentFederation()
	if rt == nil {
		writeError(w, 409, "offline", "origin must be online for the full lease plus grace observation period")
		return
	}
	teleportLeaseMu.Lock()
	defer teleportLeaseMu.Unlock()
	l, err := db.TeleportBackupForAgent(in.Agent)
	if err != nil || l == nil {
		writeError(w, 409, "no_backup", "no paused backup")
		return
	}
	_, err = teleportBackupPolicy()
	if err != nil || !rt.observeTeleportOnline(time.Now()) {
		writeError(w, 409, "offline", "origin must be online")
		return
	}
	o := rt.teleportObservation(*l, time.Now())
	rt.teleportLeases.Rows[l.Offer] = o
	if l.State != "recovering" && (l.State != "paused" && l.State != "recovery_needed" || time.Since(o.LastLive) < teleportLeaseWait(*l)) {
		writeError(w, 409, "lease_active", "wait for a full online lease plus grace without renewal; no force bypass")
		return
	}
	if l.State == "recovering" {
		rt.resumeTeleportBackup(l)
	} else {
		rt.beginTeleportRecovery(l, teleportLeaseLostBriefing(*l))
	}
	writeJSON(w, 202, map[string]any{"state": l.State, "offer": l.Offer, "epoch": l.Epoch, "error": l.LastError})
}

// Persist process incarnation before teardown. A crash after tmux removes the
// pane must not turn "no session" into proof that its harness process exited.
func prepareTeleportShutdown(l *db.FederationTeleportLease, conv string) bool {
	if l.ShutdownConv != "" && l.ShutdownConv != conv {
		l.LastError = "generation changed during teleport shutdown; inspect before proceeding"
		_, _ = db.TransitionFederationTeleportLease(*l, "")
		return false
	}
	if l.ShutdownPID != 0 {
		return true
	}
	sess := pickAliveSession(conv)
	if sess == nil {
		return true
	}
	target, err := captureLifecycleTarget(sess)
	if err != nil {
		l.LastError = err.Error()
		_, _ = db.TransitionFederationTeleportLease(*l, "")
		return false
	}
	l.ShutdownPID = target.panePID
	l.ShutdownProcessStart = moveProcessStart(target.panePID)
	l.ShutdownConv = conv
	won, err := db.TransitionFederationTeleportLease(*l, "")
	if err != nil || !won {
		return false
	}
	l.Revision++
	return true
}
func teleportShutdownAlive(l db.FederationTeleportLease) bool {
	return moveShutdownProcessAlive(db.FederationAgentMove{ShutdownPID: l.ShutdownPID, ShutdownProcessStart: l.ShutdownProcessStart})
}
