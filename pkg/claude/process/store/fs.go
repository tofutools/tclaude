package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

var safeSegmentPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

type FS struct {
	root                          string
	now                           func() time.Time
	templateLockContendedHook     func()
	templateAuthoringSnapshotHook func()
	templateAuthoringCommitHook   func(TemplateAuthoringCommit)
}

var processLocks sync.Map

func NewFS(root string) (*FS, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("process store root is required")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve absolute process store root: %w", err)
	}
	if err := os.MkdirAll(absRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create process store root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve process store root: %w", err)
	}
	return &FS{root: resolved, now: time.Now}, nil
}

// SetNowForTest swaps the filesystem store clock and returns a restore
// function. Callers must install it before using the store concurrently.
func (s *FS) SetNowForTest(now func() time.Time) func() {
	previous := s.now
	s.now = now
	return func() { s.now = previous }
}

func (s *FS) PutTemplate(ctx context.Context, tmpl *model.Template) (TemplateRecord, error) {
	return s.putTemplate(ctx, tmpl, true)
}

// PutTemplateVersion persists immutable template content without changing the
// editor head. This lets callers import or preserve historical versions without
// changing which version editors reopen.
func (s *FS) PutTemplateVersion(ctx context.Context, tmpl *model.Template) (TemplateRecord, error) {
	return s.putTemplate(ctx, tmpl, false)
}

func (s *FS) putTemplate(ctx context.Context, tmpl *model.Template, moveHead bool) (TemplateRecord, error) {
	if tmpl == nil {
		return TemplateRecord{}, fmt.Errorf("nil process template")
	}
	unlock, err := s.lockTemplate(ctx, tmpl.ID)
	if err != nil {
		return TemplateRecord{}, err
	}
	defer unlock()
	if err := s.recoverAttributedTemplateSaveUnlocked(ctx, tmpl.ID); err != nil {
		return TemplateRecord{}, err
	}
	var preservedHead TemplateRecord
	if !moveHead {
		preservedHead, err = s.getTemplateHeadUnlocked(ctx, tmpl.ID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return TemplateRecord{}, err
		}
		// getTemplateHeadUnlocked materializes a legacy fallback before the new
		// version is published. If migration fails, the write fails closed.
	}
	record, err := s.putTemplateUnlocked(ctx, tmpl, false)
	if err != nil {
		return TemplateRecord{}, err
	}
	if moveHead || preservedHead.Ref == "" {
		// A first non-moving write still establishes a head. Later imports preserve
		// it through the pre-publication path above.
		head, err := s.templateHeadForRecordUnlocked(ctx, record)
		if err != nil {
			return TemplateRecord{}, err
		}
		if err := s.writeTemplateHead(head); err != nil {
			return TemplateRecord{}, err
		}
	}
	return record, nil
}

// PutTemplateEditorSource persists an editor save. Semantic content remains
// immutable and content-addressed exactly like PutTemplate; the only extra
// behavior is updating the layout-bearing canonical source attachment when
// the semantic ref already exists. expectedSourceHash is compared to the
// current head under the same cross-process filesystem lock as the write and
// head-pointer update, so two editors cannot silently lose layout changes.
func (s *FS) PutTemplateEditorSource(ctx context.Context, tmpl *model.Template, expectedSourceHash string) (TemplateRecord, error) {
	commit, err := s.putTemplateEditorSource(ctx, tmpl, expectedSourceHash, "")
	return commit.TemplateRecord, err
}

// PutTemplateEditorSourceAttributed is PutTemplateEditorSource with a durable,
// append-preserving actor record. It is used by authenticated authoring
// surfaces; file-backed/manual store writes continue to have no invented
// actor. Repeated layout-only saves retain one event per source hash even when
// their semantic ref is identical. The returned commit is captured from the
// stored bytes before this method releases the template lock.
func (s *FS) PutTemplateEditorSourceAttributed(
	ctx context.Context,
	tmpl *model.Template,
	expectedSourceHash string,
	actor ActorRef,
) (TemplateAuthoringCommit, error) {
	if !ValidateActorRef(actor) {
		return TemplateAuthoringCommit{}, fmt.Errorf("invalid process template authoring actor %q", actor)
	}
	return s.putTemplateEditorSource(ctx, tmpl, expectedSourceHash, actor)
}

