package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// awbPickupIssueTimeout bounds each live AWB lookup the status surface makes,
// so one slow or unreachable server cannot hang `tclaude pickup ls`.
const awbPickupIssueTimeout = 5 * time.Second

// awbPickupResetWait bounds how long a reset waits for an in-progress poll of
// the same process to finish. A poll that spawns an agent can take a while;
// the operator gets a retryable conflict rather than a hung command.
var awbPickupResetWait = 15 * time.Second

// awbReadyRuntime is the in-memory record of a worker's most recent poll —
// the part of a process's health the dispatch row cannot hold, because a
// process with nothing in flight has no row.
type awbReadyRuntime struct {
	mu         sync.Mutex
	lastPollAt time.Time
	lastError  string
	hold       string
}

func (r *awbReadyRuntime) recordPoll(err error) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastPollAt = time.Now()
	r.lastError = ""
	if err != nil {
		r.lastError = err.Error()
	}
}

func (r *awbReadyRuntime) recordHold(hold *rateLimitHold) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hold = ""
	if hold != nil {
		r.hold = fmt.Sprintf("%s %s at %.1f%% (max %.1f%%) until %s", hold.Harness, hold.Window,
			hold.Pct, hold.Threshold, hold.ResetsAt.Local().Format(time.RFC3339))
	}
}

func (r *awbReadyRuntime) snapshot() (lastPollAt time.Time, lastError, hold string) {
	if r == nil {
		return time.Time{}, "", ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastPollAt, r.lastError, r.hold
}

// awbReadyRegistry holds the workers this daemon started, keyed by process, so
// the status surface reports the configuration actually running rather than
// whatever the config file says now.
var awbReadyRegistry = struct {
	mu      sync.Mutex
	workers map[string]awbReadyWorker
}{}

func registerAWBReadyWorker(w awbReadyWorker) {
	awbReadyRegistry.mu.Lock()
	defer awbReadyRegistry.mu.Unlock()
	if awbReadyRegistry.workers == nil {
		awbReadyRegistry.workers = map[string]awbReadyWorker{}
	}
	awbReadyRegistry.workers[w.process] = w
}

// RegisterAWBReadyProcessesForTest registers every ready-polling process in
// cfg exactly as daemon startup does, without starting their poll loops, so a
// flow test can drive the `tclaude pickup` endpoints. Returns a restore func.
func RegisterAWBReadyProcessesForTest(cfg *config.Config) func() {
	awbReadyRegistry.mu.Lock()
	prev := awbReadyRegistry.workers
	awbReadyRegistry.workers = nil
	awbReadyRegistry.mu.Unlock()
	policy := cfg.ResolvedAWBProxy()
	for process, polling := range policy.ReadyPolling {
		registerAWBReadyWorker(newAWBReadyWorker(policy, process, polling))
	}
	return func() {
		awbReadyRegistry.mu.Lock()
		awbReadyRegistry.workers = prev
		awbReadyRegistry.mu.Unlock()
	}
}

// RecordAWBReadyPollForTest records a poll outcome for a registered process,
// as its poll loop would.
func RecordAWBReadyPollForTest(process string, err error) {
	registeredAWBReadyWorkers()[process].runtime.recordPoll(err)
}

// HoldAWBReadyProcessForTest occupies a registered process's busy slot as an
// in-progress poll would, and shortens how long a reset waits for it. Returns
// the release func.
func HoldAWBReadyProcessForTest(process string, resetWait time.Duration) func() {
	wk := registeredAWBReadyWorkers()[process]
	wk.busy <- struct{}{}
	prev := awbPickupResetWait
	awbPickupResetWait = resetWait
	return func() {
		awbPickupResetWait = prev
		<-wk.busy
	}
}

func registeredAWBReadyWorkers() map[string]awbReadyWorker {
	awbReadyRegistry.mu.Lock()
	defer awbReadyRegistry.mu.Unlock()
	out := make(map[string]awbReadyWorker, len(awbReadyRegistry.workers))
	for k, v := range awbReadyRegistry.workers {
		out[k] = v
	}
	return out
}

// handleAWBPickupList serves GET /v1/pickup: every ready-polling process,
// its dispatch row, and the live state of the dispatched issue and agent.
func handleAWBPickupList(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "inspect AWB pickup processes") {
		return
	}
	out, err := awbPickupStatus(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAWBPickupReset serves POST /v1/pickup/{process}/reset: it drops the
// process's dispatch row so the worker polls for a new ready issue. It does
// not touch the AWB issue or the agent — both stay as they are for the
// operator to release, reassign, or retire.
func handleAWBPickupReset(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "reset AWB pickup processes") {
		return
	}
	process := strings.ToLower(strings.TrimSpace(r.PathValue("process")))
	if process == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "process is required")
		return
	}
	setAuditTargetLabel(r, process)
	var req agent.AWBPickupResetRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid JSON body: "+err.Error())
		return
	}
	// Hold the worker's busy slot across the read and the delete: the worker
	// must not be part-way through claiming or spawning the issue being
	// released, nor replace it with another issue in between.
	if wk, ok := registeredAWBReadyWorkers()[process]; ok && wk.busy != nil {
		t := time.NewTimer(awbPickupResetWait)
		select {
		case wk.busy <- struct{}{}:
			t.Stop()
			defer func() { <-wk.busy }()
		case <-t.C:
			writeError(w, http.StatusConflict, "busy", fmt.Sprintf(
				"process %s is in the middle of a poll; retry the reset", process))
			return
		case <-r.Context().Done():
			t.Stop()
			return
		}
	}
	existing, err := db.GetAWBReadyDispatch(process)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if existing == nil {
		if _, known := registeredAWBReadyWorkers()[process]; !known {
			writeError(w, http.StatusNotFound, "not_found", "no AWB pickup process named "+process)
			return
		}
		writeJSON(w, http.StatusOK, agent.AWBPickupResetResponse{Process: process})
		return
	}
	if want := strings.TrimSpace(req.IssueID); want != "" && !strings.EqualFold(want, existing.IssueID) {
		writeError(w, http.StatusConflict, "conflict", fmt.Sprintf(
			"process %s now holds issue %s, not %s; nothing reset", process, existing.IssueID, want))
		return
	}
	// Delete by process AND issue, so the row removed is always the one the
	// response and audit row describe.
	reset, err := db.ClearAWBReadyDispatch(process, existing.IssueID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	resp := agent.AWBPickupResetResponse{Process: process, Reset: reset}
	if reset {
		resp.Dispatch = awbPickupDispatchJSON(existing)
		setAuditDetail(r, fmt.Sprintf("workspace=%s issue=%s phase=%s agent_id=%s",
			existing.Workspace, existing.IssueID, existing.Phase, existing.AgentID))
		slog.Info("awb ready polling: operator reset process", "process", process,
			"workspace", existing.Workspace, "issue", existing.IssueID, "phase", existing.Phase,
			"agent_id", existing.AgentID)
	}
	writeJSON(w, http.StatusOK, resp)
}

