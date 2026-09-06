package ports

import (
	"context"
	"errors"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
)

var ErrHistoryUnsupported = errors.New("provider history capability unsupported")

type HistoryPrecision string

const (
	HistoryPrecisionNone          HistoryPrecision = "none"
	HistoryPrecisionHead          HistoryPrecision = "head"
	HistoryPrecisionMessage       HistoryPrecision = "message"
	HistoryPrecisionBeforeMessage HistoryPrecision = "before_message"
	HistoryPrecisionTurn          HistoryPrecision = "turn"
)

type HistoryCapabilities struct {
	MetadataDiscovery     bool
	ContentRead           bool
	ContinuationPrecision HistoryPrecision
	ForkPrecision         HistoryPrecision
	ForkRequiresExclusive bool
}

type HistoryDiscoveryScope struct {
	// Source is composition-owned provider configuration, such as a native
	// history root. It is never accepted as caller-supplied evidence.
	Source        string
	WorkspaceHint string
	ModifiedAfter time.Time
}

type HistoryDiscoveryRequest struct{ Scope HistoryDiscoveryScope }

// HistorySourceRegistry resolves caller-selected names to composition-owned
// provider scope. Paths and provider evidence never cross the public API.
type HistorySourceRegistry interface {
	HistorySource(harness, name string) (HistoryDiscoveryScope, bool)
}

type ProviderHistoryPoint struct {
	Token      string
	Kind       model.HistoryPointKind
	OccurredAt time.Time
}

type DiscoveredHistory struct {
	Native            model.NativeConversationEvidence
	SourceToken       string
	SourceFingerprint string
	Title             string
	WorkspaceHint     string
	ModifiedAt        time.Time
	Availability      model.HistoryAvailability
	Coverage          model.HistoryCoverage
	Points            []ProviderHistoryPoint
	Evidence          model.ProviderEvidence
}

type HistoryDiscoveryResult struct {
	Histories []DiscoveredHistory
	Coverage  model.HistoryCoverage
}

// HistorySourceSelection is resolved by application persistence from a public
// ConversationID/HistoryPointID revision check. Callers never construct it.
type HistorySourceSelection struct {
	ConversationID    model.ConversationID
	Provider          string
	Native            model.NativeConversationEvidence
	SourceToken       string
	SourceRevision    string
	SourceFingerprint string
	Point             *ProviderHistoryPoint
	Evidence          model.ProviderEvidence
	UseClaim          *model.HistoryUseClaim
}

type HistoryPartKind string

const (
	HistoryPartText        HistoryPartKind = "text"
	HistoryPartMetadata    HistoryPartKind = "metadata"
	HistoryPartUnsupported HistoryPartKind = "unsupported"
)

type HistoryPart struct {
	Kind      HistoryPartKind
	Text      string
	MediaType string
	Omitted   bool
}

type HistoryTurn struct {
	Point ProviderHistoryPoint
	Role  string
	Parts []HistoryPart
}

type HistoryReadResult struct {
	Turns    []HistoryTurn
	Coverage model.HistoryCoverage
	Evidence model.ProviderEvidence
}

type HistoryReader interface {
	Capabilities() HistoryCapabilities
	Discover(context.Context, HistoryDiscoveryRequest) (HistoryDiscoveryResult, error)
	Read(context.Context, HistorySourceSelection) (HistoryReadResult, error)
}

// HistoryProvider is implemented by the existing cohesive Provider. History
// is a focused view sharing that provider's native state and registration.
type HistoryProvider interface {
	Provider
	History() HistoryReader
}

type EffectPermit interface {
	OperationID() model.OperationID
	Consume(context.Context) error
}

type CheckoutCreateRequest struct {
	WorkspaceID model.WorkspaceID
	Intent      model.WorkspaceIntent
}

type CheckoutRemoveRequest struct {
	WorkspaceID model.WorkspaceID
	Observation model.WorkspaceObservation
	Resource    model.WorkspaceResourceEvidence
	Destructive bool
}

type CheckoutRestoreRequest struct {
	WorkspaceID model.WorkspaceID
	Intent      model.WorkspaceIntent
	Observation model.WorkspaceObservation
	Resource    model.WorkspaceResourceEvidence
}

type WorkspaceEffectResult struct {
	Disposition EffectDisposition
	Observation model.WorkspaceObservation
	Resource    model.WorkspaceResourceEvidence
}

// WorkspaceHost owns filesystem/Git mechanics. Application code owns durable
// intent, authorization, active-use checks, effect permits, and settlement.
type WorkspaceHost interface {
	CreateCheckout(context.Context, CheckoutCreateRequest, EffectPermit) (WorkspaceEffectResult, error)
	InspectWorkspace(context.Context, model.Workspace) (WorkspaceEffectResult, error)
	RemoveCheckout(context.Context, CheckoutRemoveRequest, EffectPermit) (WorkspaceEffectResult, error)
	RestoreCheckout(context.Context, CheckoutRestoreRequest, EffectPermit) (WorkspaceEffectResult, error)
}
