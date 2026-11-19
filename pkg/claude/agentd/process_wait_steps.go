package agentd

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/executor"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// Wait steps (tk_96aee1ac). A run plans a wait step as a CommandWait. The
// daemon fixes its deadline the first time it sees it, in a
// process_wait_timers row that survives restarts, and settles it through
// executor.FinishWait once the deadline passes. The wall clock keeps running
// while a run is paused: a deadline that passed meanwhile ends the wait when
// the run is resumed. A deadline that cannot be worked out (an output that is
// not a time, say) fails the step, and with it the run, naming the step.

// processWaitActor is who ends a wait whose deadline passed.
const processWaitActor = "system:process-runtime"

// processWaitMaxSleep bounds how long the wake loop sleeps without looking,
// so a deadline it was not told about is still met within this.
const processWaitMaxSleep = time.Minute

// processWaitMinSleep is the shortest the wake loop sleeps between sweeps.
const processWaitMinSleep = time.Second

var (
	// processWaitNow is the clock waits are timed by; tests swap it.
	processWaitNow = time.Now
	// processWaitMu serializes sweeps, so one wait is never ended twice at once.
	processWaitMu sync.Mutex
	// processWaitKick wakes the wake loop to sweep now.
	processWaitKick = make(chan struct{}, 1)
)

// kickProcessWaits asks the wake loop to sweep: a wait was planned, a run was
// resumed, or the periodic tick came round.
func kickProcessWaits() {
	select {
	case processWaitKick <- struct{}{}:
	default:
	}
}

// runProcessWaitLoop sweeps whenever kicked, and otherwise sleeps until the
// next deadline (at most processWaitMaxSleep).
func runProcessWaitLoop(stop <-chan struct{}) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-stop:
			return
		case <-processWaitKick:
		case <-timer.C:
		}
		next := sweepProcessWaits()
		sleep := processWaitMaxSleep
		if !next.IsZero() {
			if until := next.Sub(processWaitNow()); until < sleep {
				// A wait that is due but could not be ended (its run refused
				// it) stays due: the floor keeps that from spinning.
				sleep = max(until, processWaitMinSleep)
			}
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(sleep)
	}
}

// sweepProcessWaits fixes the deadline of every wait that has none yet, ends
// every wait that is due, and returns the next deadline (zero for none).
func sweepProcessWaits() time.Time {
	processWaitMu.Lock()
	defer processWaitMu.Unlock()
	if err := db.DeleteFinishedProcessWaitTimers(); err != nil {
		slog.Warn("process waits: clearing finished runs' timers failed", "error", err)
	}
	untimed, err := db.ListProcessUntimedWaits()
	if err != nil {
		slog.Warn("process waits: listing new waits failed", "error", err)
	}
	for _, wait := range untimed {
		startProcessWaitTimer(wait)
	}
	// A run given up withdraws its waits through its owner as it is given up.
	// One the owner did not get to (the daemon stopped in between, say) is
	// withdrawn here, so the run drains instead of waiting out the deadline.
	draining, err := db.ListDrainingProcessWaitTimers()
	if err != nil {
		slog.Warn("process waits: listing given-up runs' waits failed", "error", err)
	}
	for _, timer := range draining {
		_ = finishProcessWait(timer, processWaitActor, executor.WaitWithdrawn,
			"withdrawn: the run was given up while this step waited")
	}
	due, err := db.ListDueProcessWaitTimers(processWaitNow())
	if err != nil {
		slog.Warn("process waits: listing due waits failed", "error", err)
	}
	for _, timer := range due {
		how, reason := executor.WaitElapsed, ""
		if timer.Failure != "" {
			how, reason = executor.WaitFailed, timer.Failure
		}
		_ = finishProcessWait(timer, processWaitActor, how, reason)
	}
	next, ok, err := db.NextProcessWaitDue()
	if err != nil || !ok {
		return time.Time{}
	}
	return next
}

// startProcessWaitTimer fixes one new wait's deadline from its bound duration
// and until, reading any {{ outputs.name }} as the run has them now. One that
// cannot be worked out is recorded as due now with its failure, so the rest
// of the sweep fails the step with it.
func startProcessWaitTimer(wait db.ProcessUntimedWait) {
	now := processWaitNow().UTC()
	timer := db.ProcessWaitTimer{
		RunID: wait.RunID, CommandID: wait.CommandID, NodeID: wait.NodeID, Attempt: wait.Attempt,
		StartedAt: now, DueAt: now,
	}
	raw := processCaptureRawValues(wait.RunID, wait.NodeID)
	values := func(name string) (string, bool) {
		value, ok := raw(name)
		return strings.TrimSpace(value), ok
	}
	duration := model.InterpolateCaptures(wait.Duration, values)
	until := model.InterpolateCaptures(wait.Until, values)
	// A wait due at once (a check wait's first check) keeps now as its deadline.
	if !wait.Now {
		if missing := slices.Concat(model.CaptureReferences(duration), model.CaptureReferences(until)); len(missing) > 0 {
			timer.Failure = "wait '" + wait.NodeID + "': no step has reported output " + strconv.Quote(missing[0]) + " to wait on"
		} else if due, err := model.WaitDeadline(duration, until, now); err != nil {
			timer.Failure = "wait '" + wait.NodeID + "': " + err.Error()
		} else {
			timer.DueAt = due.UTC()
		}
	}
	if _, err := db.EnsureProcessWaitTimer(timer); err != nil {
		slog.Warn("process waits: recording a wait's deadline failed", "run", wait.RunID, "node", wait.NodeID, "error", err)
	}
}

