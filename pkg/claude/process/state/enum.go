package state

import "strings"

func (s RunStatus) IsValid() bool {
	switch s {
	case RunStatusPending,
		RunStatusRunning,
		RunStatusPaused,
		RunStatusBlocked,
		RunStatusCompleted,
		RunStatusFailed,
		RunStatusCanceled,
		RunStatusInconsistent,
		RunStatusDirty:
		return true
	default:
		return false
	}
}

func (k PauseKind) IsValid() bool {
	switch k {
	case PauseKindRateLimited, PauseKindNeedsReconcile:
		return true
	default:
		return false
	}
}

func (s NodeStatus) IsValid() bool {
	switch s {
	case NodeStatusPending,
		NodeStatusReady,
		NodeStatusRunning,
		NodeStatusWaitingHuman,
		NodeStatusWaitingAgent,
		NodeStatusWaitingProgram,
		NodeStatusWaitingTimer,
		NodeStatusWaitingSignal,
		NodeStatusBlocked,
		NodeStatusCompleted,
		NodeStatusFailed,
		NodeStatusSkipped:
		return true
	default:
		return false
	}
}

func (s CommandStatus) IsValid() bool {
	switch s {
	case CommandStatusIssued,
		CommandStatusObserved,
		CommandStatusReconciled,
		CommandStatusCanceled:
		return true
	default:
		return false
	}
}

func (k CommandKind) IsValid() bool {
	switch k {
	case CommandKindActivateNode,
		CommandKindExpandNode,
		CommandKindStartAttempt,
		CommandKindSettleAttempt,
		CommandKindRecordDecision,
		CommandKindShortCircuit,
		CommandKindGateFeedback,
		CommandKindBlockNode,
		CommandKindResolveBlock,
		CommandKindSetTimer,
		CommandKindWaitSignal,
		CommandKindCompleteRun:
		return true
	default:
		return false
	}
}

func (s WaitStatus) IsValid() bool {
	switch s {
	case WaitStatusPending,
		WaitStatusSatisfied,
		WaitStatusCanceled:
		return true
	default:
		return false
	}
}

func (k WaitKind) IsValid() bool {
	switch k {
	case WaitKindHuman,
		WaitKindAgent,
		WaitKindProgram,
		WaitKindTimer,
		WaitKindSignal:
		return true
	default:
		return false
	}
}

// ContactKindForOwner maps the typed owner namespace onto the existing
// per-kind contact policy axis. Block ownership is currently produced as
// human:operator, but keeping this mapping complete prevents the blocked path
// from baking in that present-day default.
func ContactKindForOwner(owner string) (WaitKind, bool) {
	owner = strings.TrimSpace(owner)
	switch {
	case strings.HasPrefix(owner, "human:"), strings.HasPrefix(owner, "role:"):
		return WaitKindHuman, true
	case strings.HasPrefix(owner, "agent:"):
		return WaitKindAgent, true
	case strings.HasPrefix(owner, "program:"), strings.HasPrefix(owner, "system:"), strings.HasPrefix(owner, "engine:"):
		return WaitKindProgram, true
	default:
		return "", false
	}
}