func (s *FS) putTemplateEditorSource(
	ctx context.Context,
	tmpl *model.Template,
	expectedSourceHash string,
	actor ActorRef,
) (TemplateAuthoringCommit, error) {
	if tmpl == nil {
		return TemplateAuthoringCommit{}, fmt.Errorf("nil process template")
	}
	unlock, err := s.lockTemplate(ctx, tmpl.ID)
	if err != nil {
		return TemplateAuthoringCommit{}, err
	}
	defer unlock()
	if err := s.recoverAttributedTemplateSaveUnlocked(ctx, tmpl.ID); err != nil {
		return TemplateAuthoringCommit{}, err
	}
	head, headErr := s.getTemplateHeadUnlocked(ctx, tmpl.ID)
	currentHash := ""
	if headErr == nil {
		id, hash, err := parseTemplateRef(head.Ref)
		if err != nil {
			return TemplateAuthoringCommit{}, err
		}
		source, err := s.getTemplateSourceUnlocked(ctx, id, hash, head.Ref)
		if err != nil {
			return TemplateAuthoringCommit{}, err
		}
		currentHash = sourceHash(source)
	} else if !errors.Is(headErr, ErrNotFound) {
		return TemplateAuthoringCommit{}, headErr
	}
	if expectedSourceHash != currentHash {
		return TemplateAuthoringCommit{}, &TemplateSourceConflictError{
			CurrentRef: head.Ref, CurrentSourceHash: currentHash,
		}
	}
	var intent *templateSaveIntent
	if actor != "" {
		intent, err = s.newTemplateSaveIntent(tmpl)
		if err != nil {
			return TemplateAuthoringCommit{}, err
		}
		if err := s.writeTemplateSaveIntent(intent); err != nil {
			return TemplateAuthoringCommit{}, err
		}
	}
	rollback := func(cause error) (TemplateAuthoringCommit, error) {
		if intent == nil {
			return TemplateAuthoringCommit{}, cause
		}
		if rollbackErr := s.rollbackTemplateSaveIntent(intent); rollbackErr != nil {
			cause = errors.Join(cause, fmt.Errorf("rollback attributed process template save: %w", rollbackErr))
		}
		return TemplateAuthoringCommit{}, cause
	}
	record, err := s.putTemplateUnlocked(ctx, tmpl, true)
	if err != nil {
		return rollback(err)
	}
	committedID, committedHash, err := parseTemplateRef(record.Ref)
	if err != nil {
		return rollback(err)
	}
	committedSource, err := s.getTemplateSourceUnlocked(ctx, committedID, committedHash, record.Ref)
	if err != nil {
		return rollback(err)
	}
	commit := TemplateAuthoringCommit{
		TemplateRecord: record,
		SourceHash:     sourceHash(committedSource),
	}
	if actor != "" {
		event := TemplateAuthorship{Ref: record.Ref, SourceHash: commit.SourceHash, Actor: actor, AuthoredAt: s.now().UTC()}
		if err := s.appendTemplateAuthorship(ctx, record, event); err != nil {
			return rollback(err)
		}
		commit.Actor = event.Actor
		commit.AuthoredAt = event.AuthoredAt
	}
	publishedHead := TemplateHead{ID: record.ID, Ref: record.Ref, SourceHash: commit.SourceHash, Actor: commit.Actor}
	if !commit.AuthoredAt.IsZero() {
		authoredAt := commit.AuthoredAt
		publishedHead.AuthoredAt = &authoredAt
	}
	if err := s.writeTemplateHead(publishedHead); err != nil {
		return rollback(err)
	}
	if intent != nil {
		if err := syncTemplateSaveIntentDirs(intent); err != nil {
			return rollback(err)
		}
		if err := s.finishTemplateSaveIntent(intent.ID); err != nil {
			return rollback(err)
		}
	}
	if s.templateAuthoringCommitHook != nil {
		s.templateAuthoringCommitHook(commit)
	}
	return commit, nil
}

const (
	templateSaveIntentLegacyVersion = 1
	templateSaveIntentVersion       = 2
)

// An attributed editor save updates up to five filesystem records. Its durable
// intent is written and synced before those mutations. A process crash leaves
// the intent behind, and every template read/list/write path recovers it under
// the same per-template lock before exposing state. If recovery cannot finish,
// reads fail closed rather than publishing content without its actor event.
type templateSaveIntent struct {
	Version      int                        `json:"version"`
	ID           string                     `json:"id"`
	SemanticHash string                     `json:"semanticHash"`
	Files        []templateSaveFileSnapshot `json:"files"`
}

type templateSaveFileSnapshot struct {
	Path    string      `json:"path"`
	Data    []byte      `json:"data,omitempty"`
	Mode    os.FileMode `json:"mode,omitempty"`
	ModTime time.Time   `json:"modTime,omitempty"`
	Exists  bool        `json:"exists"`
}

func (s *FS) newTemplateSaveIntent(tmpl *model.Template) (*templateSaveIntent, error) {
	semanticHash, err := model.SemanticHash(tmpl)
	if err != nil {
		return nil, err
	}
	dir, err := s.templateDir(tmpl.ID, semanticHash)
	if err != nil {
		return nil, err
	}
	paths := []string{
		filepath.Join(s.root, "templates", tmpl.ID, "head"),
		filepath.Join(s.root, "templates", tmpl.ID, "head-attribution"),
		filepath.Join(dir, "template.yaml"),
		filepath.Join(dir, "template.json"),
		filepath.Join(dir, "authorship.jsonl"),
	}
	snapshots := make([]templateSaveFileSnapshot, 0, len(paths))
	for _, path := range paths {
		snapshot, err := snapshotTemplateSaveFile(path)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snapshot)
	}
	return &templateSaveIntent{
		Version: templateSaveIntentVersion, ID: tmpl.ID, SemanticHash: semanticHash, Files: snapshots,
	}, nil
}

func snapshotTemplateSaveFile(path string) (templateSaveFileSnapshot, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return templateSaveFileSnapshot{Path: path}, nil
	}
	if err != nil {
		return templateSaveFileSnapshot{}, fmt.Errorf("snapshot process template save file %q: %w", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return templateSaveFileSnapshot{}, fmt.Errorf("stat process template save file %q: %w", path, err)
	}
	return templateSaveFileSnapshot{
		Path: path, Data: data, Mode: info.Mode().Perm(), ModTime: info.ModTime(), Exists: true,
	}, nil
}

func (s *FS) templateSaveIntentPath(id string) (string, error) {
	if err := safeSegment(id); err != nil {
		return "", fmt.Errorf("invalid template id: %w", err)
	}
	return filepath.Join(s.root, "templates", id, ".attributed-save-intent.json"), nil
}

func (s *FS) writeTemplateSaveIntent(intent *templateSaveIntent) error {
	path, err := s.templateSaveIntentPath(intent.ID)
	if err != nil {
		return err
	}
	data, err := json.Marshal(intent)
	if err != nil {
		return fmt.Errorf("encode attributed process template save intent: %w", err)
	}
	if err := writeFileAtomic(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write attributed process template save intent: %w", err)
	}
	// writeFileAtomic intentionally treats directory-sync failure as best-effort
	// for general store files. The intent is the transaction's recovery anchor,
	// so authoring must not mutate anything until its directory entry is known
	// durable.
	if err := syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync attributed process template save intent: %w", err)
	}
	return nil
}

