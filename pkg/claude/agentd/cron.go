package agentd

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// cronTickInterval is how often the scheduler checks for due jobs.
// 30s is fine-grained enough for minute-level schedules without
// burning CPU; finer-grained jobs aren't a v1 use case (the user's
// driving example is "every 10 minutes").
const cronTickInterval = 30 * time.Second

// cronAuthorityMu orders a scheduled side effect against retirement. A cached
// due row is revalidated while holding this lock; retirement takes the same
// lock around its durable revocation transaction. Therefore a fire either
// finishes before retirement can commit, or observes the disabled/retired row
// and performs no side effect after a successful retirement response.
var cronAuthorityMu sync.Mutex

var cronAfterDueListForTest func()
var cronAfterAuthorityRevalidationForTest func(int64)
var cronBeforeAuthorityLockForTest func(string)
var cronAfterSpawnCadenceForTest func(int64)

const cronReplaceStopTimeout = 5 * time.Second
const cronInterruptedRunGrace = time.Minute

// startCronScheduler spins up the agent_cron_jobs scheduler in its
// own goroutine. The goroutine ticks every cronTickInterval, fires
// any due jobs, and stamps last_run_at + last_run_status. Returns
// when stop is closed (the daemon's quit channel).
//
// Catch-up policy: if a job is overdue by N intervals, we fire it
// once and reset last_run_at to now (NOT now - missed*interval).
// Stops the daemon from replaying a flood of messages after a long
// downtime; for "I missed five 10-minute checks" the human can
// always re-trigger manually.
func startCronScheduler(stop <-chan struct{}) {
	if _, err := db.InterruptOrphanedCronSpawns(time.Now().UTC(), time.Now().UTC()); err != nil {
		slog.Warn("cron: close interrupted spawn runs at startup", "error", err)
	}
	go func() {
		// Fire one tick immediately on startup so just-added jobs
		// don't have to wait the full interval before their first
		// run. Subsequent ticks are timer-driven.
		runCronTick(time.Now())

		t := time.NewTicker(cronTickInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-t.C:
				runCronTick(now)
			}
		}
	}()
}

// runCronTick is a single sweep: list due jobs, fire each, stamp
// the result. Errors are logged per-job and never abort the sweep —
// one bad job shouldn't stop the others from firing.
func runCronTick(now time.Time) {
	if _, err := db.InterruptOrphanedCronSpawns(now.UTC(), now.UTC().Add(-cronInterruptedRunGrace)); err != nil {
		slog.Warn("cron: close stale spawn runs", "error", err)
		return
	}
	due, err := db.ListDueAgentCronJobs(now)
	if err != nil {
		slog.Warn("cron: list due jobs failed", "error", err)
		return
	}
	if cronAfterDueListForTest != nil {
		cronAfterDueListForTest()
	}
	for _, j := range due {
		fireScheduledCronJob(j.ID, now)
	}
}

func fireScheduledCronJob(jobID int64, now time.Time) {
	cronAuthorityMu.Lock()
	defer cronAuthorityMu.Unlock()
	j, err := db.GetRunnableAgentCronJob(jobID)
	if err != nil {
		slog.Warn("cron: revalidate due job failed", "job", jobID, "error", err)
		return
	}
	if j == nil {
		return
	}
	// Re-check the schedule under the authority lock. A run-now or a PATCH
	// false→true may have fired and stamped this job after the due-list query;
	// its cached candidate must not become a duplicate delivery.
	if !j.IsDue(now) {
		return
	}
	if cronAfterAuthorityRevalidationForTest != nil {
		cronAfterAuthorityRevalidationForTest(jobID)
	}
	if _, err := fireCronJobAndRecord(j, now); err != nil {
		slog.Warn("cron: stamp last_run_at failed", "job", j.ID, "error", err)
	}
}

