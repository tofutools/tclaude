package agentd

import (
	"log/slog"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/notify"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

const agentRecoverySweepInterval = time.Second

var recoveryResumeTargetResolvable = func(convID string) bool {
	h, ok := harness.Get(harness.CodexName)
	if !ok || !h.SupportsConvs() {
		return false
	}
	ref, err := h.Convs.Resolve(convID, "", true)
	return err == nil && ref != nil && ref.Harness == harness.CodexName
}

var recoverySessionAlive = session.IsTmuxSessionAlive

func startAgentRecovery(stop <-chan struct{}) {
	go func() {
		runAgentRecoverySweep(time.Now())
		ticker := time.NewTicker(agentRecoverySweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-ticker.C:
				runAgentRecoverySweep(now)
			}
		}
	}()
}

// RunAgentRecoverySweepForTest drives the production durable scheduler without
// starting a timer goroutine.
func RunAgentRecoverySweepForTest(now time.Time) { runAgentRecoverySweep(now) }

func runAgentRecoverySweep(now time.Time) {
	recoveries, err := db.ActiveAgentRecoveries()
	if err != nil {
		slog.Warn("agent recovery: list candidates failed", "error", err)
		return
	}
	for i := range recoveries {
		reconcileAgentRecovery(recoveries[i], now)
	}
}

func recoveryLiveSuccessor(r db.AgentRecovery) (*db.SessionRow, string) {
	rows, err := db.FindSessionsByConvID(r.ConvID)
	if err != nil {
		return nil, ""
	}
	for _, row := range rows {
		if row.TmuxSession == "" || !recoverySessionAlive(row.TmuxSession) {
			continue
		}
		if !r.AttemptStartedAt.IsZero() && row.UpdatedAt.Before(r.AttemptStartedAt) {
			continue
		}
		identity, err := db.GetSessionExitLaunchIdentity(row.ID)
		if err == nil && identity.Generation != "" && identity.Generation != r.PredecessorGeneration {
			return row, identity.Generation
		}
	}
	return nil, ""
}

func recoveryPredecessorStillCurrent(r db.AgentRecovery) bool {
	a, err := db.GetAgent(r.AgentID)
	if err != nil || a == nil || !a.Active() || a.CurrentConvID != r.ConvID {
		return false
	}
	latestSession, err := db.LatestInsertedSessionIDForConv(r.ConvID)
	if err != nil || latestSession != r.PredecessorSessionID {
		return false
	}
	identity, err := db.GetSessionExitLaunchIdentity(r.PredecessorSessionID)
	return err == nil && identity.Generation == r.PredecessorGeneration
}

func recoveryAgentStillCurrent(r db.AgentRecovery) bool {
	a, err := db.GetAgent(r.AgentID)
	return err == nil && a != nil && a.Active() && a.CurrentConvID == r.ConvID
}

func cancelStaleRecovery(r db.AgentRecovery, now time.Time, reason string) {
	if changed, err := db.CancelAgentRecoveryGeneration(r, reason, now); err != nil {
		slog.Warn("agent recovery: cancel stale candidate failed", "agent_id", r.AgentID, "error", err)
	} else if changed {
		if latest, _ := db.AgentRecoveryForAgent(r.AgentID); latest != nil {
			verb := db.AuditVerbAgentRecoveryCancelled
			if reason == "successor_race" || reason == "predecessor_superseded" {
				verb = db.AuditVerbAgentRecoveryRaced
			}
			_ = db.RecordAgentRecoveryAudit(*latest, verb, reason, now)
		}
	}
}

