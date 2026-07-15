package pathv1

import "fmt"

const (
	MaxOutgoingOrAllCandidates = 2_046
	MaxAnyCandidates           = 1_364
	MaxLineageDepth            = 4_096
	MaxPathRecords             = 100_000
	MaxRoutingRecords          = 200_000
	MaxIDReferences            = 400_000
	MaxRoutingList             = 4_096
	MaxRoutingMutations        = 4_096
	MaxRoutingLogEntries       = 4_096
	MaxCommandPayloadBytes     = 16 << 20
	MaxCheckpointBytes         = 16 << 20
	MaxPropagationShards       = 98
)

type Usage struct {
	Paths           int `json:"paths"`
	Records         int `json:"records"`
	References      int `json:"references"`
	LargestList     int `json:"largestList"`
	Mutations       int `json:"mutations,omitempty"`
	LogEntries      int `json:"logEntries,omitempty"`
	PayloadBytes    int `json:"payloadBytes,omitempty"`
	CheckpointBytes int `json:"checkpointBytes,omitempty"`
}

type OverBudgetError struct {
	Limit          string
	Value, Maximum int
}

func (e *OverBudgetError) Error() string {
	return fmt.Sprintf("path-v1 %s over budget: %d > %d", e.Limit, e.Value, e.Maximum)
}

func (u Usage) Validate() error {
	checks := []struct {
		name       string
		value, max int
	}{{"paths", u.Paths, MaxPathRecords}, {"records", u.Records, MaxRoutingRecords}, {"references", u.References, MaxIDReferences}, {"list", u.LargestList, MaxRoutingList}, {"mutations", u.Mutations, MaxRoutingMutations}, {"log_entries", u.LogEntries, MaxRoutingLogEntries}, {"payload_bytes", u.PayloadBytes, MaxCommandPayloadBytes}, {"checkpoint_bytes", u.CheckpointBytes, MaxCheckpointBytes}}
	for _, check := range checks {
		if check.value < 0 {
			return fmt.Errorf("path-v1 %s is negative: %d", check.name, check.value)
		}
		if check.value > check.max {
			return &OverBudgetError{Limit: check.name, Value: check.value, Maximum: check.max}
		}
	}
	return nil
}

func MutationCountExclusive(n int) (int, error) {
	if n < 0 || n > MaxOutgoingOrAllCandidates {
		return 0, fmt.Errorf("exclusive candidate count %d out of range", n)
	}
	return 2*n + 1, nil
}
func MutationCountSplit(n int) (int, error) {
	if n < 2 || n > MaxOutgoingOrAllCandidates {
		return 0, fmt.Errorf("split candidate count %d out of range", n)
	}
	return 2*n + 3, nil
}
func MutationCountAllActivate(n int) (int, error) {
	if n < 1 || n > MaxOutgoingOrAllCandidates {
		return 0, fmt.Errorf("all candidate count %d out of range", n)
	}
	return n + 4, nil
}
func MutationCountAllNonSuccess(arrived, intents int) (int, error) {
	if arrived < 0 || arrived > MaxOutgoingOrAllCandidates || intents < 0 || intents > MaxPropagationShards {
		return 0, fmt.Errorf("all non-success inputs out of range")
	}
	n := arrived + 4 + intents
	if n > MaxRoutingMutations {
		return 0, &OverBudgetError{Limit: "mutations", Value: n, Maximum: MaxRoutingMutations}
	}
	return n, nil
}
func MutationCountAny(candidates, preArrivedLosers int) (int, error) {
	if candidates < 2 || candidates > MaxAnyCandidates {
		return 0, fmt.Errorf("any candidate count %d out of range", candidates)
	}
	if preArrivedLosers < 0 || preArrivedLosers > candidates-1 {
		return 0, fmt.Errorf("pre-arrived loser count %d out of range", preArrivedLosers)
	}
	n := 2*candidates + preArrivedLosers + 3
	if n > MaxRoutingMutations {
		return 0, &OverBudgetError{Limit: "mutations", Value: n, Maximum: MaxRoutingMutations}
	}
	return n, nil
}
