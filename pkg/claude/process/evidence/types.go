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
	// EntryKindAttempt covers attempt starts and settles.
	EntryKindAttempt EntryKind = "attempt"
	// EntryKindDecision covers decision-node verdict records.
	EntryKindDecision EntryKind = "decision"
	// EntryKindGate covers gate machinery: verdicts, blocks, feedback
	// routing, loop resets, short-circuits, and timer/wait bookkeeping.
	EntryKindGate EntryKind = "gate"
	EntryKindSignal EntryKind = "signal"
	EntryKindAdmin  EntryKind = "admin"
	// EntryKindExpansion covers compound-node expansion records.
	EntryKindExpansion EntryKind = "expansion"
	// EntryKindStatus covers pure status transitions (node_status_set,
	// run_status_set, run_paused, run_resumed) that carry no verdict of
	// their own. Entries written before this kind existed carry
	// EntryKindGate.
	EntryKindStatus EntryKind = "status"
)

func (k EntryKind) IsValid() bool {
	switch k {
	case EntryKindAttempt, EntryKindDecision, EntryKindGate, EntryKindSignal, EntryKindAdmin, EntryKindExpansion, EntryKindStatus:
		return true
	default:
		return false
	}
}

// KindForEvent labels a node/run-scoped entry by its event: pure status
// transitions get EntryKindStatus, everything else defaults to the gate
// machinery kind (writers with a more specific kind pass it explicitly).
func KindForEvent(t state.EventType) EntryKind {
	switch t {
	case state.EventNodeStatusSet, state.EventRunStatusSet, state.EventRunPaused, state.EventRunResumed:
		return EntryKindStatus
	default:
		return EntryKindGate
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
