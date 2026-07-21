package agentd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	processexec "github.com/tofutools/tclaude/pkg/claude/process/exec"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/state/epochv8"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	processverify "github.com/tofutools/tclaude/pkg/claude/process/verify"
	processview "github.com/tofutools/tclaude/pkg/claude/process/view"
	"github.com/tofutools/tclaude/pkg/claude/process/worklist"
)

const (
	maxEpochV8PreviewWireBytes = 32 << 20
	maxEpochV8PreviewHandoffs  = 256
	maxEpochV8SettlementWire   = 1 << 20
	maxEpochV8SettlementText   = 64 << 10
	epochV8ApplyLeaseTTL       = 2 * time.Minute
)

type epochV8BindingDTO struct {
	Revision uint64 `json:"revision"`
	Digest   string `json:"digest"`
}

func bindingDTO(binding epochv8.Binding) epochV8BindingDTO {
	return epochV8BindingDTO{Revision: binding.Revision, Digest: binding.Digest}
}

func (binding epochV8BindingDTO) engine() epochv8.Binding {
	return epochv8.Binding{Revision: binding.Revision, Digest: binding.Digest}
}

type epochV8PreviewTargetRequest struct {
	LocalID       string `json:"localId"`
	ReservationID string `json:"reservationId"`
	NodeID        string `json:"nodeId"`
}

type epochV8PreviewHandoffRequest struct {
	Token  string                       `json:"token"`
	Action epochv8.HandoffAction        `json:"action"`
	Target *epochV8PreviewTargetRequest `json:"target,omitempty"`
}

type epochV8PreviewRequest struct {
	BaseBinding     epochV8BindingDTO              `json:"baseBinding"`
	CandidateSource string                         `json:"candidateSource"`
	Reason          *string                        `json:"reason,omitempty"`
	Handoffs        []epochV8PreviewHandoffRequest `json:"handoffs"`
}

type epochV8GraphTotalsDTO struct {
	Nodes int `json:"nodes"`
	Edges int `json:"edges"`
}

type epochV8GraphSummaryDTO struct {
	Current   epochV8GraphTotalsDTO `json:"current"`
	Candidate epochV8GraphTotalsDTO `json:"candidate"`
	Changed   bool                  `json:"changed"`
}

// epochV8LineageEntryDTO carries refs and ordinals only. EpochID is the
// opaque epoch digest: it names the epoch for the permission-gated exact
// artifact route and reveals no content by itself.
type epochV8LineageEntryDTO struct {
	Ordinal            uint64          `json:"ordinal"`
	PredecessorOrdinal *uint64         `json:"predecessorOrdinal,omitempty"`
	TemplateRef        string          `json:"templateRef"`
	EpochID            epochv8.EpochID `json:"epochId,omitempty"`
}

type epochV8LineageDTO struct {
	OriginalTemplateRef string                   `json:"originalTemplateRef"`
	CurrentTemplateRef  string                   `json:"currentTemplateRef"`
	TotalEpochs         int                      `json:"totalEpochs"`
	Truncated           bool                     `json:"truncated,omitempty"`
	Epochs              []epochV8LineageEntryDTO `json:"epochs"`
}

type epochV8AuthorityStateCountsDTO struct {
	VerifiedUnclaimed int `json:"verifiedUnclaimed"`
	Claimed           int `json:"claimed"`
	Active            int `json:"active"`
	Completed         int `json:"completed"`
	Failed            int `json:"failed"`
	Canceled          int `json:"canceled"`
	HandedOff         int `json:"handedOff"`
}

type epochV8AuthorityCountsDTO struct {
	Total    int                            `json:"total"`
	Active   int                            `json:"active"`
	Terminal int                            `json:"terminal"`
	States   epochV8AuthorityStateCountsDTO `json:"states"`
}

// epochV8StructuralSummaryDTO is the bounded safe shape of the current epoch:
// graph totals only, never node identities or topology.
type epochV8StructuralSummaryDTO struct {
	Nodes               int  `json:"nodes"`
	Edges               int  `json:"edges"`
	ChangedFromOriginal bool `json:"changedFromOriginal"`
}

// epochV8BlockerDTO names one preview blocker. The descriptor fields are the
// bounded safe affordance surface: owner epoch ordinal, safe kind/state
// class, the node, and which handoff actions are legal. The UI must render
// affordances from these fields, never by inferring from code/token alone.
// Authority identities never leave the daemon; Token stays the opaque
// binding-scoped handoff token.
type epochV8BlockerDTO struct {
	Code              epochv8.BlockerCode `json:"code"`
	Token             string              `json:"token,omitempty"`
	NodeID            string              `json:"nodeId,omitempty"`
	OwnerEpochOrdinal *uint64             `json:"ownerEpochOrdinal,omitempty"`
	KindClass         string              `json:"kindClass,omitempty"`
	StateClass        string              `json:"stateClass,omitempty"`
	HandoffClass      string              `json:"handoffClass,omitempty"`
	AllowedActions    []string            `json:"allowedActions,omitempty"`
}

