package agentd

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// The capacity spawn queue (tcl-ao1yz5). With capacity.when_full "queue", a
// spawn that does not fit waits here instead of being refused. Only the two
// human-facing spawn surfaces queue: POST /v1/groups/{name}/spawn (`tclaude
// agent spawn`) and the dashboard spawn dialog. Every other launch path keeps
// its own retry or failure handling and is refused when full.
//
// A queued entry keeps the request body as sent, and who sent it. Its launch
// replays that body through handleGroupSpawn as the same requester, so every
// permission, group and profile check runs again at launch time. The body is
// never listed and is dropped when the entry ends.

const (
	spawnQueueTick      = 5 * time.Second
	spawnQueueRetention = 7 * 24 * time.Hour
)

// spawnQueueOriginKey marks a request that arrived on a surface that may queue:
// "api" or "dashboard". Internal replays (federation, awb, bundles) never carry it.
type spawnQueueOriginKey struct{}

// spawnQueueReplayKey carries the entry a queue launch replays.
type spawnQueueReplayKey struct{}

func spawnQueueReplayFrom(r *http.Request) *db.SpawnQueueEntry {
	e, _ := r.Context().Value(spawnQueueReplayKey{}).(*db.SpawnQueueEntry)
	return e
}

func withSpawnQueueOrigin(r *http.Request, origin string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), spawnQueueOriginKey{}, origin))
}

func handleGroupSpawnAPI(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	handleGroupSpawn(w, withSpawnQueueOrigin(r, "api"), g)
}

// spawnQueueCandidate is what the spawn handler knows when it decides between
// launching, queueing and refusing.
type spawnQueueCandidate struct {
	body         *agent.SpawnRequest
	rawBody      string
	spawnerConv  string
	sudoGrant    int64
	proofToken   string
	proofDirs    []string
	cwd          string
	attachments  []string
	externalPeer bool
}

// queueSpawnIfFull queues the spawn when capacity is full (or others already
// wait) and the policy is "queue". It returns true when it wrote the response:
// queued, or refused because the spawn cannot be queued. False lets the launch
// go on; executeSpawn's gate still refuses an over-capacity launch.
func queueSpawnIfFull(w http.ResponseWriter, r *http.Request, g *db.AgentGroup, c spawnQueueCandidate) bool {
	origin, _ := r.Context().Value(spawnQueueOriginKey{}).(string)
	// Non-interactive runs enroll no agent and do not count toward capacity.
	if origin == "" || spawnQueueReplayFrom(r) != nil || c.body.NonInteractive {
		return false
	}
	limits, err := nodeCapacityLimits()
	if err != nil || !limits.Queue || c.body.NoQueue {
		return false
	}
	queued, err := db.CountActiveSpawnQueue()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "node_capacity_unknown", "cannot read the spawn queue")
		return true
	}
	reason := ""
	if queued > 0 {
		reason = fmt.Sprintf("%d spawn(s) already wait for capacity", queued)
	} else {
		if !limits.Limited() {
			return false
		}
		reason, err = nodeCapacityFull(limits, []string{c.cwd})
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "node_capacity_unknown", "cannot observe node capacity: "+err.Error())
			return true
		}
		if reason == "" {
			return false
		}
	}
	if why := spawnNotQueueable(r, c); why != "" {
		writeError(w, http.StatusConflict, fedCodeNodeBusy, reason+"; this spawn cannot be queued: "+why)
		return true
	}
	entry := &db.SpawnQueueEntry{GroupID: g.ID, Name: strings.TrimSpace(c.body.Name), ProofDirs: c.proofDirs}
	switch {
	case c.spawnerConv != "":
		entry.RequesterKind = db.SpawnRequesterAgent
		a, err := db.GetAgentByConv(c.spawnerConv)
		if err != nil || a == nil {
			writeError(w, http.StatusConflict, fedCodeNodeBusy, reason+"; this spawn cannot be queued: the requesting agent has no stable identity")
			return true
		}
		entry.RequesterAgent = a.AgentID
		entry.RequesterName = backgroundConvTitle(c.spawnerConv)
		entry.RequesterMember = convInGroup(c.spawnerConv, g.ID)
	case origin == "dashboard":
		entry.RequesterKind, entry.RequesterName = db.SpawnRequesterDashboard, "operator (dashboard)"
	default:
		entry.RequesterKind, entry.RequesterName = db.SpawnRequesterHuman, "operator"
	}
	// Queueing is a spawn request like any other for the rate limit; the
	// launch later does not claim a second slot.
	if fail := claimSpawnRateSlot(c.spawnerConv); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return true
	}
	id, position, err := db.InsertSpawnQueueEntry(entry, c.rawBody, limits.MaxQueued)
	if errors.Is(err, db.ErrSpawnQueueFull) {
		writeError(w, http.StatusConflict, "spawn_queue_full",
			fmt.Sprintf("%s, and the spawn queue already holds capacity.max_queued (%d) entries", reason, limits.MaxQueued))
		return true
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "queue spawn: "+err.Error())
		return true
	}
	// The proof was checked now; the launch re-checks the same canonical dirs
	// without the marker files, so they go now rather than linger.
	cleanupDirWriteProofMarkers(c.proofToken, c.proofDirs)
	slog.Info("spawn queued for capacity", "queue_id", id, "position", position, "group", g.Name,
		"name", entry.Name, "requester_kind", entry.RequesterKind, "requester_agent", entry.RequesterAgent, "reason", reason)
	kickSpawnQueue()
	writeJSON(w, http.StatusAccepted, map[string]any{
		"group": g.Name, "queued": true, "queue_id": id, "position": position, "queue_reason": reason,
	})
	return true
}

