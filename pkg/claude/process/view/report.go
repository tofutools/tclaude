// Package view derives the explicit, read-only DTO returned by the process
// viewer endpoint. Raw templates, state, events, prompts, commands, evidence
// text, and verification messages are intentionally not API types here.
package view

import (
	"cmp"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/plan"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	processverify "github.com/tofutools/tclaude/pkg/claude/process/verify"
)

const SchemaVersion = 1

var (
	safeIDPattern          = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	safeCodePattern        = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	safeTemplateRefPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*@sha256:[0-9a-f]{64}$`)
	safeArtifactRefPattern = regexp.MustCompile(`^artifact:sha256:[0-9a-f]{64}$`)
	stableAgentIDPattern   = regexp.MustCompile(`^agt_[0-9a-f]{32}$`)
)

// Envelope is the complete safe viewer contract. It must never grow fields
// whose types are model.Template, state.State, state.Event, or json.RawMessage.
type Envelope struct {
	Run          Run          `json:"run"`
	Graph        *Graph       `json:"graph"`
	Verification Verification `json:"verification"`
	Report       Report       `json:"report"`
}

type Run struct {
	ID              string          `json:"id"`
	TemplateRef     string          `json:"templateRef,omitempty"`
	StoredStatus    state.RunStatus `json:"storedStatus,omitempty"`
	EffectiveStatus state.RunStatus `json:"effectiveStatus"`
	CreatedAt       time.Time       `json:"createdAt,omitzero"`
	UpdatedAt       time.Time       `json:"updatedAt,omitzero"`
}

type Graph struct {
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges"`
}

type GraphNode struct {
	ID       string           `json:"id"`
	Type     model.NodeType   `json:"type,omitempty"`
	Status   state.NodeStatus `json:"status,omitempty"`
	Parent   string           `json:"parent,omitempty"`
	Stage    model.StageKind  `json:"stage,omitempty"`
	Children []string         `json:"children,omitempty"`
	Position *Position        `json:"position,omitempty"`
}

type Position struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type GraphEdge struct {
	From    string `json:"from,omitempty"`
	Outcome string `json:"outcome"`
	To      string `json:"to"`
}

type Verification struct {
	EffectiveStatus state.RunStatus `json:"effectiveStatus"`
	Dirty           bool            `json:"dirty"`
	Diagnostics     []Diagnostic    `json:"diagnostics"`
}

type Diagnostic struct {
	Layer    processverify.Layer `json:"layer"`
	Severity model.Severity      `json:"severity"`
	Code     string              `json:"code"`
	Path     string              `json:"path,omitempty"`
	Message  string              `json:"message"`
}

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

type TimelineEntry struct {
	Seq         int64              `json:"seq"`
	At          time.Time          `json:"at,omitzero"`
	Kind        evidence.EntryKind `json:"kind"`
	Event       state.EventType    `json:"event,omitempty"`
	Attempt     int                `json:"attempt,omitempty"`
	Actor       *Provenance        `json:"actor,omitempty"`
	Outcome     string             `json:"outcome,omitempty"`
	Verdict     string             `json:"verdict,omitempty"`
	EvidenceRef string             `json:"evidenceRef,omitempty"`
}

type Obligation struct {
	Kind         state.WaitKind   `json:"kind"`
	Assignee     *Provenance      `json:"assignee,omitempty"`
	Status       state.WaitStatus `json:"status"`
	Attempt      int              `json:"attempt,omitempty"`
	WaitingSince time.Time        `json:"waitingSince,omitzero"`
	DueAt        time.Time        `json:"dueAt,omitzero"`
	Contact      *Contact         `json:"contact,omitempty"`
}

type Blocked struct {
	Owner                *Provenance `json:"owner,omitempty"`
	BlockedAt            time.Time   `json:"blockedAt,omitzero"`
	BlockedAtUnavailable bool        `json:"blockedAtUnavailable"`
	Attempt              int         `json:"attempt,omitempty"`
	Contact              *Contact    `json:"contact,omitempty"`
}