// epochV8DescribeBlocker fills the safe descriptor for a blocker bound to one
// authority. Transferable means what the engine itself accepts: a bare
// verified-unclaimed frontier; everything else is retain-only protected work.
func epochV8DescribeBlocker(view epochv8.CheckpointView, authorityID epochv8.OwnerIdentity, dto *epochV8BlockerDTO) {
	ordinals := make(map[epochv8.EpochID]uint64, len(view.Epochs))
	for _, epoch := range view.Epochs {
		ordinals[epoch.ID] = epoch.Ordinal
	}
	for _, authority := range view.Authorities {
		if authority.Identity != authorityID {
			continue
		}
		ordinal := ordinals[authority.EpochID]
		dto.NodeID = authority.NodeID
		dto.OwnerEpochOrdinal = &ordinal
		dto.KindClass = string(authority.Kind)
		dto.StateClass = string(authority.State)
		dto.HandoffClass = "retain_only"
		dto.AllowedActions = []string{string(epochv8.HandoffRetain)}
		if authority.Kind == epochv8.AuthorityFrontier && authority.State == epochv8.AuthorityVerifiedUnclaimed {
			dto.HandoffClass = "transferable"
			dto.AllowedActions = append(dto.AllowedActions, string(epochv8.HandoffTransfer))
		}
		return
	}
}

type epochV8GuidanceDTO struct {
	Action            string   `json:"action"`
	Permission        string   `json:"permission"`
	Token             string   `json:"token"`
	Preconditions     []string `json:"preconditions"`
	RepreviewRequired bool     `json:"repreviewRequired"`
}

type epochV8PreviewResponse struct {
	Status          string                    `json:"status"`
	BaseBinding     epochV8BindingDTO         `json:"baseBinding"`
	CurrentBinding  epochV8BindingDTO         `json:"currentBinding"`
	Classification  string                    `json:"classification,omitempty"`
	GraphSummary    epochV8GraphSummaryDTO    `json:"graphSummary"`
	Lineage         epochV8LineageDTO         `json:"lineage"`
	AuthorityCounts epochV8AuthorityCountsDTO `json:"authorityCounts"`
	Blockers        []epochV8BlockerDTO       `json:"blockers,omitempty"`
	Guidance        *epochV8GuidanceDTO       `json:"guidance,omitempty"`
	ApplyToken      string                    `json:"applyToken,omitempty"`
}

func handleProcessEpochV8Preview(w http.ResponseWriter, r *http.Request) {
	setProcessNoStoreHeaders(w)
	r.Body = http.MaxBytesReader(w, r.Body, maxEpochV8PreviewWireBytes)
	var body epochV8PreviewRequest
	if err := decodeOneStrictJSON(r.Body, &body); err != nil {
		writeEpochV8DecodeError(w, err)
		return
	}
	if len(body.CandidateSource) == 0 || len(body.CandidateSource) > model.MaxProcessTemplateSourceBytes || len(body.Handoffs) > maxEpochV8PreviewHandoffs || body.Reason != nil && len(*body.Reason) > store.EpochV8MaxReasonBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "process_preview_budget", "process preview exceeds a request budget")
		return
	}
	if !lowerHexDigest(body.BaseBinding.Digest) {
		writeError(w, http.StatusUnprocessableEntity, "process_preview_invalid", "process preview binding is invalid")
		return
	}
	classification, err := epochv8.ClassifyTemplateSource([]byte(body.CandidateSource))
	if err != nil || classification.Candidate() == nil {
		writeError(w, http.StatusUnprocessableEntity, "process_preview_unsupported", "candidate is not supported by schema 8")
		return
	}
	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_preview_unavailable", "process preview is unavailable")
		return
	}
	var response epochV8PreviewResponse
	err = fs.WithEpochV8ExecutionView(r.Context(), r.PathValue("id"), func(view store.EpochV8ExecutionView) error {
		return buildEpochV8Preview(r, view, body, classification.Candidate(), &response)
	})
	if err != nil {
		if errors.Is(err, errEpochV8PreviewStale) {
			writeProcessJSON(w, http.StatusConflict, struct {
				Status         string            `json:"status"`
				CurrentBinding epochV8BindingDTO `json:"currentBinding"`
			}{"stale", response.CurrentBinding})
			return
		}
		writeEpochV8PreviewError(w, err)
		return
	}
	status := http.StatusOK
	if response.Status == "blocked" {
		status = http.StatusUnprocessableEntity
	}
	writeProcessJSON(w, status, response)
}

var errEpochV8PreviewStale = errors.New("schema-8 preview binding is stale")