// spawnNotQueueable names what makes a request impossible to replay later, or
// "" when it can wait. Each of these is one-time authority or material held by
// this request only.
func spawnNotQueueable(r *http.Request, c spawnQueueCandidate) string {
	switch {
	case c.externalPeer:
		return "external steward spawns are not queued"
	case peerGrantFromRequest(r) != nil:
		return "spawns on behalf of a federation peer are not queued"
	case c.sudoGrant != 0:
		return "it was authorized by a sudo grant use"
	case len(c.attachments) > 0:
		return "it carries attachments"
	}
	if parseAskHumanHeader(r) > 0 && c.spawnerConv != "" {
		// A human's one-off approval answered this request only. Queue it only
		// when the agent holds the permission without that approval.
		actx := ActionContext{Group: spawnGroupName(spawnGroupFromRequest(r))}
		if allowed, _, _, err := spawnPermissionAllowsAction(r, c.spawnerConv, actx); err != nil || !allowed {
			return "it was approved once by a human, and that approval cannot wait in a queue"
		}
	}
	return ""
}

// spawnGroupFromRequest is only for the ask-human queueability check: the
// group the spawn targets is already resolved, so a nil here just narrows
// the check to the global permission.
func spawnGroupFromRequest(r *http.Request) *db.AgentGroup {
	g, _ := db.GetAgentGroupByName(r.PathValue("name"))
	return g
}

func convInGroup(conv string, groupID int64) bool {
	groups, err := db.ListGroupsForConv(conv)
	if err != nil {
		return false
	}
	for _, g := range groups {
		if g != nil && g.ID == groupID && !g.IsArchived() {
			return true
		}
	}
	return false
}

// queueDirWriteProof checks a queued launch's directories against the ones
// proved when it was queued: the same canonical set, nothing added. The
// requester's own marker files were removed when it queued, so the daemon
// writes markers under a fresh token for those already-proved directories. The
// launch then carries a proof like any direct spawn (worktree git grants, the
// caller-owned trust check, the in-pane cwd check), and executeSpawn removes
// the markers when the launch ends, as for a direct spawn.
func queueDirWriteProof(w http.ResponseWriter, e *db.SpawnQueueEntry, rawDirs []string) (map[string]string, string, bool) {
	resolved, mapping, err := resolveDirWriteProofDirs(rawDirs)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_cwd", err.Error())
		return nil, "", false
	}
	for _, dir := range resolved {
		if !slices.Contains(e.ProofDirs, dir) {
			writeError(w, http.StatusForbidden, "write_proof_failed",
				"launch directory "+dir+" was not proved writable when this spawn was queued")
			return nil, "", false
		}
	}
	token := newDirWriteProofToken()
	for i, dir := range resolved {
		marker := filepath.Join(dir, dirWriteProofFilePrefix+token)
		if err := os.WriteFile(marker, nil, 0o600); err != nil {
			cleanupDirWriteProofMarkers(token, resolved[:i])
			writeError(w, http.StatusInternalServerError, "io", "queued write proof: "+err.Error())
			return nil, "", false
		}
	}
	spawnQueueProofTokens.Lock()
	spawnQueueProofTokens.byEntry[e.ID] = queueProof{token: token, dirs: resolved}
	spawnQueueProofTokens.Unlock()
	return mapping, token, true
}