func awbPickupStatus(ctx context.Context) (agent.AWBPickupList, error) {
	workers := registeredAWBReadyWorkers()
	// One liveness snapshot for every agent in the listing. A failed probe
	// leaves it nil, which falls back to the stored session status.
	alive, _ := cachedLiveTmuxSessions()
	rows, err := db.ListAWBReadyDispatches()
	if err != nil {
		return agent.AWBPickupList{}, err
	}
	byProcess := make(map[string]db.AWBReadyDispatch, len(rows))
	for _, row := range rows {
		byProcess[row.Process] = row
	}
	names := make([]string, 0, len(workers)+len(rows))
	for name := range workers {
		names = append(names, name)
	}
	for name := range byProcess {
		if _, ok := workers[name]; !ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	out := agent.AWBPickupList{Processes: make([]agent.AWBPickupProcess, len(names))}
	var wg sync.WaitGroup
	for i, name := range names {
		wk, configured := workers[name]
		p := agent.AWBPickupProcess{Process: name, Configured: configured}
		if configured {
			p.Workspace = wk.workspace
			p.Labels = append([]string(nil), wk.config.Labels...)
			p.Group = wk.config.Group
			p.Interval = wk.interval.String()
			lastPoll, lastErr, hold := wk.runtime.snapshot()
			if !lastPoll.IsZero() {
				p.LastPollAt = &lastPoll
			}
			p.LastError, p.RateLimitHold = lastErr, hold
		}
		if row, ok := byProcess[name]; ok {
			if !configured {
				p.Workspace = row.Workspace
			}
			p.Dispatch = awbPickupDispatchJSON(&row)
			p.Dispatch.Agent = awbPickupAgentStatus(row.AgentID, alive)
			if configured {
				wg.Add(1)
				go func(d *agent.AWBPickupDispatch) {
					defer wg.Done()
					d.Issue, d.IssueError = wk.liveIssue(ctx, d.IssueID)
				}(p.Dispatch)
			}
		}
		out.Processes[i] = p
	}
	wg.Wait()
	for i := range out.Processes {
		p := &out.Processes[i]
		var monitored bool
		if wk, ok := workers[p.Process]; ok {
			monitored = wk.config.MonitorPR || wk.config.MonitorCommit
		}
		p.State, p.Hint = awbPickupState(*p, monitored)
	}
	return out, nil
}

func awbPickupDispatchJSON(row *db.AWBReadyDispatch) *agent.AWBPickupDispatch {
	return &agent.AWBPickupDispatch{IssueID: row.IssueID, Phase: row.Phase, AgentID: row.AgentID,
		LatestError: row.LatestError, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

// liveIssue reads the dispatched issue from AWB. It is deliberately not
// audited: it is a read the operator asked for, and a watch view refreshing
// every few seconds would otherwise bury the poller's own audit trail.
func (w awbReadyWorker) liveIssue(ctx context.Context, id string) (*agent.AWBPickupIssue, string) {
	ctx, cancel := context.WithTimeout(ctx, awbPickupIssueTimeout)
	defer cancel()
	var i awbIssue
	if _, f := w.session.exec(ctx, awbCall{Method: http.MethodGet, Path: "/api/issues/" + awbSegment(id)}, &i); f != nil {
		return nil, f.Msg
	}
	if f := w.session.enforceIssueWorkspace(&i); f != nil {
		return nil, f.Msg
	}
	return &agent.AWBPickupIssue{Status: i.Status, Title: i.Title, Assignees: i.Assignees,
		PullRequestURL: i.PullRequestURL, CommitHash: i.CommitHash,
		URL: strings.TrimRight(w.session.base, "/") + "/#/issues/" + id}, ""
}

func awbPickupAgentStatus(agentID string, aliveTmux map[string]struct{}) *agent.AWBPickupAgent {
	out := &agent.AWBPickupAgent{}
	if agentID == "" {
		return out
	}
	a, err := db.GetAgent(agentID)
	if err != nil || a == nil {
		if pending, pErr := db.GetPendingSpawnByAgentID(agentID); pErr == nil && pending != nil {
			out.PendingSpawn = true
		}
		return out
	}
	out.Exists = true
	out.Retired = !a.Active()
	out.ConvID = a.CurrentConvID
	out.Name = a.PendingName
	if a.CurrentConvID != "" {
		if row := agent.FreshConvRowResolved(a.CurrentConvID); row != nil {
			if title := agent.DisplayTitle(row); title != "" {
				out.Name = title
			}
		}
		if row, err := db.FindSessionByConvID(a.CurrentConvID); err == nil && row != nil {
			out.SessionStatus = row.Status
			// The stored status lags a pane that died without a final hook;
			// a vanished tmux session is exited whatever the row says.
			if aliveTmux != nil && row.TmuxSession != "" {
				if _, ok := aliveTmux[row.TmuxSession]; !ok {
					out.SessionStatus = session.StatusExited
				}
			}
		}
	}
	return out
}

// awbPickupState condenses a process's dispatch, issue, and agent into one
// state plus a hint explaining what, if anything, the operator should do.
func awbPickupState(p agent.AWBPickupProcess, monitored bool) (string, string) {
	d := p.Dispatch
	if d == nil {
		switch {
		case p.RateLimitHold != "":
			return agent.AWBPickupStateHeld, "usage ceiling: " + p.RateLimitHold
		case p.LastError != "":
			return agent.AWBPickupStateError, p.LastError
		}
		return agent.AWBPickupStatePolling, ""
	}
	if !p.Configured {
		return agent.AWBPickupStateOrphaned, "process is not running in this daemon; reset to clear the leftover dispatch"
	}
	if d.Issue != nil && d.Issue.Status == "closed" {
		return agent.AWBPickupStateReleasing, "issue closed; released once the agent settles"
	}
	a := d.Agent
	if d.Phase != "spawned" && (a == nil || (!a.Exists && !a.PendingSpawn)) {
		if msg := firstNonEmpty(d.LatestError, p.LastError); msg != "" {
			return agent.AWBPickupStateError, msg
		}
		return agent.AWBPickupStateStarting, ""
	}
	switch {
	case a == nil || (!a.Exists && !a.PendingSpawn):
		return agent.AWBPickupStateStuck, "agent no longer exists but the issue is still open"
	case a.PendingSpawn:
		return agent.AWBPickupStateStarting, "agent spawn pending"
	case a.Retired:
		return agent.AWBPickupStateStuck, "agent retired but the issue is still open"
	case a.SessionStatus == "" || a.SessionStatus == session.StatusExited:
		return agent.AWBPickupStateStuck, "agent session is not running but the issue is still open"
	case a.SessionStatus == session.StatusIdle:
		if monitored && d.Issue != nil && (d.Issue.PullRequestURL != "" || d.Issue.CommitHash != "") {
			return agent.AWBPickupStateAwaiting, "waiting for the recorded change to reach main"
		}
		return agent.AWBPickupStateAgentIdle, "agent is idle; the issue is still open"
	}
	if msg := firstNonEmpty(d.LatestError, d.IssueError); msg != "" {
		return agent.AWBPickupStateWorking, msg
	}
	return agent.AWBPickupStateWorking, ""
}
