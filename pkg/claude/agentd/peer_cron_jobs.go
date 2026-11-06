package agentd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// Peer-owned cron jobs (S7b, tcl-ck33er). A restricted peer, or one of its
// agents, schedules recurring messages here under the grants it holds: a
// group target needs groups.messages.schedule on a visible group, an agent
// target groups.members.schedule in every group the agent is in. Such a job
// is the peer's (owner_peer), never this node's operator's:
//   - it sends messages only; a peer never spawns (S6);
//   - every fire rechecks that the peer is still linked and its grants still
//     reach the target, and records peer_denied, delivering nothing, when not;
//   - what it delivers is federation-inbound mail from the peer, under the
//     untrusted-content banner naming it, never an operator-authored message;
//   - the peer patches, enables, fires, disables and deletes its own jobs, a
//     patch rechecking what a create checks; unlinking deletes them.
// Unrestricted trust schedules as this node's operator does.

// peerCronJobRequest is what a peer may send to create or patch its job.
type peerCronJobRequest struct {
	Name             *string `json:"name"`
	Target           string  `json:"target"`
	Interval         *string `json:"interval"`
	CronExpr         *string `json:"cron_expr"`
	Subject          *string `json:"subject"`
	Body             *string `json:"body"`
	Role             *string `json:"role"`
	ActionKind       string  `json:"action_kind"`
	Enabled          *bool   `json:"enabled"`
	RunImmediately   bool    `json:"run_immediately"`
	QueueWhenOffline *bool   `json:"queue_when_offline"`
}

var errPeerCronSpawn = errors.New("a linked node schedules messages only; it never spawns on this node")

// decodePeerCronJobRequest reads a create or patch body. Any other field (a
// spawn's, an owner, a routing group) is refused.
func decodePeerCronJobRequest(w http.ResponseWriter, r *http.Request) (*peerCronJobRequest, error) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, operatorMessageMaxBody+8192))
	if err != nil {
		return nil, errors.New("request body too large")
	}
	var in peerCronJobRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return nil, errors.New("expected name, target, interval or cron_expr, subject, body, role, enabled, run_immediately and queue_when_offline: " + err.Error())
	}
	if kind := strings.TrimSpace(in.ActionKind); kind != "" && kind != db.CronActionMessage {
		return nil, errPeerCronSpawn
	}
	if in.Body != nil && (strings.TrimSpace(*in.Body) == "" || len(*in.Body) > operatorMessageMaxBody) {
		return nil, fmt.Errorf("body is required, at most %d bytes", operatorMessageMaxBody)
	}
	if in.Subject != nil && len([]rune(*in.Subject)) > operatorMessageMaxSubject {
		return nil, fmt.Errorf("subject is at most %d characters", operatorMessageMaxSubject)
	}
	if in.Name != nil {
		if err := validateCronName(*in.Name); err != nil {
			return nil, err
		}
	}
	return &in, nil
}

// peerCronTarget resolves a target the peer names: "group:<name>" or an
// agent id. It answers the status a failure takes.
type peerCronTarget struct {
	group *db.AgentGroup // the target group, or a member job's routing group
	agent *db.Agent
}

func resolvePeerCronTarget(v *peerView, selector string) (*peerCronTarget, int, error) {
	selector = strings.TrimSpace(selector)
	if name, ok := strings.CutPrefix(selector, "group:"); ok {
		g, err := db.GetAgentGroupByName(strings.TrimSpace(name))
		if err != nil || g == nil || g.IsArchived() || !fedPeerGroupVisible(v.peer.InstanceID, g.ID) {
			return nil, http.StatusNotFound, errPeerNotFound
		}
		v.groupName = g.Name
		if !fedPeerAllows(v.peer.InstanceID, g.ID, PermGroupsMessagesSchedule) {
			return nil, http.StatusForbidden, fmt.Errorf("requires %s for group:%s", PermGroupsMessagesSchedule, g.Name)
		}
		return &peerCronTarget{group: g}, 0, nil
	}
	if selector == "" {
		return nil, http.StatusBadRequest, errors.New("target is group:<name> or an agent id")
	}
	agent, ok := v.visibleAgent(selector)
	if !ok {
		return nil, http.StatusNotFound, errPeerNotFound
	}
	v.targetConv = agent.CurrentConvID
	if !peerAgentActionAllowed(v.peer.InstanceID, agent.CurrentConvID, PermGroupsMembersSchedule) {
		return nil, http.StatusForbidden, fmt.Errorf("requires %s for every group the agent is in", PermGroupsMembersSchedule)
	}
	// Its messages are routed through a group the peer sees (all of its
	// groups are granted).
	out := &peerCronTarget{agent: agent}
	if groups, err := db.ListGroupsForConv(agent.CurrentConvID); err == nil {
		for _, g := range groups {
			if !g.IsArchived() && fedPeerGroupVisible(v.peer.InstanceID, g.ID) {
				out.group = g
				break
			}
		}
	}
	return out, 0, nil
}