type Contact struct {
	Cadence          string      `json:"cadence,omitempty"`
	LastContactAt    time.Time   `json:"lastContactAt,omitzero"`
	NextContactAt    time.Time   `json:"nextContactAt,omitzero"`
	BudgetUsed       int         `json:"budgetUsed"`
	BudgetMax        int         `json:"budgetMax"`
	EscalationTarget *Provenance `json:"escalationTarget,omitempty"`
	EscalatedAt      time.Time   `json:"escalatedAt,omitzero"`
	Paused           bool        `json:"paused"`
}

type Provenance struct {
	Kind    string `json:"kind"`
	AgentID string `json:"agentId,omitempty"`
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
	return Report{SchemaVersion: SchemaVersion, Nodes: map[string]NodeReport{}, TraversedEdges: []TraversedEdge{}}
}

func NewEnvelope(runID string, verification processverify.Report) Envelope {
	return Envelope{
		Run:          Run{ID: safeID(runID), EffectiveStatus: safeRunStatus(verification.EffectiveStatus)},
		Graph:        nil,
		Verification: projectVerification(verification),
		Report:       NewReport(),
	}
}

// Build derives a viewer envelope without mutating snapshot. tmpl must be the
// exact template verified for snapshot.Run.TemplateRef; nil suppresses graph
// and traversal claims.
func Build(snapshot store.Snapshot, tmpl *model.Template, verification processverify.Report) Envelope {
	envelope := NewEnvelope(snapshot.Run.ID, verification)
	envelope.Run.TemplateRef = safeTemplateRef(snapshot.Run.TemplateRef)
	envelope.Run.CreatedAt = snapshot.Run.CreatedAt
	envelope.Run.UpdatedAt = snapshot.Run.UpdatedAt
	if snapshot.State != nil && snapshot.State.Status.IsValid() {
		envelope.Run.StoredStatus = snapshot.State.Status
	}
	envelope.Graph = projectGraph(tmpl, snapshot.State)
	envelope.Report = buildReport(snapshot, tmpl)
	return envelope
}

