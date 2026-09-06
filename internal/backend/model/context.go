package model

import "time"

type AttemptGeneration uint64

type NativeBinding struct {
	Namespace string
	Reference string
}

type ContextReadiness string

const (
	ContextReadinessPending    ContextReadiness = "pending"
	ContextReadinessReady      ContextReadiness = "ready"
	ContextReadinessUnresolved ContextReadiness = "unresolved"
)

type NativeBindingHistory struct {
	ExecutionID    ExecutionID
	ConversationID ConversationID
	Attempt        AttemptGeneration
	Binding        NativeBinding
	Disposition    string
	Correlation    string
	ProviderOrder  string
	ObservedAt     time.Time
	RecordedAt     time.Time
}