// fireCronJobAndRecord performs one delivery attempt and advances the cadence
// anchor to now. Callers serialize it with cronAuthorityMu when it can race the
// scheduler. Run-history is best-effort because delivery has already happened.
func fireCronJobAndRecord(j *db.AgentCronJob, now time.Time) (string, error) {
	if j.ActionKind != db.CronActionSpawn {
		status := fireCronJob(j, now)
		if err := db.UpdateAgentCronJobLastRun(j.ID, now, status); err != nil {
			return status, err
		}
		if _, err := db.InsertAgentCronRun(&db.AgentCronRun{
			JobID: j.ID, FiredAt: now, Status: status,
		}); err != nil {
			slog.Warn("cron: insert run row failed", "job", j.ID, "error", err)
		}
		return status, nil
	}
	runID, err := db.InsertAgentCronRun(&db.AgentCronRun{
		JobID: j.ID, FiredAt: now, Status: "running",
	})
	if err != nil {
		return "history_failed", err
	}
	// Evidence first: advance the cadence before any spawn side effect. A crash
	// after this point may leave an honestly incomplete run, but restart cannot
	// replay the same due tick under Allow or Replace and create a duplicate.
	if err := db.UpdateAgentCronJobLastRun(j.ID, now, "running"); err != nil {
		_ = db.FinishAgentCronRun(runID, "history_failed", err.Error(), 0, "")
		return "history_failed", err
	}
	if cronAfterSpawnCadenceForTest != nil {
		cronAfterSpawnCadenceForTest(j.ID)
	}
	status, detail, workerID, workerAgent := fireCronSpawn(j, runID, now)
	if err := db.FinishAgentCronRun(runID, status, detail, workerID, workerAgent); err != nil {
		slog.Warn("cron: finish run row failed", "job", j.ID, "run", runID, "error", err)
	}
	if err := db.UpdateAgentCronJobLastRun(j.ID, now, status); err != nil {
		return status, err
	}
	return status, nil
}

func fireCronSpawn(j *db.AgentCronJob, runID int64, now time.Time) (string, string, int64, string) {
	if !triggerRoutesEnabled() {
		return "feature_disabled", "features.triggers is disabled", 0, ""
	}
	if !j.IsGroupTarget() || j.GroupID <= 0 {
		return "spawn_failed", "spawn jobs require a group target", 0, ""
	}
	ownerConv, err := cronSpawnOwnerConv(j)
	if err != nil {
		return "permission_denied", err.Error(), 0, ""
	}
	active, err := db.ListActiveCronWorkers(j.ID)
	if err != nil {
		return "spawn_failed", err.Error(), 0, ""
	}
	replaced := false
	switch j.SpawnConcurrencyPolicy {
	case db.CronConcurrencyForbid:
		if len(active) > 0 {
			return "skipped_concurrent", "a previous worker is still active", 0, ""
		}
	case db.CronConcurrencyAllow:
		if len(active) >= j.SpawnMaxLiveWorkers {
			return "skipped_concurrent", "maximum live workers reached", 0, ""
		}
	case db.CronConcurrencyReplace:
		for _, worker := range active {
			conv, lookupErr := db.CurrentConvForAgent(worker.AgentID)
			if lookupErr != nil {
				return "replace_stop_failed", "resolve prior worker: " + lookupErr.Error(), 0, ""
			}
			if conv == "" {
				conv = worker.ConvID
			}
			if conv == "" {
				return "replace_stop_failed", "prior worker has no known conversation", 0, ""
			}
			stop, outcome := stopOneConvAndWait(conv, false, db.AgentExitActionStop, "", cronReplaceStopTimeout)
			if stop.Action == "error" || outcome == softExitStuck || outcome == softExitUnattempted {
				detail := strings.TrimSpace(stop.Detail)
				if detail == "" {
					detail = "prior worker did not stop within the bounded replace window"
				}
				return "replace_stop_failed", detail, 0, ""
			}
			if err := db.CompleteTriggerWorker(worker.ID, "replaced", "replaced by a later cron firing", now); err != nil {
				return "replace_stop_failed", "record prior worker replacement: " + err.Error(), 0, ""
			}
			if worker.CronRunID > 0 {
				if err := db.FinishAgentCronRun(worker.CronRunID, "replace_stopped", "worker replaced by a later firing", worker.ID, worker.AgentID); err != nil {
					return "replace_stop_failed", "record prior run replacement: " + err.Error(), 0, ""
				}
			}
			replaced = true
		}
	default:
		return "spawn_failed", "invalid concurrency policy", 0, ""
	}

	nameTemplate := strings.ReplaceAll(j.SpawnNameTemplate, "{{fire_time}}", now.UTC().Format("20060102T150405Z"))
	instructionTemplate := strings.ReplaceAll(j.SpawnInstructionTemplate, "{{fire_time}}", now.UTC().Format(time.RFC3339))
	spec := &db.TriggerSpawnAction{
		Profile: j.SpawnProfile, RoleRefs: j.SpawnRoleRefs, NameTemplate: nameTemplate,
		InstructionTemplate: instructionTemplate, MaxLiveWorkers: j.SpawnMaxLiveWorkers,
		WorkerDeadlineSeconds: j.SpawnWorkerDeadlineSeconds,
	}
	rule := &db.TriggerRule{ID: j.ID, Name: j.Name, OwnerAgent: j.OwnerAgent,
		OperatorAuthored: j.OperatorAuthored, ScopeKind: db.TriggerScopeGroup, GroupID: j.GroupID}
	event := db.TriggerPREvent{GroupIDs: []int64{j.GroupID}, OccurredAt: now}
	status, detail, agentID := executeManagedSpawn(rule, 0, spec, event, now, managedWorkerSource{
		CronJobID: j.ID, CronRunID: runID, RatePrincipal: fmt.Sprintf("cron:%d", j.ID),
		Tag: fmt.Sprintf("cron:%d", j.ID), OwnerConv: ownerConv,
	})
	workerID, _ := db.ManagedWorkerIDForAgent(agentID)
	if status == "spawned" && replaced {
		status = "replace_stopped"
	}
	return status, detail, workerID, agentID
}

