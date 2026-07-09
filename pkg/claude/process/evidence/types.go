package evidence

import (
	"strconv"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/state"
)

const (
	LogEntrySchemaVersion      = 1
	ManifestEntrySchemaVersion = 1
	ChecksumAlgorithm          = "sha256-chain-v1"
)

type EntryKind string

const (
	EntryKindAttempt   EntryKind = "attempt"
	EntryKindDecision  EntryKind = "decision"
	EntryKindGate      EntryKind = "gate"
	EntryKindSignal    EntryKind = "signal"
	EntryKindAdmin     EntryKind = "admin"
	EntryKindExpansion EntryKind = "expansion"
)

func (k EntryKind) IsValid() bool {
	switch k {
	case EntryKindAttempt, EntryKindDecision, EntryKindGate, EntryKindSignal, EntryKindAdmin, EntryKindExpansion:
		return true
	default:
		return false
	}
}

type ScopeKind string

const (
	ScopeRun  ScopeKind = "run"
	ScopeNode ScopeKind = "node"
)

type Scope struct {
	Kind ScopeKind `json:"kind"`
	ID   string    `json:"id,omitempty"`
}

type LogEntry struct {
	SchemaVersion int          `json:"schemaVersion"`
	Seq           int64        `json:"seq"`
	At            time.Time    `json:"at"`
	Scope         Scope        `json:"scope"`
	Kind          EntryKind    `json:"kind"`
	Event         *state.Event `json:"event,omitempty"`
	EvidenceRef   string       `json:"evidenceRef,omitempty"`
}

type ManifestEntry struct {
	SchemaVersion int       `json:"schemaVersion"`
	Seq           int64     `json:"seq"`
	Timestamp     time.Time `json:"ts"`
	Scope         Scope     `json:"scope"`
	EventRef      string    `json:"eventRef"`
	EntryChecksum string    `json:"entryChecksum"`
	Checksum      string    `json:"checksum"`
}

type NodeLog struct {
	NodeID  string
	Entries []LogEntry
}

type ReadErrorKind string

const (
	ReadErrorMalformed ReadErrorKind = "malformed"
	ReadErrorTornTail  ReadErrorKind = "torn_tail"
)

type ReadError struct {
	Kind ReadErrorKind
	File string
	Line int
	Err  error
}

func (e *ReadError) Error() string {
	if e == nil {
		return ""
	}
	message := "<nil>"
	if e.Err != nil {
		message = e.Err.Error()
	}
	location := "line " + itoa(e.Line)
	if e.File != "" {
		location = e.File + ":" + location
	}
	return string(e.Kind) + " at " + location + ": " + message
}

func (e *ReadError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func itoa(value int) string {
	return strconv.Itoa(value)
}