func buildEpochV8Preview(r *http.Request, view store.EpochV8ExecutionView, body epochV8PreviewRequest, candidate *epochv8.TemplateCandidate, response *epochV8PreviewResponse) error {
	current := view.Checkpoint.Binding()
	if body.BaseBinding.engine() != current {
		response.CurrentBinding = bindingDTO(current)
		return errEpochV8PreviewStale
	}
	directives := make([]epochv8.HandoffDirective, 0, len(body.Handoffs))
	seen := make(map[string]struct{}, len(body.Handoffs))
	for _, handoff := range body.Handoffs {
		if !lowerHexDigest(handoff.Token) {
			return epochv8.ErrInvalid
		}
		if _, duplicate := seen[handoff.Token]; duplicate {
			return epochv8.ErrInvalid
		}
		seen[handoff.Token] = struct{}{}
		owner, err := epochv8.ResolveHandoffToken(view.Checkpoint, handoff.Token)
		if err != nil {
			return err
		}
		directive := epochv8.HandoffDirective{Source: owner, Action: handoff.Action}
		if handoff.Target != nil {
			if len(handoff.Target.LocalID) > epochv8.MaxIdentifierBytes || len(handoff.Target.ReservationID) > epochv8.MaxIdentifierBytes || len(handoff.Target.NodeID) > epochv8.MaxIdentifierBytes {
				return epochv8.ErrOverBudget
			}
			if !boundedEpochV8Identifier(handoff.Target.LocalID) || !boundedEpochV8Identifier(handoff.Target.ReservationID) || !boundedEpochV8Identifier(handoff.Target.NodeID) {
				return epochv8.ErrInvalid
			}
			directive.TargetLocalID, directive.TargetReservationID, directive.TargetNodeID = handoff.Target.LocalID, handoff.Target.ReservationID, handoff.Target.NodeID
		}
		directives = append(directives, directive)
	}
	reasonDigest := ""
	if body.Reason != nil {
		digest := sha256.Sum256([]byte(*body.Reason))
		reasonDigest = hex.EncodeToString(digest[:])
	}
	preview, err := epochv8.PreviewApply(view.Checkpoint, epochv8.ApplyDraft{BaseBinding: current, Candidate: candidate, ReasonDigest: reasonDigest, Handoffs: directives})
	if err != nil {
		return err
	}
	*response = epochV8PreviewResponse{Status: "valid", BaseBinding: bindingDTO(body.BaseBinding.engine()), CurrentBinding: bindingDTO(current), GraphSummary: epochV8GraphSummary(view.Checkpoint, candidate), Lineage: epochV8Lineage(view.Checkpoint), AuthorityCounts: epochV8AuthorityCounts(view.Checkpoint)}
	if len(preview.Blockers) != 0 {
		response.Status = "blocked"
		response.Blockers = make([]epochV8BlockerDTO, 0, len(preview.Blockers))
		checkpointView := view.Checkpoint.View()
		for _, blocker := range preview.Blockers {
			projected := epochV8BlockerDTO{Code: blocker.Code}
			if blocker.AuthorityID != "" {
				var tokenErr error
				projected.Token, tokenErr = epochv8.HandoffToken(view.Checkpoint, blocker.AuthorityID)
				if tokenErr != nil {
					return tokenErr
				}
				epochV8DescribeBlocker(checkpointView, blocker.AuthorityID, &projected)
			}
			response.Blockers = append(response.Blockers, projected)
		}
		return nil
	}
	if _, err := epochv8.EncodeApplyPlan(preview.Plan); err != nil {
		return err
	}
	response.ApplyToken = preview.Plan.ProposalDigest()
	ownerSource := []byte(nil)
	if view.Runtime != nil {
		ownerSource = view.EpochSources[view.Runtime.EpochID]
	}
	apply, err := epochv8.PreflightRuntimeApply(r.Context(), view.Checkpoint, view.RuntimeJSON, ownerSource, []byte(body.CandidateSource), preview.Plan)
	if err != nil {
		return err
	}
	if view.Runtime == nil && apply == epochv8.RuntimeApplyRefused {
		return epochv8.ErrInvalid
	}
	if apply == epochv8.RuntimeApplyTransferReady {
		response.Classification = "rescues_now"
		return nil
	}
	settlement := pathv1.AuditedSettlementInput{Decision: "retry", Actor: "human:operator", Reason: "preview", EvidenceRef: "preview", Timestamp: time.Unix(1, 0).UTC()}
	if preflight, preflightErr := processexec.PreflightEpochV8AuditedSettlement(r.Context(), view, "", settlement); preflightErr == nil {
		response.Classification = "waits_for_settlement"
		response.Guidance = &epochV8GuidanceDTO{Action: "audited_retry", Permission: PermProcessAdvance, Token: preflight.Token, Preconditions: []string{"outer_binding_unchanged", "runtime_binding_unchanged", "unique_failed_performer_generation"}, RepreviewRequired: true}
		return nil
	}
	response.Classification = "cannot_affect_without_later_intervention"
	return nil
}

type epochV8ApplyRequest struct {
	BaseBinding     epochV8BindingDTO              `json:"baseBinding"`
	ApplyToken      string                         `json:"applyToken"`
	CandidateSource string                         `json:"candidateSource"`
	Reason          *string                        `json:"reason,omitempty"`
	Handoffs        []epochV8PreviewHandoffRequest `json:"handoffs"`
}

type epochV8ApplyResponse struct {
	Status         string            `json:"status"`
	Disposition    string            `json:"disposition"`
	ApplyToken     string            `json:"applyToken"`
	EpochID        epochv8.EpochID   `json:"epochId"`
	CurrentBinding epochV8BindingDTO `json:"currentBinding"`
	ReasonCode     string            `json:"reasonCode"`
	Actor          string            `json:"actor"`
	AppliedAt      string            `json:"appliedAt"`
}

var errEpochV8ApplyStale = errors.New("schema-8 apply binding is stale")

