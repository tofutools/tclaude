package agentd

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

var federationSpawnCompletionMu sync.Mutex

func notifyFederationSpawn(req *db.FederationSpawnRequest, p *db.FederationPeer, subject, detail string) bool {
	group := db.FederationHumanGroup(p.InstanceID)
	from := "operator@" + proto.SafeName(peerDisplay(p), true) + " (remote)"
	body := detail + "\n\nBrief (remote, untrusted):\n" + req.Brief
	if _, err := db.InsertHumanMessage(&db.HumanMessage{FromTitle: from, GroupName: group, Subject: subject, Body: body}); err != nil {
		slog.Warn("federation: spawn notice failed", "error", err)
		return false
	}
	dispatchHumanMessageNotification("", from, group, subject, body)
	return true
}

// Missing pending/session rows never establish failure: a launcher can still
// appear late. Only lifecycle-confirmed failure or explicit human abandonment
// permits another attempt. Exact reserved identity and readiness establish success.
func reconcileFederationSpawns() {
	federationSpawnCompletionMu.Lock()
	defer federationSpawnCompletionMu.Unlock()
	work, err := db.ListFederationSpawnWork()
	if err != nil {
		return
	}
	for _, req := range work {
		p, err := db.GetFederationPeer(req.FromInstance)
		if err != nil || p == nil {
			continue
		}
		if req.Status == db.FedSpawnLaunching {
			pending, err := db.GetPendingSpawnByAgentID(req.ResultAgent)
			if err != nil {
				continue
			}
			a, err := db.GetAgent(req.ResultAgent)
			if err != nil {
				continue
			}
			ready := false
			if pending == nil && a != nil && a.Active() && a.CurrentConvID != "" && req.LaunchLabel != "" {
				s, err := db.LoadSession(req.LaunchLabel)
				if err == nil && s != nil && s.ConvID == a.CurrentConvID && s.TmuxSession != "" && session.IsTmuxSessionAlive(s.TmuxSession) {
					groups, err := db.ListGroupsForAgent(req.ResultAgent)
					if err == nil {
						for _, g := range groups {
							if g.ID == req.GroupID && !g.IsArchived() {
								ready = true
								break
							}
						}
					}
				}
			}
			if ready {
				won, err := db.DecideFederationSpawnRequest(req.ID, db.FedSpawnLaunching, db.FedSpawnApproved, req.ResultAgent, "")
				if err != nil || !won {
					continue
				}
				req.Status = db.FedSpawnApproved
				// A prior unconfirmed notice must not suppress the completion notice.
				req.NoticeSent = false
				broadcastFederationCatalogs()
			} else if !req.NoticeSent && !req.LaunchStartedAt.IsZero() && time.Since(req.LaunchStartedAt) >= 30*time.Second {
				if notifyFederationSpawn(req, p, fmt.Sprintf("remote spawn request #%d still launching", req.ID), "Startup remains unconfirmed. The launch still occupies its worker slot and cannot be approved again. Inspect the worker before using tclaude federation requests abandon "+strconv.FormatInt(req.ID, 10)+" --acknowledge-late-worker.") {
					_ = db.MarkFederationSpawnNoticeSent(req.ID)
				}
				continue
			} else {
				continue
			}
		}
		if req.Status == db.FedSpawnApproved {
			if !req.ResultSent {
				if err := queueSpawnResult(req, p, proto.SpawnResultPayload{Status: proto.SpawnApproved, Agent: req.ResultAgent, Name: req.Name}); err == nil {
					_ = db.MarkFederationSpawnResultSent(req.ID)
				}
			}
			if req.Automatic && !req.NoticeSent {
				if notifyFederationSpawn(req, p, fmt.Sprintf("remote spawn request #%d auto-approved", req.ID), fmt.Sprintf("Peer %s spawned a worker in group %q under its groups.members.spawn grant.", peerDisplay(p), req.GroupName)) {
					_ = db.MarkFederationSpawnNoticeSent(req.ID)
				}
			}
		} else if req.Status == db.FedSpawnPending && req.Reason != "" && !req.NoticeSent {
			if notifyFederationSpawn(req, p, fmt.Sprintf("remote spawn request #%d needs approval", req.ID), "Launch failed: "+req.Reason+". Request remains pending for human approval.") {
				_ = db.MarkFederationSpawnNoticeSent(req.ID)
			}
		}
	}
}

func markFederationSpawnUnconfirmed(agentID, reason string) {
	if agentID == "" {
		return
	}
	req, err := db.FederationSpawnRequestForAgent(agentID)
	if err != nil || req == nil {
		return
	}
	_ = db.SetFederationSpawnUnconfirmed(req.ID, reason)
}

// This hook is called only where the lifecycle has identified a definite
// failure. Timeout and missing-readiness errors preserve launch ownership.
func federationSpawnFailed(agentID, kind, reason string) {
	if kind == "timeout" || kind == "spawn_unconfirmed" {
		markFederationSpawnUnconfirmed(agentID, reason)
		return
	}
	req, err := db.FederationSpawnRequestForAgent(agentID)
	if err != nil || req == nil {
		return
	}
	_, _ = db.ReturnFederationSpawnToPending(req.ID, reason)
	reconcileFederationSpawns()
}

func handleFederationSpawnRequestAbandon(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "abandon an unconfirmed remote spawn") {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "bad request id")
		return
	}
	var in struct {
		Acknowledge bool `json:"acknowledge_late_worker"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || !in.Acknowledge {
		writeError(w, http.StatusBadRequest, "invalid_arg", "acknowledge_late_worker is required: WARNING a late worker may still appear after abandonment")
		return
	}
	won, err := db.ReturnFederationSpawnToPending(id, "operator abandoned unconfirmed launch; a late worker may still appear")
	if err != nil {
		writeFedErr(w, err)
		return
	}
	if !won {
		writeError(w, http.StatusConflict, "decided", "request is not launching")
		return
	}
	setAuditTargetLabel(r, fmt.Sprintf("#%d", id))
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": db.FedSpawnPending, "warning": "a late worker may still appear; inspect the original launch before approving again"})
	reconcileFederationSpawns()
}

// Exposed for deterministic flow checks of restart reconciliation.
func ReconcileFederationSpawnsForTest() { reconcileFederationSpawns() }
