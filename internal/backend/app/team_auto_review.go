package app

import (
	"context"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func (s *Service) validateTeamAutoReview(ctx context.Context, team model.TeamDefinition) error {
	for _, member := range team.Members {
		desired := member.Desired
		if member.ProfileID != "" {
			if member.Overrides == nil || member.Overrides.AutoReview == nil {
				continue
			}
			mode := *member.Overrides.AutoReview
			if !mode {
				continue
			}
			desired.AutoReview = mode
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
		if err := model.ValidateAutoReview(desired.AutoReview, desired.Harness); err != nil {
			return fail(ErrInvalid, "member %s: %v", member.Key, err)
		}
	}
	return nil
}