func (s *FS) rollbackTemplateSaveIntent(intent *templateSaveIntent) error {
	if err := s.validateTemplateSaveIntent(intent); err != nil {
		return err
	}
	if err := restoreTemplateSaveFiles(intent.Files); err != nil {
		// Keep the durable marker: later reads must retry recovery or fail
		// closed instead of treating a partial save as committed.
		return err
	}
	if err := syncTemplateSaveIntentDirs(intent); err != nil {
		return err
	}
	return s.finishTemplateSaveIntent(intent.ID)
}

func syncTemplateSaveIntentDirs(intent *templateSaveIntent) error {
	seen := make(map[string]struct{}, len(intent.Files))
	for _, snapshot := range intent.Files {
		dir := filepath.Dir(snapshot.Path)
		if _, ok := seen[dir]; ok {
			continue
		}
		seen[dir] = struct{}{}
		if err := syncDir(dir); err != nil {
			return fmt.Errorf("sync attributed process template save directory %q: %w", dir, err)
		}
	}
	return nil
}

func (s *FS) finishTemplateSaveIntent(id string) error {
	path, err := s.templateSaveIntentPath(id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove attributed process template save intent: %w", err)
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync attributed process template save intent removal: %w", err)
	}
	return nil
}

func (s *FS) recoverAttributedTemplateSaveUnlocked(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.templateSaveIntentPath(id)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read attributed process template save intent: %w", err)
	}
	var intent templateSaveIntent
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&intent); err != nil {
		return fmt.Errorf("decode attributed process template save intent: %w", err)
	}
	if intent.ID != id {
		return fmt.Errorf("%w: attributed save intent id %q does not match %q", ErrContentMismatch, intent.ID, id)
	}
	if err := s.rollbackTemplateSaveIntent(&intent); err != nil {
		return fmt.Errorf("recover attributed process template save: %w", err)
	}
	return nil
}

func (s *FS) validateTemplateSaveIntent(intent *templateSaveIntent) error {
	if intent == nil || (intent.Version != templateSaveIntentLegacyVersion && intent.Version != templateSaveIntentVersion) {
		return fmt.Errorf("%w: unsupported attributed save intent", ErrContentMismatch)
	}
	dir, err := s.templateDir(intent.ID, intent.SemanticHash)
	if err != nil {
		return err
	}
	want := []string{
		filepath.Join(s.root, "templates", intent.ID, "head"),
		filepath.Join(s.root, "templates", intent.ID, "head-attribution"),
		filepath.Join(dir, "template.yaml"),
		filepath.Join(dir, "template.json"),
		filepath.Join(dir, "authorship.jsonl"),
	}
	if intent.Version == templateSaveIntentLegacyVersion {
		want = append(want[:1], want[2:]...)
	}
	if len(intent.Files) != len(want) {
		return fmt.Errorf("%w: attributed save intent has %d files", ErrContentMismatch, len(intent.Files))
	}
	for i := range want {
		if intent.Files[i].Path != want[i] {
			return fmt.Errorf("%w: attributed save intent path %q", ErrContentMismatch, intent.Files[i].Path)
		}
	}
	return nil
}

func restoreTemplateSaveFiles(snapshots []templateSaveFileSnapshot) error {
	var errs []error
	for i := len(snapshots) - 1; i >= 0; i-- {
		snapshot := snapshots[i]
		if !snapshot.Exists {
			if err := os.Remove(snapshot.Path); err != nil {
				if !errors.Is(err, os.ErrNotExist) {
					errs = append(errs, fmt.Errorf("remove %q: %w", snapshot.Path, err))
				}
			} else if err := syncDir(filepath.Dir(snapshot.Path)); err != nil {
				errs = append(errs, fmt.Errorf("sync removal of %q: %w", snapshot.Path, err))
			}
			continue
		}
		if err := writeFileAtomic(snapshot.Path, snapshot.Data, snapshot.Mode); err != nil {
			errs = append(errs, fmt.Errorf("restore %q: %w", snapshot.Path, err))
			continue
		}
		if err := os.Chtimes(snapshot.Path, snapshot.ModTime, snapshot.ModTime); err != nil {
			errs = append(errs, fmt.Errorf("restore timestamps for %q: %w", snapshot.Path, err))
			continue
		}
		if err := syncTemplateSaveFile(snapshot.Path); err != nil {
			errs = append(errs, fmt.Errorf("sync restored process template save file %q: %w", snapshot.Path, err))
		}
	}
	return errors.Join(errs...)
}

func syncTemplateSaveFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return maybeSync(f)
}

// ListTemplateAuthorship returns authoring events in append order. Legacy
// versions have no authorship sidecar and therefore return an empty slice.
func (s *FS) ListTemplateAuthorship(ctx context.Context, ref string) ([]TemplateAuthorship, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id, hash, err := parseTemplateRef(ref)
	if err != nil {
		return nil, err
	}
	unlock, err := s.lockTemplate(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := s.recoverAttributedTemplateSaveUnlocked(ctx, id); err != nil {
		return nil, err
	}
	return s.listTemplateAuthorshipUnlocked(id, hash, ref)
}

func (s *FS) listTemplateAuthorshipUnlocked(id, hash, ref string) ([]TemplateAuthorship, error) {
	dir, err := s.templateDir(id, hash)
	if err != nil {
		return nil, err
	}
	return readTemplateAuthorship(filepath.Join(dir, "authorship.jsonl"), ref)
}

func (s *FS) appendTemplateAuthorship(ctx context.Context, record TemplateRecord, event TemplateAuthorship) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := s.templateDir(record.ID, record.SemanticHash)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "authorship.jsonl")
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read process template authorship: %w", err)
	}
	if err == nil {
		if _, err := decodeTemplateAuthorship(existing, record.Ref); err != nil {
			return err
		}
	}
	line, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode process template authorship: %w", err)
	}
	data := append(append([]byte(nil), existing...), line...)
	data = append(data, '\n')
	if err := writeFileAtomic(path, data, 0o644); err != nil {
		return fmt.Errorf("write process template authorship: %w", err)
	}
	return nil
}

