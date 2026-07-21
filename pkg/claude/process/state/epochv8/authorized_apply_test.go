package epochv8

import (
	"strings"
	"testing"
)

func testApplyAuthorization(directive string) ApplyAuthorization {
	return ApplyAuthorization{
		HandoffDirectiveDigest: directive,
		ReasonCode:             ApplyReasonUnlock,
		Actor:                  "agent:agt_authorized",
		AppliedAt:              "2026-07-21T08:00:00.123Z",
	}
}

func TestAuthorizedApplyBindsProvenanceAndExactReplayIdentity(t *testing.T) {
	checkpoint, err := Initialize("authorized-apply", supportedCandidate(t, "base"), []AuthoritySeed{{
		LocalID: "frontier", ReservationID: "reservation", NodeID: "start",
		Kind: AuthorityFrontier, State: AuthorityVerifiedUnclaimed,
	}})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := PreviewApply(checkpoint, ApplyDraft{
		BaseBinding: checkpoint.Binding(), Candidate: supportedCandidate(t, "next"),
		Handoffs: retainAll(checkpoint.View().ProtectedAuthorities),
	})
	if err != nil || preview.Plan == nil {
		t.Fatalf("preview: plan=%v err=%v", preview.Plan != nil, err)
	}
	authorization := testApplyAuthorization(strings.Repeat("a", 64))
	applied, err := ApplyAuthorized(checkpoint, preview.Plan, authorization)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Provenance != authorization {
		t.Fatalf("provenance = %+v", applied.Provenance)
	}
	record := applied.Checkpoint.wire.History[0].Apply
	if record == nil || applyAuthorization(*record) != authorization {
		t.Fatalf("record provenance = %+v", record)
	}

	committed, found, err := FindCommittedAuthorizedApply(
		applied.Checkpoint, preview.Plan.BaseBinding(), preview.Plan.ProposalDigest(),
		preview.Plan.CandidateEpoch().TemplateSourceDigest, "", authorization.HandoffDirectiveDigest,
	)
	if err != nil || !found || committed.Kind != "" || committed.Provenance != authorization {
		t.Fatalf("committed = %+v found=%v err=%v", committed, found, err)
	}
	if _, found, err := FindCommittedAuthorizedApply(
		applied.Checkpoint, preview.Plan.BaseBinding(), preview.Plan.ProposalDigest(),
		preview.Plan.CandidateEpoch().TemplateSourceDigest, "", strings.Repeat("b", 64),
	); err != nil || found {
		t.Fatalf("changed directive identity replayed: found=%v err=%v", found, err)
	}
	replayed, err := ApplyAuthorized(applied.Checkpoint, committed.Plan, testApplyAuthorization(authorization.HandoffDirectiveDigest))
	if err != nil || replayed.Disposition != DispositionReplayed || replayed.Provenance != authorization {
		t.Fatalf("replay = %+v err=%v", replayed, err)
	}
}

func TestAuthorizedApplyRejectsMissingOrAlternateCoherentlyRehashedProvenance(t *testing.T) {
	checkpoint, err := Initialize("authorized-rehash", supportedCandidate(t, "base"), []AuthoritySeed{{
		LocalID: "frontier", ReservationID: "reservation", NodeID: "start",
		Kind: AuthorityFrontier, State: AuthorityVerifiedUnclaimed,
	}})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := PreviewApply(checkpoint, ApplyDraft{
		BaseBinding: checkpoint.Binding(), Candidate: supportedCandidate(t, "next"),
		Handoffs: retainAll(checkpoint.View().ProtectedAuthorities),
	})
	if err != nil {
		t.Fatal(err)
	}
	authorization := testApplyAuthorization(strings.Repeat("c", 64))
	applied, err := ApplyAuthorized(checkpoint, preview.Plan, authorization)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*ApplyRecord)
	}{
		{"missing", func(record *ApplyRecord) {
			record.HandoffDirectiveDigest, record.ReasonCode, record.Actor, record.AppliedAt = "", "", "", ""
		}},
		{"alternate_reason_code", func(record *ApplyRecord) { record.ReasonCode = "operator_reason" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tampered := &CheckpointV8{wire: cloneWire(applied.Checkpoint.wire)}
			event := &tampered.wire.History[0]
			tc.mutate(event.Apply)
			event.Apply.RecordDigest, err = applyRecordDigest(*event.Apply)
			if err != nil {
				t.Fatal(err)
			}
			event.Digest, err = historyEventDigest(*event)
			if err != nil {
				t.Fatal(err)
			}
			tampered.wire.Digest, err = checkpointDigest(tampered.wire)
			if err != nil {
				t.Fatal(err)
			}
			_, found, findErr := FindCommittedAuthorizedApply(
				tampered, preview.Plan.BaseBinding(), preview.Plan.ProposalDigest(),
				preview.Plan.CandidateEpoch().TemplateSourceDigest, "", authorization.HandoffDirectiveDigest,
			)
			if findErr == nil && found {
				t.Fatal("authorized replay accepted missing/alternate provenance")
			}
		})
	}
}

