package agentd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Scheduling and triggers over the peer view (S7a, tcl-s0rmfe; S7b,
// tcl-ck33er). This node's cron jobs and trigger rules fire on this node's
// authority, so a restricted peer reads them and stops them, and never
// authors, rewrites, re-enables or fires one (its own jobs are
// peer_cron_jobs.go's):
//   - a cron job is in a peer's reach when it targets a visible group the peer
//     holds groups.messages.schedule on, or a visible agent with
//     groups.members.schedule in every group it is in, as locally;
//   - in reach, the peer lists a job, reads its runs and disables it.
//     Disabling is reversible; deleting, patching, enabling and run-now stay
//     with the job's owner and this node's operator;
//   - a trigger rule is readable under groups.triggers.read on its visible
//     group. Global rules stay this node's. Rules are authored only by this
//     node's operator (groups.triggers.manage is FedLocal).
//
// What a peer reads is built from an allowlist, so a field added to the
// operator's view later stays the operator's until it is listed. Left out:
// conv IDs, a spawn's instruction template (template text, as S6 keeps a
// performer's prompt), run and firing detail (error text, which can name
// host paths), a firing's event reference (it names the PR's URL, which only
// groups.members.pr reads), and agents and groups the peer does not see.
// Unrestricted trust reads and disables as this node's operator does. An
// agent principal reaches only the jobs it scheduled.

// peerCronJobInReach reports whether a restricted peer reaches job.
func peerCronJobInReach(v *peerView, job *db.AgentCronJob) bool {
	if job.IsGroupTarget() {
		return job.GroupID > 0 && fedPeerGroupVisible(v.peer.InstanceID, job.GroupID) &&
			fedPeerAllows(v.peer.InstanceID, job.GroupID, PermGroupsMessagesSchedule)
	}
	if job.TargetAgent == "" {
		return false
	}
	target, ok := v.visibleAgent(job.TargetAgent)
	return ok && peerAgentActionAllowed(v.peer.InstanceID, target.CurrentConvID, PermGroupsMembersSchedule)
}

var (
	peerCronJobKeys = []string{"id", "name", "target_kind", "target_agent", "group_id", "group_name",
		"target_role", "interval_seconds", "cron_expr", "subject", "body", "action_kind", "spawn_profile", "spawn_roles",
		"spawn_name_template", "spawn_concurrency_policy", "spawn_max_live_workers", "spawn_worker_deadline_seconds",
		"enabled", "run_immediately", "queue_when_offline", "operator_authored", "disabled_reason", "created_at",
		"last_run_at", "last_run_status"}
	peerCronRunKeys    = []string{"id", "job_id", "fired_at", "status", "worker_id", "worker_agent"}
	peerTriggerKeys    = []string{"id", "name", "row_version", "revision", "enabled", "operator_authored", "scope", "group_id", "group", "source", "author_is_agent", "draft_filter", "debounce_seconds", "cooldown_seconds", "for_seconds", "actions", "firings", "dwell_states", "created_at", "updated_at"}
	peerTriggerActKeys = []string{"type", "spawn", "message"}
	peerTriggerSpawn   = []string{"profile", "roles", "name_template", "max_live_workers", "worker_deadline_seconds"}
	peerTriggerMessage = []string{"target", "subject_template", "body_template"}
	peerTriggerFiring  = []string{"id", "rule_id", "rule_revision", "source", "previous_state", "current_state", "agent_id", "agent_harness", "fact_result", "fact_observed_at", "dwell_started_at", "outcome", "started_at", "finished_at", "actions"}
	peerTriggerOutcome = []string{"action_index", "action_type", "outcome", "spawned_agent", "message_id", "created_at"}
	peerTriggerDwell   = []string{"rule_id", "agent_id", "rule_revision", "episode", "result", "harness", "fact_observed_at", "true_since", "fired_at", "updated_at"}
)

// allowKeys is m with only keys, or nil when m is not an object.
func allowKeys(m any, keys []string) map[string]any {
	doc, ok := m.(map[string]any)
	if !ok {
		return nil
	}
	return keepKeys(doc, keys)
}

// allowEach is allowKeys over each element of a list, then passed to then.
func allowEach(list any, keys []string, then func(map[string]any)) []any {
	items, _ := list.([]any)
	out := make([]any, 0, len(items))
	for _, item := range items {
		if kept := allowKeys(item, keys); kept != nil {
			if then != nil {
				then(kept)
			}
			out = append(out, kept)
		}
	}
	return out
}

// peerAllowlisted is value re-encoded through JSON and passed to redact.
func peerAllowlisted(value any, redact func(any) any) (any, bool) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	var doc any
	if json.Unmarshal(raw, &doc) != nil {
		return nil, false
	}
	return redact(doc), true
}

