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
