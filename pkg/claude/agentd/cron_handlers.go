package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/cronexpr"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// `tclaude agent cron` HTTP surface — recurring scheduled jobs.
//
// A job's target is either a single conv or a whole group. A group
// target uses the `group:<name>` / `group:<id>` selector grammar shared
// with `tclaude agent message`; the scheduler resolves the group's
// membership at fire time and fans the body out to every member.
//
// Permissions model:
//   - GET  /v1/cron          → list jobs visible to caller (own + conv
//                              jobs targeting a group caller owns +
//                              group jobs whose group caller belongs to)
//   - POST /v1/cron          → create a job. Auth depends on the target:
//   - conv target == caller → self.schedule
//   - conv target != caller → agent.schedule, OR caller owns a
//     group containing target
//   - group target          → caller is a member or owner of the
//     target group (mirrors the `group:` multicast gate)
//   - DELETE /v1/cron/{id}   → delete a job (and the by-id enable /
//     disable / run-now / patch routes). Auth: caller is the job's
//     owner_conv, OR — for a conv job — has agent.schedule / owns a
//     group containing target_conv, OR — for a group job — is a member
//     or owner of the target group.
//
// Humans (no Claude ancestor) bypass all permission checks — same
// convention as the rest of the v1 surface.

// jobJSON is the wire-shape for an AgentCronJob row. Mirrors the DB
// struct but uses ISO timestamps and seconds-as-string for human
// friendliness.
type jobJSON struct {
	ID              int64  `json:"id"`
	Name            string `json:"name,omitempty"`
	OwnerAgent      string `json:"owner_agent,omitempty"`
	OwnerConv       string `json:"owner_conv"`
	TargetKind      string `json:"target_kind"`
	TargetAgent     string `json:"target_agent,omitempty"`
	TargetConv      string `json:"target_conv"`
	GroupID         int64  `json:"group_id,omitempty"`
	GroupName       string `json:"group_name,omitempty"`
	TargetRole      string `json:"target_role,omitempty"`
	IntervalSeconds int64  `json:"interval_seconds"`
	CronExpr        string `json:"cron_expr,omitempty"`
	Subject         string `json:"subject,omitempty"`
	Body            string `json:"body"`
	Enabled         bool   `json:"enabled"`
	// DisabledReason marks WHY a disabled job is disabled (schema v94): "" for
	// a normal human enable/disable, or "group-retired" when a retire that
	// emptied the target group auto-paused it. Surfaced so a reader can tell a
	// tclaude-paused rhythm from a hand-paused one — `task-force status`
	// (JOH-346) renders it as "disabled (auto: group-retired)". omitempty:
	// only the exceptional auto-disabled state serializes.
	DisabledReason string `json:"disabled_reason,omitempty"`
	CreatedAt      string `json:"created_at,omitempty"`
	LastRunAt      string `json:"last_run_at,omitempty"`
	LastRunStatus  string `json:"last_run_status,omitempty"`
}

func toJobJSON(j *db.AgentCronJob) jobJSON {
	out := jobJSON{
		ID:              j.ID,
		Name:            j.Name,
		OwnerAgent:      j.OwnerAgent,
		OwnerConv:       j.OwnerConv,
		TargetKind:      j.TargetKind,
		TargetAgent:     j.TargetAgent,
		TargetConv:      j.TargetConv,
		GroupID:         j.GroupID,
		TargetRole:      j.TargetRole,
		IntervalSeconds: j.IntervalSeconds,
		CronExpr:        j.CronExpr,
		Subject:         j.Subject,
		Body:            j.Body,
		Enabled:         j.Enabled,
		DisabledReason:  j.DisabledReason,
		LastRunStatus:   j.LastRunStatus,
	}
	// For a group-target job, resolve the group's display name so the
	// CLI and dashboard can render "group:<name>" without a second
	// fetch. Only group-kind jobs carry a name — a conv-target job
	// routed through a group leaves group_name empty so the discriminator
	// stays unambiguous.
	if j.IsGroupTarget() && j.GroupID > 0 {
		if g, err := db.GetAgentGroupByID(j.GroupID); err == nil && g != nil {
			out.GroupName = g.Name
		}
	}
	if !j.CreatedAt.IsZero() {
		out.CreatedAt = j.CreatedAt.Format(time.RFC3339)
	}
	if !j.LastRunAt.IsZero() {
		out.LastRunAt = j.LastRunAt.Format(time.RFC3339)
	}
	return out
}

