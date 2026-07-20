package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/state/epochv8"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
	"github.com/tofutools/tclaude/pkg/common"
)

// DefaultRoot is the filesystem store shared by the agentd engine and manual
// process CLI inspection (~/.tclaude/data/processes — private daemon state).
// Commands may still accept an explicit root for portable stores and tests.
func DefaultRoot() string {
	return common.TclaudeStatePath("processes")
}

var (
	ErrNotFound                = errors.New("process store record not found")
	ErrRunExists               = errors.New("process run already exists")
	ErrTemplateConflict        = errors.New("process template content conflict")
	ErrTemplateSourceConflict  = errors.New("process template source conflict")
	ErrContentMismatch         = errors.New("process store content does not match its ref")
	ErrLeaseHeld               = errors.New("process run lease is held")
	ErrRunInconsistent         = errors.New("process run state is inconsistent with evidence")
	ErrTemplateSavePending     = errors.New("process template has an unfinished attributed save")
	ErrUnsafeRunPath           = errors.New("process run path is not a regular directory")
	ErrExecutionViewOverBudget = errors.New("execution_view_over_budget")
	ErrViewerResourceLimit     = errors.New("process viewer resource limit exceeded")
	ErrWriterInProgress        = errors.New("process store writer is in progress")
	ErrTemplateInUse           = errors.New("process template is referenced by runs that have not finished")
	ErrRunResetRequired        = errors.New("process run schema 7 requires reset")
	ErrUnsupportedRunSchema    = errors.New("process run state schema is unsupported")
)

type RunSchemaKind string

const (
	RunSchemaLegacy        RunSchemaKind = "legacy"
	RunSchemaResetRequired RunSchemaKind = "reset_required"
	RunSchemaEpochV8       RunSchemaKind = "epoch_v8"
)

// ClassifyRunStateSchema is the exhaustive authority for persisted process
// state routing. Callers must not decode a newer schema with a legacy decoder.
func ClassifyRunStateSchema(version int) (RunSchemaKind, error) {
	switch {
	case version >= 1 && version <= state.StateSchemaVersion:
		return RunSchemaLegacy, nil
	case version == pathv1.CheckpointStateSchemaVersion:
		return RunSchemaResetRequired, nil
	case version == epochv8.StateSchemaVersion:
		return RunSchemaEpochV8, nil
	default:
		return "", fmt.Errorf("%w: %d", ErrUnsupportedRunSchema, version)
	}
}

// TemplateInUseError reports which runs blocked a template deletion. Callers
// surface the run ids so an operator can act on them instead of guessing what
// is still holding the template.
//
// The two categories are kept apart because they call for different action:
// RunIDs can be finished or cancelled, whereas UnreadableRunIDs are runs whose
// record could not be decoded at all. The guard fails closed on those — an
// unreadable run cannot be shown to be unrelated to this template — so they
// need repair or removal rather than completion.
type TemplateInUseError struct {
	TemplateID       string
	RunIDs           []string
	UnreadableRunIDs []string
}

func (e *TemplateInUseError) Error() string {
	switch {
	case len(e.RunIDs) > 0 && len(e.UnreadableRunIDs) > 0:
		return fmt.Sprintf(
			"process template %q is referenced by %d unfinished run(s): %s; and %d unreadable run(s) could not be cleared: %s",
			e.TemplateID, len(e.RunIDs), strings.Join(e.RunIDs, ", "),
			len(e.UnreadableRunIDs), strings.Join(e.UnreadableRunIDs, ", "),
		)
	case len(e.UnreadableRunIDs) > 0:
		return fmt.Sprintf(
			"process template %q cannot be deleted while %d unreadable run(s) remain: %s",
			e.TemplateID, len(e.UnreadableRunIDs), strings.Join(e.UnreadableRunIDs, ", "),
		)
	default:
		return fmt.Sprintf(
			"process template %q is referenced by %d unfinished run(s): %s",
			e.TemplateID, len(e.RunIDs), strings.Join(e.RunIDs, ", "),
		)
	}
}

func (e *TemplateInUseError) Unwrap() error { return ErrTemplateInUse }

// ExecutionViewOverBudgetError identifies the exact bounded-read dimension
// that refused an execution view. It is deliberately distinct from evidence
// read errors: exceeding a resource ceiling is never evidence of a torn write.
type ExecutionViewOverBudgetError struct {
	Limit          string
	Component      string
	Value, Maximum int64
}

