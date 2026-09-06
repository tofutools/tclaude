//go:build linux || darwin

package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

const (
	workspaceResourceOwner   = "host.git-checkout"
	workspaceResourceVersion = uint32(1)
)

func (h CheckoutHost) CreateCheckout(ctx context.Context, request ports.CheckoutCreateRequest, permit ports.EffectPermit) (ports.WorkspaceEffectResult, error) {
	if permit == nil {
		return ports.WorkspaceEffectResult{Disposition: ports.EffectRefused}, fmt.Errorf("checkout effect permit is required")
	}
	if request.Intent.Provenance != model.WorkspacePlatformCreated ||
		(request.Intent.Ownership != model.WorkspaceOwned && request.Intent.Ownership != model.WorkspaceShared) {
		return ports.WorkspaceEffectResult{Disposition: ports.EffectRefused}, fmt.Errorf("created checkout requires explicit platform ownership")
	}
	if err := permit.Consume(ctx); err != nil {
		return ports.WorkspaceEffectResult{Disposition: ports.EffectRefused}, fmt.Errorf("consume checkout effect permit: %w", err)
	}
	created, err := h.Create(ctx, CheckoutIntent{
		Repository: request.Intent.Repository, Path: request.Intent.IntendedPath,
		Branch: request.Intent.Branch, Base: request.Intent.BaseRevision,
	})
	result := ports.WorkspaceEffectResult{Disposition: ports.EffectAccepted}
	if created.Evidence.Path != "" {
		result.Resource, _ = encodeWorkspaceResource(created.Evidence)
		if observation, inspectErr := h.Inspect(ctx, created.Evidence); inspectErr == nil {
			result.Observation = workspaceObservation(observation)
		}
	}
	if err != nil || created.State == CheckoutEffectUncertain {
		result.Disposition = ports.EffectUnknown
	}
	return result, err
}

func (h CheckoutHost) InspectWorkspace(ctx context.Context, workspace model.Workspace) (ports.WorkspaceEffectResult, error) {
	var evidence CheckoutEvidence
	var err error
	if len(workspace.Resource.Payload) == 0 && workspace.Intent.Provenance == model.WorkspaceRegistered {
		evidence, err = h.Register(ctx, workspace.Intent.IntendedPath)
	} else {
		evidence, err = decodeWorkspaceResource(workspace.Resource)
	}
	if err != nil {
		return ports.WorkspaceEffectResult{Disposition: ports.EffectUnknown}, err
	}
	observation, err := h.Inspect(ctx, evidence)
	result := ports.WorkspaceEffectResult{Disposition: ports.EffectAccepted, Resource: workspace.Resource}
	if len(result.Resource.Payload) == 0 {
		result.Resource, _ = encodeWorkspaceResource(evidence)
	}
	if observation.State == CheckoutEffectReady {
		result.Observation = workspaceObservation(observation)
	}
	if observation.State == CheckoutEffectAbsent {
		result.Disposition = ports.EffectRefused
		err = ErrCheckoutUnavailable
	} else if err != nil {
		result.Disposition = ports.EffectUnknown
	}
	return result, err
}

func (h CheckoutHost) RemoveCheckout(ctx context.Context, request ports.CheckoutRemoveRequest, permit ports.EffectPermit) (ports.WorkspaceEffectResult, error) {
	if permit == nil {
		return ports.WorkspaceEffectResult{Disposition: ports.EffectRefused}, fmt.Errorf("checkout effect permit is required")
	}
	evidence, err := decodeWorkspaceResource(request.Resource)
	if err != nil {
		return ports.WorkspaceEffectResult{Disposition: ports.EffectRefused}, err
	}
	// Revalidate identity and dirtiness before consuming the destructive
	// effect permit. Application has already checked durable use/sharing.
	observation, err := h.Inspect(ctx, evidence)
	if err != nil {
		return ports.WorkspaceEffectResult{Disposition: ports.EffectUnknown, Resource: request.Resource}, err
	}
	if observation.State == CheckoutEffectAbsent {
		return ports.WorkspaceEffectResult{Disposition: ports.EffectAccepted, Resource: request.Resource}, nil
	}
	if observation.Dirty && !request.Destructive {
		return ports.WorkspaceEffectResult{Disposition: ports.EffectRefused, Observation: workspaceObservation(observation), Resource: request.Resource}, ErrCheckoutDirty
	}
	if err := permit.Consume(ctx); err != nil {
		return ports.WorkspaceEffectResult{Disposition: ports.EffectRefused, Observation: workspaceObservation(observation), Resource: request.Resource}, err
	}
	removed, err := h.Remove(ctx, CheckoutRemovalRequest{Evidence: evidence, Destructive: request.Destructive})
	result := ports.WorkspaceEffectResult{Disposition: ports.EffectAccepted, Resource: request.Resource}
	if err != nil || removed.State == CheckoutEffectUncertain {
		result.Disposition = ports.EffectUnknown
		if errors.Is(err, ErrCheckoutDirty) || errors.Is(err, ErrCheckoutNotOwned) || errors.Is(err, ErrCheckoutMain) {
			result.Disposition = ports.EffectRefused
		}
	}
	return result, err
}

func workspaceObservation(value CheckoutObservation) model.WorkspaceObservation {
	return model.WorkspaceObservation{
		ActualPath: value.Evidence.Path, RepositoryRoot: value.Evidence.Repository,
		Revision: value.Commit, Branch: value.Evidence.Branch, Dirty: value.Dirty,
		ObservedAt: time.Now().UTC(),
	}
}

func encodeWorkspaceResource(value CheckoutEvidence) (model.WorkspaceResourceEvidence, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return model.WorkspaceResourceEvidence{}, err
	}
	return model.WorkspaceResourceEvidence{Owner: workspaceResourceOwner, Version: workspaceResourceVersion, Payload: payload}, nil
}

func decodeWorkspaceResource(resource model.WorkspaceResourceEvidence) (CheckoutEvidence, error) {
	if resource.Owner != workspaceResourceOwner || resource.Version != workspaceResourceVersion || len(resource.Payload) == 0 {
		return CheckoutEvidence{}, ErrCheckoutIdentityMismatch
	}
	var value CheckoutEvidence
	if err := json.Unmarshal(resource.Payload, &value); err != nil {
		return CheckoutEvidence{}, ErrCheckoutIdentityMismatch
	}
	if value.Path == "" || value.GitCommonDir == "" || value.GitDir == "" {
		return CheckoutEvidence{}, ErrCheckoutIdentityMismatch
	}
	return value, nil
}

var _ ports.WorkspaceHost = CheckoutHost{}