func handleCron(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		handleCronList(w, r)
	case http.MethodPost:
		handleCronCreate(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET or POST")
	}
}

func handleCronByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/cron/")
	rest = strings.TrimSuffix(rest, "/")
	if rest == "" {
		writeError(w, http.StatusNotFound, "not_found", "expected /v1/cron/{id}")
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "id must be an integer")
		return
	}
	if len(parts) == 2 {
		switch parts[1] {
		case "logs":
			if r.Method != http.MethodGet {
				writeError(w, http.StatusMethodNotAllowed, "method", "GET")
				return
			}
			handleCronLogs(w, r, id)
		case "enable":
			handleCronSetEnabled(w, r, id, true)
		case "disable":
			handleCronSetEnabled(w, r, id, false)
		case "run-now":
			handleCronRunNow(w, r, id)
		case "":
			writeError(w, http.StatusMethodNotAllowed, "method", "DELETE")
		default:
			writeError(w, http.StatusNotFound, "not_found", "unknown /v1/cron/{id}/"+parts[1])
		}
		return
	}
	switch r.Method {
	case http.MethodDelete:
		handleCronDelete(w, r, id)
	case http.MethodPatch:
		handleCronPatch(w, r, id)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "DELETE or PATCH")
	}
}

func handleCronSetEnabled(w http.ResponseWriter, r *http.Request, id int64, enabled bool) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST")
		return
	}
	job, err := db.GetAgentCronJob(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "lookup: "+err.Error())
		return
	}
	if job == nil {
		writeError(w, http.StatusNotFound, "not_found", "job "+strconv.FormatInt(id, 10)+" not found")
		return
	}
	if _, ok := authCronJob(w, r, job); !ok {
		return
	}
	if err := db.SetAgentCronJobEnabled(id, enabled); err != nil {
		writeError(w, http.StatusInternalServerError, "io", "update: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleCronRunNow(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST")
		return
	}
	job, err := db.GetAgentCronJob(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "lookup: "+err.Error())
		return
	}
	if job == nil {
		writeError(w, http.StatusNotFound, "not_found", "job "+strconv.FormatInt(id, 10)+" not found")
		return
	}
	if _, ok := authCronJob(w, r, job); !ok {
		return
	}
	now := time.Now()
	status := fireCronJob(job, now)
	if err := db.UpdateAgentCronJobLastRun(id, now, status); err != nil {
		writeError(w, http.StatusInternalServerError, "io", "stamp: "+err.Error())
		return
	}
	// Run-history insert is best-effort: the fire already succeeded,
	// and the denorm last_run_status gives callers their primary
	// signal. Don't 500 the response just because we couldn't append
	// the audit row.
	_, _ = db.InsertAgentCronRun(&db.AgentCronRun{
		JobID:   id,
		FiredAt: now,
		Status:  status,
	})
	writeJSON(w, http.StatusOK, map[string]any{"status": status})
}

type runJSON struct {
	ID       int64  `json:"id"`
	JobID    int64  `json:"job_id"`
	FiredAt  string `json:"fired_at"`
	Status   string `json:"status,omitempty"`
	ErrorMsg string `json:"error_msg,omitempty"`
}

func handleCronLogs(w http.ResponseWriter, r *http.Request, id int64) {
	job, err := db.GetAgentCronJob(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "lookup: "+err.Error())
		return
	}
	if job == nil {
		writeError(w, http.StatusNotFound, "not_found", "job "+strconv.FormatInt(id, 10)+" not found")
		return
	}
	// Read access: same visibility rules as ListCron (own / target /
	// group-owner). The human operator sees all; agents are scoped;
	// unidentified / unconfirmed callers are refused fail-closed.
	callerConv, isHuman, ok := authedCaller(w, r)
	if !ok {
		return
	}
	if !isHuman && !jobVisibleTo(job, callerConv) {
		writeError(w, http.StatusForbidden, "permission",
			"caller cannot view logs for this job (not the owner, target, or owner of a group containing the target)")
		return
	}
	limit := 25
	if s := r.URL.Query().Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	runs, err := db.ListAgentCronRunsForJob(id, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "list runs: "+err.Error())
		return
	}
	out := make([]runJSON, 0, len(runs))
	for _, run := range runs {
		j := runJSON{
			ID:       run.ID,
			JobID:    run.JobID,
			Status:   run.Status,
			ErrorMsg: run.ErrorMsg,
		}
		if !run.FiredAt.IsZero() {
			j.FiredAt = run.FiredAt.Format(time.RFC3339)
		}
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": out})
}

