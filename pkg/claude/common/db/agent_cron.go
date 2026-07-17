package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/cronexpr"
)

// Cron job target kinds — the value stored in agent_cron_jobs.target_kind
// (schema v41+). A job either targets a single conversation or fans out
// to every member of a group.
const (
	// CronTargetConv — target_conv is the recipient; group_id (when >0)
	// is the routing group a conv-targeted message is sent through, or 0
	// for direct group_id 0 mailbox delivery. The long-standing job shape.
	CronTargetConv = "conv"
	// CronTargetGroup — group_id IS the target group; the scheduler
	// resolves that group's membership at fire time and delivers the
	// body to every current member. target_conv is unused.
	CronTargetGroup = "group"
)

// Cron disabled_reason markers (schema v94). The value distinguishes a job
// tclaude auto-disabled from one the human paused by hand, so re-enabling on a
// group resume touches only the ones tclaude paused (JOH-345).
const (
	// CronDisabledReasonNone — '' — the normal state: whatever the enabled
	// flag says is a human-managed choice. A job the human disabled by hand
	// carries this (enabled=0, reason='') and is never auto-re-enabled.
	CronDisabledReasonNone = ""
	// CronDisabledReasonAgentRetired marks a job disabled atomically with its
	// owner's retirement. Reinstatement does not restore authority implicitly;
	// only an explicit enable can clear this marker.
	CronDisabledReasonAgentRetired = "agent-retired"
	// CronDisabledReasonGroupRetired — a group-target job auto-disabled because
	// retiring the group's members left it with no live recipients. A later
	// `groups resume` re-enables exactly the jobs carrying this marker.
	CronDisabledReasonGroupRetired = "group-retired"
)

// ErrAgentCronOwnerRetired is the canonical rejection for a cron write whose
// recorded owner no longer has authority. Callers must classify it with
// errors.Is; wrapped context is intentionally allowed at each DB operation.
var ErrAgentCronOwnerRetired = errors.New("cron job owner is retired")

// AgentCronJob is a row in agent_cron_jobs. Recurring scheduled
// task that the agentd scheduler fires on a wall-clock interval.
type AgentCronJob struct {
	ID   int64
	Name string
	// OwnerAgent / TargetAgent are the stable actor keys the job is keyed on
	// (JOH-26) — the canonical, rotation-immune identities to display.
	// OwnerConv / TargetConv are the actors' current generations, resolved at
	// read time. TargetAgent is "" for a group-target job (no single actor).
	OwnerAgent  string
	TargetAgent string
	OwnerConv   string
	TargetKind  string // CronTargetConv | CronTargetGroup
	TargetConv  string // recipient when TargetKind == CronTargetConv
	GroupID     int64  // conv-kind: routing group (0 → direct inbox). group-kind: the target group.
	// TargetRole filters a group-target job to the members whose role matches,
	// resolved at fire time against the live roster (JOH-244). "" or "all" =
	// the whole group. Unused for a conv-target job. A first-class cron
	// primitive; template rhythms materialize onto it.
	TargetRole      string
	IntervalSeconds int64  // fixed-interval mode; 0 when CronExpr is set
	CronExpr        string // cron-expression mode (cronexpr syntax); "" = interval mode
	Subject         string
	Body            string
	Enabled         bool
	// RunImmediately is the persisted creation/edit preference. It is acted on
	// only by the write path: create=true and a PATCH false→true each trigger
	// one fire. The scheduler never consumes it, so restarts cannot replay the
	// opt-in delivery.
	RunImmediately bool
	// QueueWhenOffline opts this job back into durable inbox delivery when a
	// target has no live tmux pane. The default is false: scheduled nudges are
	// time-sensitive, so offline ticks are discarded instead of accumulating.
	QueueWhenOffline bool
	// DisabledReason marks WHY a job is disabled (schema v94): '' for a normal,
	// human-managed enable/disable, or CronDisabledReasonGroupRetired for a
	// group-target job tclaude auto-paused when a retire emptied its group. A
	// group resume re-enables only the auto-paused ones. Unset (and ignored)
	// for an enabled job.
	DisabledReason string
	CreatedAt      time.Time
	LastRunAt      time.Time // zero → never run; both modes anchor first due time on CreatedAt
	LastRunStatus  string
}

