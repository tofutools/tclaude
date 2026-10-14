package agentd

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/executor"
)

// An operator pause holds a run: it dispatches no new agent turns or program
// commands, while attempts already in flight finish and are recorded. The
// hold is durable (db.SetProcessRunPaused) and is read wherever planning or
// delivery happens — the owner before every planning call, the dispatcher
// before every delivery — so a pause takes effect without routing through a
// live owner.

var errProcessRunPaused = errors.New("process run is paused; resume it to continue")

// processRunPaused reports whether an operator paused the run.
func processRunPaused(runID string) (bool, error) {
	scope, err := db.GetProcessRunScope(runID)
	if err != nil {
		return false, err
	}
	return scope.Paused(), nil
}

// processRunPauseGate orders pausing against planning. A planner holds it
// shared from its pause read through the commit that plans, and a pause
// commits under it exclusively, so once pause has answered no planner can
// still be acting on an older "not paused" read. Planning passes are short
// SQLite commits; pausing is rare.
var processRunPauseGate sync.RWMutex

// gateProcessRunPlanning takes the planning gate and reloads the pause into
// the owner's run; the caller releases the gate once its planning committed.
func gateProcessRunPlanning(runID string, run *executor.Run) func() {
	processRunPauseGate.RLock()
	refreshProcessRunHold(runID, run)
	return processRunPauseGate.RUnlock
}

// processRunResumeOwnerWait bounds how long a resume waits for an owner that
// is draining a held run. An owner with work still in flight re-reads the
// pause before it plans again, so it needs no restart.
const processRunResumeOwnerWait = 250 * time.Millisecond

// resumeProcessRun resumes (or ticks) a run after a resume released any
// pause. An owner that last read the run as paused may be exiting as the
// pause clears; resume then waits briefly for it to go and starts the run
// itself rather than leaving it to the fallback sweep.
func resumeProcessRun(runID string, unpaused bool) (bool, error) {
	started, err := processRuns.begin(runID, processRunStart{mode: processRunResume})
	if err == nil && !started && unpaused {
		if claim, live, _ := processRuns.liveClaim(runID); live {
			select {
			case <-claim.gone:
				started, err = processRuns.begin(runID, processRunStart{mode: processRunResume})
			case <-time.After(processRunResumeOwnerWait):
			}
		}
	}
	if err != nil && unpaused && (errors.Is(err, executor.ErrNeedsReconcile) || errors.Is(err, errProcessRunOrphaned)) {
		// The pause is released either way; what still blocks the run shows
		// in its action, not as a refusal of the resume.
		return false, nil
	}
	return started, err
}

// refreshProcessRunHold reloads the durable pause into the owner's run before
// it plans. A failed read holds: planning past a pause is the worse mistake.
func refreshProcessRunHold(runID string, run *executor.Run) {
	paused, err := processRunPaused(runID)
	if err != nil {
		slog.Warn("process runtime: pause check failed; holding", "run", runID, "error", err)
		paused = true
	}
	run.Hold(paused)
}

// handleProcessRunPause pauses a run. Pausing a paused run changes nothing.
func handleProcessRunPause(w http.ResponseWriter, r *http.Request) {
	caller, ok := requirePermission(w, r, PermProcessRunsManage)
	if !ok {
		return
	}
	if err := decodeEmptyProcessRuntimeRequest(w, r); err != nil {
		writeError(w, http.StatusBadRequest, "process_run_request", err.Error())
		return
	}
	actor, err := processTemplateAuthor(caller)
	if err != nil {
		writeError(w, http.StatusForbidden, "process_run_actor", err.Error())
		return
	}
	runID := strings.TrimSpace(r.PathValue("id"))
	setAuditDetail(r, "run: "+runID)
	processRunPauseGate.Lock()
	_, changed, err := db.SetProcessRunPaused(runID, true, string(actor))
	processRunPauseGate.Unlock()
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	if changed {
		// Undelivered attempts now wait with a visible reason.
		kickProcessAgentDispatch(runID)
	}
	view, err := loadProcessRunView(runID)
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	writeProcessJSON(w, http.StatusOK, map[string]any{"changed": changed, "run": view})
}

// releaseProcessRunPause clears an operator pause ahead of a resume and
// re-delivers whatever waited on it. It reports whether a pause was cleared.
func releaseProcessRunPause(runID, actor string) (bool, error) {
	_, changed, err := db.SetProcessRunPaused(runID, false, actor)
	if err != nil {
		return false, err
	}
	if changed {
		kickProcessAgentDispatch(runID)
	}
	return changed, nil
}
