package ccworkflows

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// RunStatus is the overall status of a workflow run.
type RunStatus string

const (
	RunRunning   RunStatus = "running"
	RunCompleted RunStatus = "completed"
	RunFailed    RunStatus = "failed"
	RunUnknown   RunStatus = "unknown"
)

// AgentState is a single agent's state within a run.
type AgentState string

const (
	AgentQueued  AgentState = "queued"
	AgentRunning AgentState = "running"
	AgentDone    AgentState = "done"
	AgentFailed  AgentState = "failed"
)

// Phase is one phase of a run: a meta phase joined with the live/derived status
// aggregated from the agents that ran under it.
type Phase struct {
	Index  int        `json:"index"`
	Title  string     `json:"title"`
	Detail string     `json:"detail,omitempty"`
	Status AgentState `json:"status,omitempty"`
}

// Agent is a single spawned agent within a run.
type Agent struct {
	ID         string     `json:"id"`
	Label      string     `json:"label,omitempty"`
	PhaseIndex int        `json:"phaseIndex,omitempty"`
	PhaseTitle string     `json:"phaseTitle,omitempty"`
	AgentType  string     `json:"agentType,omitempty"`
	Model      string     `json:"model,omitempty"`
	State      AgentState `json:"state"`
	Tokens     int        `json:"tokens,omitempty"`
	ToolCalls  int        `json:"toolCalls,omitempty"`
	LastTool   string     `json:"lastTool,omitempty"`
	StartedAt  int64      `json:"startedAt,omitempty"` // epoch ms, 0 if unknown
	DurationMs int64      `json:"durationMs,omitempty"`
	// Result is the agent's output. Its fidelity depends on Source: for a
	// completed-json run it is the truncated `resultPreview`; for a journal
	// (in-flight) run it is the full result text from the journal. Treat it as
	// a preview for display, not as the canonical full result.
	Result string `json:"result,omitempty"`
	// LabelConfident is false when the label/phase was inferred from static
	// script correlation for an in-flight run whose script fans out
	// dynamically (loops / .map / pipeline) — i.e. the mapping is best-effort.
	// It is always true for completed runs (the record is authoritative).
	LabelConfident bool `json:"labelConfident"`
}

// RunState is the typed, source-agnostic view of a workflow run that the CLI,
// web tab, and live-progress slices all consume.
type RunState struct {
	RunID        string    `json:"runId"`
	WorkflowName string    `json:"workflowName,omitempty"`
	Summary      string    `json:"summary,omitempty"`
	Status       RunStatus `json:"status"`
	// Source records where this view came from: "completed-json" (authoritative)
	// or "journal" (in-flight, reconstructed from journal + script correlation).
	Source       string   `json:"source"`
	StartTimeMs  int64    `json:"startTimeMs,omitempty"`
	DurationMs   int64    `json:"durationMs,omitempty"`
	AgentCount   int      `json:"agentCount"`
	TotalTokens  int      `json:"totalTokens,omitempty"`
	DefaultModel string   `json:"defaultModel,omitempty"`
	Phases       []Phase  `json:"phases"`
	Agents       []Agent  `json:"agents"`
	ScriptPath   string   `json:"scriptPath,omitempty"`
	Logs         []string `json:"logs,omitempty"`
}

// RunRef is a lightweight pointer to a run discovered by enumeration, with just
// enough metadata to list runs and join them to tclaude's session records — and
// to decide whether LoadRun will read the completed JSON or the live journal.
type RunRef struct {
	RunID string `json:"runId"`
	// SessionDir is the absolute session transcript dir the run lives under.
	SessionDir string `json:"sessionDir"`
	// SessionID is the session UUID (base of SessionDir) — the join key to
	// tclaude's existing session/conv records.
	SessionID string `json:"sessionId"`
	// ProjectDir is the encoded project directory name (parent of SessionID).
	ProjectDir string `json:"projectDir"`
	// WorkflowName is the run's workflow name (from the completed JSON when
	// present, else parsed from the script snapshot filename).
	WorkflowName string `json:"workflowName,omitempty"`
	// ScriptPath is the resolved script snapshot, if present (live or done).
	ScriptPath string `json:"scriptPath,omitempty"`
	// Status is the cheap status: from the completed JSON if present, else
	// running (an in-flight run has a journal dir but no completed JSON yet).
	Status RunStatus `json:"status"`
	// StartTimeMs is the run start (epoch ms). Authoritative from the completed
	// JSON; for in-flight runs it is a best-effort estimate from the script
	// snapshot's mtime (written at launch). 0 if unknown.
	StartTimeMs int64 `json:"startTimeMs,omitempty"`
	// HasCompletedJSON is true when a <runId>.json record exists.
	HasCompletedJSON bool `json:"hasCompletedJson"`
}