// IsGroupTarget reports whether the job fans out to a group rather than
// delivering to a single conv. Callers MUST use this (not GroupID > 0)
// as the discriminator — a conv-targeted job routed through a shared
// group also carries a non-zero GroupID.
func (j *AgentCronJob) IsGroupTarget() bool {
	return j.TargetKind == CronTargetGroup
}

// cronConvToAgentTx resolves a cron owner/target conv to its stable actor id,
// enrolling the conv as an agent when it is not already known (JOH-26 PR3a). A
// job's owner schedules it and a conv-target receives it — both are addressable
// agents, so they carry an actor like a group member does (mirrors
// AddAgentGroupMember's EnsureAgentForConv). Keying owner/target on agent_id
// means a reincarnate / Claude Code /clear no longer has to rewrite the ref: the
// actor's id never moves, and the fire path resolves it back to the actor's
// current conv at fire time. An empty conv — a group-target job, or a
// human-scheduled job with no owner attribution — maps to "" (no actor).
//
// Resolution runs inside the cron mutation transaction so a denied write
// cannot leave behind an enrolled owner/target actor. The pending-spawn lookup
// mirrors EnsureAgentForConv's reserved-identity behavior.
func cronConvToAgentTx(tx *sql.Tx, convID string) (string, error) {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return "", nil
	}
	if agentID, err := agentIDForConvTx(tx, convID); err != nil || agentID != "" {
		return agentID, err
	}
	var reservedAgentID string
	err := tx.QueryRow(`SELECT p.agent_id
		FROM pending_spawns p
		JOIN sessions s ON s.id = p.label
		WHERE s.conv_id = ? AND p.agent_id <> ''
		ORDER BY p.created_at ASC
		LIMIT 1`, convID).Scan(&reservedAgentID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if errors.Is(err, sql.ErrNoRows) {
		reservedAgentID = ""
	}
	if reservedAgentID == "" {
		agentID, err := ensureAgentForConvTx(tx, convID, "cron")
		if err != nil {
			return "", err
		}
		if agentID == "" {
			return "", fmt.Errorf("cronConvToAgentTx: no actor for conv %s", convID)
		}
		return agentID, nil
	}
	if !strings.HasPrefix(reservedAgentID, AgentIDPrefix) {
		return "", fmt.Errorf("cronConvToAgentTx: invalid reserved agent_id %q", reservedAgentID)
	}
	var occupiedConv string
	err = tx.QueryRow(`SELECT current_conv_id FROM agents WHERE agent_id = ?`, reservedAgentID).Scan(&occupiedConv)
	switch {
	case err == nil:
		return "", fmt.Errorf("cronConvToAgentTx: reserved agent %s already heads conv %s",
			reservedAgentID, occupiedConv)
	case !errors.Is(err, sql.ErrNoRows):
		return "", err
	}
	now := time.Now()
	if err := insertAgentTx(tx, reservedAgentID, convID, "cron", now); err != nil {
		return "", err
	}
	if err := linkConvTx(tx, convID, reservedAgentID, ConvRoleHead, "cron", now); err != nil {
		return "", err
	}
	return reservedAgentID, nil
}

