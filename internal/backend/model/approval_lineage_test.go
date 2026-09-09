package model

import "testing"

func TestApprovalPostureInvalidEvidenceCannotAuthorizeBaseline(t *testing.T) {
	baseline := ApprovalPosture{Known: true}
	for _, parent := range []ApprovalPosture{{}, {Known: true, Minimum: AutomaticCommands}, {Known: true, Minimum: 128, Maximum: 128}, {Known: true, Maximum: 128}} {
		if parent.Allows(baseline) {
			t.Fatalf("invalid evidence authorized baseline: %+v", parent)
		}
		if baseline.Allows(parent) {
			t.Fatalf("baseline authorized invalid evidence: %+v", parent)
		}
	}
}
