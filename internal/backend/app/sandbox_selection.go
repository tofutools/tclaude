package app

import (
	"context"
	"errors"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/sandboxpolicy"
)

type SandboxSelectionAPI interface {
	ResolveLaunchSandbox(context.Context, model.Principal, []model.SandboxScopeSelection) (model.SandboxSelection, error)
}

// ResolveLaunchSandbox validates profile choices without freezing their content.
// Each fresh launch resolves the then-current profile definitions.
func (s *Service) ResolveLaunchSandbox(ctx context.Context, principal model.Principal, scopes []model.SandboxScopeSelection) (model.SandboxSelection, error) {
	if err := requireOperator(principal); err != nil {
		return model.SandboxSelection{}, err
	}
	selected := (model.SandboxSelection{Scopes: scopes}).References()
	if err := s.verifyLaunchSandbox(ctx, &selected); err != nil {
		return model.SandboxSelection{}, err
	}
	return selected, nil
}

// currentSandboxScopes follows stable IDs even for selections saved by older
// v2 builds which included revision metadata.
func (s *Service) currentSandboxScopes(ctx context.Context, scopes []model.SandboxScopeSelection) ([]model.SandboxScopeSelection, error) {
	selection := model.SandboxSelection{Scopes: scopes}
	if err := selection.Validate(); err != nil {
		return nil, fail(ErrInvalid, "%v", err)
	}
	current := selection.Clone().Scopes
	for i := range current {
		profile, err := s.store.SandboxProfile(ctx, current[i].Ref.ProfileID)
		if err != nil {
			return nil, err
		}
		if profile.Profile.Archived {
			return nil, fail(ErrConflict, "select an active sandbox profile")
		}
		current[i].Ref = profile.Revision.Ref
	}
	return current, nil
}

func (s *Service) materializeLaunchSandbox(ctx context.Context, scopes []model.SandboxScopeSelection) (sandboxpolicy.PolicyMaterialization, error) {
	if s.sandboxPaths == nil {
		return sandboxpolicy.PolicyMaterialization{}, fail(ErrUnavailable, "sandbox path inspection is not configured")
	}
	current, err := s.currentSandboxScopes(ctx, scopes)
	if err != nil {
		return sandboxpolicy.PolicyMaterialization{}, err
	}
	result, err := sandboxpolicy.MaterializeScopes(ctx, current, s.store, s.sandboxPaths)
	if errors.Is(err, sandboxpolicy.ErrInvalidClosure) {
		return sandboxpolicy.PolicyMaterialization{}, fail(ErrInvalid, "%v", err)
	}
	return result, err
}

// Saving a choice requires an existing profile, not host preparation or a hash.
func (s *Service) verifyLaunchSandbox(ctx context.Context, selected *model.SandboxSelection) error {
	if selected == nil {
		return nil
	}
	_, err := s.currentSandboxScopes(ctx, selected.Scopes)
	return err
}

func (s *Service) prepareProviderSandbox(ctx context.Context, provider ports.Provider, selected *model.SandboxSelection) (*sandboxpolicy.PolicyMaterialization, error) {
	if selected == nil {
		return nil, nil
	}
	if !provider.Capabilities().HostSandbox {
		return nil, fail(ErrUnsupported, "provider host sandbox preparation is not configured")
	}
	if err := selected.Validate(); err != nil {
		return nil, fail(ErrInvalid, "%v", err)
	}
	materialized, err := s.materializeLaunchSandbox(ctx, selected.Scopes)
	if err != nil {
		return nil, err
	}
	return &materialized, nil
}
