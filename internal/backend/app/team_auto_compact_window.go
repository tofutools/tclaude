package app

import (
	"context"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func (s *Service) validateTeamAutoCompactWindow(ctx context.Context, team model.TeamDefinition) error {
	for _, member := range team.Members {
		desired := member.Desired
		if member.Options != nil {
			desired = member.Options.Apply(desired)
			if desired.Harness == "" {
				continue
			} // Resolved and checked at deployment.
		}
		if member.ProfileID != "" {
			if member.Overrides == nil || member.Overrides.AutoCompactWindow == nil {
				continue
			}
			mode := *member.Overrides.AutoCompactWindow
			if err := mode.Validate("claude"); err != nil {
				return fail(ErrInvalid, "member %s: %v", member.Key, err)
			}
			if mode == "" {
				continue
			}
			desired.AutoCompactWindow = mode
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
		if err := desired.AutoCompactWindow.Validate(desired.Harness); err != nil {
			return fail(ErrInvalid, "member %s: %v", member.Key, err)
		}
	}
	return nil
}
