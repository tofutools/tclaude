package state

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