func readTemplateAuthorship(path, ref string) ([]TemplateAuthorship, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return []TemplateAuthorship{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read process template authorship: %w", err)
	}
	return decodeTemplateAuthorship(data, ref)
}

func decodeTemplateAuthorship(data []byte, ref string) ([]TemplateAuthorship, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	events := []TemplateAuthorship{}
	for {
		var event TemplateAuthorship
		if err := dec.Decode(&event); errors.Is(err, io.EOF) {
			return events, nil
		} else if err != nil {
			return nil, fmt.Errorf("decode process template authorship: %w", err)
		}
		if event.Ref != ref || event.SourceHash == "" || !ValidateActorRef(event.Actor) || event.AuthoredAt.IsZero() {
			return nil, fmt.Errorf("%w: invalid authorship event for %q", ErrContentMismatch, ref)
		}
		events = append(events, event)
	}
}

func (s *FS) putTemplateUnlocked(ctx context.Context, tmpl *model.Template, updateEditorSource bool) (TemplateRecord, error) {
	if err := ctx.Err(); err != nil {
		return TemplateRecord{}, err
	}
	if tmpl == nil {
		return TemplateRecord{}, fmt.Errorf("nil process template")
	}
	semanticHash, err := model.SemanticHash(tmpl)
	if err != nil {
		return TemplateRecord{}, err
	}
	ref := model.TemplateRef(tmpl.ID, semanticHash)
	if ref == "" {
		return TemplateRecord{}, fmt.Errorf("template id and semantic hash are required")
	}
	body, err := model.CanonicalSemanticJSON(tmpl)
	if err != nil {
		return TemplateRecord{}, err
	}
	source, err := model.CanonicalYAML(tmpl)
	if err != nil {
		return TemplateRecord{}, err
	}
	dir, err := s.templateDir(tmpl.ID, semanticHash)
	if err != nil {
		return TemplateRecord{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return TemplateRecord{}, fmt.Errorf("create template dir: %w", err)
	}
	bodyPath := filepath.Join(dir, "template.json")
	if existing, err := os.ReadFile(bodyPath); err == nil {
		if !bytes.Equal(existing, body) {
			return TemplateRecord{}, fmt.Errorf("%w: %s", ErrTemplateConflict, ref)
		}
		if updateEditorSource {
			// Layout is mutable presentation state attached to a semantic
			// version. Replacing template.yaml here is sanctioned: layout is
			// excluded from CanonicalSemanticJSON, so version semantics and their
			// content identity do not change. PutTemplateEditorSource guards
			// this last-write-wins update with a filesystem-locked sourceHash CAS.
			if err := writeFileAtomic(filepath.Join(dir, "template.yaml"), source, 0o644); err != nil {
				return TemplateRecord{}, err
			}
		}
		return TemplateRecord{ID: tmpl.ID, Ref: ref, SemanticHash: semanticHash, StoredAt: fileModTime(bodyPath)}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return TemplateRecord{}, fmt.Errorf("read existing template: %w", err)
	}
	// The immutable template.json intentionally excludes editor-only layout,
	// while template.yaml keeps the complete canonical authoring document.
	// Legacy versions created before template.yaml was introduced are handled by GetTemplateSource's
	// canonical fallback rather than being rewritten in place.
	if err := writeFileAtomic(filepath.Join(dir, "template.yaml"), source, 0o644); err != nil {
		return TemplateRecord{}, err
	}
	// Publish template.json last: ListTemplates treats that file as the commit
	// marker, so an interrupted first write cannot expose a half-version.
	if err := writeFileAtomic(bodyPath, body, 0o644); err != nil {
		return TemplateRecord{}, err
	}
	return TemplateRecord{ID: tmpl.ID, Ref: ref, SemanticHash: semanticHash, StoredAt: s.now().UTC()}, nil
}

// GetTemplateHead returns the explicitly selected editor head. Stores created
// before head pointers existed fall back to their newest template.json mtime.
func (s *FS) GetTemplateHead(ctx context.Context, id string) (TemplateRecord, error) {
	if err := ctx.Err(); err != nil {
		return TemplateRecord{}, err
	}
	unlock, err := s.lockTemplate(ctx, id)
	if err != nil {
		return TemplateRecord{}, err
	}
	defer unlock()
	if err := s.recoverAttributedTemplateSaveUnlocked(ctx, id); err != nil {
		return TemplateRecord{}, err
	}
	return s.getTemplateHeadUnlocked(ctx, id)
}

// GetTemplateHeadGeneration returns the bounded exact editor generation,
// including optional same-commit attribution. It never reads template source
// or append-only authorship history.
func (s *FS) GetTemplateHeadGeneration(ctx context.Context, id string) (TemplateHead, error) {
	if err := ctx.Err(); err != nil {
		return TemplateHead{}, err
	}
	unlock, err := s.lockTemplate(ctx, id)
	if err != nil {
		return TemplateHead{}, err
	}
	defer unlock()
	if err := s.recoverAttributedTemplateSaveUnlocked(ctx, id); err != nil {
		return TemplateHead{}, err
	}
	head, _, err := s.getTemplateHeadStateUnlocked(ctx, id)
	return head, err
}

// ListTemplateHeads returns one small, sorted, committed authoring generation
// per template. Each read takes the existing per-template lock and recovers a
// crash intent before observing the pointer. Ref-only and pointer-less legacy
// heads are resolved and materialized once; steady-state polling reads only the
// bounded pointer file. Empty first-create/orphan directories are skipped.
func (s *FS) ListTemplateHeads(ctx context.Context) ([]TemplateHead, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	templatesDir := filepath.Join(s.root, "templates")
	entries, err := os.ReadDir(templatesDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read templates dir: %w", err)
	}
	heads := make([]TemplateHead, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || safeSegment(entry.Name()) != nil {
			continue
		}
		id := entry.Name()
		if err := safeSegment(id); err != nil {
			continue
		}
		unlock, lockErr := s.lockTemplate(ctx, id)
		if lockErr != nil {
			return nil, lockErr
		}
		if recoverErr := s.recoverAttributedTemplateSaveUnlocked(ctx, id); recoverErr != nil {
			unlock()
			return nil, fmt.Errorf("recover attributed save for template %q: %w", id, recoverErr)
		}
		head, _, headErr := s.getTemplateHeadStateUnlocked(ctx, id)
		unlock()
		if errors.Is(headErr, ErrNotFound) {
			continue
		}
		if headErr != nil {
			return nil, headErr
		}
		heads = append(heads, head)
	}
	slices.SortFunc(heads, func(a, b TemplateHead) int { return strings.Compare(a.ID, b.ID) })
	return heads, nil
}

func (s *FS) getTemplateHeadUnlocked(ctx context.Context, id string) (TemplateRecord, error) {
	_, record, err := s.getTemplateHeadStateUnlocked(ctx, id)
	return record, err
}

const (
	// The authoritative pointer stays v1 so a normal save remains readable by
	// the prior release. Exact attribution lives in an optional bounded sidecar
	// that older binaries ignore and new readers validate against ref+sourceHash.
	templateHeadPointerVersion     = 1
	templateHeadAttributionVersion = 1
	maxTemplateHeadPointerBytes    = 4 << 10
)

type templateHeadPointer struct {
	Version    int    `json:"version"`
	Ref        string `json:"ref"`
	SourceHash string `json:"sourceHash"`
}

type templateHeadAttribution struct {
	Version    int             `json:"version"`
	Ref        string          `json:"ref"`
	SourceHash string          `json:"sourceHash"`
	Actor      json.RawMessage `json:"actor"`
	AuthoredAt json.RawMessage `json:"authoredAt"`
}

type templateHeadAttributionWrite struct {
	Version    int        `json:"version"`
	Ref        string     `json:"ref"`
	SourceHash string     `json:"sourceHash"`
	Actor      ActorRef   `json:"actor,omitempty"`
	AuthoredAt *time.Time `json:"authoredAt,omitempty"`
}

func (s *FS) getTemplateHeadStateUnlocked(ctx context.Context, id string) (TemplateHead, TemplateRecord, error) {
	if err := safeSegment(id); err != nil {
		return TemplateHead{}, TemplateRecord{}, fmt.Errorf("invalid template id: %w", err)
	}
	headPath := filepath.Join(s.root, "templates", id, "head")
	if data, err := readTemplateHeadPointer(headPath); err == nil {
		head, legacy, decodeErr := decodeTemplateHead(id, data)
		if decodeErr != nil {
			return TemplateHead{}, TemplateRecord{}, decodeErr
		}
		record, recordErr := s.templateRecordForRefUnlocked(head.Ref)
		if recordErr != nil {
			return TemplateHead{}, TemplateRecord{}, recordErr
		}
		if legacy {
			head, recordErr = s.templateHeadForRecordUnlocked(ctx, record)
			if recordErr != nil {
				return TemplateHead{}, TemplateRecord{}, recordErr
			}
			if writeErr := s.writeTemplateHead(head); writeErr != nil {
				return TemplateHead{}, TemplateRecord{}, writeErr
			}
		} else {
			head.Actor, head.AuthoredAt = s.readTemplateHeadAttribution(id, head.Ref, head.SourceHash)
		}
		return head, record, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return TemplateHead{}, TemplateRecord{}, fmt.Errorf("read template head: %w", err)
	}
	records, err := templateRecordsForID(ctx, s, id)
	if err != nil {
		return TemplateHead{}, TemplateRecord{}, err
	}
	if len(records) == 0 {
		return TemplateHead{}, TemplateRecord{}, ErrNotFound
	}
	slices.SortFunc(records, func(a, b TemplateRecord) int {
		if !a.StoredAt.Equal(b.StoredAt) {
			if a.StoredAt.After(b.StoredAt) {
				return -1
			}
			return 1
		}
		return strings.Compare(b.SemanticHash, a.SemanticHash)
	})
	head, err := s.templateHeadForRecordUnlocked(ctx, records[0])
	if err != nil {
		return TemplateHead{}, TemplateRecord{}, err
	}
	// This is the intentional legacy migration point. The store already needs
	// write access for its cross-process lock/recovery contract; migration fails
	// closed rather than rescanning versions on every observation tick.
	if err := s.writeTemplateHead(head); err != nil {
		return TemplateHead{}, TemplateRecord{}, err
	}
	return head, records[0], nil
}

func readTemplateHeadPointer(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxTemplateHeadPointerBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxTemplateHeadPointerBytes {
		return nil, fmt.Errorf("%w: template head exceeds %d bytes", ErrContentMismatch, maxTemplateHeadPointerBytes)
	}
	return data, nil
}

func templateRecordsForID(ctx context.Context, s *FS, id string) ([]TemplateRecord, error) {
	return s.listTemplateRecordsForIDUnlocked(ctx, id)
}

func decodeTemplateHead(id string, data []byte) (TemplateHead, bool, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return TemplateHead{}, false, fmt.Errorf("%w: empty template head for %q", ErrContentMismatch, id)
	}
	if trimmed[0] != '{' {
		ref := string(trimmed)
		refID, _, err := parseTemplateRef(ref)
		if err != nil || refID != id {
			return TemplateHead{}, false, fmt.Errorf("%w: template head %q", ErrContentMismatch, ref)
		}
		return TemplateHead{ID: id, Ref: ref}, true, nil
	}
	var pointer templateHeadPointer
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&pointer); err != nil {
		return TemplateHead{}, false, fmt.Errorf("decode template head %q: %w", id, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return TemplateHead{}, false, fmt.Errorf("decode template head %q: trailing content", id)
	}
	refID, _, err := parseTemplateRef(pointer.Ref)
	if pointer.Version != templateHeadPointerVersion || err != nil || refID != id || !isHexSHA256(pointer.SourceHash) {
		return TemplateHead{}, false, fmt.Errorf("%w: invalid template head for %q", ErrContentMismatch, id)
	}
	return TemplateHead{ID: id, Ref: pointer.Ref, SourceHash: pointer.SourceHash}, false, nil
}