func handleProcessEpochV8Apply(w http.ResponseWriter, r *http.Request) {
	setProcessNoStoreHeaders(w)
	caller, ok := requirePermission(w, r, PermProcessRunsUnlock)
	if !ok {
		return
	}
	actor, err := processTemplateAuthor(caller)
	if err != nil {
		writeError(w, http.StatusForbidden, "forbidden", "caller has no stable apply identity")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxEpochV8PreviewWireBytes)
	var body epochV8ApplyRequest
	if err := decodeOneStrictJSON(r.Body, &body); err != nil {
		writeEpochV8DecodeError(w, err)
		return
	}
	if len(body.CandidateSource) == 0 || len(body.CandidateSource) > model.MaxProcessTemplateSourceBytes ||
		len(body.Handoffs) > maxEpochV8PreviewHandoffs || body.Reason != nil && len(*body.Reason) > store.EpochV8MaxReasonBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "process_unlock_budget", "process unlock exceeds a request budget")
		return
	}
	if !lowerHexDigest(body.BaseBinding.Digest) || !lowerHexDigest(body.ApplyToken) {
		writeError(w, http.StatusUnprocessableEntity, "process_unlock_invalid", "process unlock binding or token is invalid")
		return
	}
	handoffDigest, err := epochV8HandoffDirectiveDigest(body.Handoffs)
	if err != nil {
		writeEpochV8ApplyError(w, err, epochV8BindingDTO{})
		return
	}
	classification, err := epochv8.ClassifyTemplateSource([]byte(body.CandidateSource))
	if err != nil || classification.Candidate() == nil {
		writeError(w, http.StatusUnprocessableEntity, "process_unlock_unsupported", "candidate is not supported by schema 8")
		return
	}
	reason := []byte(nil)
	reasonDigest := ""
	if body.Reason != nil {
		reason = []byte(*body.Reason)
		digest := sha256.Sum256(reason)
		reasonDigest = hex.EncodeToString(digest[:])
	}
	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_unlock_unavailable", "process unlock is unavailable")
		return
	}
	lease, err := fs.AcquireMaintenanceLease(r.Context(), r.PathValue("id"), processEngineHolder()+":unlock", epochV8ApplyLeaseTTL)
	if err != nil {
		writeEpochV8ApplyError(w, err, epochV8BindingDTO{})
		return
	}
	defer func() { _ = fs.ReleaseMaintenanceLease(context.WithoutCancel(r.Context()), lease) }()

	committed, found, err := fs.VerifyCommittedEpochV8Apply(
		r.Context(), lease, body.BaseBinding.engine(), body.ApplyToken,
		[]byte(body.CandidateSource), reason, handoffDigest,
	)
	if err != nil {
		writeEpochV8ApplyError(w, err, epochV8BindingDTO{})
		return
	}
	if found {
		setAuditDetail(r, "reason_code="+epochv8.ApplyReasonUnlock+";disposition="+string(epochv8.DispositionReplayed)+";revision="+strconv.FormatUint(committed.Binding.Revision, 10))
		writeProcessJSON(w, http.StatusOK, epochV8ApplyResponse{
			Status: "already_applied", Disposition: string(epochv8.DispositionReplayed), ApplyToken: body.ApplyToken,
			EpochID: committed.EpochID, CurrentBinding: bindingDTO(committed.Binding),
			ReasonCode: committed.Provenance.ReasonCode, Actor: committed.Provenance.Actor, AppliedAt: committed.Provenance.AppliedAt,
		})
		return
	}

	requestedAuthorization := epochv8.ApplyAuthorization{
		HandoffDirectiveDigest: handoffDigest,
		ReasonCode:             epochv8.ApplyReasonUnlock,
		Actor:                  string(actor),
		AppliedAt:              time.Now().UTC().Format(time.RFC3339Nano),
	}
	var plan *epochv8.ApplyPlan
	var kind epochv8.RuntimeTransitionKind
	var currentBinding epochv8.Binding
	err = fs.WithEpochV8ExecutionView(r.Context(), r.PathValue("id"), func(view store.EpochV8ExecutionView) error {
		currentBinding = view.Checkpoint.Binding()
		if currentBinding != body.BaseBinding.engine() {
			return errEpochV8ApplyStale
		}
		directives, directiveErr := epochV8ResolveHandoffDirectives(view.Checkpoint, body.Handoffs)
		if directiveErr != nil {
			return directiveErr
		}
		preview, previewErr := epochv8.PreviewApply(view.Checkpoint, epochv8.ApplyDraft{
			BaseBinding: currentBinding, Candidate: classification.Candidate(), ReasonDigest: reasonDigest, Handoffs: directives,
		})
		if previewErr != nil {
			return previewErr
		}
		if preview.Plan == nil || len(preview.Blockers) != 0 || preview.Plan.ProposalDigest() != body.ApplyToken {
			return epochv8.ErrInvalid
		}
		plan = preview.Plan
		if view.Runtime == nil {
			kind = ""
			return nil
		}
		ownerSource := view.EpochSources[view.Runtime.EpochID]
		preflight, preflightErr := epochv8.PreflightRuntimeApply(r.Context(), view.Checkpoint, view.RuntimeJSON, ownerSource, []byte(body.CandidateSource), plan)
		if preflightErr != nil {
			return preflightErr
		}
		switch preflight {
		case epochv8.RuntimeApplyRetainReady:
			kind = epochv8.RuntimeApplyRetain
		case epochv8.RuntimeApplyTransferReady:
			kind = epochv8.RuntimeApplyTransfer
		default:
			return epochv8.ErrInvalid
		}
		return nil
	})
	if err != nil {
		writeEpochV8ApplyError(w, err, bindingDTO(currentBinding))
		return
	}

	var disposition epochv8.Disposition
	var binding epochv8.Binding
	var provenance epochv8.ApplyAuthorization
	switch kind {
	case "":
		published, publishErr := fs.PublishEpochV8Authorized(r.Context(), lease, plan, []byte(body.CandidateSource), reason, requestedAuthorization)
		if publishErr != nil {
			writeEpochV8ApplyError(w, publishErr, bindingDTO(currentBinding))
			return
		}
		disposition, binding, provenance = published.Disposition, published.Binding, published.Provenance
	case epochv8.RuntimeApplyRetain:
		published, publishErr := fs.PublishEpochV8RetainAuthorized(r.Context(), lease, plan, []byte(body.CandidateSource), reason, requestedAuthorization)
		if publishErr != nil {
			writeEpochV8ApplyError(w, publishErr, bindingDTO(currentBinding))
			return
		}
		disposition, binding, provenance = published.Disposition, published.Binding, published.Provenance
	case epochv8.RuntimeApplyTransfer:
		published, publishErr := fs.PublishEpochV8TransferAuthorized(r.Context(), lease, plan, []byte(body.CandidateSource), reason, requestedAuthorization)
		if publishErr != nil {
			writeEpochV8ApplyError(w, publishErr, bindingDTO(currentBinding))
			return
		}
		disposition, binding, provenance = published.Disposition, published.Binding, published.Provenance
	default:
		writeError(w, http.StatusConflict, "process_unlock_conflict", "process unlock constructor is inconsistent")
		return
	}
	status := "applied"
	if disposition == epochv8.DispositionReplayed {
		status = "already_applied"
	}
	setAuditDetail(r, "reason_code="+epochv8.ApplyReasonUnlock+";disposition="+string(disposition)+";revision="+strconv.FormatUint(binding.Revision, 10))
	writeProcessJSON(w, http.StatusOK, epochV8ApplyResponse{
		Status: status, Disposition: string(disposition), ApplyToken: body.ApplyToken,
		EpochID: plan.CandidateEpoch().ID, CurrentBinding: bindingDTO(binding),
		ReasonCode: provenance.ReasonCode, Actor: provenance.Actor, AppliedAt: provenance.AppliedAt,
	})
}

