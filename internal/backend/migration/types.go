// Package migration inspects explicit, offline legacy snapshots and builds a
// deterministic import plan. It deliberately performs no target writes.
package migration

import (
	"context"
	"encoding/json"

	sourcev228 "github.com/tofutools/tclaude/internal/backend/migration/source/v228"
)

const (
	BundleFormatVersion = 1
	SourceSchemaVersion = 228
	PlanFormatVersion   = 1
)

// Bundle names an operator-created snapshot bundle. Both paths are required;
// no default state directory is ever discovered.
type Bundle struct {
	Root         string
	ManifestPath string
}

type Manifest struct {
	FormatVersion int           `json:"format_version"`
	Database      ManifestFile  `json:"database"`
	Config        *ManifestFile `json:"config,omitempty"`
	Attachments   []Attachment  `json:"attachments,omitempty"`
}

type ManifestFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type Attachment struct {
	SourceTable string `json:"source_table"`
	SourceID    string `json:"source_id"`
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
}

type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityBlocking Severity = "blocking"
)

// Diagnostic is safe for product-command output: Detail must never contain
// message/config values, credentials, attachment paths, or other raw payloads.
type Diagnostic struct {
	Severity Severity `json:"severity"`
	Code     string   `json:"code"`
	Table    string   `json:"table,omitempty"`
	Count    int64    `json:"count,omitempty"`
	Detail   string   `json:"detail"`
}

type SourceSummary struct {
	SchemaVersion int    `json:"schema_version"`
	DatabaseSize  int64  `json:"database_size"`
	DatabaseHash  string `json:"database_sha256"`
	ManifestHash  string `json:"manifest_sha256"`
}

// Inspection is a redaction-safe report with a non-serialized source snapshot
// retained for Plan and a future target writer.
type Inspection struct {
	Source      SourceSummary       `json:"source"`
	Counts      map[string]int64    `json:"counts"`
	Diagnostics []Diagnostic        `json:"diagnostics,omitempty"`
	Valid       bool                `json:"valid"`
	Snapshot    sourcev228.Snapshot `json:"-"`
}

type Classification string

const (
	Preserve   Classification = "preserve"
	Inactive   Classification = "inactive"
	Reset      Classification = "reset"
	Rebuild    Classification = "rebuild"
	Quarantine Classification = "quarantine"
)

type ConversionState string

const (
	ConversionReady         ConversionState = "ready"
	ConversionPending       ConversionState = "pending_target_schema"
	ConversionInterrupted   ConversionState = "interrupted_unresolved"
	ConversionNotApplicable ConversionState = "not_applicable"
)

type TableDisposition struct {
	Table          string          `json:"table"`
	Rows           int64           `json:"rows"`
	Classification Classification  `json:"classification"`
	Conversion     ConversionState `json:"conversion"`
	ReasonCode     string          `json:"reason_code"`
}

type IdentityMapping struct {
	SourceTable string `json:"source_table"`
	SourceKey   string `json:"source_key"`
	TargetKind  string `json:"target_kind"`
	TargetID    string `json:"target_id"`
	Retained    bool   `json:"retained"`
}

type ReferenceMapping struct {
	SourceTable string `json:"source_table"`
	SourceKey   string `json:"source_key"`
	Field       string `json:"field"`
	TargetKind  string `json:"target_kind"`
	TargetID    string `json:"target_id,omitempty"`
	Resolved    bool   `json:"resolved"`
}

type MigrationPlan struct {
	FormatVersion        int                `json:"format_version"`
	Source               SourceSummary      `json:"source"`
	PlanHash             string             `json:"plan_sha256"`
	PreflightValid       bool               `json:"preflight_valid"`
	ExecutableConversion bool               `json:"executable_conversion"`
	Dispositions         []TableDisposition `json:"dispositions"`
	Identities           []IdentityMapping  `json:"identities"`
	References           []ReferenceMapping `json:"references"`
	Diagnostics          []Diagnostic       `json:"diagnostics,omitempty"`
}

// Inspector is convenient for product composition and easy to substitute in
// tests without exposing source database handles.
type Inspector interface {
	Inspect(context.Context, Bundle) (Inspection, error)
	Plan(Inspection) (MigrationPlan, error)
}

// MarshalReport demonstrates the supported redacted representation. The
// retained Snapshot is intentionally excluded by its json tag.
func (i Inspection) MarshalReport() ([]byte, error) { return json.MarshalIndent(i, "", "  ") }
