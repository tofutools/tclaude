package app

import (
	"context"
	"github.com/tofutools/tclaude/internal/backend/model"
)

// An explicit empty label set clears profile suggestions at creation.
func (s *Service) configurationDisplayLabels(ctx context.Context, ref *model.ConfigurationProfileRef, explicit *model.AgentLabels) (model.AgentLabels, error) {
	if explicit != nil {
		return *explicit, nil
	}
	if ref == nil {
		return model.AgentLabels{}, nil
	}
	selected, err := s.store.ConfigurationProfile(ctx, ref.ProfileID, ref.RevisionID)
	if err != nil {
		return model.AgentLabels{}, err
	}
	if selected.Revision.Startup == nil {
		return model.AgentLabels{}, nil
	}
	return model.AgentLabels{Role: selected.Revision.Startup.Role, Description: selected.Revision.Startup.Description}, nil
}

// Empty profile suggestions inherit the next lower tier. An explicitly supplied
// empty member pair is handled by the caller before this suggestion resolver.
func (s *Service) configurationDisplayLabelTiers(ctx context.Context, refs ...*model.ConfigurationProfileRef) (model.AgentDisplayLabels, error) {
	var out model.AgentDisplayLabels
	for _, ref := range refs {
		labels, err := s.configurationDisplayLabels(ctx, ref, nil)
		if err != nil {
			return out, err
		}
		if labels.Role != "" {
			out.Role = labels.Role
		}
		if labels.Description != "" {
			out.Description = labels.Description
		}
	}
	return out, nil
}