// POST /v1/cron: schedules a job owned by the peer.
func servePeerCronCreate(w http.ResponseWriter, r *http.Request, v *peerView, _ peerViewRule) {
	if v.unrestricted() {
		handleCronCreate(w, asDashboardHumanPeer(r))
		return
	}
	in, err := decodePeerCronJobRequest(w, r)
	if err != nil {
		peerCronRequestError(w, err)
		return
	}
	if in.Body == nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "body is required (the message the job sends)")
		return
	}
	target, status, err := resolvePeerCronTarget(v, in.Target)
	if err != nil {
		code := map[int]string{http.StatusNotFound: "not_found", http.StatusForbidden: "permission"}[status]
		if code == "" {
			code = "invalid_arg"
		}
		writeError(w, status, code, err.Error())
		return
	}
	cronSpec, interval, err := parseCronSchedule(deref(in.CronExpr), deref(in.Interval))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	job := &db.AgentCronJob{
		Name: deref(in.Name), CronExpr: cronSpec, IntervalSeconds: interval,
		Subject: deref(in.Subject), Body: *in.Body, Enabled: in.Enabled == nil || *in.Enabled,
		RunImmediately: in.RunImmediately, QueueWhenOffline: in.QueueWhenOffline != nil && *in.QueueWhenOffline,
		ActionKind: db.CronActionMessage, OwnerPeer: v.peer.InstanceID,
	}
	if v.agent != nil {
		job.OwnerPeerAgent = v.agent.ID
	}
	if job.RunImmediately && !job.Enabled {
		writeError(w, http.StatusBadRequest, "invalid_arg", "run_immediately requires enabled=true")
		return
	}
	role := strings.TrimSpace(deref(in.Role))
	switch {
	case target.agent == nil:
		job.TargetKind, job.GroupID = db.CronTargetGroup, target.group.ID
		if !strings.EqualFold(role, "all") {
			job.TargetRole = role
		}
	case role != "":
		writeError(w, http.StatusBadRequest, "invalid_arg", "role is only valid for a group: target")
		return
	default:
		job.TargetKind, job.TargetConv = db.CronTargetConv, target.agent.CurrentConvID
		if target.group != nil {
			job.GroupID = target.group.ID
		}
	}
	cronAuthorityMu.Lock()
	defer cronAuthorityMu.Unlock()
	id, err := db.InsertAgentCronJob(job)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "could not save the job")
		return
	}
	v.auditDetail = fmt.Sprintf("job: %d", id)
	row, err := db.GetAgentCronJob(id)
	if err != nil || row == nil {
		writeError(w, http.StatusInternalServerError, "io", "could not read the job back")
		return
	}
	if row.RunImmediately {
		_, _ = fireCronJobAndRecord(row, time.Now())
		if again, err := db.GetAgentCronJob(id); err == nil && again != nil {
			row = again
		}
	}
	writePeerCronJob(w, v, row)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func peerCronRequestError(w http.ResponseWriter, err error) {
	if errors.Is(err, errPeerCronSpawn) {
		writeError(w, http.StatusForbidden, "permission", err.Error())
		return
	}
	writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
}

