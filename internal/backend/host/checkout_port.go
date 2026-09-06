//go:build linux || darwin

package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
		return ports.WorkspaceEffectResult{Disposition: ports.EffectAccepted, Observation: request.Observation, Resource: request.Resource}, nil
	}
	observed := workspaceObservation(observation)
	if observation.Dirty && !request.Destructive {
		return ports.WorkspaceEffectResult{Disposition: ports.EffectRefused, Observation: observed, Resource: request.Resource}, ErrCheckoutDirty
	}
	if err := permit.Consume(ctx); err != nil {
		return ports.WorkspaceEffectResult{Disposition: ports.EffectRefused, Observation: observed, Resource: request.Resource}, err
	}
	removed, err := h.Remove(ctx, CheckoutRemovalRequest{Evidence: evidence, Destructive: request.Destructive})
	result := ports.WorkspaceEffectResult{Disposition: ports.EffectAccepted, Observation: observed, Resource: request.Resource}
	if err != nil || removed.State == CheckoutEffectUncertain {
		result.Disposition = ports.EffectUnknown
		if errors.Is(err, ErrCheckoutDirty) || errors.Is(err, ErrCheckoutNotOwned) || errors.Is(err, ErrCheckoutMain) {
			result.Disposition = ports.EffectRefused
		}
	}
	return result, err
}

func (h CheckoutHost) RestoreCheckout(ctx context.Context, request ports.CheckoutRestoreRequest, permit ports.EffectPermit) (ports.WorkspaceEffectResult, error) {
	refused := func(err error) (ports.WorkspaceEffectResult, error) {
		return ports.WorkspaceEffectResult{
			Disposition: ports.EffectRefused,
			Observation: request.Observation,
			Resource:    request.Resource,
		}, err
	}
	if permit == nil {
		return refused(fmt.Errorf("checkout effect permit is required"))
	}
	evidence, err := decodeWorkspaceResource(request.Resource)
	if err != nil {
		return refused(err)
	}
	if evidence.Ownership != CheckoutCreated || !evidence.Linked || evidence.OwnerToken == "" ||
		request.Intent.Provenance != model.WorkspacePlatformCreated || request.Intent.Ownership != model.WorkspaceOwned {
		return refused(ErrCheckoutNotOwned)
	}
	target, err := cleanAbsolute(request.Intent.IntendedPath)
	if err != nil || target != evidence.Path {
		return refused(ErrCheckoutIdentityMismatch)
	}
	repository, err := h.repositoryRoot(ctx, request.Intent.Repository)
	if err != nil || repository != evidence.Repository {
		return refused(ErrCheckoutIdentityMismatch)
	}
	common, err := h.gitOutput(ctx, repository, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || filepath.Clean(common) != evidence.GitCommonDir {
		return refused(ErrCheckoutIdentityMismatch)
	}
	base := strings.TrimSpace(request.Intent.BaseRevision)
	if base == "" {
		base = "HEAD"
	}
	branch := strings.TrimSpace(request.Intent.Branch)
	if branch == "" || branch != evidence.Branch || base != evidence.Base ||
		request.Observation.ActualPath != evidence.Path ||
		request.Observation.RepositoryRoot != evidence.Repository ||
		request.Observation.Branch != evidence.Branch || strings.TrimSpace(request.Observation.Revision) == "" {
		return refused(ErrCheckoutIdentityMismatch)
	}
	if _, err := os.Lstat(evidence.Path); err == nil || !errors.Is(err, os.ErrNotExist) {
		return refused(ErrCheckoutIdentityMismatch)
	}
	if _, err := os.Lstat(evidence.GitDir); err == nil || !errors.Is(err, os.ErrNotExist) {
		return refused(ErrCheckoutIdentityMismatch)
	}
	branchRef := "refs/heads/" + evidence.Branch
	branchCommit, err := h.gitOutput(ctx, repository, "rev-parse", "--verify", branchRef+"^{commit}")
	if err != nil || strings.TrimSpace(branchCommit) != request.Observation.Revision {
		return refused(ErrCheckoutIdentityMismatch)
	}
	if evidence.InitialCommit != "" {
		if _, err := h.gitOutput(ctx, repository, "merge-base", "--is-ancestor", evidence.InitialCommit, branchRef); err != nil {
			return refused(fmt.Errorf("%w: retained branch no longer descends from its initial commit", ErrCheckoutIdentityMismatch))
		}
	}
	if err := permit.Consume(ctx); err != nil {
		return refused(fmt.Errorf("consume checkout effect permit: %w", err))
	}
	if _, err := h.gitOutput(ctx, repository, "-c", "core.hooksPath=/dev/null", "worktree", "add", evidence.Path, evidence.Branch); err != nil {
		return ports.WorkspaceEffectResult{Disposition: ports.EffectUnknown, Observation: request.Observation, Resource: request.Resource}, fmt.Errorf("restore Git checkout: %w", err)
	}
	restored, err := h.record(ctx, evidence.Path, evidence.Ownership, evidence.Base, evidence.InitialCommit, evidence.OwnerToken)
	if err != nil || restored.Repository != evidence.Repository || restored.GitCommonDir != evidence.GitCommonDir ||
		restored.Path != evidence.Path || restored.Branch != evidence.Branch {
		if err == nil {
			err = ErrCheckoutIdentityMismatch
		}
		return ports.WorkspaceEffectResult{Disposition: ports.EffectUnknown, Observation: request.Observation, Resource: request.Resource}, err
	}
	resource, encodeErr := encodeWorkspaceResource(restored)
	if encodeErr != nil {
		return ports.WorkspaceEffectResult{Disposition: ports.EffectUnknown, Observation: request.Observation, Resource: request.Resource}, encodeErr
	}
	if err := writeCheckoutOwner(restored.GitDir, restored.OwnerToken); err != nil {
		return ports.WorkspaceEffectResult{Disposition: ports.EffectUnknown, Observation: request.Observation, Resource: resource}, err
	}
	observation, err := h.Inspect(ctx, restored)
	if err != nil || observation.State != CheckoutEffectReady || observation.Commit != request.Observation.Revision {
		if err == nil {
			err = ErrCheckoutIdentityMismatch
		}
		return ports.WorkspaceEffectResult{Disposition: ports.EffectUnknown, Observation: workspaceObservation(observation), Resource: resource}, err
	}
	return ports.WorkspaceEffectResult{Disposition: ports.EffectAccepted, Observation: workspaceObservation(observation), Resource: resource}, nil
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