func epochV8ResolveHandoffDirectives(checkpoint *epochv8.CheckpointV8, handoffs []epochV8PreviewHandoffRequest) ([]epochv8.HandoffDirective, error) {
	directives := make([]epochv8.HandoffDirective, 0, len(handoffs))
	for _, handoff := range handoffs {
		owner, err := epochv8.ResolveHandoffToken(checkpoint, handoff.Token)
		if err != nil {
			return nil, err
		}
		directive := epochv8.HandoffDirective{Source: owner, Action: handoff.Action}
		if handoff.Target != nil {
			directive.TargetLocalID = handoff.Target.LocalID
			directive.TargetReservationID = handoff.Target.ReservationID
			directive.TargetNodeID = handoff.Target.NodeID
		}
		directives = append(directives, directive)
	}
	return directives, nil
}

func epochV8HandoffDirectiveDigest(handoffs []epochV8PreviewHandoffRequest) (string, error) {
	canonical := append([]epochV8PreviewHandoffRequest(nil), handoffs...)
	seen := make(map[string]struct{}, len(canonical))
	for _, handoff := range canonical {
		if !lowerHexDigest(handoff.Token) {
			return "", epochv8.ErrInvalid
		}
		if _, duplicate := seen[handoff.Token]; duplicate {
			return "", epochv8.ErrInvalid
		}
		seen[handoff.Token] = struct{}{}
		switch handoff.Action {
		case epochv8.HandoffRetain:
			if handoff.Target != nil {
				return "", epochv8.ErrInvalid
			}
		case epochv8.HandoffTransfer:
			if handoff.Target == nil || !boundedEpochV8Identifier(handoff.Target.LocalID) ||
				!boundedEpochV8Identifier(handoff.Target.ReservationID) || !boundedEpochV8Identifier(handoff.Target.NodeID) {
				return "", epochv8.ErrInvalid
			}
		default:
			return "", epochv8.ErrInvalid
		}
	}
	sort.Slice(canonical, func(i, j int) bool { return canonical[i].Token < canonical[j].Token })
	wire, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	_, _ = io.WriteString(digest, "process-unlock-handoffs/v1\x00")
	_, _ = digest.Write(wire)
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func writeEpochV8ApplyError(w http.ResponseWriter, err error, current epochV8BindingDTO) {
	var budget *store.ExecutionViewOverBudgetError
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "process run was not found")
	case errors.Is(err, errEpochV8ApplyStale):
		writeProcessJSON(w, http.StatusConflict, struct {
			Code           string            `json:"code"`
			Status         string            `json:"status"`
			CurrentBinding epochV8BindingDTO `json:"currentBinding"`
		}{"process_unlock_stale", "stale", current})
	case errors.Is(err, store.ErrLeaseHeld):
		writeError(w, http.StatusConflict, "process_unlock_busy", "process run is busy")
	case errors.Is(err, epochv8.ErrOverBudget), errors.As(err, &budget):
		writeError(w, http.StatusRequestEntityTooLarge, "process_unlock_budget", "process unlock exceeds a budget")
	case errors.Is(err, epochv8.ErrInvalid), errors.Is(err, epochv8.ErrNonCanonical):
		writeError(w, http.StatusUnprocessableEntity, "process_unlock_invalid", "process unlock input is invalid")
	case errors.Is(err, store.ErrWriterInProgress), errors.Is(err, store.ErrRunInconsistent),
		errors.Is(err, store.ErrContentMismatch), errors.Is(err, store.ErrUnsafeRunPath):
		writeError(w, http.StatusConflict, "process_unlock_conflict", "process unlock preconditions are inconsistent")
	default:
		writeError(w, http.StatusInternalServerError, "process_unlock_unavailable", "process unlock is unavailable")
	}
}