func cronSpawnOwnerConv(j *db.AgentCronJob) (string, error) {
	if j.OperatorAuthored {
		return "", nil
	}
	owner, err := db.GetAgent(j.OwnerAgent)
	if err != nil {
		return "", err
	}
	if !owner.Active() {
		return "", errors.New("owning agent is retired or unavailable")
	}
	conv, err := db.CurrentConvForAgent(j.OwnerAgent)
	if err != nil {
		return "", err
	}
	if conv == "" {
		return "", errors.New("owning agent has no current conversation")
	}
	g, err := db.GetAgentGroupByID(j.GroupID)
	if err != nil {
		return "", err
	}
	if g == nil || g.IsArchived() {
		return "", errors.New("cron target group is unavailable")
	}
	req, _ := http.NewRequest(http.MethodPost, "http://cron.invalid", nil)
	ctx := ActionContext{Group: g.Name, structuralGroup: g.Name}
	allowed, _, authErr := permissionAllowsAction(req, conv, PermGroupsMessagesSchedule, ctx)
	if authErr != nil {
		return "", authErr
	}
	if !allowed && resolvePermissionVerdictForRequest(req, conv, PermGroupsMessagesSchedule).Resolution != permDeny {
		allowed, _ = structuralPermissionMatch(conv, PermGroupsMessagesSchedule, ctx)
	}
	if !allowed {
		return "", fmt.Errorf("owner lacks %s for group %s", PermGroupsMessagesSchedule, g.Name)
	}
	return conv, nil
}

// SetCronAfterDueListForTest installs a deterministic scheduler race hook.
func SetCronAfterDueListForTest(fn func()) func() {
	old := cronAfterDueListForTest
	cronAfterDueListForTest = fn
	return func() { cronAfterDueListForTest = old }
}

// SetCronAfterAuthorityRevalidationForTest installs a deterministic scheduler
// hook after a job is revalidated while cronAuthorityMu is held.
func SetCronAfterAuthorityRevalidationForTest(fn func(int64)) func() {
	old := cronAfterAuthorityRevalidationForTest
	cronAfterAuthorityRevalidationForTest = fn
	return func() { cronAfterAuthorityRevalidationForTest = old }
}

// SetCronBeforeAuthorityLockForTest installs a deterministic race hook for
// HTTP mutations that are about to take cronAuthorityMu. operation identifies
// "create-immediate", "create-routing", "enable", "disable", "delete",
// "patch", or "run-now".
func SetCronBeforeAuthorityLockForTest(fn func(string)) func() {
	old := cronBeforeAuthorityLockForTest
	cronBeforeAuthorityLockForTest = fn
	return func() { cronBeforeAuthorityLockForTest = old }
}

