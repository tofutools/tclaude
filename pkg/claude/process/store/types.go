package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
)

// DefaultRoot is the filesystem store shared by the agentd engine and manual
// process CLI inspection. Commands may still accept an explicit root for
// portable stores and tests.
func DefaultRoot() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".tclaude", "processes")
}

var (
	ErrNotFound         = errors.New("process store record not found")
	ErrTemplateConflict = errors.New("process template content conflict")
	ErrContentMismatch  = errors.New("process store content does not match its ref")
	ErrLeaseHeld        = errors.New("process run lease is held")
	ErrRunInconsistent  = errors.New("process run state is inconsistent with evidence")
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

// Templates stores immutable, content-addressed process template semantics.
// Implementations may persist only model.CanonicalSemanticJSON: callers should
// treat Layout as source/editor metadata, not as part of the run-pinned copy.
type Templates interface {
	PutTemplate(ctx context.Context, tmpl *model.Template) (TemplateRecord, error)
	GetTemplate(ctx context.Context, ref string) (*model.Template, error)
	ListTemplates(ctx context.Context) ([]TemplateRecord, error)
}

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

type RunRecord struct {
	ID            string            `json:"id"`
	TemplateRef   string            `json:"templateRef"`
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
