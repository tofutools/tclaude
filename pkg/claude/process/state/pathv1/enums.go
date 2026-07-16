package pathv1

type CandidateKind string

const (
	CandidateInboundEdge CandidateKind = "inbound_edge"
	CandidateScopeBranch CandidateKind = "scope_branch"
)

func (v CandidateKind) Valid() bool { return v == CandidateInboundEdge || v == CandidateScopeBranch }

type PathKind string

const (
	PathActivationOutput PathKind = "activation_output"
	PathEdge             PathKind = "edge"
	PathImpossibleEdge   PathKind = "impossible_edge"
)

func (v PathKind) Valid() bool {
	return v == PathActivationOutput || v == PathEdge || v == PathImpossibleEdge
}

type PathState string

const (
	PathLive         PathState = "live"
	PathRouted       PathState = "routed"
	PathSplit        PathState = "split"
	PathArrived      PathState = "arrived"
	PathImpossible   PathState = "impossible"
	PathConsumed     PathState = "consumed"
	PathEnded        PathState = "ended"
	PathFailed       PathState = "failed"
	PathCanceled     PathState = "canceled"
	PathSkipped      PathState = "skipped"
	PathDetachedSink PathState = "detached_sink"
)

func (v PathState) Valid() bool {
	switch v {
	case PathLive, PathRouted, PathSplit, PathArrived, PathImpossible, PathConsumed, PathEnded, PathFailed, PathCanceled, PathSkipped, PathDetachedSink:
		return true
	}
	return false
}
func (v PathState) TerminalNonSuccess() bool {
	return v == PathFailed || v == PathCanceled || v == PathSkipped
}

type ScopeState string

const (
	ScopeOpen               ScopeState = "open"
	ScopeClosedActivated    ScopeState = "closed_activated"
	ScopeClosedNoActivation ScopeState = "closed_no_activation"
)

func (v ScopeState) Valid() bool {
	return v == ScopeOpen || v == ScopeClosedActivated || v == ScopeClosedNoActivation
}

type ScopeCloseReason string

const (
	ScopeCloseNone                ScopeCloseReason = ""
	ScopeCloseAll                 ScopeCloseReason = "all"
	ScopeCloseAny                 ScopeCloseReason = "any"
	ScopeCloseAllImpossible       ScopeCloseReason = "all_impossible"
	ScopeCloseCandidateNonSuccess ScopeCloseReason = "candidate_non_success"
)

func (v ScopeCloseReason) Valid() bool {
	switch v {
	case ScopeCloseNone, ScopeCloseAll, ScopeCloseAny, ScopeCloseAllImpossible, ScopeCloseCandidateNonSuccess:
		return true
	}
	return false
}

type JoinPolicy string

const (
	JoinExclusive JoinPolicy = "exclusive"
	JoinAll       JoinPolicy = "all"
	JoinAny       JoinPolicy = "any"
)

func (v JoinPolicy) Valid() bool { return v == JoinExclusive || v == JoinAll || v == JoinAny }

type ReservationState string

const (
	ReservationOpen               ReservationState = "open"
	ReservationActivated          ReservationState = "activated"
	ReservationClosedNoActivation ReservationState = "closed_no_activation"
)

func (v ReservationState) Valid() bool {
	return v == ReservationOpen || v == ReservationActivated || v == ReservationClosedNoActivation
}

type ActivationReceiptResult string

const (
	ReceiptActivated          ActivationReceiptResult = "activated"
	ReceiptClosedNoActivation ActivationReceiptResult = "closed_no_activation"
)

func (v ActivationReceiptResult) Valid() bool {
	return v == ReceiptActivated || v == ReceiptClosedNoActivation
}

type TerminalKind string

const (
	TerminalImpossible TerminalKind = "impossible"
	TerminalFailed     TerminalKind = "failed"
	TerminalCanceled   TerminalKind = "canceled"
	TerminalSkipped    TerminalKind = "skipped"
)

func (v TerminalKind) Valid() bool {
	return v == TerminalImpossible || v == TerminalFailed || v == TerminalCanceled || v == TerminalSkipped
}

type PropagationState string

const (
	PropagationPending  PropagationState = "pending"
	PropagationComplete PropagationState = "complete"
)

func (v PropagationState) Valid() bool { return v == PropagationPending || v == PropagationComplete }

type SideEffectKind string

const (
	SideEffectCommand    SideEffectKind = "command"
	SideEffectAttempt    SideEffectKind = "attempt"
	SideEffectWait       SideEffectKind = "wait"
	SideEffectTimer      SideEffectKind = "timer"
	SideEffectContact    SideEffectKind = "contact"
	SideEffectObligation SideEffectKind = "obligation"
	SideEffectBlock      SideEffectKind = "block"
)

func (v SideEffectKind) Valid() bool {
	switch v {
	case SideEffectCommand, SideEffectAttempt, SideEffectWait, SideEffectTimer, SideEffectContact, SideEffectObligation, SideEffectBlock:
		return true
	}
	return false
}

type CommandState string

const (
	CommandIssued      CommandState = "issued"
	CommandReconciling CommandState = "reconciling"
	CommandObserved    CommandState = "observed"
	CommandReconciled  CommandState = "reconciled"
	CommandCanceled    CommandState = "canceled"
)

func (v CommandState) Valid() bool {
	switch v {
	case CommandIssued, CommandReconciling, CommandObserved, CommandReconciled, CommandCanceled:
		return true
	}
	return false
}
func (v CommandState) Active() bool { return v == CommandIssued || v == CommandReconciling }

type CommandKindV1 string

const (
	CommandInitializeRouting         CommandKindV1 = "initialize_routing_v1"
	CommandPerformAttempt            CommandKindV1 = "perform_attempt_v1"
	CommandSettleAttempt             CommandKindV1 = "settle_attempt_v1"
	CommandRoutePaths                CommandKindV1 = "route_paths_v1"
	CommandActivateGeneration        CommandKindV1 = "activate_generation_v1"
	CommandPropagateCandidateClosure CommandKindV1 = "propagate_candidate_closure_v1"
	CommandSettleDetachedSink        CommandKindV1 = "settle_detached_sink_v1"
	CommandInternDetachmentSet       CommandKindV1 = "intern_detachment_set_v1"
	CommandCompleteRun               CommandKindV1 = "complete_run_v1"
)

func (v CommandKindV1) Valid() bool {
	switch v {
	case CommandInitializeRouting, CommandPerformAttempt, CommandSettleAttempt, CommandRoutePaths, CommandActivateGeneration, CommandPropagateCandidateClosure, CommandSettleDetachedSink, CommandInternDetachmentSet, CommandCompleteRun:
		return true
	}
	return false
}