// SetCronAfterSpawnCadenceForTest installs a crash-window seam after a spawn
// run has durably advanced cadence but before worker reservation/dispatch.
func SetCronAfterSpawnCadenceForTest(fn func(int64)) func() {
	old := cronAfterSpawnCadenceForTest
	cronAfterSpawnCadenceForTest = fn
	return func() { cronAfterSpawnCadenceForTest = old }
}

// RunCronTickForTest runs one scheduler sweep synchronously.
func RunCronTickForTest(now time.Time) { runCronTick(now) }

// sudoGrantsCleanupInterval is how often the housekeeping sweep
// runs. 1 hour is fine: correctness doesn't depend on prompt
// purging (the active-grants probe filters by `expires_at` on
// every check), and a long-running daemon's table grows by maybe
// dozens of rows per hour at worst.
const sudoGrantsCleanupInterval = 1 * time.Hour

// sudoGrantsRetention is how long an expired/revoked grant row is
// kept around before hard-deletion. Keeps recent forensic context
// available ("what did agent X do yesterday?") without letting the
// table grow forever. Tunable via the sudoGrantsRetentionVar
// override below for tests.
var sudoGrantsRetention = 30 * 24 * time.Hour

// startSudoGrantsCleanup spins up the agent_sudo_grants housekeeping
// sweep. Hard-deletes rows whose expires_at slipped past the
// retention window; a long-running daemon's table is bounded
// regardless of grant volume.
//
// Runs sudoGrantsRetention behind wall-clock so the most-recent ~30d
// of forensic context stays queryable. Active grants are NOT touched
// — PurgeExpiredSudoGrants filters by `expires_at < now`, so an
// in-window grant survives even if it was inserted years ago (which
// it can't be, since the cap is 1h, but the rule is robust either
// way).
//
// The first sweep fires immediately on startup so a daemon restart
// doesn't have to wait the full hour for catch-up; subsequent ones
// are timer-driven.
func startSudoGrantsCleanup(stop <-chan struct{}) {
	go func() {
		runSudoGrantsCleanup(time.Now())
		t := time.NewTicker(sudoGrantsCleanupInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-t.C:
				runSudoGrantsCleanup(now)
			}
		}
	}()
}

func runSudoGrantsCleanup(now time.Time) {
	cutoff := now.Add(-sudoGrantsRetention)
	n, err := db.PurgeExpiredSudoGrants(cutoff)
	if err != nil {
		slog.Warn("sudo cleanup: purge failed", "error", err, "cutoff", cutoff)
		return
	}
	if n > 0 {
		slog.Info("sudo cleanup: purged expired grants",
			"count", n, "older_than", cutoff.Format(time.RFC3339))
	}
}

// fireCronJob delivers a single job's payload. Returns the status
// tag stored in last_run_status (visible in the dashboard).
//
// Two routing shapes, one delivery path:
//   - group target → fan the body out to every CURRENT member of the
//     target group (fireCronGroupJob). Membership is resolved at fire
//     time so a recurring job tracks the live roster.
//   - conv target → when online, insert one agent_messages row (group-routed
//     or direct) and let the shared async delivery pipeline handle contention,
//     retries, and inline-vs-pointer policy. Offline delivery is discarded
//     unless the job explicitly opts into QueueWhenOffline.
//
// For a conv target the stored conv is walked to its live successor
// before delivery (walkSuccession), so a job whose target has since
// reincarnated still reaches the live agent.
func fireCronJob(j *db.AgentCronJob, now time.Time) string {
	subject := j.Subject
	if subject == "" {
		subject = "cron"
	}
	// Tag the subject so the recipient can tell this is a scheduled
	// nudge vs a hand-typed peer message. Idempotent on re-stamping.
	if j.Name != "" {
		subject = "[cron:" + j.Name + "] " + subject
	} else {
		subject = "[cron] " + subject
	}

	if j.IsGroupTarget() {
		var alive map[string]struct{}
		if !j.QueueWhenOffline {
			alive = cronLiveTmuxSessions(j.ID)
		}
		return fireCronGroupJob(j, subject, alive)
	}

	// Conv target: j.TargetConv is the target actor's CURRENT conv, resolved
	// from target_agent → agents.current_conv_id at read time (JOH-26 PR3a), so
	// a job whose target reincarnated already addresses the live generation.
	// walkSuccession stays as defence-in-depth — on a current conv it is a
	// no-op (the head has no successor) — and keeps delivery succession-safe in
	// the vanishingly small window between the List resolution and this fire,
	// matching the fan-out path (fanOutToGroup) and the one-shot message path
	// (which redirects via agent.ResolveSelector). originalTo is non-empty only
	// when a redirect actually happened.
	targetConv, originalTo := walkSuccession(j.TargetConv)
	if !j.QueueWhenOffline && !isConvOnlineIn(targetConv, cronLiveTmuxSessions(j.ID)) {
		slog.Info("cron: skipped offline target", "job", j.ID, "target", targetConv)
		return "skipped_offline"
	}

	if _, err := queueCronAgentMessage(&db.AgentMessage{
		GroupID:          j.GroupID,
		FromConv:         j.OwnerConv,
		ToConv:           targetConv,
		OriginalToConv:   originalTo,
		Subject:          subject,
		Body:             j.Body,
		ToRecipients:     []string{targetConv},
		OperatorAuthored: j.OperatorAuthored,
	}, j.ID); err != nil {
		slog.Warn("cron: queue message failed", "job", j.ID, "error", err)
		return "send_failed"
	}
	return "ok"
}