func handleCronList(w http.ResponseWriter, r *http.Request) {
	callerConv, isHuman, ok := authedCaller(w, r)
	if !ok {
		return
	}
	all, err := db.ListAgentCronJobs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "list jobs: "+err.Error())
		return
	}

	// The human operator sees everything; agents see jobs they own +
	// jobs targeting any conv in a group they own (manager pattern).
	visible := make([]jobJSON, 0, len(all))
	for _, j := range all {
		if isHuman || jobVisibleTo(j, callerConv) {
			visible = append(visible, toJobJSON(j))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": visible})
}

func jobVisibleTo(j *db.AgentCronJob, callerConv string) bool {
	if callerConv == "" {
		return false
	}
	// Owner/target are the conv-ids recorded when the job was created; the
	// caller is its live conv. Compare on the stable actor (JOH-323) so a
	// caller that reincarnated / ran /clear since scheduling still sees its
	// own job, and a job recorded against a past generation of the target is
	// still visible to that agent's current generation.
	if sameActor(j.OwnerConv, callerConv) {
		return true
	}
	if j.IsGroupTarget() {
		// Group-target job: visible to every member and owner of the
		// target group — the same set that may schedule or receive it.
		if m, err := db.FindMemberInGroup(j.GroupID, callerConv); err == nil && m != nil {
			return true
		}
		owner, err := db.IsAgentGroupOwner(j.GroupID, callerConv)
		return err == nil && owner
	}
	if sameActor(j.TargetConv, callerConv) {
		return true
	}
	return ownerOfGroupContaining(callerConv, j.TargetConv)
}

func handleCronCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     string `json:"name"`
		Target   string `json:"target"`
		Owner    string `json:"owner"`     // optional; humans may attribute the job to a specific conv (default: target)
		Interval string `json:"interval"`  // e.g. "10m", "1h" — parsed via time.ParseDuration
		CronExpr string `json:"cron_expr"` // alternative schedule: a cronexpr expression; mutually exclusive with interval
		Subject  string `json:"subject"`
		Body     string `json:"body"`
		Enabled  *bool  `json:"enabled,omitempty"` // optional; defaults to true
		GroupID  int64  `json:"group_id"`          // optional explicit override; auto-inferred from shared groups when 0
		Role     string `json:"role,omitempty"`    // optional role filter for a group target ("" / "all" = whole group)
	}
	if r.ContentLength == 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "missing request body")
		return
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	if body.Target == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "target is required")
		return
	}
	if body.Body == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "body is required (the message text the cron job sends)")
		return
	}
	if err := validateCronName(body.Name); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	// Schedule: exactly one of interval / cron_expr. The expression path
	// validates through the same parser the scheduler fires with.
	cronSpec := strings.TrimSpace(body.CronExpr)
	intervalSpec := strings.TrimSpace(body.Interval)
	var intervalSeconds int64
	switch {
	case cronSpec != "" && intervalSpec != "":
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"interval and cron_expr are mutually exclusive — pick one schedule mode")
		return
	case cronSpec != "":
		if err := cronexpr.Validate(cronSpec); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
	default:
		d, err := time.ParseDuration(intervalSpec)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg",
				"interval must be a Go duration like 10m / 1h / 30s; got: "+body.Interval)
			return
		}
		if d < 30*time.Second {
			writeError(w, http.StatusBadRequest, "invalid_arg",
				"interval must be >= 30s (the scheduler tick interval)")
			return
		}
		intervalSeconds = int64(d.Seconds())
	}

	ct, err := resolveCronTarget(body.Target)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "resolve target: "+err.Error())
		return
	}
	// A role filter only makes sense for a group target (it narrows the
	// fan-out); reject it on a single-conv target rather than silently ignore.
	if strings.TrimSpace(body.Role) != "" && ct.Kind != db.CronTargetGroup {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"role is only valid for a group: target (it filters group members)")
		return
	}

	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	job := &db.AgentCronJob{
		Name:            body.Name,
		IntervalSeconds: intervalSeconds,
		CronExpr:        cronSpec,
		Subject:         body.Subject,
		Body:            body.Body,
		Enabled:         enabled,
	}

	if ct.Kind == db.CronTargetGroup {
		// Group-target job: the scheduler fans the body out to the
		// group's live membership at fire time. Auth mirrors a `group:`
		// multicast — the caller must be a member or owner of the
		// target group.
		caller, ok := authCronWriteGroup(w, r, ct.Group.ID)
		if !ok {
			return
		}
		job.TargetKind = db.CronTargetGroup
		job.GroupID = ct.Group.ID
		// Role filter (JOH-244): a group-target job may narrow to matching
		// members, resolved against the live roster at fire time. "" or "all"
		// (case-insensitive) = whole group; normalized to "" so the fan-out's
		// empty-filter path handles it.
		role := strings.TrimSpace(body.Role)
		if strings.EqualFold(role, "all") {
			role = ""
		}
		job.TargetRole = role
		// OwnerConv is the message sender at fire time. An agent caller
		// owns the job it scheduled; a human caller may attribute it to
		// a specific conv via `owner`, else it stays "" — the dashboard
		// human, no agent owner. target_conv is unused for group jobs;
		// body.GroupID (the conv-routing override) is irrelevant here.
		job.OwnerConv = caller
		if caller == "" && strings.TrimSpace(body.Owner) != "" {
			ownerConv, ok := resolveCronOwner(w, body.Owner)
			if !ok {
				return
			}
			job.OwnerConv = ownerConv
		}
	} else {
		targetConv := ct.Conv
		caller, ok := authCronWrite(w, r, targetConv)
		if !ok {
			return
		}
		owner := caller
		if owner == "" {
			// Human caller — record the target as owner so the job is
			// self-managed by the target if the human goes away.
			// Reasonable default; humans can override via `owner`.
			owner = targetConv
			if strings.TrimSpace(body.Owner) != "" {
				ownerConv, ok := resolveCronOwner(w, body.Owner)
				if !ok {
					return
				}
				owner = ownerConv
			}
		}
		// Group routing: pick the first shared group between owner and
		// target if the caller didn't override. Falls through to solo
		// (group_id=0) when there's no shared group — the scheduler then
		// direct mailbox delivery.
		groupID := body.GroupID
		if groupID == 0 && owner != targetConv {
			shared, _ := db.SharedGroupsForConvs(owner, targetConv)
			if len(shared) > 0 {
				groupID = shared[0].ID
			}
		}
		job.TargetKind = db.CronTargetConv
		job.TargetConv = targetConv
		job.GroupID = groupID
		job.OwnerConv = owner
	}

	id, err := db.InsertAgentCronJob(job)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "insert: "+err.Error())
		return
	}
	row, _ := db.GetAgentCronJob(id)
	if row == nil {
		writeJSON(w, http.StatusOK, map[string]any{"id": id})
		return
	}
	writeJSON(w, http.StatusOK, toJobJSON(row))
}

