package app

import (
	"context"

	"github.com/tofutools/tclaude/internal/backend/model"
)

// ImportBatch is a fully translated, deterministic replacement snapshot.
// ApplyImport persists it in one transaction; publication is a separate
// no-clobber filesystem step performed only after semantic verification.
type ImportBatch struct {
	Receipt               model.ImportReceipt
	IDMappings            []model.ImportIDMapping
	SourceRecords         []model.ImportedSourceRecord
	Diagnostics           []model.ImportedDiagnostic
	Agents                []model.Agent
	Groups                []model.Group
	Conversations         []model.Conversation
	ConversationLinks     []model.ConversationAssociation
	History               []model.HistoryCatalogEntry
	Messages              []model.Message
	MessageEnvelopes      []model.ImportedMessageEnvelope
	ImportedAttachments   []model.ImportedAttachment
	ConfigurationProfiles []ConfigurationProfileResult
	ConfigurationDefaults *model.ConfigurationDefaults
	Definitions           []DefinitionRecord
	AutomationRules       []AutomationRuleRecord
	Workspaces            []model.Workspace
	Usage                 []HistoricalUsageWrite
	Activity              []HistoricalActivityWrite
}

type ImportStore interface {
	ApplyImport(context.Context, ImportBatch) error
	ImportReceipt(context.Context) (model.ImportReceipt, error)
	ImportedSourceRecords(context.Context, string) ([]model.ImportedSourceRecord, error)
	ImportedMessageEnvelope(context.Context, model.MessageID) (model.ImportedMessageEnvelope, error)
	ImportedAttachment(context.Context, model.AttachmentID) (model.ImportedAttachment, error)
}
