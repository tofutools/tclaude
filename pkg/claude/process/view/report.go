// Package view derives the stable, read-only payload used to inspect a
// process run. It intentionally projects evidence instead of returning raw
// events: performer prompts, command payloads, and evidence bodies are not a
// viewer API surface.
package view

import (
	"cmp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

const SchemaVersion = 1

type Report struct {
	SchemaVersion  int                   `json:"schemaVersion"`
	Nodes          map[string]NodeReport `json:"nodes"`
	TraversedEdges []TraversedEdge       `json:"traversedEdges"`
}

type NodeReport struct {
	Summary      NodeSummary      `json:"summary"`
	Timeline     []TimelineEntry  `json:"timeline"`
	Obligation   *Obligation      `json:"obligation,omitempty"`
	Blocked      *Blocked         `json:"blocked,omitempty"`
	Conversation *ConversationRef `json:"conversation,omitempty"`
}

type NodeSummary struct {
	AttemptCount    int `json:"attemptCount"`
	RetryCount      int `json:"retryCount"`
	FailureCount    int `json:"failureCount"`
	CompletedStages int `json:"completedStages"`
	TotalStages     int `json:"totalStages"`
}

// TimelineEntry is a deliberately narrow evidence projection. Do not add raw
// state.Event or command fields here: those may contain prompts and payloads.
type TimelineEntry struct {
	Seq         int64              `json:"seq"`
	At          time.Time          `json:"at,omitzero"`
	Kind        evidence.EntryKind `json:"kind"`
	Event       state.EventType    `json:"event,omitempty"`
	Attempt     int                `json:"attempt,omitempty"`
	Actor       state.ActorRef     `json:"actor,omitempty"`
	Outcome     string             `json:"outcome,omitempty"`
	Verdict     string             `json:"verdict,omitempty"`
	EvidenceRef string             `json:"evidenceRef,omitempty"`
}

type Obligation struct {
	ID           string           `json:"id"`
	Kind         state.WaitKind   `json:"kind"`
	Assignee     string           `json:"assignee"`
	Status       state.WaitStatus `json:"status"`
	Attempt      int              `json:"attempt,omitempty"`
	WaitingSince time.Time        `json:"waitingSince,omitzero"`
	DueAt        time.Time        `json:"dueAt,omitzero"`
	Summary      string           `json:"summary,omitempty"`
	Actions      []string         `json:"actions,omitempty"`
	Contact      *Contact         `json:"contact,omitempty"`
}

type Blocked struct {
	Owner                string    `json:"owner,omitempty"`
	Reason               string    `json:"reason,omitempty"`
	BlockedAt            time.Time `json:"blockedAt,omitzero"`
	BlockedAtUnavailable bool      `json:"blockedAtUnavailable,omitempty"`
	Attempt              int       `json:"attempt,omitempty"`
	Contact              *Contact  `json:"contact,omitempty"`
}

type Contact struct {
	Cadence          string    `json:"cadence,omitempty"`
	LastContactAt    time.Time `json:"lastContactAt,omitzero"`
	NextContactAt    time.Time `json:"nextContactAt,omitzero"`
	BudgetUsed       int       `json:"budgetUsed"`
	BudgetMax        int       `json:"budgetMax"`
	EscalationTarget string    `json:"escalationTarget,omitempty"`
	EscalatedAt      time.Time `json:"escalatedAt,omitzero"`
	Paused           bool      `json:"paused"`
	PauseReason      string    `json:"pauseReason,omitempty"`
}

type ConversationRef struct {
	AgentID string `json:"agentId"`
}

type TraversedEdge struct {
	From    string    `json:"from"`
	Outcome string    `json:"outcome"`
	To      string    `json:"to"`
	Count   int       `json:"count"`
	LastAt  time.Time `json:"lastAt,omitzero"`
}

func NewReport() Report {
	return Report{
		SchemaVersion:  SchemaVersion,
		Nodes:          map[string]NodeReport{},
		TraversedEdges: []TraversedEdge{},
	}
}

// Build derives a viewer report without mutating snapshot or any persisted
// data. tmpl must be the exact template verified for snapshot.Run.TemplateRef;
// callers pass nil when that identity cannot be established.
func Build(snapshot store.Snapshot, tmpl *model.Template) Report {
	report := NewReport()
	if snapshot.State == nil {
		return report
	}

	logs := logsByNode(snapshot.NodeLogs)
	nodeIDs := make([]string, 0, len(snapshot.State.Nodes)+len(logs))
	seen := make(map[string]struct{}, len(snapshot.State.Nodes)+len(logs))
	for nodeID := range snapshot.State.Nodes {
		seen[nodeID] = struct{}{}
		nodeIDs = append(nodeIDs, nodeID)
	}
	for nodeID := range logs {
		if nodeID == "" {
			continue
		}
		if _, ok := seen[nodeID]; !ok {
			nodeIDs = append(nodeIDs, nodeID)
		}
	}
	slices.Sort(nodeIDs)

	for _, nodeID := range nodeIDs {
		node := snapshot.State.Nodes[nodeID]
		report.Nodes[nodeID] = NodeReport{
			Summary:      summarizeNode(nodeID, node, snapshot.State, logs),
			Timeline:     projectTimeline(nodeID, logs[nodeID]),
			Obligation:   latestObligation(snapshot.State, nodeID),
			Blocked:      blockedState(snapshot.State, nodeID, node),
			Conversation: conversationRef(snapshot.State, nodeID, node),
		}
	}
	report.TraversedEdges = traversedEdges(tmpl, snapshot.NodeLogs)
	return report
}

func logsByNode(nodeLogs []evidence.NodeLog) map[string][]evidence.LogEntry {
	out := make(map[string][]evidence.LogEntry, len(nodeLogs))
	for _, log := range nodeLogs {
		out[log.NodeID] = append(out[log.NodeID], log.Entries...)
	}
	return out
}

func summarizeNode(nodeID string, node state.NodeState, st *state.State, logs map[string][]evidence.LogEntry) NodeSummary {
	owned := []string{nodeID}
	if len(node.Children) > 0 {
		owned = append([]string(nil), node.Children...)
	}
	summary := NodeSummary{TotalStages: len(node.Children)}
	for _, ownedID := range owned {
		starts := 0
		for _, entry := range logs[ownedID] {
			if entry.Event == nil || entry.Event.NodeID != ownedID {
				continue
			}
			switch entry.Event.Type {
			case state.EventNodeAttemptStarted:
				starts++
				summary.AttemptCount++
			case state.EventNodeAttemptSettled:
				if state.IsFailOutcome(entry.Event.Outcome) {
					summary.FailureCount++
				}
			}
		}
		if starts > 1 {
			summary.RetryCount += starts - 1
		}
	}
	for _, childID := range node.Children {
		child, ok := st.Nodes[childID]
		if ok && (child.Status == state.NodeStatusCompleted || child.Status == state.NodeStatusSkipped) {
			summary.CompletedStages++
		}
	}
	return summary
}

func projectTimeline(nodeID string, entries []evidence.LogEntry) []TimelineEntry {
	timeline := make([]TimelineEntry, 0, len(entries))
	for _, entry := range entries {
		item := TimelineEntry{Seq: entry.Seq, At: entry.At, Kind: entry.Kind, EvidenceRef: strings.TrimSpace(entry.EvidenceRef)}
		if event := entry.Event; event != nil {
			if event.NodeID != "" && event.NodeID != nodeID {
				continue
			}
			item.Event = event.Type
			item.Attempt = event.Attempt
			item.Actor = event.Actor
			item.Outcome = event.Outcome
			if item.EvidenceRef == "" {
				item.EvidenceRef = strings.TrimSpace(event.EvidenceRef)
			}
			if event.Decision != nil {
				item.Verdict = event.Decision.Verdict
				if item.Actor == "" {
					item.Actor = event.Decision.Actor
				}
				if item.EvidenceRef == "" {
					item.EvidenceRef = strings.TrimSpace(event.Decision.EvidenceRef)
				}
			}
			if event.Resolution != nil {
				item.Verdict = string(event.Resolution.Decision)
				if item.Actor == "" {
					item.Actor = event.Resolution.Actor
				}
				if item.EvidenceRef == "" {
					item.EvidenceRef = strings.TrimSpace(event.Resolution.EvidenceRef)
				}
			}
		}
		timeline = append(timeline, item)
	}
	slices.SortStableFunc(timeline, func(a, b TimelineEntry) int {
		return cmp.Compare(a.Seq, b.Seq)
	})
	return timeline
}

func latestObligation(st *state.State, nodeID string) *Obligation {
	var selected *state.ObligationRecord
	for _, obligation := range st.Obligations {
		if obligation.NodeID != nodeID {
			continue
		}
		candidate := obligation
		if selected == nil || obligationLater(candidate, *selected) {
			selected = &candidate
		}
	}
	if selected == nil {
		return nil
	}
	result := &Obligation{
		ID:           selected.ID,
		Kind:         selected.Kind,
		Assignee:     selected.Assignee,
		Status:       selected.Status,
		Attempt:      selected.Attempt,
		WaitingSince: selected.CreatedAt,
		DueAt:        selected.DueAt,
		Summary:      selected.Summary,
		Actions:      append([]string(nil), selected.AvailableActions...),
	}
	if contact, ok := st.Contacts[selected.CommandID]; ok {
		result.Contact = projectContact(contact)
	}
	return result
}

func obligationLater(left, right state.ObligationRecord) bool {
	leftPending := left.Status == state.WaitStatusPending
	rightPending := right.Status == state.WaitStatusPending
	if leftPending != rightPending {
		return leftPending
	}
	if left.Attempt != right.Attempt {
		return left.Attempt > right.Attempt
	}
	if !left.CreatedAt.Equal(right.CreatedAt) {
		return left.CreatedAt.After(right.CreatedAt)
	}
	return left.ID > right.ID
}

func blockedState(st *state.State, nodeID string, node state.NodeState) *Blocked {
	if node.Status != state.NodeStatusBlocked && node.BlockedReason == "" && node.BlockedOwner == "" && node.BlockedAttempt == 0 && node.BlockResolution == nil {
		return nil
	}
	attempt := node.BlockedAttempt
	if attempt == 0 {
		attempt = node.Attempt
	}
	result := &Blocked{
		Owner:                node.BlockedOwner,
		Reason:               node.BlockedReason,
		BlockedAt:            node.BlockedAt,
		BlockedAtUnavailable: node.BlockedAtUnavailable,
		Attempt:              attempt,
	}
	if contact, ok := blockedContact(st, nodeID, attempt); ok {
		result.Contact = projectContact(contact)
	}
	return result
}

func blockedContact(st *state.State, nodeID string, attempt int) (state.ContactState, bool) {
	commandIDs := make([]string, 0, len(st.Contacts))
	for commandID := range st.Contacts {
		commandIDs = append(commandIDs, commandID)
	}
	slices.Sort(commandIDs)
	for _, commandID := range commandIDs {
		command, ok := st.OutstandingCommands[commandID]
		serviceable := command.Status == state.CommandStatusIssued || command.Status == state.CommandStatusObserved
		if ok && serviceable && command.Kind == state.CommandKindBlockNode && command.NodeID == nodeID && command.Attempt == attempt {
			return st.Contacts[commandID], true
		}
	}
	return state.ContactState{}, false
}

func projectContact(contact state.ContactState) *Contact {
	return &Contact{
		Cadence:          contact.Cadence,
		LastContactAt:    contact.LastContactedAt,
		NextContactAt:    contact.NextContactAt,
		BudgetUsed:       contact.Used,
		BudgetMax:        contact.Budget,
		EscalationTarget: contact.EscalationTarget,
		EscalatedAt:      contact.EscalatedAt,
		Paused:           contact.Paused,
		PauseReason:      contact.PauseReason,
	}
}

func conversationRef(st *state.State, nodeID string, node state.NodeState) *ConversationRef {
	owned := map[string]struct{}{nodeID: {}}
	for _, childID := range node.Children {
		owned[childID] = struct{}{}
	}
	var selected *state.OutstandingCommand
	for _, command := range st.OutstandingCommands {
		if _, ok := owned[command.NodeID]; !ok || stableAgentID(command.ExternalRef) == "" {
			continue
		}
		candidate := command
		if selected == nil || commandLater(candidate, *selected) {
			selected = &candidate
		}
	}
	if selected == nil {
		return nil
	}
	return &ConversationRef{AgentID: stableAgentID(selected.ExternalRef)}
}

func commandLater(left, right state.OutstandingCommand) bool {
	if left.Attempt != right.Attempt {
		return left.Attempt > right.Attempt
	}
	if !left.CreatedAt.Equal(right.CreatedAt) {
		return left.CreatedAt.After(right.CreatedAt)
	}
	return left.ID > right.ID
}

func stableAgentID(externalRef string) string {
	ref := strings.TrimSpace(externalRef)
	ref = strings.TrimPrefix(ref, "agent:")
	if !strings.HasPrefix(ref, "agt_") || !state.ValidateActorRef(state.ActorRef("agent:"+ref)) {
		return ""
	}
	return ref
}

func traversedEdges(tmpl *model.Template, nodeLogs []evidence.NodeLog) []TraversedEdge {
	if tmpl == nil {
		return []TraversedEdge{}
	}
	type edgeKey struct{ from, outcome, to string }
	declared := make(map[[2]string]string)
	for _, edge := range model.NormalizeEdges(tmpl) {
		if edge.From != "" {
			declared[[2]string{edge.From, edge.Outcome}] = edge.To
		}
	}
	counts := make(map[edgeKey]TraversedEdge)
	for _, log := range nodeLogs {
		for _, entry := range log.Entries {
			event := entry.Event
			if event == nil || event.NodeID == "" || event.NodeID != log.NodeID {
				continue
			}
			outcome := ""
			switch event.Type {
			case state.EventNodeAttemptSettled:
				outcome = event.Outcome
			case state.EventDecisionRecorded:
				outcome = event.ChosenEdge
			default:
				continue
			}
			outcome = strings.TrimSpace(outcome)
			to, ok := declared[[2]string{event.NodeID, outcome}]
			if !ok || outcome == "" || to == "" {
				continue
			}
			key := edgeKey{from: event.NodeID, outcome: outcome, to: to}
			traversed := counts[key]
			traversed.From, traversed.Outcome, traversed.To = key.from, key.outcome, key.to
			traversed.Count++
			if entry.At.After(traversed.LastAt) {
				traversed.LastAt = entry.At
			}
			counts[key] = traversed
		}
	}
	out := make([]TraversedEdge, 0, len(counts))
	for _, edge := range counts {
		out = append(out, edge)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		if out[i].Outcome != out[j].Outcome {
			return out[i].Outcome < out[j].Outcome
		}
		return out[i].To < out[j].To
	})
	return out
}