func (e *ExecutionViewOverBudgetError) Error() string {
	if e == nil {
		return ""
	}
	component := ""
	if e.Component != "" {
		component = " for " + e.Component
	}
	return fmt.Sprintf("%v: %s%s is %d, maximum %d", ErrExecutionViewOverBudget, e.Limit, component, e.Value, e.Maximum)
}

func (e *ExecutionViewOverBudgetError) Unwrap() error { return ErrExecutionViewOverBudget }

// ExecutionView is valid only for the lifetime of the callback passed to
// FS.WithExecutionView. Snapshot and Template are detached values, but callers
// must not retain them: the callback boundary is what guarantees the run and
// exact-template locks still protect every verified premise.
type ExecutionView struct {
	Snapshot               Snapshot
	Template               *model.Template
	TemplateSourceHash     string
	LegacyCheckpointJSON   []byte
	LegacyAdminRecords     map[string]pathv1.PathV1AdminRecord
	LegacyAdminResolutions map[string]pathv1.BlockResolution
}

// PathV1ExecutionView is valid only during the callback supplied to
// FS.WithPathV1ExecutionView. It contains the exact descriptor-safe checkpoint
// and template source from one run-then-template locked observation. Input is
// sealed by pathv1 verification and must not be retained past the callback.
type PathV1ExecutionView struct {
	Run            RunRecord
	Template       *model.Template
	TemplateSource []byte
	CheckpointJSON []byte
	Checkpoint     *pathv1.CheckpointV7
	Binding        pathv1.CheckpointBinding
	Input          *pathv1.VerifiedExclusiveInput
}

// PathV1RunSnapshot is the detached schema-7 read shape for live API and
// viewer callers. Unlike PathV1ExecutionView it may outlive the callback;
// every byte slice and checkpoint value is independently decoded/copied.
type PathV1RunSnapshot struct {
	Run            RunRecord
	CheckpointJSON []byte
	TemplateSource []byte
	Checkpoint     *pathv1.CheckpointV7
	// LegacyEvidence is populated only by LoadPathV1RunHistoryView and only
	// when the checkpoint carries migration projection metadata. It is raw,
	// bounded evidence; semantic replay belongs below verify/view consumers.
	LegacyEvidence        *PathV1LegacyEvidence
	LegacyEvidenceFailure PathV1LegacyEvidenceFailure
}

type PathV1LegacyEvidence struct {
	Manifest []evidence.ManifestEntry
	NodeLogs []evidence.NodeLog
}

// PathV1LegacyEvidenceFailure is deliberately content-free. A migrated
// history read can fail without invalidating the separately verified current
// schema-7 checkpoint used by the live viewer.
type PathV1LegacyEvidenceFailure string

const (
	PathV1LegacyEvidenceInvalid       PathV1LegacyEvidenceFailure = "invalid"
	PathV1LegacyEvidenceUnavailable   PathV1LegacyEvidenceFailure = "unavailable"
	PathV1LegacyEvidenceResourceLimit PathV1LegacyEvidenceFailure = "resource_limit"
)

type PathV1AppendDisposition string

const (
	PathV1AppendApplied        PathV1AppendDisposition = "applied"
	PathV1AppendAlreadyApplied PathV1AppendDisposition = "already_applied"
)

type PathV1AppendResult struct {
	Disposition PathV1AppendDisposition
	Binding     pathv1.CheckpointBinding
	Checkpoint  *pathv1.CheckpointV7
}

// ExecutionViewInconsistentError marks a stable persisted-data failure. Anchor
// and invariant disagreements receive a bounded second observation before this
// classification; immutable-template failures are already stable under lock.
type ExecutionViewInconsistentError struct {
	Err error
}

func (e *ExecutionViewInconsistentError) Error() string {
	if e == nil || e.Err == nil {
		return ErrRunInconsistent.Error()
	}
	return fmt.Sprintf("%v: %v", ErrRunInconsistent, e.Err)
}

func (e *ExecutionViewInconsistentError) Unwrap() []error {
	if e == nil || e.Err == nil {
		return []error{ErrRunInconsistent}
	}
	return []error{ErrRunInconsistent, e.Err}
}

// DecodeError identifies persisted JSON that was read successfully but could
// not be decoded. Viewer callers may distinguish corrupt history from
// permission, device, and transient filesystem failures without parsing text.
type DecodeError struct {
	Component string
	Err       error
}

func (e *DecodeError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("decode process %s: %v", e.Component, e.Err)
}