func (s *FS) readTemplateHeadAttribution(id, ref, sourceHash string) (ActorRef, *time.Time) {
	path := filepath.Join(s.root, "templates", id, "head-attribution")
	data, err := readTemplateHeadPointer(path)
	if err != nil {
		return "", nil
	}
	var pointer templateHeadAttribution
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(data)))
	dec.DisallowUnknownFields()
	if dec.Decode(&pointer) != nil {
		return "", nil
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return "", nil
	}
	if pointer.Version != templateHeadAttributionVersion || pointer.Ref != ref || pointer.SourceHash != sourceHash {
		return "", nil
	}
	var actor ActorRef
	var authoredAt *time.Time
	if len(pointer.Actor) > 0 && len(pointer.AuthoredAt) > 0 {
		if json.Unmarshal(pointer.Actor, &actor) != nil || json.Unmarshal(pointer.AuthoredAt, &authoredAt) != nil ||
			!ValidateActorRef(actor) || authoredAt == nil || authoredAt.IsZero() {
			actor = ""
			authoredAt = nil
		}
	}
	return actor, authoredAt
}

func (s *FS) templateRecordForRefUnlocked(ref string) (TemplateRecord, error) {
	id, hash, err := parseTemplateRef(ref)
	if err != nil {
		return TemplateRecord{}, err
	}
	dir, err := s.templateDir(id, hash)
	if err != nil {
		return TemplateRecord{}, err
	}
	bodyPath := filepath.Join(dir, "template.json")
	info, err := os.Stat(bodyPath)
	if errors.Is(err, os.ErrNotExist) {
		return TemplateRecord{}, fmt.Errorf("%w: template head %q", ErrContentMismatch, ref)
	}
	if err != nil {
		return TemplateRecord{}, fmt.Errorf("stat template head %q: %w", ref, err)
	}
	return TemplateRecord{ID: id, Ref: ref, SemanticHash: hash, StoredAt: info.ModTime()}, nil
}