func redactPeerTrigger(doc any) any {
	out := allowKeys(doc, peerTriggerKeys)
	if out == nil {
		return nil
	}
	if acts, ok := out["actions"]; ok {
		out["actions"] = allowEach(acts, peerTriggerActKeys, func(a map[string]any) {
			if s, ok := a["spawn"]; ok && s != nil {
				a["spawn"] = allowKeys(s, peerTriggerSpawn)
			}
			if m, ok := a["message"]; ok && m != nil {
				a["message"] = allowKeys(m, peerTriggerMessage)
			}
		})
	}
	if f, ok := out["firings"]; ok {
		out["firings"] = allowEach(f, peerTriggerFiring, func(firing map[string]any) {
			if a, ok := firing["actions"]; ok {
				firing["actions"] = allowEach(a, peerTriggerOutcome, nil)
			}
		})
	}
	if d, ok := out["dwell_states"]; ok {
		out["dwell_states"] = allowEach(d, peerTriggerDwell, nil)
	}
	return out
}

// peerHoldsInSomeGroup reports whether the peer holds any of slugs on a
// visible group: a list it holds none of answers 403, as an ungranted read
// does, rather than an empty list.
func peerHoldsInSomeGroup(v *peerView, slugs ...string) bool {
	groups, err := db.ListAgentGroups()
	if err != nil {
		return false
	}
	for _, g := range groups {
		if g.IsArchived() || !fedPeerGroupVisible(v.peer.InstanceID, g.ID) {
			continue
		}
		for _, slug := range slugs {
			if fedPeerAllows(v.peer.InstanceID, g.ID, slug) {
				return true
			}
		}
	}
	return false
}

// peerOwnsCronJob reports whether job is one the peer scheduled here: its
// operator's, or for an agent principal that agent's own. The peer's operator
// also manages the jobs its agents scheduled.
func peerOwnsCronJob(v *peerView, job *db.AgentCronJob) bool {
	if job.OwnerPeer == "" || job.OwnerPeer != v.peer.InstanceID {
		return false
	}
	return v.agent == nil || job.OwnerPeerAgent == v.agent.ID
}

// peerSchedules reports whether the peer holds a scheduling grant on any
// visible group: what a list, or a by-id route on a job not its own, needs.
func peerSchedules(v *peerView) bool {
	return peerHoldsInSomeGroup(v, PermGroupsMessagesSchedule, PermGroupsMembersSchedule)
}

// peerCronJobView is the peer's view of job: allowlisted, naming its owner
// and a member job's routing group only when the peer sees them, and its peer
// owner only when that is the asking peer.
func peerCronJobView(v *peerView, j *db.AgentCronJob) (map[string]any, bool) {
	view, ok := peerAllowlisted(toJobJSON(j), func(doc any) any { return allowKeys(doc, peerCronJobKeys) })
	if !ok {
		return nil, false
	}
	doc := view.(map[string]any)
	if !j.IsGroupTarget() && j.GroupID > 0 && !fedPeerGroupVisible(v.peer.InstanceID, j.GroupID) {
		delete(doc, "group_id")
	}
	if j.OwnerAgent != "" {
		if _, seen := v.visibleAgent(j.OwnerAgent); seen {
			doc["owner_agent"] = j.OwnerAgent
		}
	}
	if peerOwnsCronJob(v, j) {
		doc["owner_peer"] = j.OwnerPeer
		if j.OwnerPeerAgent != "" {
			doc["owner_peer_agent"] = j.OwnerPeerAgent
		}
	}
	return doc, true
}

