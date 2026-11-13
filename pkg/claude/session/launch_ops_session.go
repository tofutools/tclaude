package session

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// The session-row operations of a launch. See launch_ops.go for what a launch
// operation is and how it is fenced.

// launchSessionArgs names the session an op touches and nothing else.
type launchSessionArgs struct {
	SessionID string `json:"session_id"`
}

func (a launchSessionArgs) launchSessionID() string  { return a.SessionID }
func (a launchSessionArgs) launchGeneration() string { return "" }

// launchGenerationArgs is the argument shape of an op whose statement is
// conditional on the launch generation.
type launchGenerationArgs struct {
	SessionID  string `json:"session_id"`
	Generation string `json:"generation"`
}

func (a launchGenerationArgs) launchSessionID() string  { return a.SessionID }
func (a launchGenerationArgs) launchGeneration() string { return a.Generation }

// launchStateArgs carries a whole session state. The generation and gate
// state are set only by the op that establishes the launch.
type launchStateArgs struct {
	State      *SessionState `json:"state"`
	Generation string        `json:"generation,omitempty"`
	GateState  string        `json:"gate_state,omitempty"`
}

func (a launchStateArgs) launchSessionID() string {
	if a.State == nil {
		return ""
	}
	return a.State.ID
}
func (a launchStateArgs) launchGeneration() string { return a.Generation }

type launchExitBindingArgs struct {
	launchGenerationArgs
	TokenHash string `json:"token_hash"`
	PaneID    string `json:"pane_id"`
}

type launchStringArgs struct {
	launchSessionArgs
	Value string `json:"value"`
}

type launchPostureArgs struct {
	launchSessionArgs
	Harness string        `json:"harness"`
	Posture LaunchPosture `json:"posture"`
}

// launchNoResult is the result of an op that only succeeds or fails.
type launchNoResult struct{}

type launchRowExistsResult struct {
	Exists bool `json:"exists"`
}

func noResult(err error) (launchNoResult, error) { return launchNoResult{}, err }

