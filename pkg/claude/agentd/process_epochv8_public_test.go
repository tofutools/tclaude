package agentd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/state/epochv8"
)

func TestEpochV8PublicStateOmitsRestrictedArtifacts(t *testing.T) {
	const sourceMarker = "private-source-marker"
	initial := epochV8TestCandidate(t, "initial")
	checkpoint, err := epochv8.Initialize("public-projection", initial, []epochv8.AuthoritySeed{{
		LocalID: "frontier", ReservationID: "reservation", NodeID: "work",
		Kind: epochv8.AuthorityFrontier, State: epochv8.AuthorityVerifiedUnclaimed,
	}})
	if err != nil {
		t.Fatal(err)
	}
	frontier := checkpoint.View().Authorities[0]
	reasonSum := sha256.Sum256([]byte("private-reason-marker"))
	reasonDigest := hex.EncodeToString(reasonSum[:])
	preview, err := epochv8.PreviewApply(checkpoint, epochv8.ApplyDraft{
		BaseBinding: checkpoint.Binding(), Candidate: epochV8TestCandidate(t, sourceMarker), ReasonDigest: reasonDigest,
		Handoffs: []epochv8.HandoffDirective{{
			Source: frontier.Identity, Action: epochv8.HandoffTransfer,
			TargetLocalID: "next", TargetReservationID: "next-reservation", TargetNodeID: "work",
		}},
	})
	if err != nil || preview.Plan == nil {
		t.Fatalf("preview: plan=%v err=%v", preview.Plan != nil, err)
	}
	applied, err := epochv8.Apply(checkpoint, preview.Plan)
	if err != nil || applied.Disposition != epochv8.DispositionApplied {
		t.Fatalf("apply: disposition=%q err=%v", applied.Disposition, err)
	}

	publicJSON, err := json.Marshal(epochV8PublicState(applied.Checkpoint))
	if err != nil {
		t.Fatal(err)
	}
	publicText := string(publicJSON)
	for _, restricted := range []string{"\"history\"", "\"diff\"", "\"reasonDigest\"", sourceMarker, reasonDigest} {
		if strings.Contains(publicText, restricted) {
			t.Fatalf("public state exposed restricted schema-8 material %q: %s", restricted, publicText)
		}
	}
}

func epochV8TestCandidate(t *testing.T, prompt string) *epochv8.TemplateCandidate {
	t.Helper()
	source := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: public-projection
start: start
nodes:
  start:
    type: start
    next: work
  work:
    type: task
    performer:
      kind: agent
      prompt: ` + prompt + `
    next:
      pass: done
  done:
    type: end
    result: completed
`)
	classification, err := epochv8.ClassifyTemplateSource(source)
	if err != nil || classification.Status != epochv8.EligibilitySupported {
		t.Fatalf("classify: status=%q reason=%q err=%v", classification.Status, classification.Reason, err)
	}
	return classification.Candidate()
}
