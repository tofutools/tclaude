package agentd

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// This host execution capability is separate from the removed sandbox-profile
// break-glass. Generic permission sources never authorize host execution.
const PermHostBreakGlass = db.HostBreakGlassSlug
const breakGlassGrantOrigin = db.HostBreakGlassGrantOrigin

var breakGlassGrantMu sync.Mutex

var liveBreakGlassGrants = db.ListHostBreakGlassGrants

func handleBreakGlassStatus(w http.ResponseWriter, r *http.Request) {
	if peerActionFromRequest(r) != nil {
		writeError(w, http.StatusForbidden, "local_only", "break-glass is local only")
		return
	}
	p := peerFromContext(r.Context())
	switch classify(p) {
	case classHuman, classAgent:
	default:
		requireHuman(w, r, "inspect break-glass grants")
		return
	}
	convID := p.ConvID
	if classify(p) == classHuman {
		convID = ""
	}
	rows, err := liveBreakGlassGrants(convID)
	if err != nil {
		writeError(w, 500, "io", err.Error())
		return
	}
	writeJSON(w, 200, sudoGrantsToJSON(rows, time.Now()))
}

func handleBreakGlassGrant(w http.ResponseWriter, r *http.Request) {
	if peerActionFromRequest(r) != nil {
		writeError(w, 403, "local_only", "break-glass is local only")
		return
	}
	if !requireHuman(w, r, "grant break-glass host execution") {
		return
	}
	var body struct {
		Target   string `json:"target"`
		Duration string `json:"duration"`
		Reason   string `json:"reason"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&body); err != nil {
		writeError(w, 400, "invalid_arg", err.Error())
		return
	}
	if body.Duration == "" {
		body.Duration = "30m"
	}
	dur, err := time.ParseDuration(body.Duration)
	if err != nil || dur <= 0 {
		writeError(w, 400, "invalid_arg", "duration must be a positive Go duration")
		return
	}
	res, _, err := agent.ResolveSelector(strings.TrimSpace(body.Target))
	if err != nil {
		writeError(w, 404, "not_found", err.Error())
		return
	}
	agentID := res.AgentID
	if agentID == "" {
		agentID, _, err = db.EnsureAgentForConv(res.ConvID, "grant")
	}
	if err != nil || agentID == "" {
		writeError(w, 400, "invalid_arg", "target must resolve to an agent")
		return
	}
	breakGlassGrantMu.Lock()
	defer breakGlassGrantMu.Unlock()
	existing, err := liveBreakGlassGrants(res.ConvID)
	if err != nil {
		writeError(w, 500, "io", err.Error())
		return
	}
	now := time.Now()
	grant := &db.SudoGrant{AgentID: agentID, ConvID: res.ConvID, Slug: PermHostBreakGlass, GrantedAt: now, ExpiresAt: now.Add(dur), GrantedBy: breakGlassGrantOrigin, Reason: body.Reason}
	id, err := db.InsertSudoGrantForAgent(grant)
	if err != nil {
		writeError(w, 500, "io", err.Error())
		return
	}
	grant.ID = id
	for _, old := range existing {
		if _, err := db.RevokeSudoGrant(old.ID); err != nil {
			writeError(w, 500, "io", err.Error())
			return
		}
	}
	setAuditTargetConv(r, res.ConvID)
	setAuditDetail(r, "break-glass grant until "+grant.ExpiresAt.Format(time.RFC3339)+": "+body.Reason)
	writeJSON(w, 200, grant)
}

func handleBreakGlassRevoke(w http.ResponseWriter, r *http.Request) {
	if peerActionFromRequest(r) != nil {
		writeError(w, 403, "local_only", "break-glass is local only")
		return
	}
	if !requireHuman(w, r, "revoke break-glass host execution") {
		return
	}
	var body struct {
		Target string `json:"target"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&body); err != nil {
		writeError(w, 400, "invalid_arg", err.Error())
		return
	}
	breakGlassGrantMu.Lock()
	defer breakGlassGrantMu.Unlock()
	target := strings.TrimSpace(body.Target)
	var targetConv string
	if target != "all" {
		res, _, err := agent.ResolveSelector(target)
		if err != nil {
			writeError(w, 404, "not_found", err.Error())
			return
		}
		targetConv = res.ConvID
	}
	rows, err := liveBreakGlassGrants(targetConv)
	if err != nil {
		writeError(w, 500, "io", err.Error())
		return
	}
	for _, row := range rows {
		if _, err = db.RevokeSudoGrant(row.ID); err != nil {
			writeError(w, 500, "io", err.Error())
			return
		}
	}
	setAuditTargetConv(r, targetConv)
	setAuditDetail(r, "break-glass revoke")
	writeJSON(w, 200, map[string]any{"revoked": len(rows)})
}
