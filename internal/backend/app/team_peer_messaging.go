package app

import (
	"context"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func (s *Service) validateTeamPeerMessaging(ctx context.Context, team model.TeamDefinition) error {
	return s.validateTeamNativeBoolean(ctx, team, func(d model.DesiredConfiguration) bool { return d.PeerMessaging }, func(o *model.TeamProfileOverrides) *bool { return o.PeerMessaging }, model.ValidatePeerMessaging)
}
