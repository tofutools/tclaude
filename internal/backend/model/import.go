package model

import "time"

// ImportReceipt proves that one immutable offline snapshot was completely
// converted into a fresh replacement database. It is evidence only and never
// grants authority to imported records.
type ImportReceipt struct {
	ID                      string
	SourceSchemaVersion     int
	SourceDatabaseSHA256    string
	ManifestSHA256          string
	ImporterFormatVersion   int
	PlanFormatVersion       int
	TargetSchemaVersion     int
	PlanSHA256              string
	SemanticSHA256          string
	MetadataOnlyAttachments bool
	Counts                  map[string]int64
	CompletedAt             time.Time
}

type ImportIDMapping struct {
	SourceNamespace string
	SourceTable     string
	SourceKey       string
	TargetKind      string
	TargetID        string
}

// ImportedSourceRecord is immutable retained authoring/history. Runnable is
// deliberately absent: consumers must use a typed target record before any
// imported intent can participate in an effect.
type ImportedSourceRecord struct {
	SourceTable   string
	SourceKey     string
	SourcePath    string
	Class         string
	Conversion    string
	ReasonCode    string
	Payload       []byte
	PayloadSHA256 string
}

type ImportedDiagnostic struct {
	Severity    string
	Code        string
	SourceTable string
	SourceKey   string
	SourcePath  string
	Detail      string
}

// ImportedRecordDisposition is the safe report projection of a retained
// source record. The authored payload is intentionally available only through
// the separate ImportedSourceRecords API.
type ImportedRecordDisposition struct {
	SourceTable   string
	SourceKey     string
	SourcePath    string
	Class         string
	Conversion    string
	ReasonCode    string
	PayloadSHA256 string
}

// ImportReport is the default operator-facing provenance view. It contains
// enough information to audit conversion decisions without exposing retained
// authored values or attachment content.
type ImportReport struct {
	Receipt     ImportReceipt
	IDMappings  []ImportIDMapping
	Diagnostics []ImportedDiagnostic
	Records     []ImportedRecordDisposition
}

type ImportedAttachmentAvailability string

const (
	ImportedAttachmentAvailable ImportedAttachmentAvailability = "available"
	ImportedAttachmentMissing   ImportedAttachmentAvailability = "missing"
	ImportedAttachmentCorrupt   ImportedAttachmentAvailability = "corrupt"
)

type ImportedAttachment struct {
	AttachmentID AttachmentID
	SourceTable  string
	SourceKey    string
	MessageID    MessageID
	Position     int
	Availability ImportedAttachmentAvailability
	LossReason   string
	Attachment   Attachment
	Content      []byte
}

type ImportedMessageAddress struct {
	AddressKind          MessageAddressKind
	Audience             MessageAudienceKind
	OriginalConversation string
	OriginalAgent        string
	TargetAgentID        AgentID
	Resolved             bool
}

// ImportedMessageEnvelope retains addressing and delivery facts that the live
// collaboration projection cannot express without manufacturing an Agent.
type ImportedMessageEnvelope struct {
	MessageID        MessageID
	SourceTable      string
	SourceKey        string
	Addresses        []ImportedMessageAddress
	OriginalParent   string
	OriginalGroup    string
	DeliveredAt      *time.Time
	ReadAt           *time.Time
	ProcessedAt      *time.Time
	NudgeAttemptedAt *time.Time
	Origin           string
}