// cronSelect is the shared SELECT for reading cron jobs. owner/target are keyed
// on agent_id (JOH-26 PR3a); each LEFT JOIN resolves the actor back to its
// CURRENT conv so OwnerConv / TargetConv present (and the fire path delivers to)
// the live generation. LEFT JOIN + COALESCE so a group-target job (target_agent
// ”) or an owner-less job keeps an empty string rather than dropping the row.
// The 20 projected columns match scanAgentCronJob's field order. owner_agent /
// target_agent are projected raw (the stable keys) alongside the LEFT-JOIN-
// resolved current convs.
const cronSelect = `SELECT j.id, j.name,
	COALESCE(ow.current_conv_id, ''), j.target_kind, COALESCE(tg.current_conv_id, ''),
	j.group_id, j.interval_seconds, j.subject, j.body, j.enabled, j.created_at,
	j.last_run_at, j.last_run_status, j.owner_agent, j.target_agent, j.cron_expr, j.target_role,
	j.disabled_reason, j.run_immediately, j.queue_when_offline
	FROM agent_cron_jobs j
	LEFT JOIN agents ow ON ow.agent_id = j.owner_agent
	LEFT JOIN agents tg ON tg.agent_id = j.target_agent`

// InsertAgentCronJob writes a new job row. Returns the new ID.
// CreatedAt is stamped server-side; the caller's value is ignored.
func InsertAgentCronJob(j *AgentCronJob) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	ownerAgent, err := cronConvToAgentTx(tx, j.OwnerConv)
	if err != nil {
		return 0, err
	}
	if err := requireLiveCronOwner(tx, ownerAgent); err != nil {
		return 0, fmt.Errorf("InsertAgentCronJob: %w", err)
	}
	targetAgent, err := cronConvToAgentTx(tx, j.TargetConv)
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	kind := j.TargetKind
	if kind == "" {
		kind = CronTargetConv
	}
	res, err := tx.Exec(`INSERT INTO agent_cron_jobs
		(name, owner_agent, target_kind, target_agent, group_id, target_role, interval_seconds,
		 cron_expr, subject, body, enabled, run_immediately, queue_when_offline, created_at, last_run_at, last_run_status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', '')`,
		j.Name, ownerAgent, kind, targetAgent, j.GroupID, j.TargetRole, j.IntervalSeconds,
		j.CronExpr, j.Subject, j.Body, boolToInt(j.Enabled), boolToInt(j.RunImmediately), boolToInt(j.QueueWhenOffline), now)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// requireLiveCronOwner checks authority inside the caller's write transaction.
// The database DSN makes write transactions BEGIN IMMEDIATE, so retirement
// cannot change the result between this check and the cron mutation.
func requireLiveCronOwner(tx *sql.Tx, ownerAgent string) error {
	if ownerAgent == "" {
		return nil
	}
	var retiredAt string
	if err := tx.QueryRow(`SELECT retired_at FROM agents WHERE agent_id = ?`, ownerAgent).Scan(&retiredAt); err != nil {
		return err
	}
	if retiredAt != "" {
		return ErrAgentCronOwnerRetired
	}
	return nil
}

// requireLiveAgentCronJobOwner reports whether id exists and, when it does,
// rejects a retired owner using the same canonical sentinel as insert and
// owner reassignment. It runs inside the mutation transaction; classification
// never depends on RowsAffected being zero.
func requireLiveAgentCronJobOwner(tx *sql.Tx, id int64) (bool, error) {
	var ownerAgent string
	var retiredAt sql.NullString
	err := tx.QueryRow(`SELECT j.owner_agent, a.retired_at
		FROM agent_cron_jobs j
		LEFT JOIN agents a ON a.agent_id = j.owner_agent
		WHERE j.id = ?`, id).Scan(&ownerAgent, &retiredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if ownerAgent != "" && !retiredAt.Valid {
		return true, fmt.Errorf("cron job %d owner agent %s does not exist", id, ownerAgent)
	}
	if retiredAt.String != "" {
		return true, ErrAgentCronOwnerRetired
	}
	return true, nil
}

// GetAgentCronJob returns a single job by ID, or nil if not found.
func GetAgentCronJob(id int64) (*AgentCronJob, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	row := d.QueryRow(cronSelect+` WHERE j.id = ?`, id)
	j, err := scanAgentCronJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return j, err
}

// GetRunnableAgentCronJob revalidates a cached scheduler candidate against
// current durable state. Disabled jobs and jobs owned by retired actors return
// nil; owner-less human jobs remain runnable.
func GetRunnableAgentCronJob(id int64) (*AgentCronJob, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	row := d.QueryRow(cronSelect+` WHERE j.id = ? AND j.enabled = 1
		AND (j.owner_agent = '' OR ow.retired_at = '')`, id)
	j, err := scanAgentCronJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return j, err
}

// GetLiveOwnerAgentCronJob returns a job only while its owner still has
// authority. Unlike GetRunnableAgentCronJob it deliberately permits disabled
// rows: a manual run-now is independent of the recurring enabled toggle, but
// must still fail closed after owner retirement.
func GetLiveOwnerAgentCronJob(id int64) (*AgentCronJob, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	row := d.QueryRow(cronSelect+` WHERE j.id = ?
		AND (j.owner_agent = '' OR ow.retired_at = '')`, id)
	j, err := scanAgentCronJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		var retiredOwner int
		if lookupErr := d.QueryRow(`SELECT COUNT(*) FROM agent_cron_jobs j
			JOIN agents a ON a.agent_id = j.owner_agent AND a.retired_at <> ''
			WHERE j.id = ?`, id).Scan(&retiredOwner); lookupErr != nil {
			return nil, lookupErr
		}
		if retiredOwner > 0 {
			return nil, fmt.Errorf("GetLiveOwnerAgentCronJob: %w", ErrAgentCronOwnerRetired)
		}
		return nil, nil
	}
	return j, err
}

