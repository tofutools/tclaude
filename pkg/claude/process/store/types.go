package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/common"
)

// DefaultRoot is the filesystem root for process-template authoring data.
// Legacy run data may still be found below runs/ until agentd performs the
// one-time P0 cleanup, but no runtime reads or writes it.
func DefaultRoot() string {
	return common.TclaudeStatePath("processes")
}

var (
	ErrNotFound               = errors.New("process store record not found")
	ErrTemplateConflict       = errors.New("process template content conflict")
	ErrTemplateSourceConflict = errors.New("process template source conflict")
	ErrContentMismatch        = errors.New("process store content does not match its ref")
	ErrTemplateSavePending    = errors.New("process template has an unfinished attributed save")
)

// ActorRef records template authorship without retaining the removed runtime
// state package. Authoring accepts only human and stable-agent identities;
// program and engine identities were execution concepts.
type ActorRef string

var (
	humanActorPattern = regexp.MustCompile(`^human:[A-Za-z0-9._@-]+$`)
	agentActorPattern = regexp.MustCompile(`^agent:agt_[A-Za-z0-9]+$`)
)

func ValidateActorRef(actor ActorRef) bool {
	value := string(actor)
	return humanActorPattern.MatchString(value) || agentActorPattern.MatchString(value)
}

// Templates stores immutable, content-addressed process-template semantics and
// their canonical layout-bearing authoring source.
type Templates interface {
	PutTemplate(ctx context.Context, tmpl *model.Template) (TemplateRecord, error)
	PutTemplateVersion(ctx context.Context, tmpl *model.Template) (TemplateRecord, error)
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

type TemplateRecord struct {
	ID           string    `json:"id"`
	Ref          string    `json:"ref"`
	SemanticHash string    `json:"semanticHash"`
	StoredAt     time.Time `json:"storedAt"`
}

// TemplateHead is the bounded observation shape for editor heads. Ref tracks
// semantic identity; SourceHash also advances for layout/source-only saves.
type TemplateHead struct {
	ID         string     `json:"id"`
	Ref        string     `json:"ref"`
	SourceHash string     `json:"sourceHash"`
	Actor      ActorRef   `json:"actor,omitempty"`
	AuthoredAt *time.Time `json:"authoredAt,omitempty"`
}

type TemplateAuthorship struct {
	Ref        string    `json:"ref"`
	SourceHash string    `json:"sourceHash"`
	Actor      ActorRef  `json:"actor"`
	AuthoredAt time.Time `json:"authoredAt"`
}

type TemplateAuthoringSnapshot struct {
	Source     []byte
	Authorship []TemplateAuthorship
}

type TemplateAuthoringCommit struct {
	TemplateRecord
	SourceHash string    `json:"sourceHash"`
	Actor      ActorRef  `json:"actor,omitempty"`
	AuthoredAt time.Time `json:"authoredAt,omitempty"`
}
