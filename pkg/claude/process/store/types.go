package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/common"
)

// DefaultRoot is the filesystem store shared by the agentd engine and manual
// process CLI inspection (~/.tclaude/data/processes — private daemon state).
// Commands may still accept an explicit root for portable stores and tests.
func DefaultRoot() string {
	return common.TclaudeStatePath("processes")
}

var (
	ErrNotFound               = errors.New("process store record not found")
	ErrTemplateConflict       = errors.New("process template content conflict")
	ErrTemplateSourceConflict = errors.New("process template source conflict")
	ErrContentMismatch        = errors.New("process store content does not match its ref")
	ErrLeaseHeld              = errors.New("process run lease is held")
	ErrRunInconsistent        = errors.New("process run state is inconsistent with evidence")
)

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

// TemplateHead is the bounded observation shape for editor heads. Unlike a
// TemplateRecord it deliberately carries no version metadata: polling callers
// only need to know whether the set of ids/refs changed.
type TemplateHead struct {
	ID  string `json:"id"`
	Ref string `json:"ref"`
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
	RunID     string    `json:"runId"`
	Holder    string    `json:"holder"`
	ExpiresAt time.Time `json:"expiresAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}
