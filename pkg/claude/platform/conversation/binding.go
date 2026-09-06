// Package conversation owns logical history identity and admission of external
// harness references. A reference is neither an actor nor proof of a process.
package conversation

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
)

type Outcome string

const (
	Accepted   Outcome = "accepted"
	Historical Outcome = "historical"
	Ambiguous  Outcome = "ambiguous"
	Conflict   Outcome = "conflict"
)

// Evidence is constructed at the host adapter boundary, never decoded from a
// harness hook. Process identifies the observed workload instance, not only its
// reusable PID. MainProcess proves its association with this attempt.
type Evidence struct {
	Process     string
	MainProcess bool
	Transition  Transition
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

func (d Decision) Admitted() bool { return d.Outcome == Accepted }