func boundedEpochV8Identifier(value string) bool {
	if len(value) == 0 || len(value) > epochv8.MaxIdentifierBytes || value != strings.TrimSpace(value) {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '.' && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func lowerHexDigest(value string) bool {
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

func decodeOneStrictJSON(reader io.Reader, value any) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON input")
	}
	return nil
}

func writeEpochV8DecodeError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "process_request_budget", "process request exceeds its wire budget")
		return
	}
	writeError(w, http.StatusBadRequest, "json", "request body must contain exactly one supported JSON object")
}

func writeEpochV8PreviewError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "process run was not found")
	case errors.Is(err, errEpochV8PreviewStale):
		writeError(w, http.StatusConflict, "process_preview_stale", "process preview binding is stale")
	case errors.Is(err, epochv8.ErrOverBudget):
		writeError(w, http.StatusRequestEntityTooLarge, "process_preview_budget", "process preview exceeds a request budget")
	case errors.Is(err, epochv8.ErrInvalid):
		writeError(w, http.StatusUnprocessableEntity, "process_preview_invalid", "process preview input is invalid")
	default:
		writeError(w, http.StatusConflict, "process_preview_unavailable", "process preview could not be produced from a coherent run")
	}
}

func epochV8GraphSummary(checkpoint *epochv8.CheckpointV8, candidate *epochv8.TemplateCandidate) epochV8GraphSummaryDTO {
	view := checkpoint.View()
	current := view.Epochs[len(view.Epochs)-1]
	nodes, edges := candidate.GraphTotals()
	return epochV8GraphSummaryDTO{Current: epochV8GraphTotalsDTO{Nodes: len(current.Graph.Nodes), Edges: len(current.Graph.Edges)}, Candidate: epochV8GraphTotalsDTO{Nodes: nodes, Edges: edges}, Changed: current.TemplateRef != candidate.TemplateRef()}
}

// maxEpochV8LineageEntries bounds the projected lineage list. Longer chains
// keep their first and last halves with TotalEpochs/Truncated metadata so the
// DTO stays bounded independently of epochv8.MaxEpochs.
const maxEpochV8LineageEntries = 32

func epochV8Lineage(checkpoint *epochv8.CheckpointV8) epochV8LineageDTO {
	view := checkpoint.View()
	result := epochV8LineageDTO{OriginalTemplateRef: view.Epochs[0].TemplateRef, CurrentTemplateRef: view.Epochs[len(view.Epochs)-1].TemplateRef, TotalEpochs: len(view.Epochs)}
	entries := make([]epochV8LineageEntryDTO, 0, len(view.Epochs))
	for index, epoch := range view.Epochs {
		entry := epochV8LineageEntryDTO{Ordinal: epoch.Ordinal, TemplateRef: epoch.TemplateRef, EpochID: epoch.ID}
		if index > 0 {
			predecessor := view.Epochs[index-1].Ordinal
			entry.PredecessorOrdinal = &predecessor
		}
		entries = append(entries, entry)
	}
	result.Epochs, result.Truncated = boundEpochV8LineageEntries(entries)
	return result
}

func boundEpochV8LineageEntries(entries []epochV8LineageEntryDTO) ([]epochV8LineageEntryDTO, bool) {
	if len(entries) <= maxEpochV8LineageEntries {
		return entries, false
	}
	half := maxEpochV8LineageEntries / 2
	bounded := make([]epochV8LineageEntryDTO, 0, maxEpochV8LineageEntries)
	bounded = append(bounded, entries[:half]...)
	bounded = append(bounded, entries[len(entries)-half:]...)
	return bounded, true
}

func epochV8AuthorityCounts(checkpoint *epochv8.CheckpointV8) epochV8AuthorityCountsDTO {
	authorities := checkpoint.View().Authorities
	result := epochV8AuthorityCountsDTO{Total: len(authorities)}
	for _, authority := range authorities {
		switch authority.State {
		case epochv8.AuthorityVerifiedUnclaimed:
			result.Active++
			result.States.VerifiedUnclaimed++
		case epochv8.AuthorityClaimed:
			result.Active++
			result.States.Claimed++
		case epochv8.AuthorityActive:
			result.Active++
			result.States.Active++
		case epochv8.AuthorityCompleted:
			result.Terminal++
			result.States.Completed++
		case epochv8.AuthorityFailed:
			result.Terminal++
			result.States.Failed++
		case epochv8.AuthorityCanceled:
			result.Terminal++
			result.States.Canceled++
		default:
			result.Terminal++
			result.States.HandedOff++
		}
	}
	return result
}

func epochV8StructuralSummary(checkpoint *epochv8.CheckpointV8) epochV8StructuralSummaryDTO {
	view := checkpoint.View()
	current := view.Epochs[len(view.Epochs)-1]
	return epochV8StructuralSummaryDTO{
		Nodes:               len(current.Graph.Nodes),
		Edges:               len(current.Graph.Edges),
		ChangedFromOriginal: current.TemplateRef != view.Epochs[0].TemplateRef,
	}
}

