package ccworkflows

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// JournalEventType is the kind of a journal.jsonl event. Only "started" and
// "result" have been observed; unknown types are preserved verbatim so a future
// CC event type does not silently vanish.
type JournalEventType string

const (
	EventStarted JournalEventType = "started"
	EventResult  JournalEventType = "result"
)

// JournalEvent is one line of an in-flight run's journal.jsonl. The journal is
// the only live on-disk signal: it carries no timestamp, phase, label, or token
// data — just the agent lifecycle keyed by AgentID (Key is the agent
// cache/dedup key, not a label). Phase/label are recovered separately via
// static script-spawn correlation.
type JournalEvent struct {
	Type    JournalEventType `json:"type"`
	Key     string           `json:"key"`
	AgentID string           `json:"agentId"`
	Result  string           `json:"result,omitempty"`
}

// ParseJournal reads an append-only journal.jsonl. It tolerates an empty file
// and a partially-written final line (the journal is being appended to live):
// unparseable lines are skipped rather than failing the parse. A `result` line
// may be very large, so it reads line-by-line without a fixed token cap.
func ParseJournal(r io.Reader) ([]JournalEvent, error) {
	br := bufio.NewReader(r)
	var events []JournalEvent
	for {
		line, readErr := br.ReadString('\n')
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			var ev JournalEvent
			if json.Unmarshal([]byte(trimmed), &ev) == nil {
				events = append(events, ev)
			}
			// A line that fails to parse is treated as a truncated/in-flight
			// tail and skipped — this is the documented tolerance.
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return events, nil
			}
			return events, readErr
		}
	}
}

// buildLiveRunState reconstructs an in-flight run's typed state from its journal
// events plus the statically-parsed script (meta phases + spawn order). The
// journal's `started` order matches the script's lexical agent-call order, so we
// zip them to recover each agent's label and phase. Confidence is high only for
// a static script whose call count matches the journal; dynamic fan-out
// (loops/.map/pipeline) or a count mismatch marks labels best-effort.
func buildLiveRunState(runID string, events []JournalEvent, meta ScriptMeta, spawn []AgentCall, dynamic bool) *RunState {
	var order []string
	seen := map[string]bool{}
	done := map[string]string{}
	for _, ev := range events {
		if !seen[ev.AgentID] {
			seen[ev.AgentID] = true
			order = append(order, ev.AgentID)
		}
		if ev.Type == EventResult {
			done[ev.AgentID] = ev.Result
		}
	}

	phasesByIndex := map[int]*Phase{}
	phaseIndexByTitle := map[string]int{}
	for i, mp := range meta.Phases {
		idx := i + 1
		phasesByIndex[idx] = &Phase{Index: idx, Title: mp.Title, Detail: mp.Detail}
		phaseIndexByTitle[mp.Title] = idx
	}

	confident := !dynamic && len(spawn) == len(order)

	agents := make([]Agent, 0, len(order))
	for i, id := range order {
		ag := Agent{ID: id, State: AgentRunning, LabelConfident: confident}
		if res, ok := done[id]; ok {
			ag.State = AgentDone
			ag.Result = res
		}
		if i < len(spawn) {
			ag.Label = spawn[i].Label
			ag.PhaseTitle = spawn[i].Phase
			if idx, ok := phaseIndexByTitle[spawn[i].Phase]; ok {
				ag.PhaseIndex = idx
			}
		}
		agents = append(agents, ag)
	}

	return &RunState{
		RunID:        runID,
		WorkflowName: meta.Name,
		Summary:      meta.Description,
		Status:       RunRunning, // in-flight by construction: no completed JSON exists
		Source:       "journal",
		AgentCount:   len(agents),
		Phases:       finalizePhases(phasesByIndex, agents),
		Agents:       agents,
	}
}