// completedRun mirrors the on-disk <runId>.json record. Unknown fields are
// ignored; absent optional fields stay zero.
type completedRun struct {
	RunID          string          `json:"runId"`
	Timestamp      string          `json:"timestamp"`
	TaskID         string          `json:"taskId"`
	Script         string          `json:"script"`
	ScriptPath     string          `json:"scriptPath"`
	AgentCount     int             `json:"agentCount"`
	Logs           []string        `json:"logs"`
	DurationMs     int64           `json:"durationMs"`
	Summary        string          `json:"summary"`
	WorkflowName   string          `json:"workflowName"`
	Status         string          `json:"status"`
	StartTime      int64           `json:"startTime"`
	Phases         []MetaPhase     `json:"phases"`
	DefaultModel   string          `json:"defaultModel"`
	Progress       []progressEntry `json:"workflowProgress"`
	TotalTokens    int             `json:"totalTokens"`
	TotalToolCalls int             `json:"totalToolCalls"`
}

// progressEntry is one heterogeneous element of workflowProgress[]: either a
// phase marker (type=workflow_phase) or an agent record (type=workflow_agent).
type progressEntry struct {
	Type          string `json:"type"`
	Index         int    `json:"index"`
	Title         string `json:"title"`
	Label         string `json:"label"`
	PhaseIndex    int    `json:"phaseIndex"`
	PhaseTitle    string `json:"phaseTitle"`
	AgentID       string `json:"agentId"`
	AgentType     string `json:"agentType"`
	Model         string `json:"model"`
	State         string `json:"state"`
	StartedAt     int64  `json:"startedAt"`
	DurationMs    int64  `json:"durationMs"`
	LastToolName  string `json:"lastToolName"`
	Tokens        int    `json:"tokens"`
	ToolCalls     int    `json:"toolCalls"`
	ResultPreview string `json:"resultPreview"`
}

// ParseCompletedRun parses a <runId>.json completed-run record into RunState.
// This record is authoritative and self-contained — every agent's label, phase,
// state, and token usage is present, so no script correlation is needed.
func ParseCompletedRun(data []byte) (*RunState, error) {
	var cr completedRun
	if err := json.Unmarshal(data, &cr); err != nil {
		return nil, fmt.Errorf("parsing completed run: %w", err)
	}
	rs := &RunState{
		RunID:        cr.RunID,
		WorkflowName: cr.WorkflowName,
		Summary:      cr.Summary,
		Status:       normalizeRunStatus(cr.Status),
		Source:       "completed-json",
		StartTimeMs:  cr.StartTime,
		DurationMs:   cr.DurationMs,
		AgentCount:   cr.AgentCount,
		TotalTokens:  cr.TotalTokens,
		DefaultModel: cr.DefaultModel,
		ScriptPath:   cr.ScriptPath,
		Logs:         cr.Logs,
	}

	// Phases: prefer the workflow_phase markers (they carry index+title); fill
	// detail from meta phases by title; fall back to meta phases if no markers.
	phasesByIndex := map[int]*Phase{}
	for _, e := range cr.Progress {
		if e.Type == "workflow_phase" {
			p := &Phase{Index: e.Index, Title: e.Title}
			phasesByIndex[e.Index] = p
		}
	}
	if len(phasesByIndex) == 0 {
		for i, mp := range cr.Phases {
			phasesByIndex[i+1] = &Phase{Index: i + 1, Title: mp.Title, Detail: mp.Detail}
		}
	} else {
		for _, p := range phasesByIndex {
			if d := metaDetailFor(cr.Phases, p.Title); d != "" {
				p.Detail = d
			}
		}
	}

	for _, e := range cr.Progress {
		if e.Type != "workflow_agent" {
			continue
		}
		ag := Agent{
			ID:             e.AgentID,
			Label:          e.Label,
			PhaseIndex:     e.PhaseIndex,
			PhaseTitle:     e.PhaseTitle,
			AgentType:      e.AgentType,
			Model:          e.Model,
			State:          normalizeAgentState(e.State),
			Tokens:         e.Tokens,
			ToolCalls:      e.ToolCalls,
			LastTool:       e.LastToolName,
			StartedAt:      e.StartedAt,
			DurationMs:     e.DurationMs,
			Result:         e.ResultPreview,
			LabelConfident: true,
		}
		rs.Agents = append(rs.Agents, ag)
	}

	rs.Phases = finalizePhases(phasesByIndex, rs.Agents)
	if rs.AgentCount == 0 {
		rs.AgentCount = len(rs.Agents)
	}
	return rs, nil
}

