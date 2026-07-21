package agentd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
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
)

const (
	maxEpochV8PreviewWireBytes = 32 << 20
	maxEpochV8PreviewHandoffs  = 256
	maxEpochV8SettlementWire   = 1 << 20
	maxEpochV8SettlementText   = 64 << 10
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

type epochV8LineageEntryDTO struct {
	Ordinal            uint64  `json:"ordinal"`
	PredecessorOrdinal *uint64 `json:"predecessorOrdinal,omitempty"`
	TemplateRef        string  `json:"templateRef"`
}

type epochV8LineageDTO struct {
	OriginalTemplateRef string                   `json:"originalTemplateRef"`
	CurrentTemplateRef  string                   `json:"currentTemplateRef"`
	Epochs              []epochV8LineageEntryDTO `json:"epochs"`
}

type epochV8AuthorityCountsDTO struct {
	Total    int `json:"total"`
	Active   int `json:"active"`
	Terminal int `json:"terminal"`
}

type epochV8BlockerDTO struct {
	Code  epochv8.BlockerCode `json:"code"`
	Token string              `json:"token,omitempty"`
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
}

func handleProcessEpochV8Preview(w http.ResponseWriter, r *http.Request) {
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
		for _, blocker := range preview.Blockers {
			projected := epochV8BlockerDTO{Code: blocker.Code}
			if blocker.AuthorityID != "" {
				var tokenErr error
				projected.Token, tokenErr = epochv8.HandoffToken(view.Checkpoint, blocker.AuthorityID)
				if tokenErr != nil {
					return tokenErr
				}
			}
			response.Blockers = append(response.Blockers, projected)
		}
		return nil
	}
	if _, err := epochv8.EncodeApplyPlan(preview.Plan); err != nil {
		return err
	}
	ownerSource := []byte(nil)
	if view.Runtime != nil {
		ownerSource = view.EpochSources[view.Runtime.EpochID]
	}
	apply, err := epochv8.PreflightRuntimeApply(r.Context(), view.Checkpoint, view.RuntimeJSON, ownerSource, []byte(body.CandidateSource), preview.Plan)
	if err != nil {
		return err
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

func epochV8Lineage(checkpoint *epochv8.CheckpointV8) epochV8LineageDTO {
	view := checkpoint.View()
	result := epochV8LineageDTO{OriginalTemplateRef: view.Epochs[0].TemplateRef, CurrentTemplateRef: view.Epochs[len(view.Epochs)-1].TemplateRef, Epochs: make([]epochV8LineageEntryDTO, 0, len(view.Epochs))}
	for index, epoch := range view.Epochs {
		entry := epochV8LineageEntryDTO{Ordinal: epoch.Ordinal, TemplateRef: epoch.TemplateRef}
		if index > 0 {
			predecessor := view.Epochs[index-1].Ordinal
			entry.PredecessorOrdinal = &predecessor
		}
		result.Epochs = append(result.Epochs, entry)
	}
	return result
}

func epochV8AuthorityCounts(checkpoint *epochv8.CheckpointV8) epochV8AuthorityCountsDTO {
	authorities := checkpoint.View().Authorities
	result := epochV8AuthorityCountsDTO{Total: len(authorities)}
	for _, authority := range authorities {
		switch authority.State {
		case epochv8.AuthorityVerifiedUnclaimed, epochv8.AuthorityClaimed, epochv8.AuthorityActive:
			result.Active++
		default:
			result.Terminal++
		}
	}
	return result
}

type epochV8RunSummaryDTO struct {
	ID              string          `json:"id"`
	TemplateRef     string          `json:"templateRef"`
	EffectiveStatus state.RunStatus `json:"effectiveStatus"`
}

type epochV8SafeEnvelopeDTO struct {
	Run             epochV8RunSummaryDTO      `json:"run"`
	Graph           any                       `json:"graph"`
	Verification    processview.Verification  `json:"verification"`
	Report          processview.Report        `json:"report"`
	ViewerV2        processview.ViewerV2      `json:"viewerV2"`
	Schema          store.RunSchemaKind       `json:"schema"`
	Lineage         epochV8LineageDTO         `json:"lineage"`
	AuthorityCounts epochV8AuthorityCountsDTO `json:"authorityCounts"`
	CurrentBinding  epochV8BindingDTO         `json:"currentBinding"`
}

func epochV8SafeEnvelope(snapshot store.EpochV8RunSnapshot) epochV8SafeEnvelopeDTO {
	status := epochV8EffectiveStatus(snapshot)
	verification := processverify.Report{RunID: snapshot.Run.ID, EffectiveStatus: status}
	base := processview.NewEnvelope(snapshot.Run.ID, verification)
	base.Run.TemplateRef = snapshot.Run.TemplateRef
	base.ViewerV2 = processview.ProjectViewerV2(processview.ViewerV2Input{RunID: snapshot.Run.ID, StateSchemaVersion: epochv8.StateSchemaVersion})
	return epochV8SafeEnvelopeDTO{Run: epochV8RunSummaryDTO{ID: snapshot.Run.ID, TemplateRef: snapshot.Run.TemplateRef, EffectiveStatus: status}, Graph: nil, Verification: base.Verification, Report: base.Report, ViewerV2: base.ViewerV2, Schema: store.RunSchemaEpochV8, Lineage: epochV8Lineage(snapshot.Checkpoint), AuthorityCounts: epochV8AuthorityCounts(snapshot.Checkpoint), CurrentBinding: bindingDTO(snapshot.Checkpoint.Binding())}
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
	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_verify_unavailable", "process verification is unavailable")
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
	writeProcessJSON(w, http.StatusOK, struct {
		Verified bool                   `json:"verified"`
		View     epochV8SafeEnvelopeDTO `json:"view"`
	}{true, epochV8SafeEnvelope(snapshot)})
}

func setExactArtifactHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
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
