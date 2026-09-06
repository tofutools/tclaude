package ports

import (
	"context"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
)

// AutomationFactSource is a composition-owned read-only collector. The source
// and exact target come from validated rule authoring; implementations cannot
// redirect a rule to an arbitrary URL at poll time.
type AutomationFactSource interface {
	SourceID() string
	CollectAutomationFacts(context.Context, AutomationFactCollectRequest) (AutomationFactBatch, error)
}

type AutomationFactCollectRequest struct {
	Resource model.AutomationFactResource
	Cursor   string
	Limit    uint32
	Now      time.Time
}

type AutomationFactBatch struct {
	Facts      []model.AutomationProductFact
	NextCursor string
}

// TrustedAutomationFactIngress is the narrow composition seam used by a
// configured collector. It is deliberately absent from the public API and
// accepts only the bounded normalized fact vocabulary.
type TrustedAutomationFactIngress interface {
	IngestTrustedAutomationFacts(context.Context, string, []model.AutomationProductFact) error
}
