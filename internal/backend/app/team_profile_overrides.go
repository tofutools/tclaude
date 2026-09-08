package app

import (
	"github.com/tofutools/tclaude/internal/backend/model"
	"slices"
)

// resolveTeamProfile distinguishes inherited settings from explicit member
// intent before deployment can create workspaces or admit agents.
func (s *Service) resolveTeamProfile(member model.TeamMemberSpec, base model.DesiredConfiguration) (model.DesiredConfiguration, error) {
	desired := member.Overrides.Apply(base)
	if member.Overrides == nil || desired.Harness == base.Harness {
		return desired, nil
	}
	if s.providers == nil {
		return desired, fail(ErrInvalid, "member %s: configure harness %s before resolving its profile overrides", member.Key, desired.Harness)
	}
	provider, ok := s.providers.Provider(desired.Harness)
	if !ok || provider.Capabilities().LaunchPolicy == nil {
		return desired, fail(ErrInvalid, "member %s: harness %s does not declare profile override policy support", member.Key, desired.Harness)
	}
	policy := provider.Capabilities().LaunchPolicy
	if !slices.Contains(policy.SupportedSandbox, desired.Sandbox) {
		if member.Overrides.Sandbox == nil && slices.Contains(policy.SupportedSandbox, policy.DefaultSandbox) {
			desired.Sandbox = policy.DefaultSandbox
		} else {
			return desired, fail(ErrInvalid, "member %s: confinement %s is unsupported by %s; choose a supported confinement override", member.Key, desired.Sandbox, desired.Harness)
		}
	}
	if !slices.Contains(policy.SupportedApproval, desired.Approval) {
		if member.Overrides.Approval == nil && slices.Contains(policy.SupportedApproval, policy.DefaultApproval) {
			desired.Approval = policy.DefaultApproval
		} else {
			return desired, fail(ErrInvalid, "member %s: approval %s is unsupported by %s; choose a supported approval override", member.Key, desired.Approval, desired.Harness)
		}
	}
	return desired, nil
}
