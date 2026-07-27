package harness

// SandboxCapabilityError is the typed, actionable refusal an adapter returns
// when a harness cannot faithfully enforce a requested sandbox policy.
//
// A harness that cannot represent the requested posture must fail loudly rather
// than approximate it: an operator who denied their home directory and reopened
// only a workspace, and then silently received today's broad read access, would
// believe in isolation that does not exist. Kind is stable wire vocabulary for
// the daemon's HTTP error code.
type SandboxCapabilityError struct {
	Harness string
	Kind    string
	Message string
}

func (e *SandboxCapabilityError) Error() string { return e.Message }
