package agentd

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	platformexec "github.com/tofutools/tclaude/pkg/claude/platform/execution"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// cancelManagedResumes is called under the stable-agent launch lock. SQLite,
// not that in-process lock, arbitrates with the already-forked child.
var resumeCancellationBeforeCAS = func(db.ResumeOperationRow) {}
var activeResumeOperationsForCancellation = db.ActiveResumeOperations

func cancelManagedResumes(convID, detail string) error {
	operations, err := activeResumeOperationsForCancellation()
	if err != nil {
		return fmt.Errorf("list active Resume operations: %w", err)
	}
	var cancellationErrs []error
	for _, op := range operations {
		if op.ConvID != convID || op.State == platformexec.ResumeCancelling {
			continue
		}
		converged := false
		for range 8 { // child claim/register/release has fewer revision edges
			current, loadErr := db.GetResumeOperation(op.ID)
			if loadErr != nil {
				cancellationErrs = append(cancellationErrs,
					fmt.Errorf("reload Resume operation %s: %w", op.ID, loadErr))
				break
			}
			if current == nil {
				converged = true
				break
			}
			if current.State == platformexec.ResumeCancelling || current.State == platformexec.ResumeReady ||
				current.State == platformexec.ResumeRejected || current.State == platformexec.ResumeFailed || current.State == platformexec.ResumeCancelled {
				converged = true
				break
			}
			resumeCancellationBeforeCAS(*current)
			var won bool
			if current.LaunchPhase == "release_granted" || current.LaunchPhase == "released" {
				won, err = db.RequestReleasedResumeCancellation(current.ID, current.Revision, detail)
			} else {
				won, err = db.RequestResumeCancellation(current.ID, current.Revision, detail)
			}
			if err != nil {
				cancellationErrs = append(cancellationErrs,
					fmt.Errorf("cancel Resume operation %s: %w", current.ID, err))
				break
			}
			if won {
				slog.Info("resume: cancellation requested", "operation", current.ID, "phase", current.LaunchPhase)
				converged = true
				break
			}
		}
		if !converged {
			cancellationErrs = append(cancellationErrs,
				fmt.Errorf("Resume operation %s cancellation did not converge", op.ID))
		}
	}
	return errors.Join(cancellationErrs...)
}

// reconcileResumeOperations owns crash edges for manual and recovery Resume
// alike. It only advances from exact durable/OS evidence and never creates a
// recovery episode for an ordinary manual operation.
func reconcileResumeOperations(now time.Time, dispatchExactStop bool) {
	operations, err := db.ActiveResumeOperations()
	if err != nil {
		slog.Warn("resume: list operations for reconciliation failed", "error", err)
		return
	}
	for _, op := range operations {
		reconcileResumeOperation(op, now, dispatchExactStop)
	}
}