type epochV8RunSummaryDTO struct {
	ID              string          `json:"id"`
	TemplateRef     string          `json:"templateRef"`
	EffectiveStatus state.RunStatus `json:"effectiveStatus"`
}

type epochV8SafeEnvelopeDTO struct {
	Run               epochV8RunSummaryDTO        `json:"run"`
	Graph             any                         `json:"graph"`
	Verification      processview.Verification    `json:"verification"`
	Report            processview.Report          `json:"report"`
	ViewerV2          processview.ViewerV2        `json:"viewerV2"`
	Schema            store.RunSchemaKind         `json:"schema"`
	Adapted           bool                        `json:"adapted"`
	Lineage           epochV8LineageDTO           `json:"lineage"`
	StructuralSummary epochV8StructuralSummaryDTO `json:"structuralSummary"`
	AuthorityCounts   epochV8AuthorityCountsDTO   `json:"authorityCounts"`
	CurrentBinding    epochV8BindingDTO           `json:"currentBinding"`
	// EpochReport is the bounded safe owner-epoch report. It shares one
	// projection core with the worklist, so their states cannot disagree.
	EpochReport worklist.EpochV8Report `json:"epochReport"`
}

// epochV8SafeEnvelope builds the ordinary schema-8 viewer envelope. A typed
// projection failure is a whole-run coherence error: the caller fails closed
// instead of serving an envelope with silently missing work.
func epochV8SafeEnvelope(ctx context.Context, snapshot store.EpochV8RunSnapshot) (epochV8SafeEnvelopeDTO, error) {
	projection, err := worklist.DeriveEpochV8(ctx, snapshot)
	if err != nil {
		return epochV8SafeEnvelopeDTO{}, err
	}
	status := epochV8EffectiveStatus(snapshot)
	verification := processverify.Report{RunID: snapshot.Run.ID, EffectiveStatus: status}
	base := processview.NewEnvelope(snapshot.Run.ID, verification)
	base.Run.TemplateRef = snapshot.Run.TemplateRef
	base.ViewerV2 = processview.ProjectViewerV2(processview.ViewerV2Input{RunID: snapshot.Run.ID, StateSchemaVersion: epochv8.StateSchemaVersion})
	lineage := epochV8Lineage(snapshot.Checkpoint)
	return epochV8SafeEnvelopeDTO{Run: epochV8RunSummaryDTO{ID: snapshot.Run.ID, TemplateRef: snapshot.Run.TemplateRef, EffectiveStatus: status}, Graph: nil, Verification: base.Verification, Report: base.Report, ViewerV2: base.ViewerV2, Schema: store.RunSchemaEpochV8, Adapted: lineage.TotalEpochs > 1, Lineage: lineage, StructuralSummary: epochV8StructuralSummary(snapshot.Checkpoint), AuthorityCounts: epochV8AuthorityCounts(snapshot.Checkpoint), CurrentBinding: bindingDTO(snapshot.Checkpoint.Binding()), EpochReport: projection.Report}, nil
}

func epochV8EffectiveStatus(snapshot store.EpochV8RunSnapshot) state.RunStatus {
	if snapshot.Runtime != nil {
		if checkpoint, err := pathv1.DecodeCheckpointV7(snapshot.Runtime.Checkpoint); err == nil {
			status := state.RunStatus(pathv1.CurrentRunStatus(checkpoint))
			if status.IsValid() {
				return status
			}
		}
	}
	return state.RunStatusRunning
}

func handleProcessEpochV8Verify(w http.ResponseWriter, r *http.Request) {
	setProcessNoStoreHeaders(w)
	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_verify_unavailable", "process verification is unavailable")
		return
	}
	// A genuinely missing run is 404 like every sibling run route; only
	// confirmed-existing runs may report the 409 coherence alarm below.
	if _, schemaErr := supportedProcessRunSchema(r.Context(), fs, r.PathValue("id")); errors.Is(schemaErr, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "process run was not found")
		return
	}
	snapshot, err := fs.LoadEpochV8RunView(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "process run was not found")
		} else {
			writeError(w, http.StatusConflict, "process_verify_inconsistent", "schema-8 process run is not coherent")
		}
		return
	}
	envelope, err := epochV8SafeEnvelope(r.Context(), snapshot)
	if err != nil {
		writeError(w, http.StatusConflict, "process_verify_inconsistent", "schema-8 process run is not coherent")
		return
	}
	writeProcessJSON(w, http.StatusOK, struct {
		Verified bool                   `json:"verified"`
		View     epochV8SafeEnvelopeDTO `json:"view"`
	}{true, envelope})
}

// setProcessNoStoreHeaders marks a process response as non-cacheable exact
// content. Schema-8-bearing routes (including their error and denial paths)
// call it before any permission check or store lookup so every response —
// success, stale, denied, missing — carries the same contract. Schema 1-7
// response bodies on mixed routes are unchanged; only these headers differ.
func setProcessNoStoreHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

func setExactArtifactHeaders(w http.ResponseWriter) {
	setProcessNoStoreHeaders(w)
}