// GET /v1/cron: the jobs in the peer's reach and the ones it scheduled. An
// agent principal lists only its own.
func servePeerCronList(w http.ResponseWriter, r *http.Request, v *peerView, _ peerViewRule) {
	full := v.unrestricted()
	if !full && v.agent == nil && !peerSchedules(v) && !peerHasCronJobs(v) {
		writeError(w, http.StatusForbidden, "permission", "requires "+PermGroupsMessagesSchedule+" or "+PermGroupsMembersSchedule)
		return
	}
	jobs, err := db.ListAgentCronJobs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "list jobs")
		return
	}
	out := make([]any, 0, len(jobs))
	for _, j := range jobs {
		if full {
			out = append(out, toJobJSON(j))
			continue
		}
		own := peerOwnsCronJob(v, j)
		if !own && (v.agent != nil || !peerCronJobInReach(v, j)) {
			continue
		}
		if view, ok := peerCronJobView(v, j); ok {
			out = append(out, view)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out})
}

// peerHasCronJobs reports whether the peer scheduled any job here.
func peerHasCronJobs(v *peerView) bool {
	jobs, err := db.ListAgentCronJobs()
	if err != nil {
		return false
	}
	for _, j := range jobs {
		if peerOwnsCronJob(v, j) {
			return true
		}
	}
	return false
}

// peerCronJob is the job a by-id route names, when the peer reaches it: one
// it scheduled (kept in reach after its grant is gone, so it can still read,
// stop and delete it), or for the peer's operator one in its grants' reach.
// own says which.
func peerCronJob(w http.ResponseWriter, r *http.Request, v *peerView) (job *db.AgentCronJob, own bool, ok bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "id must be a positive integer")
		return nil, false, false
	}
	v.auditDetail = "job: " + strconv.FormatInt(id, 10)
	job, err = db.GetAgentCronJob(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "lookup")
		return nil, false, false
	}
	full := v.unrestricted()
	own = job != nil && peerOwnsCronJob(v, job)
	switch {
	case full || own:
	case v.agent == nil && !peerSchedules(v):
		writeError(w, http.StatusForbidden, "permission", "requires "+PermGroupsMessagesSchedule+" or "+PermGroupsMembersSchedule)
		return nil, false, false
	case job == nil || v.agent != nil || !peerCronJobInReach(v, job):
		writeError(w, http.StatusNotFound, "not_found", "object not found")
		return nil, false, false
	}
	if job == nil {
		writeError(w, http.StatusNotFound, "not_found", "object not found")
		return nil, false, false
	}
	if job.IsGroupTarget() {
		if g, _ := db.GetAgentGroupByID(job.GroupID); g != nil {
			v.groupName = g.Name
		}
	} else {
		v.targetConv = job.TargetConv
	}
	return job, own, true
}

// GET /v1/cron/{id}/logs: a job's recent runs.
func servePeerCronLogs(w http.ResponseWriter, r *http.Request, v *peerView, _ peerViewRule) {
	job, _, ok := peerCronJob(w, r, v)
	if !ok {
		return
	}
	if v.unrestricted() {
		handleCronLogs(w, asDashboardHumanPeer(r), job.ID)
		return
	}
	rec := httptest.NewRecorder()
	handleCronLogs(rec, asDashboardHumanPeer(r), job.ID)
	if rec.Code < 200 || rec.Code >= 300 {
		// This node's error text stays its own.
		writeError(w, rec.Code, "cron_logs", "the job's runs could not be read")
		return
	}
	writeRedacted(w, rec, func(doc map[string]any) {
		doc["runs"] = allowEach(doc["runs"], peerCronRunKeys, nil)
	})
}