func (s *FS) templateHeadForRecordUnlocked(ctx context.Context, record TemplateRecord) (TemplateHead, error) {
	id, hash, err := parseTemplateRef(record.Ref)
	if err != nil {
		return TemplateHead{}, err
	}
	source, err := s.getTemplateSourceUnlocked(ctx, id, hash, record.Ref)
	if err != nil {
		return TemplateHead{}, err
	}
	return TemplateHead{ID: id, Ref: record.Ref, SourceHash: sourceHash(source)}, nil
}

func (s *FS) writeTemplateHead(head TemplateHead) error {
	refID, _, err := parseTemplateRef(head.Ref)
	if head.ID == "" || refID != head.ID || err != nil || !isHexSHA256(head.SourceHash) ||
		(head.Actor == "") != (head.AuthoredAt == nil) ||
		(head.Actor != "" && !ValidateActorRef(head.Actor)) ||
		(head.AuthoredAt != nil && head.AuthoredAt.IsZero()) {
		return fmt.Errorf("%w: invalid template head for %q", ErrContentMismatch, head.ID)
	}
	data, err := json.Marshal(templateHeadPointer{
		Version: templateHeadPointerVersion, Ref: head.Ref, SourceHash: head.SourceHash,
	})
	if err != nil {
		return fmt.Errorf("encode template head: %w", err)
	}
	if len(data)+1 > maxTemplateHeadPointerBytes {
		return fmt.Errorf("%w: template head exceeds %d bytes", ErrContentMismatch, maxTemplateHeadPointerBytes)
	}
	dir := filepath.Join(s.root, "templates", head.ID)
	attributionPath := filepath.Join(dir, "head-attribution")
	if head.Actor != "" {
		attribution, marshalErr := json.Marshal(templateHeadAttributionWrite{
			Version: templateHeadAttributionVersion, Ref: head.Ref, SourceHash: head.SourceHash,
			Actor: head.Actor, AuthoredAt: head.AuthoredAt,
		})
		if marshalErr != nil {
			return fmt.Errorf("encode template head attribution: %w", marshalErr)
		}
		if len(attribution)+1 > maxTemplateHeadPointerBytes {
			return fmt.Errorf("%w: template head attribution exceeds %d bytes", ErrContentMismatch, maxTemplateHeadPointerBytes)
		}
		if err := writeFileAtomic(attributionPath, append(attribution, '\n'), 0o644); err != nil {
			return fmt.Errorf("write template head attribution: %w", err)
		}
	} else if err := os.Remove(attributionPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove template head attribution: %w", err)
	}
	return writeFileAtomic(filepath.Join(dir, "head"), append(data, '\n'), 0o644)
}

func sourceHash(source []byte) string {
	sum := sha256.Sum256(source)
	return hex.EncodeToString(sum[:])
}