// resolveCronTarget turns a cron `target` selector into a concrete
// target. A "group:" prefix — the multicast grammar shared with
// `tclaude agent message` — resolves to a group; anything else resolves
// to a single conv via agent.ResolveSelector.
func resolveCronTarget(selector string) (cronTarget, error) {
	if strings.HasPrefix(selector, multicastPrefix) {
		token := strings.TrimPrefix(selector, multicastPrefix)
		g, err := resolveGroupToken(token)
		if err != nil {
			return cronTarget{}, err
		}
		return cronTarget{Kind: db.CronTargetGroup, Group: g}, nil
	}
	res, _, err := agent.ResolveSelector(selector)
	if err != nil {
		return cronTarget{}, err
	}
	return cronTarget{Kind: db.CronTargetConv, Conv: res.ConvID}, nil
}

// cronTarget is the resolved target of a cron `target` selector —
// either a single conv or a whole group.
type cronTarget struct {
	Kind  string         // db.CronTargetConv | db.CronTargetGroup
	Conv  string         // set when Kind == db.CronTargetConv
	Group *db.AgentGroup // set when Kind == db.CronTargetGroup
}

// resolveGroupToken resolves the token after a "group:" prefix to a
// concrete group: by name first, then — for an all-digit token with no
// name match — by numeric id. The non-HTTP, error-returning twin of
// resolveMulticastGroup; the cron create/patch paths need a group
// without resolveMulticastGroup's response-writing and own-group
// ("group:" with an empty token) behaviour. A cron job is recurring and
// has no sender at fire time, so an empty token has no well-defined
// "own group" and is rejected here.
func resolveGroupToken(token string) (*db.AgentGroup, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New(
			"a group: cron target needs a group name or id, e.g. group:my-team")
	}
	g, err := db.GetAgentGroupByName(token)
	if err != nil {
		return nil, err
	}
	if g != nil {
		return g, nil
	}
	// No name match — fall back to a numeric group id, all-digits only
	// (the documented grammar excludes signed forms like "+7").
	allDigits := true
	for _, r := range token {
		if r < '0' || r > '9' {
			allDigits = false
			break
		}
	}
	if allDigits {
		if id, perr := strconv.ParseInt(token, 10, 64); perr == nil {
			g, err = db.GetAgentGroupByID(id)
			if err != nil {
				return nil, err
			}
			if g != nil {
				return g, nil
			}
		}
	}
	return nil, fmt.Errorf("no group named or numbered %q", token)
}