// POST /v1/cron/{id}/disable: pauses a job, the peer's own or one in reach.
func servePeerCronDisable(w http.ResponseWriter, r *http.Request, v *peerView, _ peerViewRule) {
	job, _, ok := peerCronJob(w, r, v)
	if !ok {
		return
	}
	if v.unrestricted() {
		handleCronSetEnabled(w, asDashboardHumanPeer(r), job.ID, false)
		return
	}
	// Only an enabled job is paused, as a hand pause: one tclaude paused
	// itself keeps the marker that re-enables it (a group's resume), so a
	// peer's disable stays reversible.
	cronAuthorityMu.Lock()
	defer cronAuthorityMu.Unlock()
	if _, err := db.DisableEnabledAgentCronJob(job.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "io", "update")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// peerTriggerInReach reports whether a restricted peer reads rule.
func peerTriggerInReach(v *peerView, rule *db.TriggerRule) bool {
	return rule.ScopeKind == db.TriggerScopeGroup && rule.GroupID > 0 &&
		fedPeerGroupVisible(v.peer.InstanceID, rule.GroupID) &&
		fedPeerAllows(v.peer.InstanceID, rule.GroupID, PermGroupsTriggersRead)
}

// GET /v1/triggers: the rules the peer reads, each with its latest firing.
func servePeerTriggers(w http.ResponseWriter, r *http.Request, v *peerView, _ peerViewRule) {
	if !v.unrestricted() && !peerHoldsInSomeGroup(v, PermGroupsTriggersRead) {
		writeError(w, http.StatusForbidden, "permission", "requires "+PermGroupsTriggersRead)
		return
	}
	rules, err := db.ListTriggerRules()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "list triggers")
		return
	}
	full := v.unrestricted()
	out := make([]any, 0, len(rules))
	for _, rule := range rules {
		if !full && !peerTriggerInReach(v, rule) {
			continue
		}
		view := triggerView(rule)
		view.Firings, _ = db.ListTriggerFirings(rule.ID, 1)
		if full {
			out = append(out, view)
		} else if doc, ok := peerAllowlisted(view, redactPeerTrigger); ok {
			out = append(out, doc)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"triggers": out})
}

// GET /v1/triggers/{id}: one rule, with its recent firings and dwell states.
func servePeerTrigger(w http.ResponseWriter, r *http.Request, v *peerView, _ peerViewRule) {
	raw := strings.TrimSpace(r.PathValue("id"))
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "id must be positive")
		return
	}
	v.auditDetail = "trigger: " + raw
	rule, err := db.GetTriggerRule(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "lookup")
		return
	}
	full := v.unrestricted()
	if !full && !peerHoldsInSomeGroup(v, PermGroupsTriggersRead) {
		writeError(w, http.StatusForbidden, "permission", "requires "+PermGroupsTriggersRead)
		return
	}
	if rule == nil || (!full && !peerTriggerInReach(v, rule)) {
		writeError(w, http.StatusNotFound, "not_found", "object not found")
		return
	}
	view := triggerView(rule)
	v.groupName = view.Group
	view.Firings, _ = db.ListTriggerFirings(rule.ID, 20)
	view.DwellStates, _ = db.ListTriggerDwellStates(rule.ID)
	if full {
		writeJSON(w, http.StatusOK, view)
		return
	}
	doc, ok := peerAllowlisted(view, redactPeerTrigger)
	if !ok {
		writeError(w, http.StatusInternalServerError, "io", "unreadable trigger view")
		return
	}
	writeJSON(w, http.StatusOK, doc)
}
