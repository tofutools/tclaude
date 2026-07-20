package epochv8

import (
	"bytes"
	"testing"
)

func FuzzDecodeCheckpointV8(f *testing.F) {
	classification, err := ClassifyTemplateSource(testTemplateSource("fuzz-checkpoint"))
	if err != nil || classification.Candidate() == nil {
		f.Fatalf("build fuzz candidate: classification=%+v err=%v", classification, err)
	}
	checkpoint, err := Initialize("fuzz-run", classification.Candidate(), []AuthoritySeed{{
		LocalID: "frontier", ReservationID: "frontier-r", NodeID: "work", Kind: AuthorityFrontier, State: AuthorityVerifiedUnclaimed,
	}})
	if err != nil {
		f.Fatal(err)
	}
	seed, err := EncodeCheckpointV8(checkpoint)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add([]byte(`{"stateSchemaVersion":8}`))
	f.Add([]byte{0xff, 0xfe, 0xfd})
	f.Fuzz(func(t *testing.T, data []byte) {
		checkpoint, err := DecodeCheckpointV8(data)
		if err != nil {
			return
		}
		if err := VerifyCheckpointV8(checkpoint); err != nil {
			t.Fatalf("decoder returned unverified checkpoint: %v", err)
		}
		encoded, err := EncodeCheckpointV8(checkpoint)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(encoded, data) {
			t.Fatal("successful decode changed canonical bytes")
		}
	})
}

func FuzzDecodeApplyPlan(f *testing.F) {
	checkpoint := testCheckpointForFuzz(f)
	frontier := checkpoint.View().Authorities[0]
	directives := []HandoffDirective{{
		Source: frontier.Identity, Action: HandoffTransfer, TargetLocalID: "next", TargetReservationID: "next-r", TargetNodeID: "work",
	}}
	classification, _ := ClassifyTemplateSource(testTemplateSource("fuzz-plan-next"))
	preview, err := PreviewApply(checkpoint, ApplyDraft{
		BaseBinding: checkpoint.Binding(), Candidate: classification.Candidate(),
		ReasonDigest: testDigest("fuzz-reason"), Handoffs: directives,
	})
	if err != nil || preview.Plan == nil {
		f.Fatalf("build fuzz plan: preview=%+v err=%v", preview, err)
	}
	seed, err := EncodeApplyPlan(preview.Plan)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add([]byte(`{}`))
	f.Add([]byte("null\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		plan, err := DecodeApplyPlan(data)
		if err != nil {
			return
		}
		encoded, err := EncodeApplyPlan(plan)
		if err != nil {
			t.Fatalf("decoder returned invalid plan: %v", err)
		}
		if !bytes.Equal(encoded, data) {
			t.Fatal("successful plan decode changed canonical bytes")
		}
	})
}

type fuzzTesting interface {
	Helper()
	Fatal(args ...any)
}

func testCheckpointForFuzz(t fuzzTesting) *CheckpointV8 {
	t.Helper()
	classification, err := ClassifyTemplateSource(testTemplateSource("fuzz-base"))
	if err != nil || classification.Candidate() == nil {
		t.Fatal("build fuzz base candidate")
	}
	checkpoint, err := Initialize("fuzz-run", classification.Candidate(), []AuthoritySeed{{
		LocalID: "frontier", ReservationID: "frontier-r", NodeID: "work", Kind: AuthorityFrontier, State: AuthorityVerifiedUnclaimed,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return checkpoint
}
