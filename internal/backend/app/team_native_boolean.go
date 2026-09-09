package app

import (
	"context"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func (s *Service) validateTeamNativeBoolean(ctx context.Context, team model.TeamDefinition, value func(model.DesiredConfiguration) bool, override func(*model.TeamProfileOverrides) *bool, validate func(bool, string) error) error {
	for _, member := range team.Members {
		desired := member.Desired
		if member.Options != nil {
			desired = member.Options.Apply(desired)
			if desired.Harness == "" {
				continue
			} // Resolved and checked at deployment.
		}
		enabled := value(desired)
		if member.ProfileID != "" {
			if member.Overrides == nil || override(member.Overrides) == nil {
				continue
			}
			mode := *override(member.Overrides)
			if !mode {
				continue
			}
			enabled = mode
			if member.Overrides.Harness != nil {
				desired.Harness = *member.Overrides.Harness
			} else {
				profile, err := s.store.ConfigurationProfile(ctx, member.ProfileID, "")
				if err != nil {
					return err
				}
				desired.Harness = profile.Revision.AuthoredHarness()
				if desired.Harness == "" {
					// An inherited harness is resolved and validated at deployment.
					continue
				}
			}
		}
		if err := validate(enabled, desired.Harness); err != nil {
			return fail(ErrInvalid, "member %s: %v", member.Key, err)
		}
	}
	return nil
}
