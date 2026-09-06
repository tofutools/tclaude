package execution

import "testing"

func TestResumeTransitions(t *testing.T) {
	valid := []struct{ from, to ResumeState }{
		{ResumeRequested, ResumeAccepted}, {ResumeRequested, ResumeRejected},
		{ResumeAccepted, ResumeStarted}, {ResumeAccepted, ResumeReady},
		{ResumeAccepted, ResumeFailed}, {ResumeAccepted, ResumeUnknown},
		{ResumeStarted, ResumeReady}, {ResumeStarted, ResumeUnknown},
		{ResumeUnknown, ResumeStarted}, {ResumeUnknown, ResumeReady},
		{ResumeUnknown, ResumeFailed}, {ResumeUnknown, ResumeCancelling},
		{ResumeCancelling, ResumeCancelled}, {ResumeCancelling, ResumeFailed},
	}
	for _, tt := range valid {
		if !CanTransition(tt.from, tt.to) {
			t.Errorf("CanTransition(%q, %q) = false", tt.from, tt.to)
		}
	}
	invalid := []struct{ from, to ResumeState }{
		{ResumeRejected, ResumeReady}, {ResumeFailed, ResumeReady},
		{ResumeCancelled, ResumeReady}, {ResumeUnknown, ResumeAccepted},
	}
	for _, tt := range invalid {
		if CanTransition(tt.from, tt.to) {
			t.Errorf("CanTransition(%q, %q) = true", tt.from, tt.to)
		}
	}
}

func TestValidateTransitionRequiresRevisionAndExecutionForReady(t *testing.T) {
	op := ResumeOperation{ID: NewOperationID(), State: ResumeUnknown, Revision: 3}
	if err := ValidateTransition(op, ResumeReady, 3); err == nil {
		t.Fatal("ready without intended execution must fail")
	}
	op.Attempt.ExecutionID = ID("11111111111111111111111111111111")
	if err := ValidateTransition(op, ResumeReady, 2); err == nil {
		t.Fatal("stale revision must fail")
	}
	if err := ValidateTransition(op, ResumeReady, 3); err != nil {
		t.Fatalf("exact ready transition: %v", err)
	}
}

func TestOperationIDValidation(t *testing.T) {
	if err := ValidateOperationID(NewOperationID()); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []OperationID{"", "op_short", "op_ABCDEF0123456789ABCDEF0123456789"} {
		if err := ValidateOperationID(raw); err == nil {
			t.Errorf("ValidateOperationID(%q) unexpectedly succeeded", raw)
		}
	}
}
