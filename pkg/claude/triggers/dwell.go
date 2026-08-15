package triggers

import "time"

const (
	FactTrue    = "true"
	FactFalse   = "false"
	FactUnknown = "unknown"
)

type DwellState struct {
	RuleRevision int64
	Episode      int64
	Result       string
	TrueSince    time.Time
	FiredAt      time.Time
}

type DwellInput struct {
	RuleRevision int64
	For          time.Duration
	Result       string
	FactSince    time.Time
	Now          time.Time
}

type DwellPlan struct {
	State DwellState
	Fire  bool
	DueAt time.Time
}

// PlanDwell applies once-per-episode semantics to one fact observation.
// Unknown conservatively breaks continuous truth by clearing true_since, but
// it never clears FiredAt: only an observed false condition re-arms a fired
// episode. A following true observation therefore starts a new dwell clock
// only when the episode has not already fired.
func PlanDwell(previous *DwellState, in DwellInput) DwellPlan {
	now := in.Now.UTC()
	state := DwellState{RuleRevision: in.RuleRevision, Result: in.Result}
	if previous != nil {
		state.Episode = previous.Episode
		state.FiredAt = previous.FiredAt
	}
	switch in.Result {
	case FactFalse:
		state.FiredAt = time.Time{}
		return DwellPlan{State: state}
	case FactUnknown:
		return DwellPlan{State: state}
	case FactTrue:
		continuing := previous != nil && previous.Result == FactTrue
		if continuing {
			state.TrueSince = previous.TrueSince
		} else if state.FiredAt.IsZero() {
			state.Episode++
			state.TrueSince = now
			if previous == nil || previous.Result == FactFalse {
				if !in.FactSince.IsZero() && in.FactSince.Before(now) {
					state.TrueSince = in.FactSince.UTC()
				}
			}
		}
	default:
		state.Result = FactUnknown
		return DwellPlan{State: state}
	}
	if !state.FiredAt.IsZero() || state.TrueSince.IsZero() {
		return DwellPlan{State: state}
	}
	due := state.TrueSince.Add(in.For)
	if now.Before(due) {
		return DwellPlan{State: state, DueAt: due}
	}
	state.FiredAt = now
	return DwellPlan{State: state, Fire: true}
}