func reconcileResumeOperation(op db.ResumeOperationRow, now time.Time, dispatchExactStop bool) {
	switch op.State {
	case platformexec.ResumeRequested:
		// No child can claim requested. Once the acceptor's bounded startup
		// window has elapsed, revocation plus absent process/pane is proof that
		// this crash edge cannot produce a workload.
		if !op.RequestedAt.IsZero() && now.Sub(op.RequestedAt) > 2*time.Minute {
			won, err := db.RequestResumeCancellation(op.ID, op.Revision, "reconciler revoked stale requested operation")
			if err == nil && won {
				_, _ = db.FinalizeResumeFailed(op.ID, op.Revision+1, "request was never accepted; no workload can emerge")
			}
		}
	case platformexec.ResumeCancelling:
		if (op.LaunchPhase == "release_granted" || op.LaunchPhase == "released") &&
			dispatchExactStop && resumeAttemptIsCurrent(op) {
			managedExecutionRuntime.stop(op.ConvID, true, db.AgentExitActionForceStop, "", stopWaitForExit(0))
		}
		if op.TmuxSession != "" && session.IsTmuxSessionAlive(op.TmuxSession) {
			// Before release the pane contains only the bounded gate wrapper. The
			// exact registered tmux/session/E check prevents touching a successor.
			if op.LaunchPhase == "revoked" && resumeAttemptIsCurrent(op) {
				_ = clcommon.TmuxCommand("kill-session", "-t", clcommon.ExactTarget(op.TmuxSession)).Run()
			}
			return
		}
		if resumeClaimProcessCurrent(op.ClaimPID, op.ClaimProcessStart) {
			return
		}
		removeResumeGateArtifacts(op.GatePath)
		latest, err := db.GetResumeOperation(op.ID)
		if err == nil && latest != nil && latest.State == platformexec.ResumeCancelling {
			if strings.HasPrefix(latest.FailureDetail, "reconciler revoked") {
				_, _ = db.FinalizeResumeFailed(latest.ID, latest.Revision, "revocation and exact cleanup prove no workload can emerge")
			} else {
				_, _ = db.FinalizeResumeCancelled(latest.ID, latest.Revision, "claim revoked and exact launch cleanup observed")
			}
		}
	case platformexec.ResumeAccepted, platformexec.ResumeUnknown:
		if op.LaunchPhase == "release_granted" {
			if _, err := os.Stat(op.GatePath + ".ack"); op.StartedAt.IsZero() && (err == nil || resumeAttemptReleased(op)) {
				_, _ = db.MarkResumeReleased(op.ID, op.Attempt.ExecutionID, op.Attempt.LegacySessionID, op.TmuxSession, op.PaneID)
				return
			}
		}
		// An accepted dispatch that outlives the gate window without exact
		// release evidence is ambiguous. Unknown blocks any duplicate replay.
		if op.State == platformexec.ResumeAccepted && !op.RequestedAt.IsZero() && now.Sub(op.RequestedAt) > 2*time.Minute {
			_ = db.TransitionResumeOperation(op.ID, op.Revision, platformexec.ResumeUnknown, op.LaunchPhase, "resume effect remains unresolved")
			return
		}
		if op.State == platformexec.ResumeUnknown && op.LaunchPhase != "release_granted" && op.LaunchPhase != "released" {
			won, err := db.RequestResumeCancellation(op.ID, op.Revision, "reconciler revoked unresolved admission")
			if err == nil && won {
				latest, _ := db.GetResumeOperation(op.ID)
				if latest != nil {
					reconcileResumeOperation(*latest, now, dispatchExactStop)
				}
			}
		}
	case platformexec.ResumeStarted:
		if !op.RequestedAt.IsZero() && now.Sub(op.RequestedAt) > 2*time.Minute {
			_ = db.TransitionResumeOperation(op.ID, op.Revision, platformexec.ResumeUnknown, "released", "exact workload started but readiness remains unresolved")
		}
	}
}

func resumeAttemptIsCurrent(op db.ResumeOperationRow) bool {
	if op.Attempt.LegacySessionID == "" || op.TmuxSession == "" {
		return false
	}
	row, err := db.LoadSession(op.Attempt.LegacySessionID)
	if err != nil || row == nil || row.TmuxSession != op.TmuxSession {
		return false
	}
	identity, err := db.GetSessionExitLaunchIdentity(row.ID)
	return err == nil && identity.Generation == op.Attempt.ExecutionID.String()
}

func resumeAttemptReleased(op db.ResumeOperationRow) bool {
	if !resumeAttemptIsCurrent(op) {
		return false
	}
	identity, err := db.GetSessionExitLaunchIdentity(op.Attempt.LegacySessionID)
	return err == nil && identity.GateState == db.SessionExitGateReleased
}

func removeResumeGateArtifacts(path string) {
	clean := filepath.Clean(strings.TrimSpace(path))
	root := filepath.Join(filepath.Clean(config.DataDir()), "exit-launch")
	if clean == "." || filepath.Dir(clean) != root || !strings.HasPrefix(filepath.Base(clean), "barrier-") {
		return
	}
	for _, candidate := range []string{clean, clean + ".ack", clean + ".abort"} {
		if err := os.Remove(candidate); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("resume: remove exact gate artifact failed", "path", candidate, "error", err)
		}
	}
}