func (s *FS) GetTemplate(ctx context.Context, ref string) (*model.Template, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id, hash, err := parseTemplateRef(ref)
	if err != nil {
		return nil, err
	}
	unlock, err := s.lockTemplate(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := s.recoverAttributedTemplateSaveUnlocked(ctx, id); err != nil {
		return nil, err
	}
	return s.getTemplateUnlocked(ctx, id, hash, ref)
}

func (s *FS) getTemplateUnlocked(ctx context.Context, id, hash, ref string) (*model.Template, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := s.templateDir(id, hash)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "template.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read template: %w", err)
	}
	var tmpl model.Template
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&tmpl); err != nil {
		return nil, fmt.Errorf("decode template: %w", err)
	}
	semanticHash, err := model.SemanticHash(&tmpl)
	if err != nil {
		return nil, err
	}
	if semanticHash != hash {
		return nil, fmt.Errorf("%w: template ref %q points at semantic hash %q", ErrContentMismatch, ref, semanticHash)
	}
	return &tmpl, nil
}

// GetTemplateSource returns the complete canonical authoring document for a
// template version, including editor-owned layout. Older stores contain only
// template.json; synthesizing canonical YAML for those records keeps them
// readable without mutating an already-addressed version.
func (s *FS) GetTemplateSource(ctx context.Context, ref string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id, hash, err := parseTemplateRef(ref)
	if err != nil {
		return nil, err
	}
	unlock, err := s.lockTemplate(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := s.recoverAttributedTemplateSaveUnlocked(ctx, id); err != nil {
		return nil, err
	}
	return s.getTemplateSourceUnlocked(ctx, id, hash, ref)
}

// GetTemplateAuthoringSnapshot returns source and provenance from one recovered
// per-template lock. Callers rendering attribution must use this instead of
// composing GetTemplateSource and ListTemplateAuthorship, which would permit a
// layout-only save to land between the two reads.
func (s *FS) GetTemplateAuthoringSnapshot(ctx context.Context, ref string) (TemplateAuthoringSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return TemplateAuthoringSnapshot{}, err
	}
	id, hash, err := parseTemplateRef(ref)
	if err != nil {
		return TemplateAuthoringSnapshot{}, err
	}
	unlock, err := s.lockTemplate(ctx, id)
	if err != nil {
		return TemplateAuthoringSnapshot{}, err
	}
	defer unlock()
	if err := s.recoverAttributedTemplateSaveUnlocked(ctx, id); err != nil {
		return TemplateAuthoringSnapshot{}, err
	}
	source, err := s.getTemplateSourceUnlocked(ctx, id, hash, ref)
	if err != nil {
		return TemplateAuthoringSnapshot{}, err
	}
	if s.templateAuthoringSnapshotHook != nil {
		s.templateAuthoringSnapshotHook()
	}
	authorship, err := s.listTemplateAuthorshipUnlocked(id, hash, ref)
	if err != nil {
		return TemplateAuthoringSnapshot{}, err
	}
	return TemplateAuthoringSnapshot{Source: source, Authorship: authorship}, nil
}

func (s *FS) getTemplateSourceUnlocked(ctx context.Context, id, hash, ref string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := s.templateDir(id, hash)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "template.yaml"))
	if err == nil {
		return data, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read template source: %w", err)
	}
	tmpl, err := s.getTemplateUnlocked(ctx, id, hash, ref)
	if err != nil {
		return nil, err
	}
	return model.CanonicalYAML(tmpl)
}

// DeleteTemplate atomically removes one template and all of its authoring
// history. Legacy runs no longer participate in authoring: agentd removes the
// obsolete runs tree before serving requests and no runtime can create a new
// reference.
func (s *FS) DeleteTemplate(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateTemplateID(id); err != nil {
		return err
	}
	unlock, err := s.lockTemplate(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	// Settle any half-finished attributed save before removing the tree, so a
	// concurrent authoring transaction cannot be left pointing at files we are
	// about to delete.
	if err := s.recoverAttributedTemplateSaveUnlocked(ctx, id); err != nil {
		return err
	}
	dir := filepath.Join(s.root, "templates", id)
	detached := filepath.Join(s.root, "templates", ".deleting-"+id)
	// Reclaim any tree left detached by a crash between the rename and the
	// removal below. This runs BEFORE the existence check on purpose: after such
	// a crash templates/<id> is already gone, so a later delete would return
	// ErrNotFound here and the residue would never be collected.
	if err := os.RemoveAll(detached); err != nil {
		return fmt.Errorf("reclaim detached process template: %w", err)
	}
	if _, statErr := os.Stat(dir); errors.Is(statErr, os.ErrNotExist) {
		return ErrNotFound
	} else if statErr != nil {
		return fmt.Errorf("stat process template: %w", statErr)
	}
	// Remove atomically from the reader's point of view: rename the whole tree
	// aside first so a failure part-way through cannot leave `head` pointing at
	// an already-deleted version directory, then drop the detached copy. The
	// rest of this store is scrupulous about atomic writes; deletion matches.
	// The detached name leads with a dot, which safeSegmentPattern forbids as a
	// first character, so ListTemplates skips it and it can never collide with a
	// real template id.
	if err := os.Rename(dir, detached); err != nil {
		return fmt.Errorf("detach process template: %w", err)
	}
	if err := syncDir(filepath.Dir(dir)); err != nil {
		return fmt.Errorf("sync process template detach: %w", err)
	}
	// The rename above is the durability point. If the process dies before this
	// completes, the reclaim at the top of this function collects the remains on
	// the next delete of the same id.
	if err := os.RemoveAll(detached); err != nil {
		return fmt.Errorf("remove process template: %w", err)
	}
	return nil
}

// RemoveLegacyRuntimeData deletes only the obsolete filesystem runtime. It
// deliberately leaves the process root, templates, template locks, and every
// other authoring path intact.
func (s *FS) RemoveLegacyRuntimeData() error {
	if err := os.RemoveAll(filepath.Join(s.root, "runs")); err != nil {
		return fmt.Errorf("remove legacy process runs: %w", err)
	}
	return removeLegacyRunLocks(s.root)
}

func isLegacyRunLockName(name string) bool {
	runID, ok := strings.CutPrefix(name, "run-")
	if !ok {
		return false
	}
	runID, ok = strings.CutSuffix(runID, ".lock")
	return ok && safeSegmentPattern.MatchString(runID)
}

func (s *FS) ListTemplates(ctx context.Context) ([]TemplateRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	templatesDir := filepath.Join(s.root, "templates")
	idEntries, err := os.ReadDir(templatesDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read templates dir: %w", err)
	}
	var records []TemplateRecord
	for _, idEntry := range idEntries {
		if !idEntry.IsDir() {
			continue
		}
		id := idEntry.Name()
		if err := safeSegment(id); err != nil {
			continue
		}
		unlock, err := s.lockTemplate(ctx, id)
		if err != nil {
			return nil, err
		}
		if err := s.recoverAttributedTemplateSaveUnlocked(ctx, id); err != nil {
			unlock()
			return nil, fmt.Errorf("recover attributed save for template %q: %w", id, err)
		}
		idRecords, err := s.listTemplateRecordsForIDUnlocked(ctx, id)
		unlock()
		if err != nil {
			return nil, err
		}
		records = append(records, idRecords...)
	}
	slices.SortFunc(records, func(a, b TemplateRecord) int {
		if a.ID != b.ID {
			return strings.Compare(a.ID, b.ID)
		}
		return strings.Compare(a.Ref, b.Ref)
	})
	return records, nil
}

func (s *FS) listTemplateRecordsForIDUnlocked(ctx context.Context, id string) ([]TemplateRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	templateDir := filepath.Join(s.root, "templates", id)
	hashEntries, err := os.ReadDir(templateDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read template hashes for %q: %w", id, err)
	}
	var records []TemplateRecord
	for _, hashEntry := range hashEntries {
		if !hashEntry.IsDir() {
			continue
		}
		hash, ok := strings.CutPrefix(hashEntry.Name(), "sha256-")
		if !ok || !isHexSHA256(hash) {
			continue
		}
		bodyPath := filepath.Join(templateDir, hashEntry.Name(), "template.json")
		if _, err := os.Stat(bodyPath); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, fmt.Errorf("stat template %q: %w", model.TemplateRef(id, hash), err)
		}
		records = append(records, TemplateRecord{
			ID: id, Ref: model.TemplateRef(id, hash), SemanticHash: hash, StoredAt: fileModTime(bodyPath),
		})
	}
	return records, nil
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create parent dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := maybeSync(tmp); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}
	_ = syncDir(dir)
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return maybeSync(d)
}