func projectGraph(tmpl *model.Template, st *state.State) *Graph {
	if tmpl == nil || st == nil {
		return nil
	}
	ids := make([]string, 0, len(tmpl.Nodes)+len(st.Nodes))
	seen := make(map[string]struct{}, len(tmpl.Nodes)+len(st.Nodes))
	for id := range tmpl.Nodes {
		if !safeIDPattern.MatchString(id) {
			return nil
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	for id := range st.Nodes {
		if !safeIDPattern.MatchString(id) {
			return nil
		}
		if _, ok := seen[id]; !ok {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	graph := &Graph{Nodes: make([]GraphNode, 0, len(ids)), Edges: []GraphEdge{}}
	for _, id := range ids {
		templateNode := tmpl.Nodes[id]
		runtimeNode := st.Nodes[id]
		nodeType := templateNode.Type
		if nodeType == "" {
			nodeType = runtimeNode.Type
		}
		if nodeType != "" && !validNodeType(nodeType) {
			return nil
		}
		node := GraphNode{ID: id, Type: nodeType}
		if runtimeNode.Status.IsValid() {
			node.Status = runtimeNode.Status
		}
		if runtimeNode.Parent != "" {
			if !safeIDPattern.MatchString(runtimeNode.Parent) {
				return nil
			}
			node.Parent = runtimeNode.Parent
		}
		if runtimeNode.Stage != "" && runtimeNode.Stage.IsValid() {
			node.Stage = runtimeNode.Stage
		}
		for _, child := range runtimeNode.Children {
			if !safeIDPattern.MatchString(child) {
				return nil
			}
			node.Children = append(node.Children, child)
		}
		if tmpl.Layout != nil {
			if position, ok := tmpl.Layout.Nodes[id]; ok && finite(position.X) && finite(position.Y) {
				node.Position = &Position{X: position.X, Y: position.Y}
			}
		}
		graph.Nodes = append(graph.Nodes, node)
	}
	for _, edge := range model.NormalizeEdges(tmpl) {
		if (edge.From != "" && !safeIDPattern.MatchString(edge.From)) || !safeIDPattern.MatchString(edge.Outcome) || !safeIDPattern.MatchString(edge.To) {
			return nil
		}
		graph.Edges = append(graph.Edges, GraphEdge{From: edge.From, Outcome: edge.Outcome, To: edge.To})
	}
	return graph
}

func buildReport(snapshot store.Snapshot, tmpl *model.Template) Report {
	report := NewReport()
	if snapshot.State == nil {
		return report
	}
	logs := logsByNode(snapshot.NodeLogs)
	ids := make([]string, 0, len(snapshot.State.Nodes))
	for id := range snapshot.State.Nodes {
		if safeIDPattern.MatchString(id) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	for _, id := range ids {
		node := snapshot.State.Nodes[id]
		report.Nodes[id] = NodeReport{
			Summary:      summarizeNode(id, node, snapshot.State, logs),
			Timeline:     projectTimeline(id, logs[id], tmpl),
			Obligation:   latestObligation(snapshot.State, id),
			Blocked:      blockedState(snapshot.State, id, node),
			Conversation: conversationRef(snapshot.State, id, node),
		}
	}
	report.TraversedEdges = traversedEdges(tmpl, snapshot.NodeLogs)
	return report
}

func logsByNode(nodeLogs []evidence.NodeLog) map[string][]evidence.LogEntry {
	out := make(map[string][]evidence.LogEntry, len(nodeLogs))
	for _, log := range nodeLogs {
		if safeIDPattern.MatchString(log.NodeID) {
			out[log.NodeID] = append(out[log.NodeID], log.Entries...)
		}
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

func projectTimeline(nodeID string, entries []evidence.LogEntry, tmpl *model.Template) []TimelineEntry {
	timeline := make([]TimelineEntry, 0, len(entries))
	for _, entry := range entries {
		kind, ok := safeEntryKind(entry.Kind)
		if !ok {
			continue
		}
		item := TimelineEntry{Seq: entry.Seq, At: entry.At, Kind: kind, EvidenceRef: safeEvidenceRef(entry.EvidenceRef)}
		if event := entry.Event; event != nil {
			if event.NodeID != "" && event.NodeID != nodeID {
				continue
			}
			item.Event, _ = safeEventType(event.Type)
			item.Attempt = event.Attempt
			item.Actor = safeProvenance(string(event.Actor))
			if event.Type == state.EventNodeAttemptSettled {
				if state.IsPassOutcome(event.Outcome) {
					item.Outcome = "pass"
				} else if event.NodeStatus == state.NodeStatusFailed {
					item.Outcome = "fail"
				}
			}
			if item.EvidenceRef == "" {
				item.EvidenceRef = safeEvidenceRef(event.EvidenceRef)
			}
			if event.Decision != nil {
				if label, _, ok := resolveDecisionEdge(tmpl, nodeID, event.Decision.Verdict); ok {
					item.Verdict = label
				}
				if item.Actor == nil {
					item.Actor = safeProvenance(string(event.Decision.Actor))
				}
				if item.EvidenceRef == "" {
					item.EvidenceRef = safeEvidenceRef(event.Decision.EvidenceRef)
				}
			}
			if event.Resolution != nil && event.Resolution.Decision.IsValid() {
				item.Verdict = string(event.Resolution.Decision)
				if item.Actor == nil {
					item.Actor = safeProvenance(string(event.Resolution.Actor))
				}
				if item.EvidenceRef == "" {
					item.EvidenceRef = safeEvidenceRef(event.Resolution.EvidenceRef)
				}
			}
		}
		timeline = append(timeline, item)
	}
	slices.SortStableFunc(timeline, func(a, b TimelineEntry) int { return cmp.Compare(a.Seq, b.Seq) })
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
	if !selected.Kind.IsValid() || !selected.Status.IsValid() {
		return nil
	}
	result := &Obligation{
		Kind: selected.Kind, Assignee: safeProvenance(selected.Assignee), Status: selected.Status,
		Attempt: selected.Attempt, WaitingSince: selected.CreatedAt, DueAt: selected.DueAt,
	}
	if contact, ok := st.Contacts[selected.CommandID]; ok {
		result.Contact = projectContact(contact)
	}
	return result
}

func obligationLater(left, right state.ObligationRecord) bool {
	leftPending, rightPending := left.Status == state.WaitStatusPending, right.Status == state.WaitStatusPending
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
	if node.Status != state.NodeStatusBlocked {
		return nil
	}
	attempt := node.BlockedAttempt
	if attempt == 0 {
		attempt = node.Attempt
	}
	result := &Blocked{
		Owner: safeProvenance(node.BlockedOwner), BlockedAt: node.BlockedAt,
		BlockedAtUnavailable: node.BlockedAtUnavailable, Attempt: attempt,
	}
	if contact, ok := authoritativeBlockedContact(st, nodeID, attempt); ok {
		result.Contact = projectContact(contact)
	}
	return result
}

func authoritativeBlockedContact(st *state.State, nodeID string, attempt int) (state.ContactState, bool) {
	var selected *state.OutstandingCommand
	ambiguous := false
	for _, command := range st.OutstandingCommands {
		serviceable := command.Status == state.CommandStatusIssued || command.Status == state.CommandStatusObserved
		if !serviceable || command.Kind != state.CommandKindBlockNode || command.NodeID != nodeID || command.Attempt != attempt {
			continue
		}
		if _, ok := st.Contacts[command.ID]; !ok {
			continue
		}
		candidate := command
		if selected == nil || candidate.CreatedAt.After(selected.CreatedAt) {
			selected, ambiguous = &candidate, false
		} else if candidate.CreatedAt.Equal(selected.CreatedAt) {
			ambiguous = true
		}
	}
	if selected == nil || ambiguous {
		return state.ContactState{}, false
	}
	return st.Contacts[selected.ID], true
}

func projectContact(contact state.ContactState) *Contact {
	result := &Contact{
		Cadence: safeCadence(contact.Cadence), LastContactAt: contact.LastContactedAt,
		NextContactAt: contact.NextContactAt, BudgetUsed: nonnegative(contact.Used),
		BudgetMax: nonnegative(contact.Budget), EscalatedAt: contact.EscalatedAt, Paused: contact.Paused,
	}
	result.EscalationTarget = safeProvenance(contact.EscalationTarget)
	return result
}

func conversationRef(st *state.State, nodeID string, node state.NodeState) *ConversationRef {
	owned := map[string]struct{}{nodeID: {}}
	for _, childID := range node.Children {
		owned[childID] = struct{}{}
	}
	var selected *state.OutstandingCommand
	ambiguous := false
	for _, command := range st.OutstandingCommands {
		if _, ok := owned[command.NodeID]; !ok || command.Kind != state.CommandKindStartAttempt {
			continue
		}
		if command.Status != state.CommandStatusIssued && command.Status != state.CommandStatusObserved {
			continue
		}
		runtimeNode, ok := st.Nodes[command.NodeID]
		if !ok || !currentStartAttempt(runtimeNode, command.Attempt) {
			continue
		}
		candidate := command
		if selected == nil || candidate.CreatedAt.After(selected.CreatedAt) {
			selected, ambiguous = &candidate, false
		} else if candidate.CreatedAt.Equal(selected.CreatedAt) {
			ambiguous = true
		}
	}
	if selected == nil || ambiguous {
		return nil
	}
	agentID := stableAgentID(selected.ExternalRef)
	if agentID == "" {
		return nil
	}
	return &ConversationRef{AgentID: agentID}
}

func currentStartAttempt(node state.NodeState, attempt int) bool {
	if node.ActiveAttempt != nil && node.ActiveAttempt.Outcome == "" && node.ActiveAttempt.SettledAt.IsZero() {
		return attempt == node.ActiveAttempt.Attempt
	}
	return node.Status == state.NodeStatusReady && attempt == node.Attempt+1
}

func traversedEdges(tmpl *model.Template, nodeLogs []evidence.NodeLog) []TraversedEdge {
	if tmpl == nil {
		return []TraversedEdge{}
	}
	type key struct{ from, outcome, to string }
	counts := make(map[key]TraversedEdge)
	for _, log := range nodeLogs {
		for _, entry := range log.Entries {
			event := entry.Event
			if event == nil || event.NodeID == "" || event.NodeID != log.NodeID {
				continue
			}
			label, target, ok := routedEdge(tmpl, event)
			if !ok {
				continue
			}
			k := key{from: event.NodeID, outcome: label, to: target}
			item := counts[k]
			item.From, item.Outcome, item.To = k.from, k.outcome, k.to
			item.Count++
			if entry.At.After(item.LastAt) {
				item.LastAt = entry.At
			}
			counts[k] = item
		}
	}
	out := make([]TraversedEdge, 0, len(counts))
	for _, item := range counts {
		out = append(out, item)
	}
	slices.SortFunc(out, func(a, b TraversedEdge) int {
		if n := cmp.Compare(a.From, b.From); n != 0 {
			return n
		}
		if n := cmp.Compare(a.Outcome, b.Outcome); n != 0 {
			return n
		}
		return cmp.Compare(a.To, b.To)
	})
	return out
}

func routedEdge(tmpl *model.Template, event *state.Event) (string, string, bool) {
	node, ok := tmpl.Nodes[event.NodeID]
	if !ok {
		return "", "", false
	}
	switch event.Type {
	case state.EventNodeAttemptSettled:
		switch event.NodeStatus {
		case state.NodeStatusCompleted:
			return resolvePassEdge(node.Next, event.Outcome)
		case state.NodeStatusFailed:
			return resolveFailEdge(node.Next)
		default:
			return "", "", false
		}
	case state.EventDecisionRecorded:
		return resolveDecisionEdge(tmpl, event.NodeID, event.ChosenEdge)
	default:
		return "", "", false
	}
}

func resolvePassEdge(next model.Next, verdict string) (string, string, bool) {
	keys := append([]string{verdict, strings.ToLower(strings.TrimSpace(verdict))}, model.PassOutcomeLabels()...)
	seen := map[string]struct{}{}
	for _, label := range keys {
		if _, ok := seen[label]; ok {
			continue
		}
		seen[label] = struct{}{}
		if target := next[label]; target != "" && safeIDPattern.MatchString(label) && safeIDPattern.MatchString(target) {
			return label, target, true
		}
	}
	if len(next) == 1 {
		for label, target := range next {
			if safeIDPattern.MatchString(label) && safeIDPattern.MatchString(target) {
				return label, target, true
			}
		}
	}
	return "", "", false
}

func resolveFailEdge(next model.Next) (string, string, bool) {
	target := plan.ResolveFailEdge(next)
	if target == "" {
		return "", "", false
	}
	for _, label := range []string{"fail", "failed", "failure", "error"} {
		if next[label] == target && safeIDPattern.MatchString(target) {
			return label, target, true
		}
	}
	return "", "", false
}

func resolveDecisionEdge(tmpl *model.Template, nodeID, verdict string) (string, string, bool) {
	if tmpl == nil {
		return "", "", false
	}
	node, ok := tmpl.Nodes[nodeID]
	if !ok {
		return "", "", false
	}
	if target, ok := node.Next[verdict]; ok && safeIDPattern.MatchString(verdict) && safeIDPattern.MatchString(target) {
		return verdict, target, true
	}
	label := strings.ToLower(strings.TrimSpace(verdict))
	target, ok := plan.DecisionEdge(node.Next, verdict)
	if !ok || !safeIDPattern.MatchString(label) || !safeIDPattern.MatchString(target) {
		return "", "", false
	}
	return label, target, true
}

func projectVerification(report processverify.Report) Verification {
	result := Verification{EffectiveStatus: safeRunStatus(report.EffectiveStatus), Dirty: report.Dirty, Diagnostics: []Diagnostic{}}
	for _, diagnostic := range report.Diagnostics {
		layer := safeLayer(diagnostic.Layer)
		severity := safeSeverity(diagnostic.Severity)
		code := diagnostic.Code
		if !safeCodePattern.MatchString(code) {
			code = "verification_error"
		}
		path := safeDiagnosticPath(diagnostic.Path)
		result.Diagnostics = append(result.Diagnostics, Diagnostic{
			Layer: layer, Severity: severity, Code: code, Path: path, Message: diagnosticMessage(layer, code),
		})
	}
	return result
}

func diagnosticMessage(layer processverify.Layer, code string) string {
	switch code {
	case "read_torn_tail":
		return "persisted evidence ends with an incomplete record"
	case "read_malformed":
		return "persisted evidence contains an invalid record"
	case "snapshot_unreadable":
		return "persisted run data could not be decoded"
	case "template_unavailable":
		return "the exact pinned template is unavailable"
	case "embedded_template_mismatch", "pinned_template_mismatch":
		return "the pinned template identity does not match persisted content"
	}
	switch layer {
	case processverify.LayerEvidence:
		return "persisted evidence does not match the materialized run state"
	case processverify.LayerSemantic:
		return "materialized run state violates process invariants"
	default:
		return "persisted run data failed verification"
	}
}

func safeProvenance(raw string) *Provenance {
	value := strings.TrimSpace(raw)
	if agentID := stableAgentID(value); agentID != "" {
		return &Provenance{Kind: "agent", AgentID: agentID}
	}
	for _, item := range []struct{ prefix, kind string }{
		{"human:", "human"}, {"engine:", "engine"}, {"program:", "program"},
	} {
		if strings.HasPrefix(value, item.prefix) {
			return &Provenance{Kind: item.kind}
		}
	}
	if value == "" {
		return nil
	}
	return nil
}

func safeDiagnosticPath(path string) string {
	switch path {
	case "run.template", "run.templateRef", "state.json", "manifest.jsonl":
		return path
	default:
		return ""
	}
}

func stableAgentID(raw string) string {
	value := strings.TrimPrefix(strings.TrimSpace(raw), "agent:")
	if !stableAgentIDPattern.MatchString(value) {
		return ""
	}
	return value
}

func safeEvidenceRef(ref string) string {
	value := strings.TrimSpace(ref)
	if safeArtifactRefPattern.MatchString(value) {
		return value
	}
	return ""
}

func safeCadence(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 32 {
		return ""
	}
	if _, err := time.ParseDuration(value); err != nil {
		return ""
	}
	return value
}

func safeEntryKind(kind evidence.EntryKind) (evidence.EntryKind, bool) {
	return kind, kind.IsValid()
}

func safeEventType(value state.EventType) (state.EventType, bool) {
	switch value {
	case state.EventRunInitialized, state.EventRunStatusSet, state.EventRunPaused, state.EventRunResumed,
		state.EventNodeStatusSet, state.EventNodeExpanded, state.EventNodeAttemptStarted, state.EventNodeAttemptSettled,
		state.EventFeedbackRecorded, state.EventGateLoopReset, state.EventGateShortCircuited, state.EventDecisionRecorded,
		state.EventNodeBlocked, state.EventNodeUnblocked, state.EventBlockResolutionRecorded, state.EventWaitCreated,
		state.EventWaitSatisfied, state.EventTimerCreated, state.EventTimerSatisfied, state.EventCommandIssued,
		state.EventCommandDispatched, state.EventCommandObserved, state.EventObligationCreated, state.EventObligationResolved,
		state.EventContactScheduled, state.EventContactUpdated, state.EventAdminRepairRecorded, state.EventAdminProgramsAllowed,
		state.EventTemplateDivergenceMarked:
		return value, true
	default:
		return "", false
	}
}

func safeLayer(layer processverify.Layer) processverify.Layer {
	switch layer {
	case processverify.LayerLoad, processverify.LayerEvidence, processverify.LayerSemantic:
		return layer
	default:
		return processverify.LayerLoad
	}
}

func safeSeverity(severity model.Severity) model.Severity {
	switch severity {
	case model.SeverityError, model.SeverityWarning:
		return severity
	default:
		return model.SeverityError
	}
}

func safeRunStatus(status state.RunStatus) state.RunStatus {
	if status.IsValid() {
		return status
	}
	return state.RunStatusInconsistent
}

func safeID(value string) string {
	if safeIDPattern.MatchString(value) {
		return value
	}
	return ""
}

func safeTemplateRef(value string) string {
	if safeTemplateRefPattern.MatchString(value) {
		return value
	}
	return ""
}

func validNodeType(nodeType model.NodeType) bool {
	switch nodeType {
	case model.NodeTypeTask, model.NodeTypeDecision, model.NodeTypeWait, model.NodeTypeStart, model.NodeTypeEnd:
		return true
	default:
		return false
	}
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func nonnegative(value int) int {
	if value < 0 {
		return 0
	}
	return value
}