// LoadRun returns the typed state of a run. The completed-run JSON is preferred
// whenever present (authoritative); otherwise the run is in-flight and is
// reconstructed from its journal plus static script-spawn correlation.
func LoadRun(sessionDir, runID string) (*RunState, error) {
	if data, err := os.ReadFile(completedRunPath(sessionDir, runID)); err == nil {
		return ParseCompletedRun(data)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading completed run %s: %w", runID, err)
	}

	journalPath := runJournalPath(sessionDir, runID)
	jf, err := os.Open(journalPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("run %s not found (no completed JSON, no journal)", runID)
		}
		return nil, fmt.Errorf("opening journal for %s: %w", runID, err)
	}
	defer func() { _ = jf.Close() }()

	events, err := ParseJournal(jf)
	if err != nil {
		return nil, fmt.Errorf("parsing journal for %s: %w", runID, err)
	}

	var meta ScriptMeta
	var spawn []AgentCall
	dynamic := false
	scriptPath, hasScript := findRunScript(sessionDir, runID)
	if hasScript {
		if src, err := os.ReadFile(scriptPath); err == nil {
			meta, _ = ParseScriptMeta(string(src))
			spawn = ParseSpawnOrder(string(src))
			dynamic = ScriptHasDynamicSpawns(string(src))
		}
	}
	rs := buildLiveRunState(runID, events, meta, spawn, dynamic)
	if hasScript {
		rs.ScriptPath = scriptPath
	}
	return rs, nil
}

// ListRuns enumerates every workflow run under projectsRoot (machine-wide),
// across all CC sessions — the cross-agent "who is using workflows" view. Each
// ref carries the session UUID + project dir so callers can join to tclaude's
// existing session/conv records. Runs are returned newest-session-path first is
// not guaranteed; results are sorted by (ProjectDir, SessionID, RunID) for
// stability.
func ListRuns(projectsRoot string) ([]RunRef, error) {
	projEntries, err := os.ReadDir(projectsRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading projects root: %w", err)
	}

	var refs []RunRef
	for _, pe := range projEntries {
		if !pe.IsDir() {
			continue
		}
		projectDir := pe.Name()
		projectPath := filepath.Join(projectsRoot, projectDir)
		sessEntries, err := os.ReadDir(projectPath)
		if err != nil {
			continue // unreadable project dir — skip, don't fail the whole walk
		}
		for _, se := range sessEntries {
			if !se.IsDir() {
				continue
			}
			sessionDir := filepath.Join(projectPath, se.Name())
			refs = append(refs, runsInSession(sessionDir, projectDir, se.Name())...)
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].ProjectDir != refs[j].ProjectDir {
			return refs[i].ProjectDir < refs[j].ProjectDir
		}
		if refs[i].SessionID != refs[j].SessionID {
			return refs[i].SessionID < refs[j].SessionID
		}
		return refs[i].RunID < refs[j].RunID
	})
	return refs, nil
}