type queueProof struct {
	token string
	dirs  []string
}

// spawnQueueProofTokens lets the launcher remove a replay's markers when the
// launch was refused before executeSpawn took ownership of them.
var spawnQueueProofTokens = struct {
	sync.Mutex
	byEntry map[int64]queueProof
}{byEntry: map[int64]queueProof{}}

func takeSpawnQueueProof(id int64) (queueProof, bool) {
	spawnQueueProofTokens.Lock()
	defer spawnQueueProofTokens.Unlock()
	p, ok := spawnQueueProofTokens.byEntry[id]
	delete(spawnQueueProofTokens.byEntry, id)
	return p, ok
}

var spawnQueueWake = make(chan struct{}, 1)

func kickSpawnQueue() {
	select {
	case spawnQueueWake <- struct{}{}:
	default:
	}
}

// spawnQueueRun serializes queue passes: two wakeups never launch the same head.
var spawnQueueRun sync.Mutex

func startSpawnQueue(stop <-chan struct{}) {
	go func() {
		reconcileSpawnQueue()
		runSpawnQueue()
		ticker := time.NewTicker(spawnQueueTick)
		defer ticker.Stop()
		lastPrune := time.Time{}
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
			case <-spawnQueueWake:
			}
			runSpawnQueue()
			if time.Since(lastPrune) > time.Hour {
				lastPrune = time.Now()
				if err := db.PruneSpawnQueue(time.Now().Add(-spawnQueueRetention)); err != nil {
					slog.Warn("spawn queue: prune failed", "error", err)
				}
			}
		}
	}()
}

// reconcileSpawnQueue settles entries a previous daemon left launching. The
// launch reserved its agent id before the fork (a pending spawn row, then the
// enrolled agent), so either record means it launched; neither means it
// never reached the spawner and it goes back to its place.
func reconcileSpawnQueue() {
	spawnQueueRun.Lock()
	defer spawnQueueRun.Unlock()
	reconcileSpawnQueueLocked()
}

func reconcileSpawnQueueLocked() {
	entries, err := db.ListSpawnQueue(false)
	if err != nil {
		slog.Warn("spawn queue: reconcile failed", "error", err)
		return
	}
	for _, e := range entries {
		if e.State != db.SpawnQueueLaunching {
			continue
		}
		launched, err := spawnQueueLaunchStarted(e.ReservedAgent)
		if err != nil {
			slog.Warn("spawn queue: reconcile entry failed", "queue_id", e.ID, "error", err)
			continue
		}
		if launched {
			_, _ = db.FinishSpawnQueueEntry(e.ID, db.SpawnQueueLaunched, "launched as "+e.ReservedAgent+" (settled after a daemon restart)")
		} else {
			_, _ = db.RequeueSpawnQueueEntry(e.ID)
		}
	}
}

func spawnQueueLaunchStarted(agentID string) (bool, error) {
	if agentID == "" {
		return false, nil
	}
	if a, err := db.GetAgent(agentID); err != nil {
		return false, err
	} else if a != nil {
		return true, nil
	}
	p, err := db.GetPendingSpawnByAgentID(agentID)
	return p != nil, err
}

// runSpawnQueue launches queue heads while capacity allows, strictly in order.
func runSpawnQueue() {
	spawnQueueRun.Lock()
	defer spawnQueueRun.Unlock()
	for {
		entries, err := db.ListSpawnQueue(false)
		if err != nil {
			slog.Warn("spawn queue: list failed", "error", err)
			return
		}
		if settleFederationQueueEntries(entries) {
			continue
		}
		if len(entries) == 0 {
			return
		}
		head := entries[0]
		if head.State == db.SpawnQueueLaunching {
			// Only a crashed earlier pass leaves this; settle it first.
			reconcileSpawnQueueLocked()
			if again, err := db.GetSpawnQueueEntry(head.ID); err != nil || again == nil || again.State == db.SpawnQueueLaunching {
				return
			}
			continue
		}
		if !launchSpawnQueueHead(head) {
			return
		}
	}
}

