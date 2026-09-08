package app

import (
	"context"
	"errors"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/sandboxpolicy"
)

// ResolveLaunchSandbox returns a server-computed selection for an explicit set
// of immutable scopes. This is read-only preparation, not launch authorization.
func (s *Service) ResolveLaunchSandbox(ctx context.Context, principal model.Principal, scopes []model.SandboxScopeSelection) (model.SandboxSelection, error) {
	if err := requireOperator(principal); err != nil {
		return model.SandboxSelection{}, err
	}
	if len(scopes) == 0 || len(scopes) > 3 {
		return model.SandboxSelection{}, ErrInvalid
	}
	for _, scope := range scopes {
		if scope.Ref.ProfileID.Validate() != nil {
			return model.SandboxSelection{}, ErrInvalid
		}
		profile, err := s.store.SandboxProfile(ctx, scope.Ref.ProfileID)
		if err != nil {
			return model.SandboxSelection{}, err
		}
		if profile.Profile.Archived || profile.Profile.Imported {
			return model.SandboxSelection{}, fail(ErrConflict, "select an active authored sandbox profile")
		}
	}
	materialized, err := s.materializeLaunchSandbox(ctx, scopes)
	if err != nil {
		return model.SandboxSelection{}, err
	}
	selected, err := materialized.LaunchSelection()
	if err != nil {
		return model.SandboxSelection{}, fail(ErrInvalid, "%v", err)
	}
	return selected, nil
}

func (s *Service) materializeLaunchSandbox(ctx context.Context, scopes []model.SandboxScopeSelection) (sandboxpolicy.PolicyMaterialization, error) {
	if s.sandboxPaths == nil {
		return sandboxpolicy.PolicyMaterialization{}, fail(ErrUnavailable, "sandbox path inspection is not configured")
	}
	result, err := sandboxpolicy.MaterializeScopes(ctx, scopes, s.store, s.sandboxPaths)
	if errors.Is(err, sandboxpolicy.ErrInvalidClosure) {
		return sandboxpolicy.PolicyMaterialization{}, fail(ErrInvalid, "%v", err)
	}
	return result, err
}

// verifyLaunchSandbox never accepts a caller-supplied policy hash as proof of
// resolved content. Persisted references are immutable, including after rename.
func (s *Service) verifyLaunchSandbox(ctx context.Context, selected *model.SandboxSelection) error {
	if selected == nil {
		return nil
	}
	if err := selected.Validate(); err != nil {
		return fail(ErrInvalid, "%v", err)
	}
	materialized, err := s.materializeLaunchSandbox(ctx, selected.Scopes)
	if err != nil {
		return err
	}
	actual, err := materialized.LaunchSelection()
	if err != nil {
		return fail(ErrInvalid, "%v", err)
	}
	if !selected.Equal(actual) {
		return fail(ErrConflict, "resolved sandbox policy changed; review the selection")
	}
	return nil
}