var (
	// opLaunchRowExists reports whether the session row predates this launch,
	// which decides whether a failed launch may delete it.
	opLaunchRowExists = defineLaunchOp("session.row_exists", fenceSessionOnly,
		func(a launchSessionArgs) (launchRowExistsResult, error) {
			row, err := db.LoadSession(a.SessionID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return launchRowExistsResult{}, err
			}
			return launchRowExistsResult{Exists: row != nil}, nil
		})

	// opSaveLaunchRow is the one write that establishes a launch: it stamps
	// the row with the launch generation, clearing the predecessor's callback
	// and lifecycle-intent authority in the same statement.
	opSaveLaunchRow = defineLaunchOp("session.save_launch_row", fenceSessionOnly,
		func(a launchStateArgs) (launchNoResult, error) {
			if a.State == nil || a.Generation == "" {
				return noResult(fmt.Errorf("launch row requires a state and a generation"))
			}
			return noResult(SaveSessionStateForLaunch(a.State, a.Generation, a.GateState))
		})

	// opDeleteLaunchRow takes back the row of a launch that failed. It is
	// conditional on the generation, so a newer launch's row survives.
	opDeleteLaunchRow = defineLaunchOp("session.delete_launch_row", fenceSessionOnly,
		func(a launchGenerationArgs) (launchNoResult, error) {
			return noResult(db.DeleteSessionForLaunchGeneration(a.SessionID, a.Generation))
		})

	// opSaveLaunchedState records what only exists once the pane is up: its
	// pid and the launch-directory provenance.
	opSaveLaunchedState = defineLaunchOp("session.save_launched_state", fenceCurrentGeneration,
		func(a launchStateArgs) (launchNoResult, error) {
			if a.State == nil {
				return noResult(fmt.Errorf("launched state is missing"))
			}
			return noResult(SaveSessionState(a.State))
		})

	opSetResumeProvenance = defineLaunchOp("session.set_resume_provenance", fenceCurrentGeneration,
		func(a launchStringArgs) (launchNoResult, error) {
			return noResult(db.SetSessionResumeProvenance(a.SessionID, a.Value))
		})

	opSetExecutionBoundary = defineLaunchOp("session.set_execution_boundary", fenceCurrentGeneration,
		func(a launchStringArgs) (launchNoResult, error) {
			return noResult(db.SetSessionExecutionBoundary(a.SessionID, a.Value))
		})

	opRecordModelProxy = defineLaunchOp("session.record_model_proxy", fenceCurrentGeneration,
		func(a launchStringArgs) (launchNoResult, error) {
			return noResult(db.RecordSessionModelProxy(a.SessionID, a.Value))
		})

	opSeedModel = defineLaunchOp("session.seed_model", fenceCurrentGeneration,
		func(a launchStringArgs) (launchNoResult, error) {
			return noResult(db.UpdateSessionModel(a.SessionID, a.Value))
		})

	opSeedModelID = defineLaunchOp("session.seed_model_id", fenceCurrentGeneration,
		func(a launchStringArgs) (launchNoResult, error) {
			return noResult(db.UpdateSessionModelID(a.SessionID, a.Value))
		})

	opSeedEffort = defineLaunchOp("session.seed_effort", fenceCurrentGeneration,
		func(a launchStringArgs) (launchNoResult, error) {
			return noResult(db.UpdateSessionEffort(a.SessionID, a.Value))
		})

	// opRecordLaunchPosture is best-effort by design (see RecordLaunchPosture):
	// each column logs its own failure where the write runs.
	opRecordLaunchPosture = defineLaunchOp("session.record_launch_posture", fenceCurrentGeneration,
		func(a launchPostureArgs) (launchNoResult, error) {
			h, ok := harness.Get(a.Harness)
			if !ok {
				return noResult(fmt.Errorf("unknown harness %q", a.Harness))
			}
			writeLaunchPosture(a.SessionID, h, a.Posture)
			return noResult(nil)
		})

	// The exit-audit gate. Every one of these statements is conditional on
	// the launch generation it is given; fencing them as well only turns a
	// superseded launcher's failure into the same refusal every other
	// operation gets.
	opExitMarkPending = defineLaunchOp("exit.mark_pending", fenceCurrentGeneration,
		func(a launchGenerationArgs) (launchNoResult, error) {
			return noResult(db.MarkSessionExitLaunchPending(a.SessionID, a.Generation))
		})

	opExitMarkReleasing = defineLaunchOp("exit.mark_releasing", fenceCurrentGeneration,
		func(a launchGenerationArgs) (launchNoResult, error) {
			return noResult(db.MarkSessionExitLaunchReleasing(a.SessionID, a.Generation))
		})

	opExitMarkReleased = defineLaunchOp("exit.mark_released", fenceCurrentGeneration,
		func(a launchGenerationArgs) (launchNoResult, error) {
			return noResult(db.MarkSessionExitLaunchReleased(a.SessionID, a.Generation))
		})

	opExitMarkUngated = defineLaunchOp("exit.mark_ungated", fenceCurrentGeneration,
		func(a launchGenerationArgs) (launchNoResult, error) {
			return noResult(db.MarkSessionExitLaunchUngated(a.SessionID, a.Generation))
		})

	opExitSetBinding = defineLaunchOp("exit.set_binding", fenceCurrentGeneration,
		func(a launchExitBindingArgs) (launchNoResult, error) {
			return noResult(db.SetSessionExitLaunchBinding(a.SessionID, a.Generation, a.TokenHash, a.PaneID))
		})

	opExitClearBinding = defineLaunchOp("exit.clear_binding", fenceCurrentGeneration,
		func(a launchGenerationArgs) (launchNoResult, error) {
			return noResult(db.ClearSessionExitLaunchBinding(a.SessionID, a.Generation))
		})
)

func launchSession(sessionID string) launchSessionArgs {
	return launchSessionArgs{SessionID: sessionID}
}

func launchGenerationOf(sessionID, generation string) launchGenerationArgs {
	return launchGenerationArgs{SessionID: sessionID, Generation: generation}
}

func launchString(sessionID, value string) launchStringArgs {
	return launchStringArgs{launchSessionArgs: launchSession(sessionID), Value: value}
}
