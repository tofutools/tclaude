package app

import (
	"context"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
)

type ActivityStore interface {
	QueryActivity(context.Context, ActivityFilter) (ActivityResult, error)
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

// HistoricalActivityWrite is the offline import boundary. SourceKey remains
// private and imported rows cannot become operation or decision authority.
type HistoricalActivityWrite struct {
	Record         model.ActivityRecord
	SourceKey      string
	SourceRevision string
}
