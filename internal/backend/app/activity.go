package app

import (
	"context"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
)

type ActivityStore interface {
	QueryActivity(context.Context, ActivityFilter, model.AuthorityRequest, time.Time) (ActivityResult, error)
	ImportHistoricalActivity(context.Context, HistoricalActivityWrite) (model.ActivityRecord, bool, error)
}

type ActivityTarget struct {
	AgentID        model.AgentID
	ConversationID model.ConversationID
	ExecutionID    model.ExecutionID
	WorkRunID      model.WorkRunID
}

type ActivityFilter struct {
	Target ActivityTarget
	Kinds  []model.ActivityKind
	After  time.Time
	Before time.Time
	Limit  int
	Cursor string
}

type ActivityResult struct {
	Records    []model.ActivityRecord
	NextCursor string
}

type QueryActivityRequest struct {
	Principal model.Principal
	Filter    ActivityFilter
}

func (s *Service) QueryActivity(ctx context.Context, req QueryActivityRequest) (ActivityResult, error) {
	store, ok := s.store.(ActivityStore)
	if !ok {
		return ActivityResult{}, fail(ErrUnavailable, "activity persistence is unavailable")
	}
	resource, err := activityResource(req.Filter.Target)
	if err != nil {
		return ActivityResult{}, err
	}
	return store.QueryActivity(ctx, req.Filter, model.AuthorityRequest{Principal: req.Principal, Action: model.ActionReadActivity, Resource: resource}, s.now().UTC())
}

func activityResource(target ActivityTarget) (model.ResourceSelector, error) {
	populated := 0
	if target.AgentID != "" {
		populated++
	}
	if target.ConversationID != "" {
		populated++
	}
	if target.ExecutionID != "" {
		populated++
	}
	if target.WorkRunID != "" {
		populated++
	}
	if populated != 1 {
		return model.ResourceSelector{}, fail(ErrInvalid, "select exactly one activity target")
	}
	switch {
	case target.AgentID != "":
		return model.ResourceSelector{Kind: model.ResourceAgent, AgentID: target.AgentID}, nil
	case target.ConversationID != "":
		return model.ResourceSelector{Kind: model.ResourceConversation, ConversationID: target.ConversationID}, nil
	case target.ExecutionID != "":
		return model.ResourceSelector{Kind: model.ResourceExecution, ExecutionID: target.ExecutionID}, nil
	default:
		return model.ResourceSelector{Kind: model.ResourceWorkRun, WorkRunID: target.WorkRunID}, nil
	}
}

// HistoricalActivityWrite is the offline import boundary. SourceKey remains
// private and imported rows cannot become operation or decision authority.
type HistoricalActivityWrite struct {
	Record         model.ActivityRecord
	SourceKey      string
	SourceRevision string
}
