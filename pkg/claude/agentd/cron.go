package agentd

import (
	"log/slog"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// cronTickInterval is how often the scheduler checks for due jobs.
// 30s is fine-grained enough for minute-level schedules without
// burning CPU; finer-grained jobs aren't a v1 use case (the user's
// driving example is "every 10 minutes").
const cronTickInterval = 30 * time.Second

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
	due, err := db.ListDueAgentCronJobs(now)
	if err != nil {
		slog.Warn("cron: list due jobs failed", "error", err)
		return
	}
	for _, j := range due {
		status := fireCronJob(j, now)
		if err := db.UpdateAgentCronJobLastRun(j.ID, now, status); err != nil {
			slog.Warn("cron: stamp last_run_at failed", "job", j.ID, "error", err)
		}
		// Append a run-history row so `cron logs` can show "last
		// few executions" without mining slog. Best-effort —
		// failure here doesn't roll back the fire.
		if _, err := db.InsertAgentCronRun(&db.AgentCronRun{
			JobID:   j.ID,
			FiredAt: now,
			Status:  status,
		}); err != nil {
			slog.Warn("cron: insert run row failed", "job", j.ID, "error", err)
		}
	}
}

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
// Three delivery paths, keyed off the job's target:
//   - group target → fan the body out to every CURRENT member of the
//     target group (fireCronGroupJob). Membership is resolved at fire
//     time so a recurring job tracks the live roster.
//   - conv target, GroupID > 0 → insert one agent_messages row, let
//     the existing flush nudge pipeline pick it up next time the
//     target is alive. Reliable across target offline windows.
//   - conv target, GroupID == 0 → direct tmux send-keys into the
//     target's pane. Requires the target to be alive RIGHT NOW;
//     "no_target" status when no live pane is found.
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
		return fireCronGroupJob(j, subject)
	}

	// Conv target: resolve the live successor of the stored conv before
	// delivering. MigrateCronJobConvRef re-points target_conv at
	// reincarnate time, but that is best-effort with no retry — walking
	// the succession chain here makes delivery succession-safe
	// regardless, matching the fan-out path (fanOutToGroup) and the
	// one-shot message path (which redirects via agent.ResolveSelector).
	// originalTo is non-empty only when a redirect actually happened.
	targetConv, originalTo := walkSuccession(j.TargetConv)

	if j.GroupID > 0 {
		_, err := db.InsertAgentMessage(&db.AgentMessage{
			GroupID:        j.GroupID,
			FromConv:       j.OwnerConv,
			ToConv:         targetConv,
			OriginalToConv: originalTo,
			Subject:        subject,
			Body:           j.Body,
		})
		if err != nil {
			slog.Warn("cron: insert message failed", "job", j.ID, "error", err)
			return "send_failed"
		}
		// Best-effort nudge — flush only fires if the target is alive
		// right now. Otherwise the message sits in the inbox until the
		// next agent_messages-aware request from the target. goBackground
		// (not a bare `go`) so a flow test firing a cron job can drain the
		// nudge before its cleanup restores the clcommon.Default tmux swap.
		goBackground(func() { flush(targetConv, realFlushSender) })
		return "ok"
	}

	// Solo path: send the body directly via tmux. Subject is dropped
	// since there's no message envelope.
	sess := pickAliveSession(targetConv)
	if sess == nil {
		return "no_target"
	}
	target := sess.TmuxSession + ":0.0"
	if err := clcommon.TmuxCommand("send-keys", "-t", target, j.Body).Run(); err != nil {
		slog.Warn("cron: solo send failed", "job", j.ID, "error", err)
		return "send_failed"
	}
	// 500ms gap — same paste-mode coalescing reasoning as
	// injectTextAndSubmit; see comment there.
	time.Sleep(500 * time.Millisecond)
	if err := clcommon.TmuxCommand("send-keys", "-t", target, "Enter").Run(); err != nil {
		slog.Warn("cron: solo submit failed", "job", j.ID, "error", err)
		return "send_failed"
	}
	time.Sleep(500 * time.Millisecond)
	_ = clcommon.TmuxCommand("send-keys", "-t", target, "Enter").Run()
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
// agent_messages row + a tmux nudge when alive.
//
// Status: "no_target" if the group was deleted out from under the job;
// "send_failed" if any recipient row failed to insert; "ok" otherwise
// — including a fan-out to zero other members, which (as with an
// empty-group multicast) is a successful no-op, not an error.
func fireCronGroupJob(j *db.AgentCronJob, subject string) string {
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
	recipients, err := fanOutToGroup(g, j.OwnerConv, subject, j.Body, "", nil)
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
	return "ok"
}