func (e *DecodeError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func IsDecodeError(err error) bool {
	var decodeErr *DecodeError
	return errors.As(err, &decodeErr)
}

type ConflictError struct {
	RunID       string
	ExpectedSeq int64
	ActualSeq   int64
}

func (e *ConflictError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("process run %q append conflict: expected seq %d, actual seq %d", e.RunID, e.ExpectedSeq, e.ActualSeq)
}

func IsConflict(err error) bool {
	var conflict *ConflictError
	return errors.As(err, &conflict)
}

// Templates stores immutable, content-addressed process template semantics and
// their canonical authoring source. Callers must treat Layout as mutable
// source/editor metadata attached to a semantic version, never as part of the
// run-pinned copy or its semantic identity.
type Templates interface {
	PutTemplate(ctx context.Context, tmpl *model.Template) (TemplateRecord, error)
	GetTemplate(ctx context.Context, ref string) (*model.Template, error)
	GetTemplateSource(ctx context.Context, ref string) ([]byte, error)
	GetTemplateHead(ctx context.Context, id string) (TemplateRecord, error)
	ListTemplateHeads(ctx context.Context) ([]TemplateHead, error)
	ListTemplates(ctx context.Context) ([]TemplateRecord, error)
}

type TemplateSourceConflictError struct {
	CurrentRef        string
	CurrentSourceHash string
}

func (e *TemplateSourceConflictError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%v: current head %q has source hash %q", ErrTemplateSourceConflict, e.CurrentRef, e.CurrentSourceHash)
}

func (e *TemplateSourceConflictError) Unwrap() error { return ErrTemplateSourceConflict }

type Runs interface {
	CreateRun(ctx context.Context, run RunRecord, initial state.State) (RunRecord, error)
	GetRun(ctx context.Context, runID string) (RunRecord, error)
	LoadRunState(ctx context.Context, runID string) (*state.State, error)
	LoadRun(ctx context.Context, runID string) (Snapshot, error)
	ListRuns(ctx context.Context) ([]RunRecord, error)
}

type Events interface {
	Append(ctx context.Context, runID string, expectedSeq int64, entries []evidence.LogEntry) (AppendResult, error)
	ReadManifest(ctx context.Context, runID string) ([]evidence.ManifestEntry, error)
	ReadNodeLog(ctx context.Context, runID, nodeID string) ([]evidence.LogEntry, error)
	ReadRunLog(ctx context.Context, runID string) ([]evidence.LogEntry, error)
}

type Artifacts interface {
	PutArtifact(ctx context.Context, runID, name string, r io.Reader) (ArtifactRecord, error)
	GetArtifact(ctx context.Context, runID, ref string) (io.ReadCloser, error)
}

type Leases interface {
	AcquireRunLease(ctx context.Context, runID, holder string, ttl time.Duration) (LeaseRecord, error)
	ReleaseRunLease(ctx context.Context, runID, holder string) error
}

type Store interface {
	Templates
	Runs
	Events
	Artifacts
	Leases
}

type TemplateRecord struct {
	ID           string    `json:"id"`
	Ref          string    `json:"ref"`
	SemanticHash string    `json:"semanticHash"`
	StoredAt     time.Time `json:"storedAt"`
}

// TemplateHead is the bounded observation shape for editor heads. Ref tracks
// semantic identity; SourceHash also advances for layout/source-only saves
// that retain the same Ref. Actor and AuthoredAt are an optional exact-head
// index: legacy/manual heads leave both empty rather than inferring provenance.
type TemplateHead struct {
	ID         string         `json:"id"`
	Ref        string         `json:"ref"`
	SourceHash string         `json:"sourceHash"`
	Actor      state.ActorRef `json:"actor,omitempty"`
	AuthoredAt *time.Time     `json:"authoredAt,omitempty"`
}

// TemplateAuthorship is one append-only authoring event for a process-template
// semantic version. SourceHash distinguishes layout/source-only edits that
// deliberately share one content-addressed Ref; keeping every event means
// attribution does not collapse into mutable last-writer metadata.
type TemplateAuthorship struct {
	Ref        string         `json:"ref"`
	SourceHash string         `json:"sourceHash"`
	Actor      state.ActorRef `json:"actor"`
	AuthoredAt time.Time      `json:"authoredAt"`
}

// TemplateAuthoringSnapshot is one lock-consistent view of a version's
// layout-bearing source and its append-only authoring provenance.
type TemplateAuthoringSnapshot struct {
	Source     []byte
	Authorship []TemplateAuthorship
}

// TemplateAuthoringCommit is the immutable result of one CAS save, captured
// before releasing the template lock. TemplateRecord is embedded so existing
// record consumers retain direct access to Ref and SemanticHash.
type TemplateAuthoringCommit struct {
	TemplateRecord
	SourceHash string         `json:"sourceHash"`
	Actor      state.ActorRef `json:"actor,omitempty"`
	AuthoredAt time.Time      `json:"authoredAt,omitempty"`
}

type RunRecord struct {
	ID          string `json:"id"`
	TemplateRef string `json:"templateRef"`
	// Template is the immutable canonical snapshot pinned at instantiation.
	// Keeping it inside run.json makes the run independently auditable after
	// the store-level template library is unavailable. Legacy runs omit it and
	// verification falls back to TemplateRef.
	Template      *model.Template   `json:"template,omitempty"`
	Params        map[string]string `json:"params,omitempty"`
	AllowPrograms bool              `json:"allowPrograms,omitempty"`
	CreatedAt     time.Time         `json:"createdAt"`
	UpdatedAt     time.Time         `json:"updatedAt"`
}

type Snapshot struct {
	Run      RunRecord
	State    *state.State
	Manifest []evidence.ManifestEntry
	NodeLogs []evidence.NodeLog
}

type AppendResult struct {
	Entries  []evidence.LogEntry
	Manifest []evidence.ManifestEntry
	State    *state.State
}

type ArtifactRecord struct {
	Ref    string    `json:"ref"`
	Name   string    `json:"name,omitempty"`
	Size   int64     `json:"size"`
	SHA256 string    `json:"sha256"`
	At     time.Time `json:"at"`
}

type LeaseRecord struct {
	RunID  string `json:"runId"`
	Holder string `json:"holder"`
	// Kind is omitted by legacy engine leases. An empty persisted kind is
	// therefore interpreted as LeaseKindEngine, never as an untyped domain.
	Kind      LeaseKind `json:"kind,omitempty"`
	Token     string    `json:"token,omitempty"`
	ExpiresAt time.Time `json:"expiresAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type LeaseKind string

const (
	LeaseKindEngine      LeaseKind = "engine"
	LeaseKindMaintenance LeaseKind = "maintenance"

	MaxLeaseHolderBytes = 256
	MaxLeaseTTL         = time.Hour
)

type MaintenanceLease struct {
	RunID     string
	Holder    string
	Token     string
	ExpiresAt time.Time
}

const (
	EpochV8MaxSourceBytes    = model.MaxProcessTemplateSourceBytes
	EpochV8MaxReasonBytes    = 64 << 10
	EpochV8MaxTotalReadBytes = 64 << 20
	EpochV8GCMaxEntries      = 128
	EpochV8GCMinOrphanAge    = time.Hour

	EpochV8InitialFrontierLocalID       = "initial-frontier"
	EpochV8InitialFrontierReservationID = "initial-reservation"
)

type EpochV8InitializationDisposition string

const (
	EpochV8InitializationApplied        EpochV8InitializationDisposition = "applied"
	EpochV8InitializationAlreadyApplied EpochV8InitializationDisposition = "already_applied"
)

type EpochV8InitializationResult struct {
	Disposition EpochV8InitializationDisposition
	Run         RunRecord
	Checkpoint  *epochv8.CheckpointV8
}

type EpochV8RunSnapshot struct {
	Run            RunRecord
	CheckpointJSON []byte
	Checkpoint     *epochv8.CheckpointV8
	EpochSources   map[epochv8.EpochID][]byte
}

func (snapshot EpochV8RunSnapshot) SourceForOwner(owner epochv8.OwnerIdentity) ([]byte, error) {
	if snapshot.Checkpoint == nil {
		return nil, fmt.Errorf("%w: schema-8 checkpoint is absent", ErrRunInconsistent)
	}
	for _, authority := range snapshot.Checkpoint.View().Authorities {
		if authority.Identity == owner {
			source, ok := snapshot.EpochSources[authority.EpochID]
			if !ok {
				return nil, fmt.Errorf("%w: owner epoch source is absent", ErrRunInconsistent)
			}
			return append([]byte(nil), source...), nil
		}
	}
	return nil, fmt.Errorf("%w: owner identity is absent", ErrRunInconsistent)
}

type EpochV8PublicationResult struct {
	Disposition epochv8.Disposition
	Binding     epochv8.Binding
	Checkpoint  *epochv8.CheckpointV8
}

type EpochV8GCResult struct {
	Scanned int
	Removed int
}
