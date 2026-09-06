package model

import "time"

type ActivityKind string

const (
	ActivityOperation  ActivityKind = "operation"
	ActivityWorkRun    ActivityKind = "work_run"
	ActivityEvidence   ActivityKind = "work_evidence"
	ActivityDecision   ActivityKind = "decision"
	ActivityHistorical ActivityKind = "historical_audit"
)

// ActivityRecord is a bounded public projection of authoritative durable
// records. It does not carry provider evidence, credentials, or raw payloads.
type ActivityRecord struct {
	ID             string
	Kind           ActivityKind
	Actor          ActivityActor
	AgentID        AgentID
	ConversationID ConversationID
	ExecutionID    ExecutionID
	WorkRunID      WorkRunID
	Outcome        string
	Reason         string
	StartedAt      time.Time
	FinishedAt     *time.Time
	Historical     bool
	Provenance     string
}

// ActivityActor is identity for display and filtering, never an authority
// credential or delegation snapshot.
type ActivityActor struct {
	Kind          PrincipalKind
	AgentID       AgentID
	ExecutionID   ExecutionID
	AutomationRun string
}
