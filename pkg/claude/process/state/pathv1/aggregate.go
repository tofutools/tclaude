package pathv1

import (
	"errors"
	"fmt"
)

const MaxInvariantDiagnostics = 128

// AggregateView is the complete, schema-independent input to the dormant
// path-v1 authority validator. It deliberately is not embedded in state.State
// and has no planner, reducer, executor, or persistence wiring.
type AggregateView struct {
	RunID              string
	TemplateRef        string
	TemplateSourceHash string
	Authority          *AggregateAuthority
	Routing            *RoutingState
	Commands           map[string]CommandRecord
	SideEffects        map[string]SideEffectIdentity
	AdminRecords       map[string]PathV1AdminRecord
	AdminResolutions   map[string]BlockResolution
	// CheckpointBytes is the encoded post-state checkpoint size when known. A
	// zero value still validates the encoded routing envelope, which is a lower
	// bound on the full checkpoint.
	CheckpointBytes int
}

type InvariantDiagnostic struct {
	Code, Path, Message string
}

type InvariantReport struct {
	Diagnostics []InvariantDiagnostic
	Suppressed  int
	Usage       Usage
}

func (r InvariantReport) Valid() bool { return len(r.Diagnostics) == 0 && r.Suppressed == 0 }

type diagnosticCollector struct {
	report *InvariantReport
}

func (c diagnosticCollector) add(code, path, format string, args ...any) {
	if len(c.report.Diagnostics) == MaxInvariantDiagnostics {
		c.report.Suppressed++
		return
	}
	c.report.Diagnostics = append(c.report.Diagnostics, InvariantDiagnostic{
		Code: code, Path: path, Message: fmt.Sprintf(format, args...),
	})
}

var (
	ErrAggregateInvalid   = errors.New("path-v1 aggregate is invalid")
	ErrAggregateUnsettled = errors.New("path-v1 aggregate is unsettled")
)

type AggregateCompletion struct {
	Result              string
	TerminalCauseDigest CauseDigest
}

// AssessAggregateCompletion is a pure fail-closed fold. It does not construct,
// authorize, claim, or replay complete_run_v1; those surfaces remain reserved
// for the later mutation/recovery layer.
func AssessAggregateCompletion(view AggregateView) (AggregateCompletion, error) {
	report := ValidateAggregate(view)
	if !report.Valid() {
		return AggregateCompletion{}, fmt.Errorf("%w: %d diagnostics (%d suppressed)", ErrAggregateInvalid, len(report.Diagnostics), report.Suppressed)
	}

	for _, id := range sortedMapKeys(view.Routing.Paths) {
		path := view.Routing.Paths[id]
		if path.State == PathLive || path.State == PathArrived {
			return AggregateCompletion{}, fmt.Errorf("%w: active path %q in state %q", ErrAggregateUnsettled, path.ID, path.State)
		}
	}
	for _, id := range sortedMapKeys(view.Routing.Reservations) {
		reservation := view.Routing.Reservations[id]
		if reservation.State == ReservationOpen {
			return AggregateCompletion{}, fmt.Errorf("%w: open reservation %q", ErrAggregateUnsettled, reservation.ID)
		}
	}
	for _, id := range sortedMapKeys(view.Routing.Propagation) {
		intent := view.Routing.Propagation[id]
		if intent.State == PropagationPending {
			return AggregateCompletion{}, fmt.Errorf("%w: pending propagation %q", ErrAggregateUnsettled, intent.ID)
		}
	}
	for _, id := range sortedMapKeys(view.Commands) {
		command := view.Commands[id]
		if command.State.Active() {
			return AggregateCompletion{}, fmt.Errorf("%w: active command %q", ErrAggregateUnsettled, command.ID)
		}
	}
	for _, id := range sortedMapKeys(view.SideEffects) {
		effect := view.SideEffects[id]
		if ActiveSideEffect(effect) {
			return AggregateCompletion{}, fmt.Errorf("%w: active %s %q", ErrAggregateUnsettled, effect.Kind, effect.ID)
		}
	}

	ids := make([]CauseID, 0)
	kinds := make([]TerminalKind, 0)
	for _, id := range sortedMapKeys(view.Routing.Paths) {
		path := view.Routing.Paths[id]
		if !path.State.TerminalNonSuccess() {
			continue
		}
		cause := view.Routing.CauseRecords[path.TerminalCauseID]
		ids = append(ids, cause.ID)
		kinds = append(kinds, cause.TerminalKind)
	}
	digest, err := CauseSetIdentity(ids)
	if err != nil {
		return AggregateCompletion{}, err
	}
	result, err := TerminalResult(kinds)
	if err != nil {
		return AggregateCompletion{}, err
	}
	return AggregateCompletion{Result: result, TerminalCauseDigest: digest}, nil
}