func TestAuthorizedApplyRejectsTransferBeforeRuntimeGenesis(t *testing.T) {
	checkpoint, err := Initialize("authorized-pregen-transfer", supportedCandidate(t, "base"), []AuthoritySeed{{
		LocalID: "frontier", ReservationID: "reservation", NodeID: "start",
		Kind: AuthorityFrontier, State: AuthorityVerifiedUnclaimed,
	}})
	if err != nil {
		t.Fatal(err)
	}
	owner := checkpoint.View().ProtectedAuthorities[0]
	preview, err := PreviewApply(checkpoint, ApplyDraft{
		BaseBinding: checkpoint.Binding(), Candidate: supportedCandidate(t, "next"),
		Handoffs: []HandoffDirective{{
			Source: owner.Identity, Action: HandoffTransfer,
			TargetLocalID: "next-frontier", TargetReservationID: "next-reservation", TargetNodeID: "start",
		}},
	})
	if err != nil || preview.Plan == nil {
		t.Fatalf("preview: plan=%v err=%v", preview.Plan != nil, err)
	}
	preflight, err := PreflightRuntimeApply(t.Context(), checkpoint, nil, nil, testTemplateSource("next"), preview.Plan)
	if err != nil || preflight != RuntimeApplyRefused {
		t.Fatalf("preflight=%q err=%v", preflight, err)
	}
	if _, err := ApplyAuthorized(checkpoint, preview.Plan, testApplyAuthorization(strings.Repeat("f", 64))); err == nil {
		t.Fatal("authorized pre-genesis transfer was accepted")
	}
	attached, err := AttachGenesis(t.Context(), checkpoint, testTemplateSource("base"))
	if err != nil || attached.Artifact == nil {
		t.Fatalf("refusal stranded genesis: artifact=%v err=%v", attached.Artifact != nil, err)
	}
}

func TestAuthorizedRuntimeRetainAndTransferRecordUnlockReason(t *testing.T) {
	source0 := testTemplateSource("base")
	checkpoint, err := Initialize("authorized-runtime", supportedCandidate(t, "base"), []AuthoritySeed{{
		LocalID: "frontier", ReservationID: "reservation", NodeID: "start",
		Kind: AuthorityFrontier, State: AuthorityVerifiedUnclaimed,
	}})
	if err != nil {
		t.Fatal(err)
	}
	attached, err := AttachGenesis(t.Context(), checkpoint, source0)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := PreviewApply(attached.Checkpoint, ApplyDraft{
		BaseBinding: attached.Checkpoint.Binding(), Candidate: supportedCandidate(t, "retained"),
		Handoffs: retainAll(attached.Checkpoint.View().ProtectedAuthorities),
	})
	if err != nil {
		t.Fatal(err)
	}
	retainAuth := testApplyAuthorization(strings.Repeat("d", 64))
	retained, err := ApplyRetainHeadAuthorized(t.Context(), attached.Checkpoint, attached.ArtifactJSON, source0, preview.Plan, retainAuth)
	if err != nil || retained.Provenance.ReasonCode != ApplyReasonUnlock {
		t.Fatalf("retain provenance=%+v err=%v", retained.Provenance, err)
	}
	frontier := findAuthority(t, retained.Checkpoint.View().Authorities, func(authority AuthorityRecord) bool {
		return authority.Kind == AuthorityFrontier && authority.State == AuthorityVerifiedUnclaimed
	})
	source2 := testTemplateSource("transferred")
	transfer := testPlan(t, retained.Checkpoint, "transferred", frontier.Identity, "next-frontier", "next-reservation")
	transferAuth := testApplyAuthorization(strings.Repeat("e", 64))
	transferred, err := ApplyTransferHeadAuthorized(t.Context(), retained.Checkpoint, retained.ArtifactJSON, source0, source2, transfer, transferAuth)
	if err != nil || transferred.Provenance.ReasonCode != ApplyReasonUnlock {
		t.Fatalf("transfer provenance=%+v err=%v", transferred.Provenance, err)
	}
}