// runsInSession discovers the runs in a single session dir: the union of
// completed-run JSONs (workflows/wf_*.json) and in-flight journal dirs
// (subagents/workflows/wf_*/).
func runsInSession(sessionDir, projectDir, sessionID string) []RunRef {
	byID := map[string]*RunRef{}
	ref := func(runID string) *RunRef {
		r, ok := byID[runID]
		if !ok {
			r = &RunRef{RunID: runID, SessionDir: sessionDir, SessionID: sessionID, ProjectDir: projectDir, Status: RunUnknown}
			byID[runID] = r
		}
		return r
	}

	// Completed runs.
	if matches, _ := filepath.Glob(filepath.Join(sessionWorkflowsDir(sessionDir), runIDPrefix+"*.json")); len(matches) > 0 {
		for _, m := range matches {
			runID := strings.TrimSuffix(filepath.Base(m), ".json")
			r := ref(runID)
			r.HasCompletedJSON = true
			head := readCompletedHead(m)
			r.Status = head.status
			r.WorkflowName = head.workflowName
			r.StartTimeMs = head.startTime
		}
	}

	// In-flight (or completed) runs that have a journal dir.
	if dirs, _ := filepath.Glob(filepath.Join(sessionDir, "subagents", "workflows", runIDPrefix+"*")); len(dirs) > 0 {
		for _, d := range dirs {
			info, err := os.Stat(d)
			if err != nil || !info.IsDir() {
				continue
			}
			r := ref(filepath.Base(d))
			if !r.HasCompletedJSON {
				r.Status = RunRunning // journal present, no completed JSON => in-flight
			}
		}
	}

	out := make([]RunRef, 0, len(byID))
	for runID, r := range byID {
		if scriptPath, ok := findRunScript(sessionDir, runID); ok {
			r.ScriptPath = scriptPath
			if r.WorkflowName == "" {
				r.WorkflowName = workflowNameFromScriptPath(scriptPath, runID)
			}
			if r.StartTimeMs == 0 {
				// Best-effort: the script snapshot is written at launch.
				if info, err := os.Stat(scriptPath); err == nil {
					r.StartTimeMs = info.ModTime().UnixMilli()
				}
			}
		}
		out = append(out, *r)
	}
	return out
}

// completedHead is the cheap subset of a completed-run JSON read during
// enumeration (avoids parsing the full workflowProgress for every run).
type completedHead struct {
	status       RunStatus
	workflowName string
	startTime    int64
}

func readCompletedHead(path string) completedHead {
	data, err := os.ReadFile(path)
	if err != nil {
		return completedHead{status: RunUnknown}
	}
	var head struct {
		Status       string `json:"status"`
		WorkflowName string `json:"workflowName"`
		StartTime    int64  `json:"startTime"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return completedHead{status: RunUnknown}
	}
	return completedHead{
		status:       normalizeRunStatus(head.Status),
		workflowName: head.WorkflowName,
		startTime:    head.StartTime,
	}
}

// workflowNameFromScriptPath recovers the workflow name from a script snapshot
// path named "<name>-<runId>.js".
func workflowNameFromScriptPath(scriptPath, runID string) string {
	base := strings.TrimSuffix(filepath.Base(scriptPath), ".js")
	return strings.TrimSuffix(base, "-"+runID)
}

func normalizeRunStatus(s string) RunStatus {
	switch strings.ToLower(s) {
	case "completed", "complete", "done", "success", "succeeded":
		return RunCompleted
	case "failed", "error", "errored":
		return RunFailed
	case "running", "in_progress", "in-progress", "started":
		return RunRunning
	case "":
		return RunUnknown
	default:
		return RunStatus(strings.ToLower(s))
	}
}

func normalizeAgentState(s string) AgentState {
	switch strings.ToLower(s) {
	case "done", "completed", "complete", "success", "succeeded":
		return AgentDone
	case "failed", "error", "errored":
		return AgentFailed
	case "running", "in_progress", "in-progress", "started", "active":
		return AgentRunning
	case "queued", "pending", "waiting":
		return AgentQueued
	case "":
		return AgentRunning
	default:
		return AgentState(strings.ToLower(s))
	}
}

func metaDetailFor(phases []MetaPhase, title string) string {
	for _, p := range phases {
		if p.Title == title {
			return p.Detail
		}
	}
	return ""
}

// finalizePhases sorts phases by index and aggregates each phase's status from
// its agents: failed if any failed, else running if any running/queued, else
// done if it had agents, else its zero status (e.g. a phase with no agents yet).
func finalizePhases(byIndex map[int]*Phase, agents []Agent) []Phase {
	for _, ag := range agents {
		p, ok := byIndex[ag.PhaseIndex]
		if !ok {
			continue
		}
		p.Status = mergePhaseStatus(p.Status, ag.State)
	}
	out := make([]Phase, 0, len(byIndex))
	for _, p := range byIndex {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}

// mergePhaseStatus folds an agent state into a phase's running aggregate.
// Precedence: failed > running > queued > done.
func mergePhaseStatus(cur, ag AgentState) AgentState {
	rank := func(s AgentState) int {
		switch s {
		case AgentFailed:
			return 4
		case AgentRunning:
			return 3
		case AgentQueued:
			return 2
		case AgentDone:
			return 1
		default:
			return 0
		}
	}
	if rank(ag) > rank(cur) {
		return ag
	}
	return cur
}
