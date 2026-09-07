package app

import (
	"context"
	"github.com/tofutools/tclaude/internal/backend/model"
	"strings"
)

// UpdateGroup replaces the authored name and ordered membership at one revision.
// Ownership is changed only through SetGroupOwner and its separate authority.
type UpdateGroupRequest struct {
	Context          model.Principal
	ID               model.GroupID
	ExpectedRevision model.Revision
	Name             string
	Members          []model.AgentID
}

func (s *Service) UpdateGroup(ctx context.Context, req UpdateGroupRequest) (GroupResult, error) {
	if err := req.ID.Validate(); err != nil {
		return GroupResult{}, fail(ErrInvalid, "%v", err)
	}
	if strings.TrimSpace(req.Name) == "" || len(req.Name) > 1024 || len(req.Members) > 1024 || req.ExpectedRevision == 0 {
		return GroupResult{}, ErrInvalid
	}
	seen := map[model.AgentID]bool{}
	for _, id := range req.Members {
		if id.Validate() != nil || seen[id] {
			return GroupResult{}, ErrInvalid
		}
		seen[id] = true
	}
	group, err := s.store.UpdateGroup(ctx, req, s.now().UTC())
	return GroupResult{Group: group}, err
}