func writePeerCronJob(w http.ResponseWriter, v *peerView, job *db.AgentCronJob) {
	view, ok := peerCronJobView(v, job)
	if !ok {
		writeError(w, http.StatusInternalServerError, "io", "unreadable job view")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// peerOwnJob is the job a by-id route names when it is the peer's own; a job
// of this node in reach answers 409, its change and firing being its owner's
// and this node's operator's.
func peerOwnJob(w http.ResponseWriter, r *http.Request, v *peerView) (*db.AgentCronJob, bool) {
	job, own, ok := peerCronJob(w, r, v)
	if !ok {
		return nil, false
	}
	if !own {
		writeError(w, http.StatusConflict, "cron_job_not_yours", "this node's job; only its owner or this node's operator changes, fires or deletes it")
		return nil, false
	}
	return job, true
}

// peerCronStillReaches answers 403 when the peer's grants no longer reach its
// job's target: what would fire it, or keep it firing, is refused.
func peerCronStillReaches(w http.ResponseWriter, v *peerView, job *db.AgentCronJob) bool {
	if peerCronJobInReach(v, job) {
		return true
	}
	writeError(w, http.StatusForbidden, "permission", "the grants this node gave yours no longer reach the job's target")
	return false
}

// PATCH /v1/cron/{id}: changes the peer's own job: its name, schedule,
// subject, body, role and offline delivery. Its target and action stay.
func servePeerCronPatch(w http.ResponseWriter, r *http.Request, v *peerView, _ peerViewRule) {
	if v.unrestricted() {
		if job, _, ok := peerCronJob(w, r, v); ok {
			handleCronPatch(w, asDashboardHumanPeer(r), job.ID)
		}
		return
	}
	job, ok := peerOwnJob(w, r, v)
	if !ok {
		return
	}
	in, err := decodePeerCronJobRequest(w, r)
	if err != nil {
		peerCronRequestError(w, err)
		return
	}
	if strings.TrimSpace(in.Target) != "" || in.Enabled != nil || in.RunImmediately {
		writeError(w, http.StatusBadRequest, "invalid_arg", "a patch does not change the target or enabled state: delete and schedule again, or enable it")
		return
	}
	if !peerCronStillReaches(w, v, job) {
		return
	}
	patch := db.UpdateCronPatch{Name: in.Name, Subject: in.Subject, Body: in.Body, QueueWhenOffline: in.QueueWhenOffline}
	if in.Role != nil {
		if !job.IsGroupTarget() {
			writeError(w, http.StatusBadRequest, "invalid_arg", "role is only valid for a group: target")
			return
		}
		role := strings.TrimSpace(*in.Role)
		if strings.EqualFold(role, "all") {
			role = ""
		}
		patch.TargetRole = &role
	}
	if in.CronExpr != nil || in.Interval != nil {
		cronSpec, interval, err := parseCronSchedule(deref(in.CronExpr), deref(in.Interval))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		patch.CronExpr, patch.IntervalSeconds = &cronSpec, &interval
	}
	if patch.Empty() {
		writeError(w, http.StatusBadRequest, "invalid_arg", "nothing to change")
		return
	}
	cronAuthorityMu.Lock()
	defer cronAuthorityMu.Unlock()
	if _, err := db.UpdateAgentCronJobFields(job.ID, patch); err != nil {
		writeError(w, http.StatusInternalServerError, "io", "could not update the job")
		return
	}
	row, err := db.GetAgentCronJob(job.ID)
	if err != nil || row == nil {
		writeError(w, http.StatusInternalServerError, "io", "could not read the job back")
		return
	}
	writePeerCronJob(w, v, row)
}

// DELETE /v1/cron/{id}: deletes the peer's own job.
func servePeerCronDelete(w http.ResponseWriter, r *http.Request, v *peerView, _ peerViewRule) {
	if v.unrestricted() {
		if job, _, ok := peerCronJob(w, r, v); ok {
			handleCronDelete(w, asDashboardHumanPeer(r), job.ID)
		}
		return
	}
	job, ok := peerOwnJob(w, r, v)
	if !ok {
		return
	}
	cronAuthorityMu.Lock()
	defer cronAuthorityMu.Unlock()
	if err := db.DeleteAgentCronJob(job.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "io", "could not delete the job")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /v1/cron/{id}/enable: enables the peer's own job, while its grants
// still reach the target.
func servePeerCronEnable(w http.ResponseWriter, r *http.Request, v *peerView, _ peerViewRule) {
	if v.unrestricted() {
		if job, _, ok := peerCronJob(w, r, v); ok {
			handleCronSetEnabled(w, asDashboardHumanPeer(r), job.ID, true)
		}
		return
	}
	job, ok := peerOwnJob(w, r, v)
	if !ok || !peerCronStillReaches(w, v, job) {
		return
	}
	cronAuthorityMu.Lock()
	defer cronAuthorityMu.Unlock()
	if err := db.SetAgentCronJobEnabled(job.ID, true); err != nil {
		writeError(w, http.StatusInternalServerError, "io", "could not enable the job")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /v1/cron/{id}/run-now: fires the peer's own job once.
func servePeerCronRunNow(w http.ResponseWriter, r *http.Request, v *peerView, _ peerViewRule) {
	if v.unrestricted() {
		if job, _, ok := peerCronJob(w, r, v); ok {
			handleCronRunNow(w, asDashboardHumanPeer(r), job.ID)
		}
		return
	}
	job, ok := peerOwnJob(w, r, v)
	if !ok || !peerCronStillReaches(w, v, job) {
		return
	}
	cronAuthorityMu.Lock()
	defer cronAuthorityMu.Unlock()
	job, err := db.GetAgentCronJob(job.ID)
	if err != nil || job == nil {
		writeError(w, http.StatusNotFound, "not_found", "object not found")
		return
	}
	status, err := fireCronJobAndRecord(job, time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "could not record the run")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": status})
}

// cronPeerDenied is a peer job's run status when the peer is gone or its
// grants no longer reach the target: nothing is delivered.
const cronPeerDenied = "peer_denied"

// firePeerCronJob delivers one run of a peer-owned job, after rechecking the
// peer's authority, as federation-inbound mail from the peer.
func firePeerCronJob(j *db.AgentCronJob, subject string) string {
	p, err := db.GetFederationPeer(j.OwnerPeer)
	if err != nil || p == nil {
		return cronPeerDenied
	}
	var principal *proto.PeerViewAgent
	name := "operator"
	if j.OwnerPeerAgent != "" {
		policy, err := db.GetFederationPeerAgentViews(p.InstanceID)
		if err != nil || policy != db.FederationPeerAgentViewsAllow {
			return cronPeerDenied
		}
		principal = &proto.PeerViewAgent{ID: j.OwnerPeerAgent}
		name = "agent " + proto.SafeName(j.OwnerPeerAgent, false)
	}
	v := &peerView{peer: p, agent: principal}
	if !peerCronJobInReach(v, j) {
		return cronPeerDenied
	}
	var alive map[string]struct{}
	if !j.QueueWhenOffline {
		alive = cronLiveTmuxSessions(j.ID)
	}
	var recipients []string
	if j.IsGroupTarget() {
		members, err := db.ListAgentGroupMembers(j.GroupID)
		if err != nil {
			return "send_failed"
		}
		for _, m := range members {
			if j.TargetRole != "" && !roleLabelMatches(m.Role, j.TargetRole) {
				continue
			}
			conv, _ := walkSuccession(m.ConvID)
			recipients = append(recipients, conv)
		}
	} else {
		conv, _ := walkSuccession(j.TargetConv)
		recipients = append(recipients, conv)
	}
	if len(recipients) == 0 {
		return "no_target"
	}
	body := fedRemoteBanner(name, peerDisplay(p), p.InstanceID) + j.Body
	delivered, skipped := 0, 0
	for _, conv := range recipients {
		if !j.QueueWhenOffline && !isConvOnlineIn(conv, alive) {
			skipped++
			continue
		}
		_, err := db.InsertFederationInboundMessage(&db.AgentMessage{GroupID: j.GroupID, ToConv: conv, Subject: subject, Body: body,
			ToRecipients: []string{conv}}, db.FederationInbound{EnvelopeID: uuid.NewString(), FromInstance: p.InstanceID,
			FromAgent: j.OwnerPeerAgent, FromName: name}, time.Now().Add(24*time.Hour), regularAgentMessageQueueLimit, nil)
		if err != nil {
			return "send_failed"
		}
		enqueueDeliveryForConv(conv)
		delivered++
	}
	switch {
	case delivered == 0:
		return "skipped_offline"
	case skipped > 0:
		return "partial_offline"
	}
	return "ok"
}