// launchSpawnQueueHead tries the head entry once. It returns true when the
// entry ended (launched or failed) and the next one may be tried.
func launchSpawnQueueHead(e *db.SpawnQueueEntry) bool {
	if e.FedRequestID != 0 {
		return promoteFederationQueueHead(e)
	}
	limits, err := nodeCapacityLimits()
	if err != nil {
		return false
	}
	g, _ := db.GetAgentGroupByID(e.GroupID)
	if g == nil || g.IsArchived() {
		finishSpawnQueueEntry(e, db.SpawnQueueFailed, "the target group no longer exists or is archived", "")
		return true
	}
	if limits.Limited() {
		// The directory this launch will use: the request's own, else the group's.
		var requested struct {
			Cwd string `json:"cwd"`
		}
		if body, err := db.SpawnQueueBody(e.ID); err == nil {
			_ = json.Unmarshal([]byte(body), &requested)
		}
		cwd := cmp.Or(strings.TrimSpace(requested.Cwd), g.DefaultCwd)
		if reason, err := nodeCapacityFull(limits, []string{cwd}); err != nil || reason != "" {
			return false
		}
	}
	req, fail := spawnQueueReplayRequest(e, g)
	if fail != "" {
		finishSpawnQueueEntry(e, db.SpawnQueueFailed, fail, "")
		return true
	}
	reserved := db.NewAgentID()
	claimed, err := db.ClaimSpawnQueueEntry(e.ID, reserved)
	if err != nil || !claimed {
		return false
	}
	e.ReservedAgent = reserved
	req = req.WithContext(context.WithValue(req.Context(), reservedAgentIDContextKey{}, reserved))
	rec := httptest.NewRecorder()
	handleGroupSpawn(rec, req, g)
	proof, minted := takeSpawnQueueProof(e.ID)
	if minted && rec.Code >= 300 && !strings.Contains(rec.Body.String(), spawnUnconfirmedKind) {
		// Refused before or by the spawn core, nothing will run there: drop the
		// markers. A launched or possibly-running child keeps them until
		// executeSpawn's own cleanup.
		cleanupDirWriteProofMarkers(proof.token, proof.dirs)
	}
	if rec.Code >= 200 && rec.Code <= 299 {
		var out agent.SpawnResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		finishSpawnQueueEntry(e, db.SpawnQueueLaunched, "launched as "+cmp.Or(out.AgentID, reserved), cmp.Or(out.AgentID, reserved))
		return true
	}
	// writeError's shape: the spawnFailure Kind travels as "code".
	var refusal struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &refusal)
	if refusal.Code == fedCodeNodeBusy || refusal.Code == "node_capacity_unknown" {
		// Capacity went between the check and the gate: keep its place.
		if started, _ := spawnQueueLaunchStarted(reserved); !started {
			_, _ = db.RequeueSpawnQueueEntry(e.ID)
			return false
		}
	}
	msg := strings.TrimSpace(refusal.Error)
	if msg == "" {
		msg = "HTTP " + strconv.Itoa(rec.Code)
	}
	if refusal.Code == spawnUnconfirmedKind {
		// The child may still come up; the pending spawn row now tracks it.
		finishSpawnQueueEntry(e, db.SpawnQueueLaunched, "launch unconfirmed, tracked as a pending spawn: "+msg, reserved)
		return true
	}
	finishSpawnQueueEntry(e, db.SpawnQueueFailed, "launch refused: "+msg, "")
	return true
}

