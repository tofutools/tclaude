package agentd

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// `tclaude agent cron` HTTP surface — recurring scheduled jobs.
//
// Permissions model:
//   - GET  /v1/cron          → list jobs visible to caller (own + jobs
//                              targeting any conv in a group caller owns)
//   - POST /v1/cron          → create a job. Auth depends on the target:
//                              - target == caller → self.schedule
//                              - target != caller → agent.schedule, OR
//                                caller owns a group containing target
//   - DELETE /v1/cron/{id}   → delete a job. Auth: caller is the job's
//                              owner_conv, OR has agent.schedule, OR owns
//                              a group containing the job's target_conv
//
// Humans (no Claude ancestor) bypass all permission checks — same
// convention as the rest of the v1 surface.

// jobJSON is the wire-shape for an AgentCronJob row. Mirrors the DB
// struct but uses ISO timestamps and seconds-as-string for human
// friendliness.
type jobJSON struct {
	ID              int64  `json:"id"`
	Name            string `json:"name,omitempty"`
	OwnerConv       string `json:"owner_conv"`
	TargetConv      string `json:"target_conv"`
	GroupID         int64  `json:"group_id,omitempty"`
	IntervalSeconds int64  `json:"interval_seconds"`
	Subject         string `json:"subject,omitempty"`
	Body            string `json:"body"`
	Enabled         bool   `json:"enabled"`
	CreatedAt       string `json:"created_at,omitempty"`
	LastRunAt       string `json:"last_run_at,omitempty"`
	LastRunStatus   string `json:"last_run_status,omitempty"`
}

func toJobJSON(j *db.AgentCronJob) jobJSON {
	out := jobJSON{
		ID:              j.ID,
		Name:            j.Name,
		OwnerConv:       j.OwnerConv,
		TargetConv:      j.TargetConv,
		GroupID:         j.GroupID,
		IntervalSeconds: j.IntervalSeconds,
		Subject:         j.Subject,
		Body:            j.Body,
		Enabled:         j.Enabled,
		LastRunStatus:   j.LastRunStatus,
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
	if _, ok := authCronWrite(w, r, job.TargetConv); !ok {
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
	if _, ok := authCronWrite(w, r, job.TargetConv); !ok {
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
	// group-owner). Humans always pass.
	p := peerFromContext(r.Context())
	if p.HasClaudeAncestor && !jobVisibleTo(job, p.ConvID) {
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
	p := peerFromContext(r.Context())
	all, err := db.ListAgentCronJobs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "list jobs: "+err.Error())
		return
	}

	// Humans see everything; agents see jobs they own + jobs targeting
	// any conv in a group they own (manager pattern).
	visible := make([]jobJSON, 0, len(all))
	for _, j := range all {
		if !p.HasClaudeAncestor || jobVisibleTo(j, p.ConvID) {
			visible = append(visible, toJobJSON(j))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": visible})
}

func jobVisibleTo(j *db.AgentCronJob, callerConv string) bool {
	if callerConv == "" {
		return false
	}
	if j.OwnerConv == callerConv || j.TargetConv == callerConv {
		return true
	}
	return ownerOfGroupContaining(callerConv, j.TargetConv)
}

func handleCronCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name       string `json:"name"`
		Target     string `json:"target"`
		Interval   string `json:"interval"` // e.g. "10m", "1h" — parsed via time.ParseDuration
		Subject    string `json:"subject"`
		Body       string `json:"body"`
		GroupID    int64  `json:"group_id"` // optional explicit override; auto-inferred from shared groups when 0
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
	d, err := time.ParseDuration(strings.TrimSpace(body.Interval))
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

	res, _, err := agent.ResolveSelector(body.Target)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "resolve target: "+err.Error())
		return
	}
	targetConv := res.ConvID

	// Auth: who is the caller?
	caller, ok := authCronWrite(w, r, targetConv)
	if !ok {
		return
	}
	owner := caller
	if owner == "" {
		// Human caller — record the target as owner so the job is
		// self-managed by the target if the human goes away. Reasonable
		// default; humans can override with an explicit owner_conv field
		// in v2 if needed.
		owner = targetConv
	}

	// Group routing: pick the first shared group between owner and
	// target if the caller didn't override. Falls through to solo
	// (group_id=0) when there's no shared group — scheduler will
	// then send-keys directly.
	groupID := body.GroupID
	if groupID == 0 && owner != targetConv {
		shared, _ := db.SharedGroupsForConvs(owner, targetConv)
		if len(shared) > 0 {
			groupID = shared[0].ID
		}
	}

	id, err := db.InsertAgentCronJob(&db.AgentCronJob{
		Name:            body.Name,
		OwnerConv:       owner,
		TargetConv:      targetConv,
		GroupID:         groupID,
		IntervalSeconds: int64(d.Seconds()),
		Subject:         body.Subject,
		Body:            body.Body,
		Enabled:         true,
	})
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
	if _, ok := authCronWrite(w, r, job.TargetConv); !ok {
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
		Interval *string `json:"interval,omitempty"`
		Subject  *string `json:"subject,omitempty"`
		Body     *string `json:"body,omitempty"`
		Enabled  *bool   `json:"enabled,omitempty"`
		GroupID  *int64  `json:"group_id,omitempty"`
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
	}
	if body.Target != nil {
		res, _, err := agent.ResolveSelector(*body.Target)
		if err != nil {
			writeError(w, http.StatusNotFound, "not_found", "resolve target: "+err.Error())
			return db.UpdateCronPatch{}, false
		}
		t := res.ConvID
		patch.TargetConv = &t
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
	if _, ok := authCronWrite(w, r, job.TargetConv); !ok {
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
//   - human (no Claude ancestor)
//   - target == caller AND caller has self.schedule
//   - caller has agent.schedule
//   - caller owns a group containing the target
//
// Returns (callerConvID, ok); callerConvID is "" for humans.
func authCronWrite(w http.ResponseWriter, r *http.Request, targetConv string) (string, bool) {
	p := peerFromContext(r.Context())
	if p.PID == 0 {
		writeError(w, http.StatusUnauthorized, "auth",
			"could not determine peer PID; refusing to evaluate permission")
		return "", false
	}
	if !p.HasClaudeAncestor {
		return "", true
	}
	if p.ConvID == "" {
		writeError(w, http.StatusForbidden, "auth",
			"caller has a Claude Code ancestor but no resolvable conv-id")
		return "", false
	}
	caller := p.ConvID
	if caller == targetConv {
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