// resolveCronOwner resolves a human-supplied `owner` selector to a conv
// id. On a resolution failure the 404 response is already written and
// ok is false.
func resolveCronOwner(w http.ResponseWriter, selector string) (string, bool) {
	res, _, err := agent.ResolveSelector(selector)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "resolve owner: "+err.Error())
		return "", false
	}
	return res.ConvID, true
}

// validateCronName enforces the spec's name charset: alphanumeric +
// '-' / '_'. Empty is allowed (name is optional). Stricter than the
// group name validator on purpose — cron-job names appear in subject
// prefixes ("[cron:<name>] ..."), in dashboard table rows, and in the
// `cron logs` output, so the conservative shape avoids quoting +
// rendering surprises across those surfaces.
func validateCronName(name string) error {
	if name == "" {
		return nil
	}
	for _, r := range name {
		isAlnum := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !isAlnum && r != '-' && r != '_' {
			return errors.New("name may contain only alphanumeric, '-', or '_'")
		}
	}
	return nil
}

// handleCronPatch applies a partial update to one job. Validation
// mirrors handleCronCreate; only fields explicitly present in the
// JSON body are touched. last_run_at is never modified — see
// db.UpdateAgentCronJobFields docstring.
func handleCronPatch(w http.ResponseWriter, r *http.Request, id int64) {
	job, err := db.GetAgentCronJob(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "lookup: "+err.Error())
		return
	}
	if job == nil {
		writeError(w, http.StatusNotFound, "not_found", "job "+strconv.FormatInt(id, 10)+" not found")
		return
	}
	if _, ok := authCronJob(w, r, job); !ok {
		return
	}
	patch, ok := decodeCronPatchBody(w, r)
	if !ok {
		return
	}
	n, err := db.UpdateAgentCronJobFields(id, patch)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "update: "+err.Error())
		return
	}
	// Setting an expression re-anchors the schedule at "now" (keeping the
	// last-run status pill): the due check fires an expr job when its next
	// match after last_run_at (or created_at) has passed, so without the
	// re-anchor an edit at 14:00 to "0 9 * * *" would fire within one tick —
	// this morning's 9:00 already passed relative to the old anchor. Real
	// crond's semantics — an edited schedule evaluates forward from the
	// moment of the edit — are what a human expects. Interval edits keep
	// their long-standing behaviour (never bump last_run_at; one catch-up
	// fire after a long idle is semantically true for "every N").
	if n > 0 && patch.CronExpr != nil && *patch.CronExpr != "" {
		if err := db.UpdateAgentCronJobLastRun(id, time.Now(), job.LastRunStatus); err != nil {
			writeError(w, http.StatusInternalServerError, "io", "re-anchor: "+err.Error())
			return
		}
	}
	if n == 0 {
		// Row vanished between Get and Update, or empty patch — both
		// are 200 OK with the current row, just like POST returns the
		// row after insert.
		row, _ := db.GetAgentCronJob(id)
		if row == nil {
			writeError(w, http.StatusNotFound, "not_found",
				"job "+strconv.FormatInt(id, 10)+" not found")
			return
		}
		writeJSON(w, http.StatusOK, toJobJSON(row))
		return
	}
	row, _ := db.GetAgentCronJob(id)
	if row == nil {
		writeJSON(w, http.StatusOK, map[string]any{"id": id})
		return
	}
	writeJSON(w, http.StatusOK, toJobJSON(row))
}