// ListAgentCronJobs returns every job, ordered by ID asc.
func ListAgentCronJobs() ([]*AgentCronJob, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	return listAgentCronJobs(d, cronSelect+` ORDER BY j.id`)
}

func listAgentCronJobs(d *sql.DB, query string) ([]*AgentCronJob, error) {
	rows, err := d.Query(query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*AgentCronJob
	for rows.Next() {
		j, err := scanAgentCronJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ListDueAgentCronJobs returns enabled jobs whose next-fire time has passed.
//
// Both schedule modes anchor a never-run job on created_at. Thus a new job
// waits for its first interval/expression match; an optional immediate fire is
// an explicit write-path action, not an implicit scheduler special case.
func ListDueAgentCronJobs(now time.Time) ([]*AgentCronJob, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	// Retirement disables owned jobs in the same transaction as every other
	// authority row. The active-owner predicate is the scheduler-side defense:
	// even a hand-edited/re-enabled stale row cannot execute for a retired actor.
	jobs, err := listAgentCronJobs(d, cronSelect+`
		WHERE j.owner_agent = '' OR ow.retired_at = ''
		ORDER BY j.id`)
	if err != nil {
		return nil, err
	}
	out := make([]*AgentCronJob, 0, len(jobs))
	for _, j := range jobs {
		if !j.Enabled {
			continue
		}
		if j.IsDue(now) {
			out = append(out, j)
		}
	}
	return out, nil
}

// IsDue reports whether the job's next scheduled fire is at or before now.
// Callers separately enforce enabled/authority. The scheduler uses this both
// when listing candidates and again under cronAuthorityMu immediately before
// delivery, closing the race with run-now and immediate edit fires.
func (j *AgentCronJob) IsDue(now time.Time) bool {
	base := j.LastRunAt
	if base.IsZero() {
		base = j.CreatedAt
	}
	if j.CronExpr != "" {
		next, err := cronexpr.Next(j.CronExpr, base)
		return err == nil && !next.IsZero() && !next.After(now)
	}
	if j.IntervalSeconds <= 0 || base.IsZero() {
		return false
	}
	return !base.Add(time.Duration(j.IntervalSeconds) * time.Second).After(now)
}

// DeleteAgentCronJob removes a job by ID. Idempotent.
func DeleteAgentCronJob(id int64) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM agent_cron_jobs WHERE id = ?`, id)
	return err
}

// UpdateAgentCronJobLastRun stamps the most recent fire time and
// status. status is a short tag (e.g. "ok", "no_target", "send_failed")
// the dashboard surfaces as a pill.
func UpdateAgentCronJobLastRun(id int64, when time.Time, status string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE agent_cron_jobs SET last_run_at = ?, last_run_status = ?
		WHERE id = ?`, when.UTC().Format(time.RFC3339), status, id)
	return err
}

// SetAgentCronJobEnabled flips the enabled flag without touching
// the last_run_at timestamp (so re-enabling a paused job doesn't
// immediately fire if it ran recently).
//
// It also clears any auto-disabled marker (disabled_reason → ”): an explicit
// enable/disable is a human-managed decision, so the job stops being a
// candidate for the group-resume auto-re-enable (JOH-345). A job the human
// manually re-enabled after an emptying retire therefore won't be silently
// re-touched, and one the human manually paused stays paused across a resume.
func SetAgentCronJobEnabled(id int64, enabled bool) error {
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	exists, err := requireLiveAgentCronJobOwner(tx, id)
	if err != nil {
		return fmt.Errorf("SetAgentCronJobEnabled: %w", err)
	}
	if !exists {
		return nil
	}
	if _, err := tx.Exec(`UPDATE agent_cron_jobs
		SET enabled = ?, disabled_reason = '' WHERE id = ?`, boolToInt(enabled), id); err != nil {
		return err
	}
	return tx.Commit()
}

// DisableGroupTargetCronJobsForRetire disables every currently-ENABLED
// group-target cron job aimed at groupID, stamping disabled_reason =
// CronDisabledReasonGroupRetired. Called when a retire leaves the group with no
// live members: its template-seeded rhythms would otherwise fire forever with
// nobody to receive them. Returns the number of jobs disabled.
//
// The `enabled = 1` guard is the crux: a job the human already disabled by hand
// (enabled=0, disabled_reason=”) is left untouched, so a later resume does not
// silently re-enable it. Only jobs this call paused carry the marker.
func DisableGroupTargetCronJobsForRetire(groupID int64) (int, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(
		`UPDATE agent_cron_jobs SET enabled = 0, disabled_reason = ?
		 WHERE target_kind = ? AND group_id = ? AND enabled = 1`,
		CronDisabledReasonGroupRetired, CronTargetGroup, groupID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ReenableGroupRetiredCronJobs re-enables every group-target cron job for
// groupID that tclaude auto-disabled on an emptying retire (disabled_reason =
// CronDisabledReasonGroupRetired), clearing the marker back to ”. Called when a
// group is resumed. Returns the number of jobs re-enabled.
//
// The disabled_reason match is the crux: only jobs THIS mechanism paused are
// touched — a job the human disabled by hand (disabled_reason=”) stays
// disabled. last_run_at is deliberately left alone (like SetAgentCronJobEnabled),
// so re-enabling after a long pause does not fire a flood of catch-ups.
func ReenableGroupRetiredCronJobs(groupID int64) (int, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(
		`UPDATE agent_cron_jobs SET enabled = 1, disabled_reason = ''
		 WHERE target_kind = ? AND group_id = ? AND disabled_reason = ?
		 AND (owner_agent = '' OR EXISTS (
			SELECT 1 FROM agents WHERE agent_id = owner_agent AND retired_at = ''
		 ))`,
		CronTargetGroup, groupID, CronDisabledReasonGroupRetired)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// DeleteGroupTargetCronJobs removes every group-target cron job aimed at
// groupID (including template-seeded rhythms). Used by the task-force
// stand-down sweep, which — unlike a plain retire — deletes the rhythms rather
// than disabling them (the group is being wound down, not paused). Conv-target
// jobs merely routed THROUGH the group are left alone. agent_cron_runs cascade-
// clean via their job FK. Returns the number of jobs removed. Idempotent.
func DeleteGroupTargetCronJobs(groupID int64) (int, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(
		`DELETE FROM agent_cron_jobs WHERE target_kind = ? AND group_id = ?`,
		CronTargetGroup, groupID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// UpdateCronPatch is the partial-update shape for UpdateAgentCronJobFields.
// nil → leave field unchanged. Pointer-shaped so callers can distinguish
// "set to zero" from "don't touch".
type UpdateCronPatch struct {
	Name             *string
	OwnerConv        *string
	TargetKind       *string
	TargetConv       *string
	GroupID          *int64
	TargetRole       *string
	IntervalSeconds  *int64
	CronExpr         *string
	Subject          *string
	Body             *string
	Enabled          *bool
	RunImmediately   *bool
	QueueWhenOffline *bool
}

// Empty reports whether the patch requests no stored-field mutation.
func (p UpdateCronPatch) Empty() bool {
	return p.Name == nil && p.OwnerConv == nil && p.TargetKind == nil &&
		p.TargetConv == nil && p.GroupID == nil && p.TargetRole == nil &&
		p.IntervalSeconds == nil && p.CronExpr == nil && p.Subject == nil &&
		p.Body == nil && p.Enabled == nil && p.RunImmediately == nil &&
		p.QueueWhenOffline == nil
}

// UpdateAgentCronJobFields applies a partial update to one row. Only
// non-nil fields in the patch are written. Returns the number of rows
// affected (0 → no such id).
//
// Never touches last_run_at or last_run_status — re-enabling a paused
// job after a long pause must not fire a flood of catch-ups, and
// editing the body should not reset the run-history pointer either.
func UpdateAgentCronJobFields(id int64, p UpdateCronPatch) (int, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	if p.Empty() {
		return 0, nil
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	exists, err := requireLiveAgentCronJobOwner(tx, id)
	if err != nil {
		return 0, fmt.Errorf("UpdateAgentCronJobFields: %w", err)
	}
	if !exists {
		return 0, nil
	}
	sets := make([]string, 0, 9)
	args := make([]any, 0, 10)
	patchedOwnerAgent := ""
	hasPatchedOwner := false
	if p.Name != nil {
		sets = append(sets, "name = ?")
		args = append(args, *p.Name)
	}
	if p.OwnerConv != nil {
		// Re-key the patched owner conv onto its actor (JOH-26 PR3a); a switch
		// to a group target clears it via TargetConv="" → owner_agent unchanged.
		ownerAgent, aerr := cronConvToAgentTx(tx, *p.OwnerConv)
		if aerr != nil {
			return 0, aerr
		}
		sets = append(sets, "owner_agent = ?")
		args = append(args, ownerAgent)
		patchedOwnerAgent = ownerAgent
		hasPatchedOwner = true
	}
	if p.TargetKind != nil {
		sets = append(sets, "target_kind = ?")
		args = append(args, *p.TargetKind)
	}
	if p.TargetConv != nil {
		// "" (a switch to a group target) resolves to "" — clearing the
		// target_agent, mirroring the pre-cutover target_conv clear.
		targetAgent, aerr := cronConvToAgentTx(tx, *p.TargetConv)
		if aerr != nil {
			return 0, aerr
		}
		sets = append(sets, "target_agent = ?")
		args = append(args, targetAgent)
	}
	if p.GroupID != nil {
		sets = append(sets, "group_id = ?")
		args = append(args, *p.GroupID)
	}
	if p.TargetRole != nil {
		sets = append(sets, "target_role = ?")
		args = append(args, *p.TargetRole)
	}
	if p.IntervalSeconds != nil {
		sets = append(sets, "interval_seconds = ?")
		args = append(args, *p.IntervalSeconds)
	}
	if p.CronExpr != nil {
		sets = append(sets, "cron_expr = ?")
		args = append(args, *p.CronExpr)
	}
	if p.Subject != nil {
		sets = append(sets, "subject = ?")
		args = append(args, *p.Subject)
	}
	if p.Body != nil {
		sets = append(sets, "body = ?")
		args = append(args, *p.Body)
	}
	if p.Enabled != nil {
		// An explicit API/user enablement decision supersedes an automatic
		// group/agent retirement pause marker. Clear the marker for both true
		// and false: otherwise a later group resume could resurrect a job the
		// human explicitly kept disabled.
		sets = append(sets, "enabled = ?", "disabled_reason = ''")
		args = append(args, boolToInt(*p.Enabled))
	}
	if p.RunImmediately != nil {
		sets = append(sets, "run_immediately = ?")
		args = append(args, boolToInt(*p.RunImmediately))
	}
	if p.QueueWhenOffline != nil {
		sets = append(sets, "queue_when_offline = ?")
		args = append(args, boolToInt(*p.QueueWhenOffline))
	}
	if hasPatchedOwner {
		if err := requireLiveCronOwner(tx, patchedOwnerAgent); err != nil {
			return 0, fmt.Errorf("UpdateAgentCronJobFields: replacement %w", err)
		}
	}
	args = append(args, id)
	query := `UPDATE agent_cron_jobs SET ` + strings.Join(sets, ", ") + ` WHERE id = ?`
	res, err := tx.Exec(query, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(n), nil
}

// AgentCronRun is a row in agent_cron_runs — one entry per
// scheduler-fire of a cron job. Lets `cron logs` show the recent
// execution history without mining slog output.
type AgentCronRun struct {
	ID       int64
	JobID    int64
	FiredAt  time.Time
	Status   string
	ErrorMsg string
}

// InsertAgentCronRun appends one execution record. Returns the
// row ID; in practice callers ignore it.
func InsertAgentCronRun(r *AgentCronRun) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(`INSERT INTO agent_cron_runs
		(job_id, fired_at, status, error_msg)
		VALUES (?, ?, ?, ?)`,
		r.JobID, r.FiredAt.UTC().Format(time.RFC3339), r.Status, r.ErrorMsg)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListAgentCronRunsForJob returns the most-recent runs for one job,
// newest first. limit caps the result set; pass 0 for "no limit".
//
// Ordering is by id DESC (autoincrement = insertion order), NOT fired_at.
// fired_at is a stored timestamp string; two runs that fire in the same
// whole second serialise identically, so ORDER BY fired_at leaves their
// relative order unspecified — and under LIMIT that can drop the genuinely
// newest run from a "last N runs" view. id is monotonic with insertion,
// giving a correct, total newest-first order. Same class as the inbox/outbox
// fix in #411 and the undelivered-queue fix in #242.
func ListAgentCronRunsForJob(jobID int64, limit int) ([]*AgentCronRun, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	q := `SELECT id, job_id, fired_at, status, error_msg
		FROM agent_cron_runs WHERE job_id = ? ORDER BY id DESC`
	args := []any{jobID}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*AgentCronRun
	for rows.Next() {
		var r AgentCronRun
		var fired string
		if err := rows.Scan(&r.ID, &r.JobID, &fired, &r.Status, &r.ErrorMsg); err != nil {
			return nil, err
		}
		r.FiredAt = parseTimeOrZero(fired)
		out = append(out, &r)
	}
	return out, rows.Err()
}

func scanAgentCronJob(s rowScanner) (*AgentCronJob, error) {
	var j AgentCronJob
	var enabled, runImmediately, queueWhenOffline int
	var created, lastRun string
	err := s.Scan(&j.ID, &j.Name, &j.OwnerConv, &j.TargetKind, &j.TargetConv, &j.GroupID,
		&j.IntervalSeconds, &j.Subject, &j.Body, &enabled, &created,
		&lastRun, &j.LastRunStatus, &j.OwnerAgent, &j.TargetAgent, &j.CronExpr, &j.TargetRole,
		&j.DisabledReason, &runImmediately, &queueWhenOffline)
	if err != nil {
		return nil, err
	}
	if j.TargetKind == "" {
		j.TargetKind = CronTargetConv
	}
	j.Enabled = enabled != 0
	j.RunImmediately = runImmediately != 0
	j.QueueWhenOffline = queueWhenOffline != 0
	j.CreatedAt = parseTimeOrZero(created)
	j.LastRunAt = parseTimeOrZero(lastRun)
	return &j, nil
}
