package app

import (
	"context"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func (s *Service) validateTeamFastMode(ctx context.Context, team model.TeamDefinition) error {
	for _, member := range team.Members {
		desired := member.Desired
		if member.ProfileID != "" {
			if member.Overrides == nil || member.Overrides.FastMode == nil {
				continue
			}
			mode := *member.Overrides.FastMode
			if err := mode.Validate("codex"); err != nil {
				return fail(ErrInvalid, "member %s: %v", member.Key, err)
			}
			if mode == "" {
				continue
			}
			desired.FastMode = mode
			if member.Overrides.Harness != nil {
				desired.Harness = *member.Overrides.Harness
			} else {
				profile, err := s.store.ConfigurationProfile(ctx, member.ProfileID, "")
				if err != nil {
					return err
				}
				desired.Harness = profile.Revision.Desired.Harness
			}
		}
		if err := desired.FastMode.Validate(desired.Harness); err != nil {
			return fail(ErrInvalid, "member %s: %v", member.Key, err)
		}
	}
	return nil
}