// decodeCronPatchBody decodes the PATCH JSON into a typed db patch.
// Returns ok=false (and writes the error response) on bad input.
// Empty body / no recognised fields is allowed and produces an
// empty patch — handleCronPatch then no-ops cleanly.
func decodeCronPatchBody(w http.ResponseWriter, r *http.Request) (db.UpdateCronPatch, bool) {
	var body struct {
		Name     *string `json:"name,omitempty"`
		Target   *string `json:"target,omitempty"`
		Owner    *string `json:"owner,omitempty"`
		Interval *string `json:"interval,omitempty"`
		CronExpr *string `json:"cron_expr,omitempty"`
		Subject  *string `json:"subject,omitempty"`
		Body     *string `json:"body,omitempty"`
		Enabled  *bool   `json:"enabled,omitempty"`
		GroupID  *int64  `json:"group_id,omitempty"`
		Role     *string `json:"role,omitempty"`
	}
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return db.UpdateCronPatch{}, false
		}
	}
	patch := db.UpdateCronPatch{
		Name:    body.Name,
		Subject: body.Subject,
		Body:    body.Body,
		Enabled: body.Enabled,
		GroupID: body.GroupID,
	}
	// Role filter (JOH-244): normalize "all" → "" (whole group) so the stored
	// value drives the fan-out's empty-filter path.
	if body.Role != nil {
		role := strings.TrimSpace(*body.Role)
		if strings.EqualFold(role, "all") {
			role = ""
		}
		patch.TargetRole = &role
	}
	if body.Name != nil {
		if err := validateCronName(*body.Name); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return db.UpdateCronPatch{}, false
		}
	}
	if body.Body != nil && *body.Body == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"body must not be empty when present (the message text the cron job sends)")
		return db.UpdateCronPatch{}, false
	}
	// Schedule fields preserve the exactly-one-mode invariant without
	// reading the row: setting either mode also clears the other. A
	// non-empty cron_expr switches to expression mode (interval → 0); an
	// interval switches to interval mode (cron_expr → ""). The only legal
	// combination is interval + empty cron_expr (an explicit mode switch);
	// interval + non-empty cron_expr is ambiguous, and an empty cron_expr
	// alone would leave the job with no schedule at all.
	//
	// Presence-normalize first so the two mode-switch shapes are symmetric:
	// {cron_expr: "...", interval: ""} means the same as cron_expr alone,
	// mirroring the blessed {interval: "...", cron_expr: ""} form.
	if body.Interval != nil && strings.TrimSpace(*body.Interval) == "" &&
		body.CronExpr != nil && strings.TrimSpace(*body.CronExpr) != "" {
		body.Interval = nil
	}
	if body.CronExpr != nil {
		expr := strings.TrimSpace(*body.CronExpr)
		if expr != "" {
			if body.Interval != nil {
				writeError(w, http.StatusBadRequest, "invalid_arg",
					"interval and cron_expr are mutually exclusive — pick one schedule mode")
				return db.UpdateCronPatch{}, false
			}
			if err := cronexpr.Validate(expr); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
				return db.UpdateCronPatch{}, false
			}
			zero := int64(0)
			patch.CronExpr = &expr
			patch.IntervalSeconds = &zero
		} else if body.Interval == nil {
			writeError(w, http.StatusBadRequest, "invalid_arg",
				"clearing cron_expr requires an interval in the same patch (a job must keep a schedule)")
			return db.UpdateCronPatch{}, false
		}
	}
	if body.Interval != nil {
		d, err := time.ParseDuration(strings.TrimSpace(*body.Interval))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg",
				"interval must be a Go duration like 10m / 1h / 30s; got: "+*body.Interval)
			return db.UpdateCronPatch{}, false
		}
		if d < 30*time.Second {
			writeError(w, http.StatusBadRequest, "invalid_arg",
				"interval must be >= 30s (the scheduler tick interval)")
			return db.UpdateCronPatch{}, false
		}
		secs := int64(d.Seconds())
		patch.IntervalSeconds = &secs
		empty := ""
		patch.CronExpr = &empty
	}
	if body.Target != nil {
		ct, err := resolveCronTarget(*body.Target)
		if err != nil {
			writeError(w, http.StatusNotFound, "not_found", "resolve target: "+err.Error())
			return db.UpdateCronPatch{}, false
		}
		if ct.Kind == db.CronTargetGroup {
			// Switch the job to a group target: group_id becomes the
			// target group and target_conv is cleared. This overrides any
			// group_id the body also carried.
			kind := db.CronTargetGroup
			gid := ct.Group.ID
			empty := ""
			patch.TargetKind = &kind
			patch.GroupID = &gid
			patch.TargetConv = &empty
		} else {
			kind := db.CronTargetConv
			patch.TargetKind = &kind
			patch.TargetConv = &ct.Conv
		}
	}
	if body.Owner != nil {
		o, ok := resolveCronOwner(w, *body.Owner)
		if !ok {
			return db.UpdateCronPatch{}, false
		}
		patch.OwnerConv = &o
	}
	return patch, true
}