// fireCronGroupJob delivers a group-targeted job: it fans the body out
// to every CURRENT member of the target group, reusing fanOutToGroup —
// the same path that backs `group:` multicast sends. Membership is
// resolved here, at fire time, so a recurring job always tracks the
// live roster as members join and leave between ticks.
//
// The owner conv is the message sender, and (like a `group:` multicast)
// is skipped if it is itself a member — a PO that schedules a recurring
// team ping does not ping itself. Each recipient gets its own
// agent_messages row + a tmux nudge when alive. By default offline members are
// omitted before persistence; QueueWhenOffline restores the old durable-queue
// behaviour for jobs whose reminder should survive downtime.
//
// Status: "no_target" if the group was deleted out from under the job;
// "send_failed" if any recipient row failed to insert; "skipped_offline" if
// every eligible recipient was offline; "partial_offline" if a mixed roster
// delivered only to online recipients; "ok" otherwise.
func fireCronGroupJob(j *db.AgentCronJob, subject string, alive map[string]struct{}) string {
	g, err := db.GetAgentGroupByID(j.GroupID)
	if err != nil {
		slog.Warn("cron: group lookup failed",
			"job", j.ID, "group", j.GroupID, "error", err)
		return "send_failed"
	}
	if g == nil {
		// Target group deleted — nothing to fan out to. Mirrors the solo
		// path's "no_target".
		return "no_target"
	}
	// TargetRole (JOH-244) filters the fan-out to matching members, resolved
	// against the LIVE roster here at fire time so membership changes stay
	// correct. "" = whole group (fanOutToGroup reads an empty filter that way).
	var onlineOnly func(string) bool
	if !j.QueueWhenOffline {
		onlineOnly = func(convID string) bool { return isConvOnlineIn(convID, alive) }
	}
	recipients, skippedOffline, err := fanOutToGroupFiltered(
		g, j.OwnerConv, subject, j.Body, j.TargetRole, nil, onlineOnly, false, j.OperatorAuthored, j.ID)
	if err != nil {
		slog.Warn("cron: group fan-out failed",
			"job", j.ID, "group", g.Name, "error", err)
		return "send_failed"
	}
	for _, r := range recipients {
		// fanOutToGroup records a failed insert as MessageID 0; surface
		// the job as send_failed so the dashboard pill flags it.
		if r.MessageID == 0 {
			return "send_failed"
		}
	}
	if skippedOffline > 0 {
		if len(recipients) == 0 {
			return "skipped_offline"
		}
		return "partial_offline"
	}
	return "ok"
}

// cronLiveTmuxSessions takes one timeout-bounded batch snapshot for a fire.
// Probe failure fails closed to "all offline": the job records a skipped tick
// instead of queuing stale automation or wedging every cron authority holder.
func cronLiveTmuxSessions(jobID int64) map[string]struct{} {
	alive, err := liveTmuxSessionsWithTimeout()
	if err != nil {
		slog.Warn("cron: tmux liveness snapshot failed; treating targets as offline",
			"job", jobID, "error", err)
		return map[string]struct{}{}
	}
	return alive
}