// spawnQueueReplayRequest rebuilds the queued request as its original
// requester. An agent requester must still be active and, if it was a member
// of the group when it queued, still be one; there is no fallback identity.
func spawnQueueReplayRequest(e *db.SpawnQueueEntry, g *db.AgentGroup) (*http.Request, string) {
	body, err := db.SpawnQueueBody(e.ID)
	if err != nil {
		return nil, "cannot read the queued request: " + err.Error()
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/groups/"+url.PathEscape(g.Name)+"/spawn", bytes.NewReader([]byte(body)))
	req.SetPathValue("name", g.Name)
	switch e.RequesterKind {
	case db.SpawnRequesterAgent:
		a, err := db.GetAgent(e.RequesterAgent)
		if err != nil {
			return nil, "cannot read the requesting agent: " + err.Error()
		}
		if a == nil || !a.Active() {
			return nil, "the requesting agent was retired while the spawn waited"
		}
		if a.CurrentConvID == "" {
			return nil, "the requesting agent has no current conversation"
		}
		if e.RequesterMember && !convInGroup(a.CurrentConvID, g.ID) {
			return nil, "the requesting agent left group " + g.Name + " while the spawn waited"
		}
		req = req.WithContext(context.WithValue(req.Context(), peerKey{}, &peer{PID: -1, ConvID: a.CurrentConvID, HasClaudeAncestor: true}))
	case db.SpawnRequesterDashboard:
		req = asDashboardSpawnPeer(req)
	case db.SpawnRequesterHuman:
		req = req.WithContext(context.WithValue(req.Context(), peerKey{}, &peer{PID: os.Getpid(), HumanTokenValid: true}))
	default:
		return nil, "unknown requester kind " + e.RequesterKind
	}
	req = req.WithContext(context.WithValue(req.Context(), spawnQueueReplayKey{}, e))
	return req, ""
}

// finishSpawnQueueEntry ends an entry and tells the requester. Messages name
// the entry, group and outcome only, never the request body.
func finishSpawnQueueEntry(e *db.SpawnQueueEntry, state, reason, agentID string) {
	ended, err := db.FinishSpawnQueueEntry(e.ID, state, reason)
	if err != nil {
		slog.Warn("spawn queue: finish failed", "queue_id", e.ID, "error", err)
		return
	}
	if !ended {
		return
	}
	slog.Info("spawn queue entry ended", "queue_id", e.ID, "group", e.GroupName, "name", e.Name, "state", state, "reason", reason)
	// A remote requester hears the outcome of its request as a spawn result,
	// and the request has its own federation audit trail.
	if state == db.SpawnQueueCancelled || e.RequesterKind == db.SpawnRequesterFederation {
		return
	}
	auditSpawnQueueOutcome(e, state, reason, agentID)
	label := fmt.Sprintf("queued spawn #%d", e.ID)
	if e.Name != "" {
		label += fmt.Sprintf(" (%q)", e.Name)
	}
	var subject, body string
	if state == db.SpawnQueueLaunched {
		subject = "Queued spawn launched"
		body = fmt.Sprintf("Your %s in group %q launched: %s.", label, e.GroupName, reason)
		if agentID != "" {
			body += fmt.Sprintf(" Agent: %s.", agentID)
		}
	} else {
		subject = "Queued spawn failed"
		body = fmt.Sprintf("Your %s in group %q did not launch: %s.", label, e.GroupName, reason)
	}
	if e.RequesterKind == db.SpawnRequesterAgent {
		a, err := db.GetAgent(e.RequesterAgent)
		if err != nil || !a.Active() || a.CurrentConvID == "" {
			return
		}
		if _, _, err := queueRegularAgentMessage(&db.AgentMessage{
			FromConv: "", ToConv: a.CurrentConvID, Subject: "[spawn queue] " + subject, Body: body,
			ToRecipients: []string{a.CurrentConvID}, OperatorAuthored: true,
		}); err != nil {
			slog.Warn("spawn queue: tell requester failed", "queue_id", e.ID, "error", err)
		}
		return
	}
	if _, err := recordHumanMessage("", subject, body); err != nil {
		slog.Warn("spawn queue: tell operator failed", "queue_id", e.ID, "error", err)
	}
}

// spawnQueueEntryJSON is the public shape of an entry: who, where, which
// place, how long. Never the request.
type spawnQueueEntryJSON struct {
	ID            int64  `json:"id"`
	Group         string `json:"group"`
	Name          string `json:"name,omitempty"`
	RequesterKind string `json:"requester_kind"`
	RequestedBy   string `json:"requested_by"`
	RequesterID   string `json:"requester_agent_id,omitempty"`
	State         string `json:"state"`
	Position      int    `json:"position,omitempty"`
	Reason        string `json:"reason,omitempty"`
	CreatedAt     string `json:"created_at"`
	AgeSeconds    int64  `json:"age_seconds"`
}

type spawnQueueCapacityJSON struct {
	MaxLiveAgents int    `json:"max_live_agents"`
	UsedAgents    *int   `json:"used_agents,omitempty"`
	MinFreeRAMMB  int64  `json:"min_free_ram_mb,omitempty"`
	MinFreeDiskMB int64  `json:"min_free_disk_mb,omitempty"`
	WhenFull      string `json:"when_full"`
	MaxQueued     int    `json:"max_queued"`
	Queued        int    `json:"queued"`
}

type spawnQueueListJSON struct {
	Capacity spawnQueueCapacityJSON `json:"capacity"`
	Entries  []spawnQueueEntryJSON  `json:"entries"`
}

func spawnQueueView(e *db.SpawnQueueEntry, now time.Time) spawnQueueEntryJSON {
	return spawnQueueEntryJSON{
		ID: e.ID, Group: e.GroupName, Name: e.Name, RequesterKind: e.RequesterKind,
		RequestedBy: e.RequesterName, RequesterID: e.RequesterAgent, State: e.State,
		Position: e.Position, Reason: e.Reason, CreatedAt: e.CreatedAt.UTC().Format(time.RFC3339),
		AgeSeconds: max(int64(0), int64(now.Sub(e.CreatedAt)/time.Second)),
	}
}

// spawnQueueCapacity is the capacity line: the limits, and the agents counted
// toward max_live_agents when there is one. Queued is left for the caller.
func spawnQueueCapacity(limits config.CapacityLimits) spawnQueueCapacityJSON {
	out := spawnQueueCapacityJSON{MaxLiveAgents: limits.MaxLiveAgents, MinFreeRAMMB: limits.MinFreeRAMMB,
		MinFreeDiskMB: limits.MinFreeDiskMB, WhenFull: config.CapacityWhenFullRefuse, MaxQueued: limits.MaxQueued}
	if limits.Queue {
		out.WhenFull = config.CapacityWhenFullQueue
	}
	if limits.MaxLiveAgents > 0 {
		nodeAdmission.Lock()
		if used, err := nodeCapacityUsed(""); err == nil {
			out.UsedAgents = &used
		}
		nodeAdmission.Unlock()
	}
	return out
}

// spawnQueueCaller resolves who is asking: the human ("" conv) or an agent.
func spawnQueueCaller(w http.ResponseWriter, r *http.Request) (conv string, ok bool) {
	p := peerFromContext(r.Context())
	switch classify(p) {
	case classHuman:
		return "", true
	case classAgent:
		return p.ConvID, true
	case classUnidentified:
		writeError(w, http.StatusUnauthorized, "auth", "could not determine peer PID")
	case classUnconfirmed:
		writeUnconfirmed(w, r)
	default:
		writeError(w, http.StatusForbidden, "auth", "agent identity could not be resolved")
	}
	return "", false
}

// spawnQueueVisible: humans see everything; an agent sees its own entries and
// those targeting a group it is in, as `agent ls` shows members.
func spawnQueueVisible(e *db.SpawnQueueEntry, callerAgent string, callerGroups map[int64]bool) bool {
	return (callerAgent != "" && e.RequesterAgent == callerAgent) || callerGroups[e.GroupID]
}

// GET /v1/spawn-queue[?all=1]
func handleSpawnQueueList(w http.ResponseWriter, r *http.Request) {
	conv, ok := spawnQueueCaller(w, r)
	if !ok {
		return
	}
	entries, err := db.ListSpawnQueue(r.URL.Query().Get("all") == "1")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	limits, _ := nodeCapacityLimits()
	out := spawnQueueListJSON{Entries: []spawnQueueEntryJSON{}, Capacity: spawnQueueCapacity(limits)}
	callerAgent, callerGroups := "", map[int64]bool{}
	if conv != "" {
		callerAgent, _ = db.AgentIDForConv(conv)
		groups, _ := db.ListGroupsForConv(conv)
		for _, g := range groups {
			if g != nil && !g.IsArchived() {
				callerGroups[g.ID] = true
			}
		}
	}
	now := time.Now()
	for _, e := range entries {
		if e.Active() {
			out.Capacity.Queued++
		}
		if conv != "" && !spawnQueueVisible(e, callerAgent, callerGroups) {
			continue
		}
		out.Entries = append(out.Entries, spawnQueueView(e, now))
	}
	writeJSON(w, http.StatusOK, out)
}

// DELETE /v1/spawn-queue/{id} cancels a waiting entry. Allowed for the human,
// the requesting agent, and owners of the target group.
func handleSpawnQueueCancel(w http.ResponseWriter, r *http.Request) {
	conv, ok := spawnQueueCaller(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "queue id must be a positive integer")
		return
	}
	e, err := db.GetSpawnQueueEntry(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if e != nil && conv != "" && e.FedRequestID != 0 {
		// Cancelling a remote request denies it, which is the operator's call
		// (as `federation requests deny`), whoever owns the group.
		if !spawnQueueVisible(e, "", map[int64]bool{e.GroupID: convInGroup(conv, e.GroupID)}) {
			e = nil
		} else {
			writeError(w, http.StatusForbidden, "permission", "only the operator may cancel a queued remote spawn request")
			return
		}
	}
	if e != nil && conv != "" {
		callerAgent, _ := db.AgentIDForConv(conv)
		owner, _ := db.IsAgentGroupOwner(e.GroupID, conv)
		if !owner && (callerAgent == "" || callerAgent != e.RequesterAgent) {
			// Not distinguishable from a missing entry for an agent that may not see it.
			if !spawnQueueVisible(e, callerAgent, map[int64]bool{e.GroupID: convInGroup(conv, e.GroupID)}) {
				e = nil
			} else {
				writeError(w, http.StatusForbidden, "permission",
					"only the requesting agent, an owner of the target group, or the operator may cancel a queued spawn")
				return
			}
		}
	}
	if e == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such queued spawn")
		return
	}
	// Serialize with the launcher so a cancel never races a launch of the same entry.
	spawnQueueRun.Lock()
	ended, err := db.CancelSpawnQueueEntry(e.ID)
	if err == nil && !ended {
		e, err = db.GetSpawnQueueEntry(id)
	}
	spawnQueueRun.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if !ended {
		writeError(w, http.StatusConflict, "not_queued", "queued spawn #"+strconv.FormatInt(id, 10)+" already ended: "+e.State)
		return
	}
	if e.FedRequestID != 0 {
		endQueuedFederationRequest(e.FedRequestID, "cancelled while queued for capacity")
	}
	slog.Info("spawn queue entry cancelled", "queue_id", id, "by", cmp.Or(short8(conv), "operator"))
	kickSpawnQueue()
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "state": db.SpawnQueueCancelled})
}