func handleCronDelete(w http.ResponseWriter, r *http.Request, id int64) {
	job, err := db.GetAgentCronJob(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "lookup: "+err.Error())
		return
	}
	if job == nil {
		writeError(w, http.StatusNotFound, "not_found", "job "+strconv.FormatInt(id, 10)+" not found")
		return
	}
	if _, ok := authCronJob(w, r, job); !ok {
		return
	}
	if err := db.DeleteAgentCronJob(id); err != nil {
		writeError(w, http.StatusInternalServerError, "io", "delete: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// authCronWrite gates create/delete. Caller passes if any of:
//
//   - the human operator (classHuman)
//   - target == caller AND caller has self.schedule
//   - caller has agent.schedule
//   - caller owns a group containing the target
//
// Returns (callerConvID, ok); callerConvID is "" for the human.
func authCronWrite(w http.ResponseWriter, r *http.Request, targetConv string) (string, bool) {
	caller, isHuman, ok := authedCaller(w, r)
	if !ok {
		return "", false
	}
	if isHuman {
		return "", true
	}
	// Self path → self.schedule (the laxer, default-granted slug). Match on
	// the stable actor (JOH-323): scheduling on a past generation of oneself
	// (e.g. --target<own-old-conv>) is still a self-action and must not be
	// pushed onto the stricter cross-agent path. sameActor only ever widens
	// the self path to the SAME agent's other generations — two distinct
	// agents still differ and take the cross path unchanged.
	if sameActor(caller, targetConv) {
		// Self path → self.schedule.
		if _, ok := requirePermission(w, r, PermSelfSchedule); !ok {
			return "", false
		}
		return caller, true
	}
	// Cross path → agent.schedule OR group-owner.
	if _, ok := requireCrossAgentPermission(w, r, PermAgentSchedule, targetConv); !ok {
		return "", false
	}
	return caller, true
}

// authCronWriteGroup gates create / patch / delete of a GROUP-target
// cron job. It mirrors handleMulticast's broadcast gate: you may
// schedule (or manage) a recurring multicast into a group only if you
// belong to it or own it. Caller passes if any of:
//
//   - human (no Claude ancestor)
//   - caller is a member of the target group
//   - caller owns the target group
//
// Returns (callerConvID, ok); callerConvID is "" for humans.
func authCronWriteGroup(w http.ResponseWriter, r *http.Request, groupID int64) (string, bool) {
	caller, isHuman, ok := authedCaller(w, r)
	if !ok {
		return "", false
	}
	if isHuman {
		return "", true
	}
	member, err := db.FindMemberInGroup(groupID, caller)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return "", false
	}
	if member != nil {
		return caller, true
	}
	owner, err := db.IsAgentGroupOwner(groupID, caller)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return "", false
	}
	if owner {
		return caller, true
	}
	writeError(w, http.StatusForbidden, "auth",
		"scheduling or managing a recurring multicast into a group requires "+
			"you to be a member or owner of that group")
	return "", false
}

// authCronJob gates the by-id mutations (enable / disable / run-now /
// patch / delete) on an existing job. It dispatches to the conv- or
// group-target gate so a group-target job — whose target_conv is empty
// — is authorised against its target group rather than against "".
// Returns (callerConvID, ok); callerConvID is "" for humans.
func authCronJob(w http.ResponseWriter, r *http.Request, job *db.AgentCronJob) (string, bool) {
	if job.IsGroupTarget() {
		return authCronWriteGroup(w, r, job.GroupID)
	}
	return authCronWrite(w, r, job.TargetConv)
}