func handleProcessEpochV8Artifact(w http.ResponseWriter, r *http.Request) {
	setExactArtifactHeaders(w)
	if _, ok := requirePermission(w, r, PermProcessRunsUnlockRead); !ok {
		return
	}
	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_artifact_unavailable", "process artifact is unavailable")
		return
	}
	artifacts, err := fs.ReadEpochV8AppliedArtifacts(r.Context(), r.PathValue("id"), epochv8.EpochID(r.PathValue("epoch")))
	if err != nil {
		writeEpochV8ArtifactError(w, err)
		return
	}
	kind := r.PathValue("artifact")
	var data []byte
	switch kind {
	case "diff":
		w.Header().Set("Content-Type", "application/json")
		data = artifacts.Diff
	case "reason":
		if !artifacts.HasReason {
			writeError(w, http.StatusNotFound, "not_found", "process artifact was not found")
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		data = artifacts.Reason
	default:
		writeError(w, http.StatusNotFound, "not_found", "process artifact was not found")
		return
	}
	w.Header().Set("Content-Length", jsonNumber(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func jsonNumber(value int) string {
	return strconv.Itoa(value)
}

func writeEpochV8ArtifactError(w http.ResponseWriter, err error) {
	var budget *store.ExecutionViewOverBudgetError
	var inconsistent *store.ExecutionViewInconsistentError
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "process artifact was not found")
	case errors.As(err, &budget):
		writeError(w, http.StatusRequestEntityTooLarge, "process_artifact_budget", "process artifact exceeds its read budget")
	case errors.As(err, &inconsistent), errors.Is(err, store.ErrUnsafeRunPath), errors.Is(err, store.ErrRunInconsistent), errors.Is(err, store.ErrContentMismatch):
		writeError(w, http.StatusConflict, "process_artifact_inconsistent", "process artifact is not coherent")
	default:
		writeError(w, http.StatusInternalServerError, "process_artifact_unavailable", "process artifact is unavailable")
	}
}

type epochV8SettlementRequest struct {
	BaseBinding epochV8BindingDTO `json:"baseBinding"`
	Token       string            `json:"token"`
	Decision    string            `json:"decision"`
	Reason      string            `json:"reason"`
	EvidenceRef string            `json:"evidenceRef"`
}

func handleProcessEpochV8Settlement(w http.ResponseWriter, r *http.Request) {
	setProcessNoStoreHeaders(w)
	caller, ok := requirePermission(w, r, PermProcessAdvance)
	if !ok {
		return
	}
	actor, err := processTemplateAuthor(caller)
	if err != nil {
		writeError(w, http.StatusForbidden, "forbidden", "caller has no stable settlement identity")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxEpochV8SettlementWire)
	var body epochV8SettlementRequest
	if err := decodeOneStrictJSON(r.Body, &body); err != nil {
		writeEpochV8DecodeError(w, err)
		return
	}
	if len(body.Reason) > maxEpochV8SettlementText || len(body.EvidenceRef) > maxEpochV8SettlementText {
		writeError(w, http.StatusRequestEntityTooLarge, "process_settlement_budget", "process settlement exceeds a request budget")
		return
	}
	if !lowerHexDigest(body.BaseBinding.Digest) || !lowerHexDigest(body.Token) || len(body.Reason) == 0 || len(body.EvidenceRef) == 0 || body.Decision != "retry" && body.Decision != "skip" && body.Decision != "cancel" {
		writeError(w, http.StatusUnprocessableEntity, "process_settlement_invalid", "process settlement decision is invalid")
		return
	}
	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_settlement_unavailable", "process settlement is unavailable")
		return
	}
	var preflight processexec.EpochV8SettlementPreflight
	err = fs.WithEpochV8ExecutionView(r.Context(), r.PathValue("id"), func(view store.EpochV8ExecutionView) error {
		if view.Checkpoint.Binding() != body.BaseBinding.engine() {
			return errEpochV8PreviewStale
		}
		settlement := pathv1.AuditedSettlementInput{Decision: body.Decision, Actor: string(actor), Reason: body.Reason, EvidenceRef: body.EvidenceRef, Timestamp: time.Now().UTC()}
		var preflightErr error
		preflight, preflightErr = processexec.PreflightEpochV8AuditedSettlement(r.Context(), view, body.Token, settlement)
		return preflightErr
	})
	if err != nil {
		writeEpochV8SettlementError(w, err)
		return
	}
	result, err := fs.AppendEpochV8SettlementAtBinding(r.Context(), r.PathValue("id"), body.BaseBinding.engine(), preflight.Transition)
	if err != nil {
		writeEpochV8SettlementError(w, err)
		return
	}
	disposition := "applied"
	if result.Disposition == epochv8.DispositionReplayed {
		disposition = "replayed"
	}
	setAuditDetail(r, "decision="+body.Decision+";disposition="+disposition)
	writeProcessJSON(w, http.StatusOK, struct {
		Settled           bool   `json:"settled"`
		Decision          string `json:"decision"`
		RepreviewRequired bool   `json:"repreviewRequired"`
	}{true, body.Decision, true})
}

func writeEpochV8SettlementError(w http.ResponseWriter, err error) {
	var budget *store.ExecutionViewOverBudgetError
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "process run was not found")
	case errors.As(err, &budget):
		writeError(w, http.StatusRequestEntityTooLarge, "process_settlement_budget", "process settlement exceeds a budget")
	default:
		writeError(w, http.StatusConflict, "process_settlement_conflict", "process settlement preconditions are no longer satisfied")
	}
}
