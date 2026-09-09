package app

import (
	"context"
	"github.com/tofutools/tclaude/internal/backend/model"
)

// Operator-owned automation retains its human origin. An agent-originated
// automation does not become an operator merely because it runs in the daemon.
func OperatorProfileCaller(principal model.Principal) bool {
	return principal.Kind == model.PrincipalOperator || (principal.Kind == model.PrincipalAutomation && principal.Authority.Kind == model.AuthorityOperator)
}

func ConfigurationProfileCreationAllowed(profile model.ConfigurationProfile, principal model.Principal) error {
	if profile.OperatorOnly && !OperatorProfileCaller(principal) {
		return fail(ErrUnauthorized, "configuration profile %q is restricted to operator creation", profile.Name)
	}
	return nil
}

func (s *Service) requireProfileCreation(ctx context.Context, principal model.Principal, profile model.ConfigurationProfile) error {
	if OperatorProfileCaller(principal) {
		return nil
	}
	if err := ConfigurationProfileCreationAllowed(profile, principal); err != nil {
		return err
	}
	defaults, err := s.store.ConfigurationDefaults(ctx)
	if err != nil {
		return err
	}
	if defaults.Global == nil || defaults.Global.ProfileID == profile.ID {
		return nil
	}
	global, err := s.store.ConfigurationProfile(ctx, defaults.Global.ProfileID, "")
	if err != nil {
		return err
	}
	return ConfigurationProfileCreationAllowed(global.Profile, principal)
}