func (s *FS) lockTemplate(ctx context.Context, id string) (func(), error) {
	if err := safeSegment(id); err != nil {
		return func() {}, fmt.Errorf("invalid template id: %w", err)
	}
	lockValue, _ := processLocks.LoadOrStore(s.root+"\x00template\x00"+id, newLocalRunLock())
	localLock := lockValue.(*localRunLock)
	if err := localLock.Lock(ctx, s.templateLockContendedHook); err != nil {
		return func() {}, err
	}
	lockDir := filepath.Join(s.root, ".locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		localLock.Unlock()
		return func() {}, fmt.Errorf("create lock dir: %w", err)
	}
	fl := flock.New(filepath.Join(lockDir, templateLockFileName(id)))
	locked, err := fl.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		localLock.Unlock()
		return func() {}, fmt.Errorf("lock template: %w", err)
	}
	if !locked {
		localLock.Unlock()
		if err := ctx.Err(); err != nil {
			return func() {}, err
		}
		return func() {}, fmt.Errorf("lock template: lock not acquired")
	}
	return func() {
		_ = fl.Unlock()
		localLock.Unlock()
	}, nil
}

type localRunLock struct {
	ch chan struct{}
}

func newLocalRunLock() *localRunLock {
	lock := &localRunLock{ch: make(chan struct{}, 1)}
	lock.ch <- struct{}{}
	return lock
}

func (l *localRunLock) Lock(ctx context.Context, contended func()) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-l.ch:
		return nil
	default:
	}
	if contended != nil {
		contended()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-l.ch:
		return nil
	}
}

func (l *localRunLock) Unlock() {
	select {
	case l.ch <- struct{}{}:
	default:
	}
}

func (s *FS) templateDir(id, hash string) (string, error) {
	if err := safeSegment(id); err != nil {
		return "", fmt.Errorf("invalid template id: %w", err)
	}
	if !isHexSHA256(hash) {
		return "", fmt.Errorf("invalid template hash %q", hash)
	}
	return filepath.Join(s.root, "templates", id, "sha256-"+hash), nil
}
func templateLockFileName(id string) string { return "template-" + id + ".lock" }

func safeSegment(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("empty path segment")
	}
	if value == "." || value == ".." || strings.ContainsAny(value, `/\`) {
		return fmt.Errorf("unsafe path segment %q", value)
	}
	if !safeSegmentPattern.MatchString(value) {
		return fmt.Errorf("path segment %q must match %s", value, safeSegmentPattern.String())
	}
	return nil
}

// ValidateTemplateID reports whether id is safe for use as a persisted
// template identity. REST callers use it to classify invalid input before a
// filesystem operation turns that client error into an apparent store fault.
func ValidateTemplateID(id string) error {
	return safeSegment(id)
}

func parseTemplateRef(ref string) (string, string, error) {
	id, hashRef, ok := strings.Cut(ref, "@sha256:")
	if !ok || id == "" || !isHexSHA256(hashRef) {
		return "", "", fmt.Errorf("invalid template ref %q", ref)
	}
	return id, hashRef, nil
}

func isHexSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, ch := range value {
		if (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') {
			continue
		}
		return false
	}
	return true
}

func fileModTime(path string) time.Time {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime().UTC()
}