func reconcileAgentRecovery(r db.AgentRecovery, now time.Time) {
	if r.Status == db.AgentRecoveryStatusRecovered {
		if !r.HealthySince.IsZero() && now.Sub(r.HealthySince) >= db.AgentRecoveryHealthyReset {
			successor, generation := recoveryLiveSuccessor(r)
			if successor != nil && successor.ID == r.SuccessorSessionID && generation == r.SuccessorGeneration {
				if changed, err := db.ResetHealthyAgentRecovery(r, now); err != nil {
					slog.Warn("agent recovery: healthy reset failed", "agent_id", r.AgentID, "error", err)
				} else if changed {
					_ = db.RecordAgentRecoveryAudit(r, db.AuditVerbAgentRecoveryReset, "healthy_runtime", now)
				}
			}
		}
		return
	}
	if r.Status == db.AgentRecoveryStatusSuppressed {
		publishRecoverySuppressed(r, now)
		return
	}
	if successor, generation := recoveryLiveSuccessor(r); successor != nil {
		if r.Status != db.AgentRecoveryStatusRestarting {
			cancelStaleRecovery(r, now, "successor_race")
			return
		}
		if changed, err := db.ConfirmAgentRecovery(r, successor.ID, generation, now); err != nil {
			slog.Warn("agent recovery: confirm live successor failed", "agent_id", r.AgentID, "error", err)
		} else if changed {
			if latest, _ := db.AgentRecoveryForAgent(r.AgentID); latest != nil {
				_ = db.RecordAgentRecoveryAudit(*latest, db.AuditVerbAgentRecoveryConfirmed, "live_confirmed", now)
			}
		}
		return
	}
	if !recoveryAgentStillCurrent(r) {
		cancelStaleRecovery(r, now, "predecessor_superseded")
		return
	}
	if r.Status == db.AgentRecoveryStatusRestarting {
		if !r.LeaseExpiresAt.IsZero() && !r.LeaseExpiresAt.After(now) {
			if changed, err := db.ExpireAgentRecoveryLease(r, now); err != nil {
				slog.Warn("agent recovery: expire launch lease failed", "agent_id", r.AgentID, "error", err)
			} else if changed {
				recoveryEnteredBackoff(r.AgentID, now, "live_confirmation_timeout")
			}
		}
		return
	}
	if !recoveryPredecessorStillCurrent(r) {
		cancelStaleRecovery(r, now, "predecessor_superseded")
		return
	}
	publishRecoveryTransition(r, now)
	if r.NextAttemptAt.IsZero() || r.NextAttemptAt.After(now) {
		return
	}
	launchLock := resumeLaunchLock(r.ConvID)
	launchLock.Lock()
	defer launchLock.Unlock()
	// Manual resume/stop may have won while this sweep waited for the mutex.
	latest, err := db.AgentRecoveryForAgent(r.AgentID)
	if err != nil || latest == nil || latest.PredecessorGeneration != r.PredecessorGeneration ||
		(latest.Status != db.AgentRecoveryStatusCrashed && latest.Status != db.AgentRecoveryStatusBackoff) {
		return
	}
	if isConvOnline(r.ConvID) || !recoveryPredecessorStillCurrent(*latest) {
		cancelStaleRecovery(*latest, now, "successor_race")
		return
	}
	if !recoveryResumeTargetResolvable(r.ConvID) {
		claim, claimErr := db.ClaimAgentRecovery(r.AgentID, r.PredecessorGeneration, now)
		if claimErr == nil && claim != nil {
			if changed, _ := db.SuppressAgentRecovery(*claim, now, "resume_target_unresolved"); changed {
				if row, _ := db.AgentRecoveryForAgent(claim.AgentID); row != nil {
					_ = db.RecordAgentRecoveryAudit(*row, db.AuditVerbAgentRecoverySuppressed, "resume_target_unresolved", now)
				}
			}
		}
		return
	}
	claim, err := db.ClaimAgentRecovery(r.AgentID, r.PredecessorGeneration, now)
	if err != nil || claim == nil {
		if err != nil {
			slog.Warn("agent recovery: claim failed", "agent_id", r.AgentID, "error", err)
		}
		return
	}
	_ = db.RecordAgentRecoveryAudit(*claim, db.AuditVerbAgentRecoveryStarted, "scheduled_retry", now)
	current, err := db.AgentRecoveryClaimCurrent(*claim)
	if err != nil {
		slog.Warn("agent recovery: revalidate claim failed", "agent_id", claim.AgentID, "error", err)
		return
	}
	if !current {
		return
	}
	result := resumeOneConvUnderLaunchLock(claim.ConvID, false, false, claim)
	if result.Action == "resumed" || result.Action == "skipped:already_online" || result.Action == "skipped:recovery_cancelled" {
		// A successful spawn is confirmed only after its concrete session and
		// generation are observable. An already-online result means a manual or
		// competing path won after our claim; the same confirmation pass handles
		// it without launching again or manufacturing a failure.
		return
	}
	if strings.HasPrefix(result.Action, "error:resume_provenance") || result.Action == "error:missing_cwd" ||
		(result.Action == "error" && !strings.HasPrefix(result.Detail, "spawn:")) {
		if changed, _ := db.SuppressAgentRecovery(*claim, now, "resume_ineligible"); changed {
			if row, _ := db.AgentRecoveryForAgent(claim.AgentID); row != nil {
				_ = db.RecordAgentRecoveryAudit(*row, db.AuditVerbAgentRecoverySuppressed, "resume_ineligible", now)
			}
		}
		return
	}
	if changed, err := db.FailAgentRecoveryLaunch(*claim, now, "launch_failed"); err != nil {
		slog.Warn("agent recovery: persist launch failure failed", "agent_id", claim.AgentID, "error", err)
	} else if changed {
		recoveryEnteredBackoff(claim.AgentID, now, "launch_failed")
	}
}

func publishRecoverySuppressed(r db.AgentRecovery, now time.Time) {
	_, _ = db.MarkAgentRecoveryNotified(r, false)
}

func publishRecoveryTransition(r db.AgentRecovery, now time.Time) {
	backoff := r.Status == db.AgentRecoveryStatusBackoff
	changed, err := db.MarkAgentRecoveryNotified(r, backoff)
	if err != nil || !changed {
		return
	}
	verb, reason, status := "", "", "crashed"
	if backoff {
		verb, reason, status = db.AuditVerbAgentRecoveryBackoff, "crash_loop", "crash loop / backoff"
	}
	if verb != "" {
		_ = db.RecordAgentRecoveryAudit(r, verb, reason, now)
	}
	if row, _ := db.FindSessionByConvID(r.ConvID); row != nil {
		notify.OnAgentRecoveryTransition(row.ID, r.ConvID, session.StatusExited, status,
			row.Cwd, agent.FreshTitle(r.ConvID), row.Harness)
	}
}

func recoveryEnteredBackoff(agentID string, now time.Time, reason string) {
	r, err := db.AgentRecoveryForAgent(agentID)
	if err != nil || r == nil {
		return
	}
	_ = db.RecordAgentRecoveryAudit(*r, db.AuditVerbAgentRecoveryFailed, reason, now)
	_ = db.RecordAgentRecoveryAudit(*r, db.AuditVerbAgentRecoveryScheduled, "retry_scheduled", now)
	publishRecoveryTransition(*r, now)
}
