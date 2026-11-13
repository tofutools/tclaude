package session

import (
	"fmt"
	"log/slog"
	"runtime"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/launchundo"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// claimDarwinRouteLaunch is runNew's Darwin route step. For a
// DarwinRouteCapable launch it resolves the conversation owner's stable agent
// identity into params.DarwinRouteAgentID, reserves the kernel route slots and
// registers the pending exact-generation claim, adding the claim's removal to
// undo. Any other launch is a no-op that returns a nil reservation.
//
// The reservation is returned even alongside an error once it was taken, so
// the caller's deferred Release still frees it.
func claimDarwinRouteLaunch(params *NewParams, harnessName string, outerLayer bool, sessionID, rowConvID, exitGeneration string, undo *launchundo.Undo) (reservation *DarwinRouteSlotReservation, err error) {
	if !params.DarwinRouteCapable {
		return nil, nil
	}
	if runtime.GOOS != "darwin" {
		return reservation, fmt.Errorf("darwin route-capable launch requires macOS")
	}
	if !outerLayer || !tclaudeLayerWrapsPane(harnessName) {
		return reservation, fmt.Errorf("darwin route-capable launch requires the pane tclaude’s sandbox")
	}
	if rowConvID == "" {
		return reservation, fmt.Errorf("darwin route-capable launch requires a conversation generation")
	}
	resolved, resolveErr := opAgentIDForConv.call(launchConv(sessionID, rowConvID))
	resolvedAgentID := resolved.AgentID
	if resolveErr != nil {
		return reservation, fmt.Errorf("resolve Darwin route agent identity: %w", resolveErr)
	}
	if params.DarwinRouteAgentID != "" && params.DarwinRouteAgentID != resolvedAgentID {
		return reservation, fmt.Errorf("darwin route agent identity does not match conversation owner")
	}
	params.DarwinRouteAgentID = resolvedAgentID
	if params.DarwinRouteAgentID == "" {
		return reservation, fmt.Errorf("darwin route-capable launch requires a stable agent identity")
	}
	reservation, err = ReserveDarwinRouteSlots()
	if err != nil {
		return reservation, fmt.Errorf("reserve Darwin route launch slots: %w", err)
	}
	// Claim the kernel-reserved slots before any launch contract is
	// rendered. The normalized DB claims are the logical exclusion that
	// survives reservation-FD closure; every later rendering/launch step
	// is therefore downstream of one pending exact-generation claim.
	route := launchDarwinRouteArgs{
		launchGenerationArgs: launchGenerationOf(sessionID, exitGeneration),
		AgentID:              params.DarwinRouteAgentID, ConvID: rowConvID,
	}
	register := route
	register.Slots = reservation.Slots()
	if _, err := opDarwinRegisterRoute.call(register); err != nil {
		return reservation, fmt.Errorf("register Darwin route launch contract: %w", err)
	}
	undo.Add(func() {
		if _, err := opDarwinDeleteRoute.call(route); err != nil {
			slog.Warn("Darwin route launch cleanup failed", "conv", rowConvID, "error", err)
		}
	})
	return reservation, nil
}

// launchRowInput is what createLaunchRow records on the fresh session row and
// needs for that row's rollback.
type launchRowInput struct {
	sessionID, tmuxSession, cwd, convID, exitGeneration string
	h                                                   *harness.Harness
	params                                              *NewParams
	harnessBuiltinMode                                  string
	sandboxImplementation                               sandboxpolicy.Implementation
	sandboxChosenBy                                     string
	osSandbox                                           harness.LaunchOSSandbox
	effectiveSandbox                                    *sandboxpolicy.Snapshot
	approvalPolicy                                      string
	autoReview                                          bool
	askTimeout                                          string
	envExports                                          string
	// paneCommand points at runNew's pane command, which is only assembled
	// after the row exists; the rollback reads it when it runs.
	paneCommand *string
}

// createLaunchRow is runNew's session-row step: it builds the launch's
// SessionState, adds the rollback of a row this launch created to undo, and
// saves the row with this launch's exit generation.
func createLaunchRow(in launchRowInput, undo *launchundo.Undo) (*SessionState, error) {
	launchCreated := time.Now()
	state := &SessionState{
		ID:                       in.sessionID,
		TmuxSession:              in.tmuxSession,
		Cwd:                      in.cwd,
		ConvID:                   in.convID,
		Status:                   StatusIdle,
		Harness:                  in.h.Name,
		HarnessBuiltinMode:       sandboxDescr(in.harnessBuiltinMode, in.params.PermissionProfile),
		SandboxImplementation:    string(in.sandboxImplementation),
		HarnessBuiltinModeSource: in.sandboxChosenBy,
		OSSandboxState:           in.osSandbox.State,
		OSSandboxSource:          in.osSandbox.Source,
		OSSandboxUnverified:      in.osSandbox.Unverified,
		EffectiveSandbox:         in.effectiveSandbox,
		ApprovalPolicy:           in.approvalPolicy,
		ApprovalAutoReview:       in.autoReview,
		AskUserQuestionTimeout:   in.askTimeout,
		Created:                  launchCreated,
		Updated:                  launchCreated,
	}
	if in.h.UsesCommandInput() {
		state.Status = StatusRunning
	}
	// Establish the fresh launch identity before any private barrier/token
	// filesystem setup. Row reuse therefore cannot retain predecessor callback
	// or lifecycle-intent authority when that optional setup degrades.
	//
	// A launch that fails between this row write and its pane surviving must
	// take the row back out: the row may carry a preset conv-id that will now
	// never exist, and a dead convless row lingers as a ghost session the
	// operator has to clear by hand (the "command too long" incident's second
	// bug). Only a row this launch CREATED is deleted — a resume/relaunch
	// reuses its existing row (same PK) and keeps its prior state on failure,
	// exactly as before.
	prior, err := opLaunchRowExists.call(launchSession(in.sessionID))
	if err != nil {
		return nil, fmt.Errorf("check for existing session row: %w", err)
	}
	launchRowOwned := !prior.Exists
	// The pane command is assigned by the caller once it is fully assembled,
	// so the rollback below reads it through in.paneCommand at unwind time and
	// reports what this launch actually tried to run.
	undo.Add(func() {
		if !launchRowOwned {
			return
		}
		// Rolling the row back also destroys the exit audit's only anchor: a
		// pane-died callback arriving afterwards cannot load the session and
		// fails with "sql: no rows in result set", so neither the pane's output
		// nor the command that produced it is recorded anywhere. Write the
		// command here, where it is still known.
		//
		// Redacted: the command carries the whole forwarded environment, and
		// output.log is not a place for the operator's API keys.
		if launchPaneCommand := *in.paneCommand; launchPaneCommand != "" {
			if redacted, why := RedactPaneCommandWhy(
				launchPaneCommand, in.envExports, authoredPaneCommandSecrets(in.effectiveSandbox)); why == "" {
				slog.Error("launch failed; rolling back its session row",
					"session_id", in.sessionID, "tmux_session", in.tmuxSession,
					"pane_command", redacted)
			} else {
				// Fail closed: without proof the environment was removed, the
				// command cannot be written at all.
				slog.Error("launch failed; rolling back its session row",
					"session_id", in.sessionID, "tmux_session", in.tmuxSession,
					"pane_command", "<withheld: "+why+">")
			}
		}
		// Generation-conditional: a concurrent same-label launch that re-wrote
		// the row after us must keep it — only the exact row THIS launch wrote
		// (still stamped with our exit generation) is rolled back.
		if _, derr := opDeleteLaunchRow.call(launchGenerationOf(in.sessionID, in.exitGeneration)); derr != nil {
			slog.Warn("launch failed; could not remove its session row",
				"session_id", in.sessionID, "error", derr)
		}
	})
	if _, err := opSaveLaunchRow.call(launchStateArgs{
		State: state, Generation: in.exitGeneration, GateState: db.SessionExitGateUngated,
	}); err != nil {
		return nil, fmt.Errorf("prepare managed pane exit identity: %w", err)
	}
	return state, nil
}