// finishProcessWait ends one wait through the run's owner. A wait that is no
// longer outstanding (a cancel or a send-back took it) just loses its timer.
func finishProcessWait(timer db.ProcessWaitTimer, actor, how, reason string) error {
	_, err := processRuns.submit(timer.RunID, actor, processRunInput{wait: &executor.WaitFinish{
		CommandID: timer.CommandID, NodeID: timer.NodeID, Attempt: timer.Attempt,
		How: how, Reason: reason, StartedAt: timer.StartedAt, DueAt: timer.DueAt,
	}})
	if err == nil || errors.Is(err, engine.ErrStaleObservation) {
		if derr := db.DeleteProcessWaitTimer(timer.RunID, timer.CommandID); derr != nil {
			slog.Warn("process waits: dropping an ended wait's timer failed", "run", timer.RunID, "node", timer.NodeID, "error", derr)
		}
		return err
	}
	slog.Warn("process waits: ending a wait failed", "run", timer.RunID, "node", timer.NodeID, "how", how, "error", err)
	return err
}

// withdrawProcessWaits fails every wait of a run that was given up, so it can
// drain and finish. Called by the run's owner, like the agent withdrawal.
func withdrawProcessWaits(runID string, run *executor.Run) {
	if !run.Draining() {
		return
	}
	for _, command := range run.WaitCommands() {
		finish := executor.WaitFinish{
			CommandID: command.ID, NodeID: command.NodeID, Attempt: command.Attempt,
			How: executor.WaitWithdrawn, Reason: "withdrawn: the run was given up while this step waited",
		}
		if timer, ok, err := db.GetProcessWaitTimer(runID, command.ID); err == nil && ok {
			finish.StartedAt, finish.DueAt = timer.StartedAt, timer.DueAt
		}
		if err := executor.FinishWait(run, executor.EngineActor, finish); err != nil {
			slog.Warn("process waits: withdrawal not applied", "run", runID, "node", command.NodeID, "error", err)
			continue
		}
		_ = db.DeleteProcessWaitTimer(runID, command.ID)
	}
}

// processWaitTimersOf is a run's wait timers by command id, for its view.
func processWaitTimersOf(runID string) map[string]db.ProcessWaitTimer {
	timers, err := db.ListProcessWaitTimersForRun(runID)
	out := make(map[string]db.ProcessWaitTimer, len(timers))
	if err != nil {
		return out
	}
	for _, timer := range timers {
		out[timer.CommandID] = timer
	}
	return out
}

// processRunWaitView is a waiting wait step as a run's view shows it: what it
// waits for and, once the daemon has fixed it, until when.
type processRunWaitView struct {
	Duration  string     `json:"duration,omitempty"`
	Until     string     `json:"until,omitempty"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
	DueAt     *time.Time `json:"dueAt,omitempty"`
	// Failure says the deadline could not be worked out; the step fails.
	Failure string `json:"failure,omitempty"`
	// Check is a wait for a criterion's progress: checks made, failed in a
	// row, the last answer. This timer is its wait for the next check.
	Check *engine.WaitCheckState `json:"check,omitempty"`
}

func processRunWaitViewOf(command engine.Command, timers map[string]db.ProcessWaitTimer, checks map[string]engine.WaitCheckState) *processRunWaitView {
	view := &processRunWaitView{}
	if state, ok := checks[command.NodeID]; ok {
		view.Check = &state
	}
	if command.Wait != nil {
		view.Duration, view.Until = command.Wait.Duration, command.Wait.Until
	}
	if timer, ok := timers[command.ID]; ok {
		started, due := timer.StartedAt.UTC(), timer.DueAt.UTC()
		view.StartedAt, view.DueAt, view.Failure = &started, &due, timer.Failure
	}
	return view
}

// errProcessNoWait refuses a skip for a step that is not waiting.
var errProcessNoWait = errors.New("that step is not waiting")

// processRunSkipWaitRequest is the body of POST .../skip-wait.
type processRunSkipWaitRequest struct {
	NodeID string `json:"nodeId"`
}

// handleProcessRunSkipWait ends a waiting wait step now, as skipped by the
// caller: the run goes on along the step's route. process.runs.manage only.
func handleProcessRunSkipWait(w http.ResponseWriter, r *http.Request) {
	caller, ok := requirePermission(w, r, PermProcessRunsManage)
	if !ok {
		return
	}
	var request processRunSkipWaitRequest
	if err := decodeProcessRuntimeRequest(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "process_run_request", err.Error())
		return
	}
	actor, err := processRequestActor(r, caller)
	if err != nil {
		writeError(w, http.StatusForbidden, "process_run_actor", err.Error())
		return
	}
	runID := r.PathValue("id")
	setAuditDetail(r, "run: "+runID+", skip wait: "+stripControl(request.NodeID))
	view, err := loadProcessRunView(runID)
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	var timer db.ProcessWaitTimer
	found := false
	for _, command := range view.Commands {
		if command.Kind == engine.CommandWait && command.NodeID == request.NodeID {
			timer = db.ProcessWaitTimer{RunID: runID, CommandID: command.CommandID, NodeID: command.NodeID, Attempt: command.Attempt}
			if command.Wait != nil && command.Wait.StartedAt != nil && command.Wait.DueAt != nil {
				timer.StartedAt, timer.DueAt = *command.Wait.StartedAt, *command.Wait.DueAt
			}
			found = true
		}
	}
	if !found {
		writeProcessRuntimeError(w, fmt.Errorf("%w: %q", errProcessNoWait, request.NodeID))
		return
	}
	processWaitMu.Lock()
	err = finishProcessWait(timer, string(actor), executor.WaitSkipped, "")
	processWaitMu.Unlock()
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	view, err = loadProcessRunView(runID)
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	writeProcessJSON(w, http.StatusAccepted, map[string]any{"run": view})
}
