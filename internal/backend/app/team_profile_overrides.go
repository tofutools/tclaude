package app

import (
	"github.com/tofutools/tclaude/internal/backend/model"
	"slices"
)

// resolveTeamProfile distinguishes inherited settings from explicit member
// intent before deployment can create workspaces or admit agents.
func (s *Service) resolveTeamProfile(member model.TeamMemberSpec, base model.DesiredConfiguration) (model.DesiredConfiguration, error) {
	return s.resolveNativeConfigurationOverrides("member "+member.Key, base, member.Overrides)
}

func (s *Service) resolveNativeConfigurationOverrides(subject string, base model.DesiredConfiguration, overrides *model.NativeConfigurationOptions) (model.DesiredConfiguration, error) {
	desired := overrides.Apply(base)
	if overrides == nil || desired.Harness == base.Harness {
		return desired, nil
	}
	if s.providers == nil {
		return desired, fail(ErrInvalid, "%s: configure harness %s before resolving its profile overrides", subject, desired.Harness)
	}
	provider, ok := s.providers.Provider(desired.Harness)
	if !ok || provider.Capabilities().LaunchPolicy == nil {
		return desired, fail(ErrInvalid, "%s: harness %s does not declare profile override policy support", subject, desired.Harness)
	}
	policy := provider.Capabilities().LaunchPolicy
	if !slices.Contains(policy.SupportedSandbox, desired.Sandbox) {
		if overrides.Sandbox == nil && slices.Contains(policy.SupportedSandbox, policy.DefaultSandbox) {
			desired.Sandbox = policy.DefaultSandbox
		} else {
			return desired, fail(ErrInvalid, "%s: confinement %s is unsupported by %s; choose a supported confinement override", subject, desired.Sandbox, desired.Harness)
		}
	}
	if !slices.Contains(policy.SupportedApproval, desired.Approval) {
		if overrides.Approval == nil && slices.Contains(policy.SupportedApproval, policy.DefaultApproval) {
			desired.Approval = policy.DefaultApproval
		} else {
			return desired, fail(ErrInvalid, "%s: approval %s is unsupported by %s; choose a supported approval override", subject, desired.Approval, desired.Harness)
		}
	}
	return desired, nil
}
