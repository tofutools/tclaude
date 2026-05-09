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

// fireCronJob delivers a single job's payload. Returns the status
// tag stored in last_run_status (visible in the dashboard).
//
// Two delivery paths, mirroring the reincarnate / clone follow-up
// fork:
//   - GroupID > 0 → insert agent_messages row, let the existing
//     flush nudge pipeline pick it up next time the target is alive.
//     Reliable across target offline windows.
//   - GroupID == 0 → direct tmux send-keys into the target's pane.
//     Requires the target to be alive RIGHT NOW; "no_target" status
//     when no live pane is found.
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

	if j.GroupID > 0 {
		_, err := db.InsertAgentMessage(&db.AgentMessage{
			GroupID:  j.GroupID,
			FromConv: j.OwnerConv,
			ToConv:   j.TargetConv,
			Subject:  subject,
			Body:     j.Body,
		})
		if err != nil {
			slog.Warn("cron: insert message failed", "job", j.ID, "error", err)
			return "send_failed"
		}
		// Best-effort nudge — flush only fires if the target is alive
		// right now. Otherwise the message sits in the inbox until the
		// next agent_messages-aware request from the target.
		go flush(j.TargetConv, realFlushSender)
		return "ok"
	}

	// Solo path: send the body directly via tmux. Subject is dropped
	// since there's no message envelope.
	sess := pickAliveSession(j.TargetConv)
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