// auditSpawnQueueOutcome records the daemon's launch (or failed launch) of a
// queued entry, which never passes the HTTP audit middleware. The enqueue was
// audited there as an ordinary spawn row. Only the name, group, requester and
// outcome are recorded, as in the spawn audit's redacted form.
func auditSpawnQueueOutcome(e *db.SpawnQueueEntry, state, reason, agentID string) {
	kind, conv, label := db.AuditActorHuman, "", "operator"
	if e.RequesterKind == db.SpawnRequesterAgent {
		kind, label = db.AuditActorAgent, e.RequesterName
		if a, err := db.GetAgent(e.RequesterAgent); err == nil && a != nil {
			conv = a.CurrentConvID
		}
	}
	status := http.StatusOK
	if state != db.SpawnQueueLaunched {
		status = http.StatusConflict
	}
	entry := db.AuditLogEntry{
		ActorKind: kind, ActorConv: conv, ActorLabel: label, Verb: "spawn.queue." + state,
		TargetLabel: cmp.Or(e.Name, "(unnamed)"), GroupName: e.GroupName,
		Detail: auditClip(fmt.Sprintf("queue #%d: %s", e.ID, reason), 300),
		Method: http.MethodPost, Path: "/v1/groups/" + e.GroupName + "/spawn", Status: status,
		// The deferred effect of the command that queued it, so the same surface.
		Source: db.AuditSourceCLI,
	}
	if e.RequesterKind == db.SpawnRequesterDashboard {
		entry.Source = db.AuditSourceDashboard
	}
	if agentID != "" {
		if a, err := db.GetAgent(agentID); err == nil && a != nil {
			entry.TargetConv = a.CurrentConvID
		}
	}
	if _, err := db.InsertAuditLog(entry); err != nil {
		slog.Warn("spawn queue: audit failed", "queue_id", e.ID, "error", err)
	}
}

