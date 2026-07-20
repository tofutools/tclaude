package epochv8

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

const identityPrefix = "tclaude.process/epoch-v8/"

var identifierPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

func digestValue(tag string, value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, _ = h.Write([]byte(identityPrefix))
	_, _ = h.Write([]byte(tag))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(encoded)
	return hex.EncodeToString(h.Sum(nil)), nil
}

func canonicalDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func validIdentifier(value string) bool {
	return len(value) > 0 && len(value) <= MaxIdentifierBytes &&
		value == strings.TrimSpace(value) && identifierPattern.MatchString(value)
}

func canonicalTemplateRef(value string) bool {
	if strings.Count(value, "@sha256:") != 1 {
		return false
	}
	id, digest, ok := strings.Cut(value, "@sha256:")
	return ok && validIdentifier(id) && canonicalDigest(digest)
}

func productionCapabilities() []Capability {
	return []Capability{CapabilityFoundationV1, CapabilityParallelAllV1, CapabilityParallelAnyV1}
}

func capabilitiesValid(values []Capability, exactProduction bool) bool {
	if !slices.IsSorted(values) || len(slices.Compact(slices.Clone(values))) != len(values) {
		return false
	}
	for _, capability := range values {
		switch capability {
		case CapabilityFoundationV1, CapabilityParallelAllV1, CapabilityParallelAnyV1:
		default:
			return false
		}
	}
	return !exactProduction || slices.Equal(values, productionCapabilities())
}

func graphDigest(graph EpochGraph) (string, error) {
	graph.Digest = ""
	return digestValue("graph/v1", graph)
}

func epochIdentity(runID string, epoch TemplateEpoch) (EpochID, error) {
	value := struct {
		RunID                string       `json:"runId"`
		Ordinal              uint64       `json:"ordinal"`
		PredecessorEpochID   EpochID      `json:"predecessorEpochId,omitempty"`
		TemplateRef          string       `json:"templateRef"`
		TemplateSourceDigest string       `json:"templateSourceDigest"`
		RequiredCapabilities []Capability `json:"requiredCapabilities"`
		GraphDigest          string       `json:"graphDigest"`
	}{runID, epoch.Ordinal, epoch.PredecessorEpochID, epoch.TemplateRef, epoch.TemplateSourceDigest, epoch.RequiredCapabilities, epoch.Graph.Digest}
	digest, err := digestValue("epoch/v1", value)
	return EpochID(digest), err
}

func authorityIdentity(runID string, authority AuthorityRecord) (OwnerIdentity, error) {
	value := struct {
		RunID         string        `json:"runId"`
		EpochID       EpochID       `json:"epochId"`
		LocalID       string        `json:"localId"`
		ReservationID string        `json:"reservationId"`
		NodeID        string        `json:"nodeId"`
		Kind          AuthorityKind `json:"kind"`
	}{runID, authority.EpochID, authority.LocalID, authority.ReservationID, authority.NodeID, authority.Kind}
	digest, err := digestValue("owner-identity/v1", value)
	return OwnerIdentity(digest), err
}

func anchorDigest(anchor InitializationAnchor) (string, error) {
	anchor.Digest = ""
	return digestValue("initialization-anchor/v1", anchor)
}

func diffDigest(diff Diff) (string, error) {
	diff.Digest = ""
	return digestValue("diff/v1", diff)
}

func protectedDigest(authorities []AuthorityRecord) (string, error) {
	return digestValue("protected-authority-closure/v1", authorities)
}

func handoffIdentity(source OwnerIdentity, action HandoffAction, target *AuthorityRecord, proposalBasis string) (string, error) {
	return digestValue("handoff/v1", struct {
		Source        OwnerIdentity    `json:"source"`
		Action        HandoffAction    `json:"action"`
		Target        *AuthorityRecord `json:"target,omitempty"`
		ProposalBasis string           `json:"proposalBasis"`
	}{source, action, target, proposalBasis})
}

func handoffSetDigest(handoffs []Handoff) (string, error) {
	return digestValue("handoff-set/v1", handoffs)
}

func proposalDigest(core applyCore) (string, error) {
	core.ProposalDigest = ""
	return digestValue("apply-proposal/v1", core)
}

func applyRecordDigest(record ApplyRecord) (string, error) {
	record.RecordDigest = ""
	return digestValue("apply-record/v1", record)
}

func finishIdentity(receipt FinishReceipt) (string, error) {
	receipt.ID = ""
	return digestValue("finish-claimed/v1", receipt)
}

func historyEventDigest(event HistoryEvent) (string, error) {
	event.Digest = ""
	return digestValue("history-event/v1", event)
}

func checkpointDigest(wire checkpointWire) (string, error) {
	wire.Digest = ""
	return digestValue("checkpoint/v1", wire)
}

func (binding Binding) validate() error {
	if !canonicalDigest(binding.Digest) {
		return fmt.Errorf("%w: checkpoint binding digest is not canonical", ErrInvalid)
	}
	return nil
}
