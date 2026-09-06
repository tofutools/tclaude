// Package conversation owns logical history identity and admission of external
// harness references. A reference is neither an actor nor proof of a process.
package conversation

import "github.com/tofutools/tclaude/pkg/claude/platform/execution"

// ID identifies a platform-owned history, independently of harness storage.
type ID string

type Revision int64

// Reference names a history in one harness state store. Namespace is the
// adapter's recorded store identity, not the hook's caller-controlled cwd.
type Reference struct {
	Harness   string
	Namespace string
	Value     string
}

type Transition string

const (
	Unspecified Transition = ""
	Continue    Transition = "continue"
	Clear       Transition = "clear"
	Resume      Transition = "resume"
	Reincarnate Transition = "reincarnate"
	Fork        Transition = "fork"
)

type Origin string

const (
	Managed    Origin = "managed"
	Discovered Origin = "discovered"
)

type EvidenceStrength string

const (
	// VerifiedMainProcess means the internal host adapter associated the
	// observation with the attempt's OS main process. The store still compares
	// its durable session/generation/attachment tuple in the transaction.
	VerifiedMainProcess EvidenceStrength = "verified-main-process"
)

type EvidenceSource string

const HostAdapter EvidenceSource = "host-adapter"

type Outcome string

const (
	Accepted   Outcome = "accepted"
	Duplicate  Outcome = "duplicate"
	Historical Outcome = "historical"
	Ambiguous  Outcome = "ambiguous"
	Conflict   Outcome = "conflict"
	Rejected   Outcome = "rejected"
)

// Evidence is constructed at the internal host-adapter boundary, never
// decoded from harness hook input. ID is stable across retries of one
// observation. ProcessInstance distinguishes PID reuse; the remaining fields
// are the durable association snapshot the store rechecks atomically.
type Evidence struct {
	ID              string
	ProcessInstance string
	MainProcess     bool
	Strength        EvidenceStrength
	Source          EvidenceSource
	PID             int
	TmuxSession     string
	PaneID          string
}

// Admission is the normalized input to the single managed binding writer.
// ExpectedRevision is a compare-and-swap expectation; callers never choose
// the revision that will be persisted.
type Admission struct {
	Origin           Origin
	Attempt          execution.AttemptRef
	ExpectedRevision Revision
	Reference        Reference
	Transition       Transition
	Evidence         Evidence
}

// Selection is the selected main history at one execution binding revision.
// Historical references remain attributable after the selection advances.
type Selection struct {
	Conversation ID
	Reference    Reference
	Revision     Revision
}

type Decision struct {
	Outcome   Outcome
	Reason    string
	Selection Selection
	Changed   bool
	// CarriedName is a compatibility effect for a genuine clear only.
	CarriedName string
}

func (d Decision) Admitted() bool {
	return d.Outcome == Accepted || d.Outcome == Duplicate
}
