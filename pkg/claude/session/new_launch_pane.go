package session

import (
	"fmt"
	"log/slog"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// newLaunchExitGuard is runNew's exit-audit step: the private launch gate
// that keeps the harness behind a bounded barrier until its pane-local exit
// hook and durable binding are ready. When the gate or its pending state
// cannot be set up, the launch continues with a disabled guard rather than
// failing. The caller owns the guard's abort.
func newLaunchExitGuard(sessionID, tmuxSession, exitGeneration string) *exitLaunchGuard {
	exitGuard, err := newExitLaunchGuard(sessionID, tmuxSession, exitGeneration)
	if err != nil {
		slog.Warn("exit audit: private launch setup unavailable; continuing without callback",
			"session_id", sessionID, "tmux_session", tmuxSession, "error", err)
		exitGuard = disabledExitLaunchGuard(sessionID, tmuxSession, exitGeneration)
	} else if err := db.MarkSessionExitLaunchPending(sessionID, exitGeneration); err != nil {
		slog.Warn("exit audit: launch gate state unavailable; continuing without callback",
			"session_id", sessionID, "tmux_session", tmuxSession, "error", err)
		exitGuard.abort()
		exitGuard = disabledExitLaunchGuard(sessionID, tmuxSession, exitGeneration)
	}
	return exitGuard
}

// guardLaunchWithDirProof is runNew's directory-proof step. When the launch
// carries a cwd or dir write proof, or its sandbox profile grants host write
// paths, it creates the readiness file and wraps harnessCmd in the proof guard.
// It returns the command, the readiness path (empty when no proof runs) and a
// cleanup the caller defers, which is a no-op when no file was created.
func guardLaunchWithDirProof(params *NewParams, launchSandbox *sandboxpolicy.Snapshot, harnessCmd string,
) (string, string, func(), error) {
	cleanup := func() {}
	proofReadyPath := ""
	proofToken := params.CwdWriteProof
	if proofToken == "" {
		proofToken = params.DirWriteProof
	}
	profileWriteDirs := sandboxSnapshotHostDirs(launchSandbox, sandboxpolicy.AccessWrite)
	profileWriteFiles := sandboxSnapshotHostFiles(launchSandbox, sandboxpolicy.AccessWrite)
	if proofToken != "" || len(profileWriteDirs) > 0 || len(profileWriteFiles) > 0 {
		path, cleanupProofReady, readyErr := newSpawnCwdReadinessFile()
		if readyErr != nil {
			return "", "", nil, readyErr
		}
		cleanup = cleanupProofReady
		proofReadyPath = path
		harnessCmd = guardHarnessCommandWithDirProof(
			harnessCmd, proofToken, proofReadyPath, params.CwdWriteProof != "",
			params.GitWorktreeWriteDirs, profileWriteDirs, profileWriteFiles)
	}
	return harnessCmd, proofReadyPath, cleanup, nil
}

// logLaunchPaneCommand emits the fully assembled pane command at debug,
// redacted, or a warning that it was withheld when the environment prefix
// cannot be located to redact.
func logLaunchPaneCommand(harnessCmd, envExports string, effectiveSandbox *sandboxpolicy.Snapshot, sessionID, tmuxSession string) {
	if redacted, ok := RedactPaneCommand(
		harnessCmd, envExports, authoredPaneCommandSecrets(effectiveSandbox)); ok {
		slog.Debug("launching managed pane",
			"session_id", sessionID, "tmux_session", tmuxSession,
			"pane_command", redacted)
	} else {
		slog.Warn("launching managed pane; command withheld from the log",
			"session_id", sessionID, "tmux_session", tmuxSession,
			"reason", "environment prefix could not be located to redact")
	}
}

// launchSettleInput is what settleLaunchedPane needs to finish a launched
// pane.
type launchSettleInput struct {
	exitGuard                 *exitLaunchGuard
	proofReadyPath            string
	killLaunchPane            func()
	timing                    func(string, ...any)
	tmuxSession, sessionID    string
	params                    *NewParams
	h                         *harness.Harness
	state                     *SessionState
	model, effort             string
	autoMemory, remoteControl bool
	contextFeatures           map[string]string
	autoCompactWindow         string
	ordinaryNativeGeneration  string
	executionBoundaryJSON     []byte
}

// settleLaunchedPane is runNew's after-launch step. It arms and binds the
// exit guard, waits for the directory proof, captures provenance, configures
// the pane, saves the row with its PID and provenance, records the launch
// posture, releases the exit guard, activates a generated Codex permission
// profile and publishes the execution boundary. Every failure kills the pane
// before returning, so the caller's uncommitted undo can roll the launch back.
func settleLaunchedPane(in launchSettleInput) error {
	in.exitGuard.armPaneHook()
	in.exitGuard.bind()
	if in.proofReadyPath != "" {
		if err := waitForSpawnCwdReadiness(in.proofReadyPath); err != nil {
			in.killLaunchPane()
			return err
		}
	}
	in.timing("pane_gate_and_directory_proof_complete")
	resumeProvenance, err := captureLaunchProvenance(in.tmuxSession, in.sessionID, in.params.ManagedLaunch)
	if err != nil {
		in.killLaunchPane()
		return err
	}
	configureLaunchedPane(in.tmuxSession, in.sessionID, in.h)

	// Get the PID of claude in the tmux session
	pid := ParsePIDFromTmux(in.tmuxSession)

	in.state.PID = pid
	in.state.ResumeProvenance = resumeProvenance

	if err := SaveSessionState(in.state); err != nil {
		// Kill the pane like the sibling failure paths below: leaving it alive
		// while the launch-row defer rolls the row back would orphan a live
		// pane tclaude can no longer see.
		in.killLaunchPane()
		return fmt.Errorf("failed to save session state: %w", err)
	}
	if resumeProvenance != "" {
		// SessionStart may have inserted this row before the launch parent got
		// here. SaveSession deliberately never updates provenance on conflict;
		// the launch boundary therefore owns this explicit post-upsert write.
		if err := db.SetSessionResumeProvenance(in.sessionID, resumeProvenance); err != nil {
			in.killLaunchPane()
			return fmt.Errorf("persist managed launch resume provenance: %w", err)
		}
	}
	recordLaunchPostureAndSeeds(in.sessionID, in.h, in.params, in.model, in.effort, in.autoMemory, in.remoteControl, in.contextFeatures, in.autoCompactWindow)
	if err := in.exitGuard.release(); err != nil {
		in.killLaunchPane()
		return fmt.Errorf("bind managed pane exit audit: %w", err)
	}
	// Launch bookkeeping and exit attribution are durable. Publish successor
	// readiness only at this final boundary: any earlier failure unregisters
	// this generation and cannot prune a still-resumable predecessor.
	if in.ordinaryNativeGeneration != "" {
		if err := ActivateCodexNativePermissionProfile(in.ordinaryNativeGeneration); err != nil {
			in.killLaunchPane()
			return fmt.Errorf("activate generated Codex permission profile: %w", err)
		}
	}
	if len(in.executionBoundaryJSON) > 0 {
		// Publish only at the same successful launch-commit boundary as the row.
		// Until here a relaunch retains its predecessor's last-known-good record;
		// any persistence failure tears down this pane and leaves that record intact.
		if err := db.SetSessionExecutionBoundary(in.sessionID, string(in.executionBoundaryJSON)); err != nil {
			slog.Warn("could not persist launch execution boundary",
				"session_id", in.sessionID, "error", err)
		}
	}
	return nil
}