// settleFederationQueueEntries ends remote entries whose request no longer
// waits: expired, decided elsewhere, or from a peer no longer trusted. Any
// entry, not only the head, so an expired request does not hold a place.
// True when an entry ended and the queue should be listed again.
func settleFederationQueueEntries(entries []*db.SpawnQueueEntry) bool {
	changed := false
	now := time.Now()
	for _, e := range entries {
		if e.FedRequestID == 0 || e.State != db.SpawnQueueQueued {
			continue
		}
		req, err := db.GetFederationSpawnRequest(e.FedRequestID)
		if err != nil {
			slog.Warn("spawn queue: read remote request failed", "queue_id", e.ID, "request", e.FedRequestID, "error", err)
			continue
		}
		reason := ""
		switch {
		case req == nil:
			reason = "the remote spawn request is gone"
		case req.Status == db.FedSpawnPending:
			// Promoted, but a daemon crash cut the hand-off short: do it again.
			// Automatic approval re-checks the request; at worst the operator
			// is told twice.
			handOffPromotedFederationRequest(req)
			finishSpawnQueueEntry(e, db.SpawnQueueLaunched, fmt.Sprintf("handed on as remote spawn request #%d", req.ID), "")
			changed = true
			continue
		case req.Status == db.FedSpawnLaunching || req.Status == db.FedSpawnApproved:
			finishSpawnQueueEntry(e, db.SpawnQueueLaunched, fmt.Sprintf("handed on as remote spawn request #%d", req.ID), "")
			changed = true
			continue
		case req.Status != db.FedSpawnQueued:
			reason = "the remote spawn request was " + req.Status
		case req.Expired(now):
			reason = "the remote spawn request expired while queued for capacity"
			endQueuedFederationRequest(req.ID, reason)
		default:
			if p, err := db.GetFederationPeer(req.FromInstance); err == nil && p == nil {
				reason = "the requesting instance is no longer trusted"
				endQueuedFederationRequest(req.ID, reason)
			}
		}
		if reason != "" {
			finishSpawnQueueEntry(e, db.SpawnQueueFailed, reason, "")
			changed = true
		}
	}
	return changed
}

// promoteFederationQueueHead hands a remote request at the head of the queue
// to its decision once capacity allows: it becomes pending (reserving its
// slot, under the same gate every launch takes) and then goes to automatic
// approval or the operator, exactly as a request that found room on arrival.
// True when the entry ended.
func promoteFederationQueueHead(e *db.SpawnQueueEntry) bool {
	limits, err := nodeCapacityLimits()
	if err != nil {
		return false
	}
	req, err := db.GetFederationSpawnRequest(e.FedRequestID)
	if err != nil || req == nil || req.Status != db.FedSpawnQueued {
		// settleFederationQueueEntries ends it on the next pass.
		return false
	}
	nodeAdmission.Lock()
	reason := ""
	if limits.Limited() {
		reason, err = nodeCapacityFullLocked(limits, "", []string{fedSpawnRequestDir(req)})
	}
	promoted := false
	if err == nil && reason == "" {
		promoted, err = db.PromoteQueuedFederationSpawnRequest(req.ID)
	}
	nodeAdmission.Unlock()
	if err != nil || !promoted {
		return false
	}
	req.Status = db.FedSpawnPending
	// Hand off before the entry ends, so a crash in between is redone by
	// settleFederationQueueEntries rather than lost.
	handOffPromotedFederationRequest(req)
	finishSpawnQueueEntry(e, db.SpawnQueueLaunched, fmt.Sprintf("handed on as remote spawn request #%d", req.ID), "")
	return true
}

// handOffPromotedFederationRequest gives a request the queue made pending to
// automatic approval or the operator, as on arrival.
func handOffPromotedFederationRequest(req *db.FederationSpawnRequest) {
	peer, _ := db.GetFederationPeer(req.FromInstance)
	if peer == nil {
		// Trust went in between; the pending request now waits for the
		// operator's deny like any request from a peer that was removed.
		return
	}
	if fedPeerAllows(peer.InstanceID, req.GroupID, PermGroupsMembersSpawn) {
		goBackground(func() { autoApproveFederationSpawn(req, peer) })
	} else {
		notifyFederationSpawnRequest(peer, req, req.FromName)
	}
}

// endQueuedFederationRequest denies a remote request that waited in the
// queue and tells its requester why.
func endQueuedFederationRequest(reqID int64, reason string) {
	req, err := db.GetFederationSpawnRequest(reqID)
	if err != nil || req == nil {
		return
	}
	won, err := db.DecideFederationSpawnRequest(req.ID, db.FedSpawnQueued, db.FedSpawnDenied, "", reason)
	if err != nil || !won {
		return
	}
	takeRequestClaudeSubscription(req.ID)
	if peer, _ := db.GetFederationPeer(req.FromInstance); peer != nil {
		_ = queueSpawnResult(req, peer, proto.SpawnResultPayload{Status: proto.SpawnDenied, Reason: reason})
	}
	recordFederationAudit("federation.spawn.in", fedSpawnRequesterLabel(req), "", req.GroupName,
		fmt.Sprintf("#%d denied: %s", req.ID, reason), 409)
}
