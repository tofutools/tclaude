package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
)

const artifactRefPrefix = "artifact:sha256:"

var safeSegmentPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

type FS struct {
	root                          string
	now                           func() time.Time
	templateLockContendedHook     func()
	templateAuthoringSnapshotHook func()
	templateAuthoringCommitHook   func(TemplateAuthoringCommit)
	viewerReadHook                func()
	viewerReadChunkHook           func(string, int64)
	viewerDecodeHook              func(string)
	viewerMaxFileBytes            int64
	viewerMaxTotalBytes           int64
	viewerMaxRecords              int
	viewerMaxDirectoryEntries     int
	executionRunLockedHook        func()
	executionTemplateLockedHook   func()
	executionReobserveHook        func()
	pathV1InitializeBeforeCommit  func() error
	pathV1InitializeAfterCommit   func() error
	pathV1InitializeDirSync       func() error
	pathV1AppendBeforeCommit      func() error
	pathV1AppendAfterCommit       func() error
	pathV1AppendDirSync           func() error
	epochV8PublishBeforeEpoch     func() error
	epochV8PublishAfterEpoch      func() error
	epochV8PublishBeforeState     func() error
	epochV8PublishAfterState      func() error
	epochV8InitializeBeforeCommit func() error
	epochV8InitializeAfterCommit  func() error
	epochV8InitializeDirSync      func() error
	leaseGenerationAfterBurn      func() error
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

// SetViewerReadHookForTest installs a hook that runs after LoadRunView has
// acquired the run lock and before it reads the snapshot. Tests use it to
// prove Append and viewer reads cannot interleave.
func (s *FS) SetViewerReadHookForTest(hook func()) func() {
	previous := s.viewerReadHook
	s.viewerReadHook = hook
	return func() { s.viewerReadHook = previous }
}

// SetViewerResourceLimitsForTest installs smaller deterministic limits and
// read/decode hooks for boundary and cancellation tests.
func (s *FS) SetViewerResourceLimitsForTest(maxFile, maxTotal int64, maxRecords, maxEntries int) func() {
	oldFile, oldTotal := s.viewerMaxFileBytes, s.viewerMaxTotalBytes
	oldRecords, oldEntries := s.viewerMaxRecords, s.viewerMaxDirectoryEntries
	s.viewerMaxFileBytes, s.viewerMaxTotalBytes = maxFile, maxTotal
	s.viewerMaxRecords, s.viewerMaxDirectoryEntries = maxRecords, maxEntries
	return func() {
		s.viewerMaxFileBytes, s.viewerMaxTotalBytes = oldFile, oldTotal
		s.viewerMaxRecords, s.viewerMaxDirectoryEntries = oldRecords, oldEntries
	}
}

// SetViewerIOHooksForTest installs deterministic read/decode synchronization
// points for viewer cancellation tests.
func (s *FS) SetViewerIOHooksForTest(read func(string, int64), decode func(string)) func() {
	oldRead, oldDecode := s.viewerReadChunkHook, s.viewerDecodeHook
	s.viewerReadChunkHook, s.viewerDecodeHook = read, decode
	return func() { s.viewerReadChunkHook, s.viewerDecodeHook = oldRead, oldDecode }
}

// SetExecutionViewHooksForTest installs deterministic critical-section and
// bounded re-observation synchronization points. Install before concurrent use.
func (s *FS) SetExecutionViewHooksForTest(runLocked, templateLocked, reobserve func()) func() {
	oldRun, oldTemplate, oldReobserve := s.executionRunLockedHook, s.executionTemplateLockedHook, s.executionReobserveHook
	s.executionRunLockedHook, s.executionTemplateLockedHook, s.executionReobserveHook = runLocked, templateLocked, reobserve
	return func() {
		s.executionRunLockedHook, s.executionTemplateLockedHook, s.executionReobserveHook = oldRun, oldTemplate, oldReobserve
	}
}

// SetPathV1InitializeHooksForTest installs deterministic crash-boundary hooks
// around the schema-7 atomic replacement. Install before concurrent
// use. An after-commit error models an ambiguous acknowledgement after rename.
func (s *FS) SetPathV1InitializeHooksForTest(beforeCommit, afterCommit func() error) func() {
	oldBefore, oldAfter := s.pathV1InitializeBeforeCommit, s.pathV1InitializeAfterCommit
	s.pathV1InitializeBeforeCommit, s.pathV1InitializeAfterCommit = beforeCommit, afterCommit
	return func() {
		s.pathV1InitializeBeforeCommit, s.pathV1InitializeAfterCommit = oldBefore, oldAfter
	}
}

// SetPathV1InitializeDirSyncHookForTest injects the durability result for the
// schema-transition parent-directory fsync. Exact replay must repeat this step
// before acknowledging an already-installed checkpoint.
func (s *FS) SetPathV1InitializeDirSyncHookForTest(hook func() error) func() {
	old := s.pathV1InitializeDirSync
	s.pathV1InitializeDirSync = hook
	return func() { s.pathV1InitializeDirSync = old }
}

// SetPathV1AppendHooksForTest installs crash-boundary hooks around one
// schema-7 execution append. An after-commit error models an ambiguous durable
// rename acknowledgement; callers recover by exact desired-state replay.
func (s *FS) SetPathV1AppendHooksForTest(beforeCommit, afterCommit func() error) func() {
	oldBefore, oldAfter := s.pathV1AppendBeforeCommit, s.pathV1AppendAfterCommit
	s.pathV1AppendBeforeCommit, s.pathV1AppendAfterCommit = beforeCommit, afterCommit
	return func() {
		s.pathV1AppendBeforeCommit, s.pathV1AppendAfterCommit = oldBefore, oldAfter
	}
}

func (s *FS) SetPathV1AppendDirSyncHookForTest(hook func() error) func() {
	old := s.pathV1AppendDirSync
	s.pathV1AppendDirSync = hook
	return func() { s.pathV1AppendDirSync = old }
}

// SetEpochV8PublishHooksForTest installs deterministic crash boundaries around
// immutable epoch publication and checkpoint replacement.
func (s *FS) SetEpochV8PublishHooksForTest(beforeEpoch, afterEpoch, beforeState, afterState func() error) func() {
	oldBeforeEpoch, oldAfterEpoch := s.epochV8PublishBeforeEpoch, s.epochV8PublishAfterEpoch
	oldBeforeState, oldAfterState := s.epochV8PublishBeforeState, s.epochV8PublishAfterState
	s.epochV8PublishBeforeEpoch, s.epochV8PublishAfterEpoch = beforeEpoch, afterEpoch
	s.epochV8PublishBeforeState, s.epochV8PublishAfterState = beforeState, afterState
	return func() {
		s.epochV8PublishBeforeEpoch, s.epochV8PublishAfterEpoch = oldBeforeEpoch, oldAfterEpoch
		s.epochV8PublishBeforeState, s.epochV8PublishAfterState = oldBeforeState, oldAfterState
	}
}

func (s *FS) SetEpochV8InitializeHooksForTest(beforeCommit, afterCommit func() error) func() {
	oldBefore, oldAfter := s.epochV8InitializeBeforeCommit, s.epochV8InitializeAfterCommit
	s.epochV8InitializeBeforeCommit, s.epochV8InitializeAfterCommit = beforeCommit, afterCommit
	return func() {
		s.epochV8InitializeBeforeCommit, s.epochV8InitializeAfterCommit = oldBefore, oldAfter
	}
}

func (s *FS) SetEpochV8InitializeDirSyncHookForTest(hook func() error) func() {
	old := s.epochV8InitializeDirSync
	s.epochV8InitializeDirSync = hook
	return func() { s.epochV8InitializeDirSync = old }
}

// SetLeaseGenerationAfterBurnHookForTest injects a crash boundary after the
// monotonic generation is durable and before its lease capability is published.
func (s *FS) SetLeaseGenerationAfterBurnHookForTest(hook func() error) func() {
	old := s.leaseGenerationAfterBurn
	s.leaseGenerationAfterBurn = hook
	return func() { s.leaseGenerationAfterBurn = old }
}

func (s *FS) PutTemplate(ctx context.Context, tmpl *model.Template) (TemplateRecord, error) {
	return s.putTemplate(ctx, tmpl, true)
}

// PutTemplateVersion persists immutable template content without changing the
// editor head. Run creation uses this for templates loaded from files: merely
// instantiating an older file must not change which version editors reopen.
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
		// A first non-moving write still establishes a head. Later file-backed
		// runs will preserve it through the pre-publication path above.
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
	actor state.ActorRef,
) (TemplateAuthoringCommit, error) {
	if !state.ValidateActorRef(actor) {
		return TemplateAuthoringCommit{}, fmt.Errorf("invalid process template authoring actor %q", actor)
	}
	return s.putTemplateEditorSource(ctx, tmpl, expectedSourceHash, actor)
}

func (s *FS) putTemplateEditorSource(
	ctx context.Context,
	tmpl *model.Template,
	expectedSourceHash string,
	actor state.ActorRef,
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
		if event.Ref != ref || event.SourceHash == "" || !state.ValidateActorRef(event.Actor) || event.AuthoredAt.IsZero() {
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
			// excluded from CanonicalSemanticJSON, so run-pinned semantics and
			// their audit identity do not change. PutTemplateEditorSource guards
			// this last-write-wins update with a filesystem-locked sourceHash CAS.
			if err := writeFileAtomic(filepath.Join(dir, "template.yaml"), source, 0o644); err != nil {
				return TemplateRecord{}, err
			}
		}
		return TemplateRecord{ID: tmpl.ID, Ref: ref, SemanticHash: semanticHash, StoredAt: fileModTime(bodyPath)}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return TemplateRecord{}, fmt.Errorf("read existing template: %w", err)
	}
	// The run-pinned template.json intentionally excludes editor-only layout,
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
	Version    int            `json:"version"`
	Ref        string         `json:"ref"`
	SourceHash string         `json:"sourceHash"`
	Actor      state.ActorRef `json:"actor,omitempty"`
	AuthoredAt *time.Time     `json:"authoredAt,omitempty"`
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

func (s *FS) readTemplateHeadAttribution(id, ref, sourceHash string) (state.ActorRef, *time.Time) {
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
	var actor state.ActorRef
	var authoredAt *time.Time
	if len(pointer.Actor) > 0 && len(pointer.AuthoredAt) > 0 {
		if json.Unmarshal(pointer.Actor, &actor) != nil || json.Unmarshal(pointer.AuthoredAt, &authoredAt) != nil ||
			!state.ValidateActorRef(actor) || authoredAt == nil || authoredAt.IsZero() {
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
		(head.Actor != "" && !state.ValidateActorRef(head.Actor)) ||
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

// GetTemplateExact reads one immutable semantic version without running
// attributed-save recovery. Viewer reads must never roll back or complete an
// authoring transaction as a side effect. An unfinished intent therefore
// fails closed and remains untouched for an authoring caller to recover.
func (s *FS) GetTemplateExact(ctx context.Context, ref string) (*model.Template, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id, hash, err := parseTemplateRef(ref)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid exact template ref", ErrContentMismatch)
	}
	if err := safeSegment(id); err != nil {
		return nil, fmt.Errorf("%w: invalid exact template id", ErrContentMismatch)
	}
	exists, err := s.hasTemplateExactView(id, hash)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	unlock, err := s.lockTemplateView(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	data, err := s.getTemplateExactBody(ctx, id, hash)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var tmpl model.Template
	if err := runViewDecode(ctx, s.viewerDecodeHook, "template", func() error {
		return decodeViewJSON(ctx, data, &tmpl, true)
	}); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: exact template content cannot be decoded", ErrContentMismatch)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	semanticHash, err := model.SemanticHash(&tmpl)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil || semanticHash != hash {
		return nil, fmt.Errorf("%w: exact template content does not match its ref", ErrContentMismatch)
	}
	return &tmpl, nil
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

// runStatusHeader is the bounded projection of runs/<id>/state.json needed to
// classify a run for the deletion guard, across BOTH persisted schemas. Legacy
// checkpoints carry a top-level status; schema-7 (pathv1) checkpoints carry a
// mutable execution head instead, and an installed schema-7 checkpoint that
// predates that head is running by definition (mirroring pathv1.CurrentRunStatus).
type runStatusHeader struct {
	StateSchemaVersion int    `json:"stateSchemaVersion"`
	Status             string `json:"status"`
	Execution          *struct {
		Status string `json:"status"`
	} `json:"execution"`
}

// runIsFinishedUnlocked classifies one run WITHOUT taking that run's view lock.
//
// The lock-free read is deliberate and load-bearing: the execution views take
// the run-view lock and THEN the template lock (WithExecutionView →
// lockRunView → lockTemplateView, which is the same lock identity as
// lockTemplate). DeleteTemplate holds the template lock while it scans, so
// acquiring a run-view lock here would invert that order and deadlock against a
// concurrent execution-view read.
//
// Reading without the lock only ever costs precision, never safety: a status
// observed mid-transition is stale in the conservative direction (we may refuse
// a delete that had just become legal), and any read or decode failure is
// treated as unfinished. The race that MATTERS — a new run pinning this
// template — is closed by the template lock itself, not by run locks.
func (s *FS) runIsFinishedUnlocked(runID string) bool {
	if err := safeSegment(runID); err != nil {
		return false
	}
	// This runs once per run in the store on every delete, so the read is
	// bounded by the same per-file ceiling the viewer applies, and refuses
	// anything that is not a regular file. The status fields sit at the top of
	// the document but JSON must be decoded whole, so the cap is on file size
	// rather than a truncating reader — a partial read would fail to parse and
	// wrongly block every delete on a large-but-healthy run.
	file, err := os.Open(filepath.Join(s.runDir(runID), "state.json"))
	if err != nil {
		return false
	}
	defer file.Close()
	info, statErr := file.Stat()
	if statErr != nil || !info.Mode().IsRegular() || info.Size() > viewerDefaultMaxFileBytes {
		return false
	}
	var header runStatusHeader
	if err := json.NewDecoder(file).Decode(&header); err != nil {
		return false
	}
	if header.StateSchemaVersion == pathv1.CheckpointStateSchemaVersion {
		if header.Execution == nil {
			return false
		}
		return runStatusIsFinished(state.RunStatus(header.Execution.Status))
	}
	return runStatusIsFinished(state.RunStatus(header.Status))
}

// templateRunGuard is the result of scanning the store for runs that must
// prevent a template from being deleted. The two categories are reported
// separately because they call for different operator action: an unfinished run
// can be finished or cancelled, whereas an unreadable one needs repair.
type templateRunGuard struct {
	unfinished []string
	unreadable []string
}

func (g templateRunGuard) blocks() bool { return len(g.unfinished) > 0 || len(g.unreadable) > 0 }

// templateRunGuardUnlocked finds every run that must block deleting id.
//
// It fails CLOSED. A run whose record could not be decoded surfaces from
// ListRuns as a bare id with no template ref; we cannot prove such a run does
// not reference this template, so it blocks. Likewise a run we can read but
// cannot classify counts as unfinished rather than finished.
//
// A finished run is only safe to leave behind if it pinned its own template
// snapshot at instantiation. Legacy runs recorded before pinning existed have
// no copy of their own, so deleting the library entry would destroy their only
// definition — those block too, whatever their status.
func (s *FS) templateRunGuardUnlocked(ctx context.Context, id string) (templateRunGuard, error) {
	runEntries, readDirErr := os.ReadDir(filepath.Join(s.root, "runs"))
	if readDirErr != nil && !errors.Is(readDirErr, os.ErrNotExist) {
		return templateRunGuard{}, readDirErr
	}
	if len(runEntries) > viewerDefaultMaxDirectoryEntries {
		return templateRunGuard{}, fmt.Errorf("%w: template deletion run scan exceeded %d entries", ErrViewerResourceLimit, viewerDefaultMaxDirectoryEntries)
	}
	runs, err := s.ListRuns(ctx)
	if err != nil {
		return templateRunGuard{}, err
	}
	var guard templateRunGuard
	for _, run := range runs {
		runTemplateID, _, refErr := parseTemplateRef(run.TemplateRef)
		if refErr != nil {
			guard.unreadable = append(guard.unreadable, run.ID)
			continue
		}
		version, versionErr := s.runStateSchemaVersionUnlocked(run.ID)
		if versionErr != nil {
			guard.unreadable = append(guard.unreadable, run.ID)
			continue
		}
		kind, classErr := ClassifyRunStateSchema(version)
		if classErr != nil {
			guard.unreadable = append(guard.unreadable, run.ID)
			continue
		}
		switch kind {
		case RunSchemaEpochV8:
			snapshot, viewErr := s.loadEpochV8RunViewUnlocked(ctx, run.ID, s.newEpochV8Budget(ctx))
			if viewErr != nil {
				guard.unreadable = append(guard.unreadable, run.ID)
				continue
			}
			for _, epoch := range snapshot.Checkpoint.View().Epochs {
				epochTemplateID, _, epochRefErr := parseTemplateRef(epoch.TemplateRef)
				if epochRefErr != nil {
					guard.unreadable = append(guard.unreadable, run.ID)
					break
				}
				if epochTemplateID == id {
					guard.unfinished = append(guard.unfinished, run.ID)
					break
				}
			}
		case RunSchemaResetRequired:
			if runTemplateID == id {
				guard.unfinished = append(guard.unfinished, run.ID)
			}
		case RunSchemaLegacy:
			if runTemplateID == id && (!s.runIsFinishedUnlocked(run.ID) || run.Template == nil) {
				guard.unfinished = append(guard.unfinished, run.ID)
			}
		}
	}
	return guard, nil
}

func (s *FS) runStateSchemaVersionUnlocked(runID string) (int, error) {
	if err := safeSegment(runID); err != nil {
		return 0, err
	}
	file, err := os.Open(filepath.Join(s.runDir(runID), "state.json"))
	if err != nil {
		return 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > viewerDefaultMaxFileBytes {
		return 0, fmt.Errorf("invalid bounded state file")
	}
	var header struct {
		StateSchemaVersion int `json:"stateSchemaVersion"`
	}
	if err := json.NewDecoder(file).Decode(&header); err != nil {
		return 0, err
	}
	return header.StateSchemaVersion, nil
}

// runStatusIsFinished reports whether a run has reached a state it can never
// leave. It is deliberately narrower than plan.AllowsExecution: a paused,
// blocked, dirty, or inconsistent run cannot execute right now but may still be
// resumed or repaired, and both paths need the stored template.
func runStatusIsFinished(status state.RunStatus) bool {
	switch status {
	case state.RunStatusCompleted, state.RunStatusFailed, state.RunStatusCanceled:
		return true
	default:
		return false
	}
}

// DeleteTemplate removes a template id and every version stored under it. It
// refuses while any run that still needs the stored template references it, so
// an in-flight process cannot lose the definition it is executing.
//
// Deletion is irreversible: the version history, editor source, and authorship
// trail for that id all go away. A finished run keeps the template snapshot it
// pinned at instantiation, so its own record stays self-describing — but note
// that the execution-view and verification surfaces read the template body from
// the library and will report the run as inconsistent once it is gone.
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
	// The scan runs under the template lock so a concurrent instantiate cannot
	// pin this template between the check and the removal (CreateRun takes the
	// same lock across its pin and its run write). It deliberately takes no run
	// locks — see runIsFinishedUnlocked for the lock-ordering reason.
	guard, err := s.templateRunGuardUnlocked(ctx, id)
	if err != nil {
		return err
	}
	if guard.blocks() {
		return &TemplateInUseError{TemplateID: id, RunIDs: guard.unfinished, UnreadableRunIDs: guard.unreadable}
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

func (s *FS) CreateRun(ctx context.Context, run RunRecord, initial state.State) (RunRecord, error) {
	if err := ctx.Err(); err != nil {
		return RunRecord{}, err
	}
	if run.AllowPrograms {
		return RunRecord{}, fmt.Errorf("allowPrograms must be enabled after an admin audit entry")
	}
	if err := safeSegment(run.ID); err != nil {
		return RunRecord{}, fmt.Errorf("invalid run id: %w", err)
	}
	if strings.TrimSpace(run.TemplateRef) == "" {
		return RunRecord{}, fmt.Errorf("templateRef is required")
	}
	if err := validateInitialAdminWrites(initial.AdminRecords); err != nil {
		return RunRecord{}, err
	}
	unlock, err := s.lockRun(ctx, run.ID)
	if err != nil {
		return RunRecord{}, err
	}
	defer unlock()
	// Pin the template under its own lock and HOLD that lock until run.json has
	// landed. DeleteTemplate takes the same lock for its scan-and-remove, so this
	// is what makes the two mutually exclusive: either this run is durable before
	// the scan sees it, or the delete completes first and the pin below fails
	// with ErrNotFound instead of stranding a run on a template that is gone.
	//
	// Lock order is run → template throughout the store (the execution views take
	// lockRunView, the same identity as lockRun, before lockTemplateView, the
	// same identity as lockTemplate). Acquiring the template lock here, AFTER the
	// run lock, keeps that order; inverting it would deadlock against a
	// concurrent execution-view read.
	templateID, templateHash, err := parseTemplateRef(run.TemplateRef)
	if err != nil {
		return RunRecord{}, fmt.Errorf("pin run template %q: %w", run.TemplateRef, err)
	}
	unlockTemplate, err := s.lockTemplate(ctx, templateID)
	if err != nil {
		return RunRecord{}, err
	}
	defer unlockTemplate()
	if err := s.recoverAttributedTemplateSaveUnlocked(ctx, templateID); err != nil {
		return RunRecord{}, fmt.Errorf("pin run template %q: %w", run.TemplateRef, err)
	}
	pinnedTemplate, err := s.getTemplateUnlocked(ctx, templateID, templateHash, run.TemplateRef)
	if err != nil {
		return RunRecord{}, fmt.Errorf("pin run template %q: %w", run.TemplateRef, err)
	}
	run.Template = pinnedTemplate
	if run.CreatedAt.IsZero() {
		run.CreatedAt = s.now().UTC()
	}
	if run.UpdatedAt.IsZero() {
		run.UpdatedAt = run.CreatedAt
	}
	dir := s.runDir(run.ID)
	runsDir := filepath.Dir(dir)
	_, runsStatErr := os.Stat(runsDir)
	runsDirCreated := errors.Is(runsStatErr, os.ErrNotExist)
	if runsStatErr != nil && !runsDirCreated {
		return RunRecord{}, fmt.Errorf("stat runs dir: %w", runsStatErr)
	}
	if err := os.MkdirAll(filepath.Join(dir, "nodes"), 0o755); err != nil {
		return RunRecord{}, fmt.Errorf("create run dirs: %w", err)
	}
	if runsDirCreated {
		if err := syncDir(s.root); err != nil {
			return RunRecord{}, fmt.Errorf("sync store root: %w", err)
		}
	}
	if err := syncDir(runsDir); err != nil {
		return RunRecord{}, fmt.Errorf("sync runs dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "artifacts"), 0o755); err != nil {
		return RunRecord{}, fmt.Errorf("create artifacts dir: %w", err)
	}
	runPath := filepath.Join(dir, "run.json")
	if _, err := os.Stat(runPath); err == nil {
		return RunRecord{}, fmt.Errorf("%w: %q", ErrRunExists, run.ID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return RunRecord{}, fmt.Errorf("stat run: %w", err)
	}
	if initial.RunID == "" {
		initial.RunID = run.ID
	}
	if initial.OriginalTemplateRef == "" {
		initial.OriginalTemplateRef = run.TemplateRef
	}
	if initial.CurrentTemplateRef == "" {
		initial.CurrentTemplateRef = run.TemplateRef
	}
	stateData, err := state.Encode(&initial)
	if err != nil {
		return RunRecord{}, err
	}
	if err := writeFileAtomic(filepath.Join(dir, "state.json"), stateData, 0o644); err != nil {
		return RunRecord{}, err
	}
	runData, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return RunRecord{}, fmt.Errorf("encode run: %w", err)
	}
	runData = append(runData, '\n')
	if err := writeFileAtomic(runPath, runData, 0o644); err != nil {
		return RunRecord{}, err
	}
	return run, nil
}

// SetProgramsAllowed enables program performers only after the run evidence
// log contains the explicit admin opt-in event. The audit is therefore durable
// before run.json becomes executable; a crash between the two leaves the run
// safely disabled and the operation can be retried.
func (s *FS) SetProgramsAllowed(ctx context.Context, runID string) (RunRecord, error) {
	if err := ctx.Err(); err != nil {
		return RunRecord{}, err
	}
	unlock, err := s.lockRun(ctx, runID)
	if err != nil {
		return RunRecord{}, err
	}
	defer unlock()
	snapshot, err := s.LoadRun(ctx, runID)
	if err != nil {
		return RunRecord{}, err
	}
	diagnostics := append(
		evidence.VerifySequence(snapshot.Manifest, snapshot.NodeLogs),
		evidence.VerifyStateAnchors(snapshot.State, snapshot.Manifest)...,
	)
	if diagnostics.HasErrors() {
		return RunRecord{}, fmt.Errorf("%w: program opt-in audit is not fully committed: %v", ErrRunInconsistent, diagnostics)
	}
	var audit *state.Event
	for _, log := range snapshot.NodeLogs {
		if log.NodeID != "" {
			continue
		}
		for _, entry := range log.Entries {
			if entry.Event != nil && entry.Event.Type == state.EventAdminProgramsAllowed {
				audit = entry.Event
				break
			}
		}
	}
	if audit == nil || !adminRecordApplied(snapshot.State, *audit) {
		return RunRecord{}, fmt.Errorf("process run %q has no admin program opt-in audit entry", runID)
	}
	run := snapshot.Run
	if run.AllowPrograms {
		return run, nil
	}
	run.AllowPrograms = true
	run.UpdatedAt = s.now().UTC()
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return RunRecord{}, fmt.Errorf("encode run: %w", err)
	}
	data = append(data, '\n')
	if err := writeFileAtomic(filepath.Join(s.runDir(runID), "run.json"), data, 0o644); err != nil {
		return RunRecord{}, err
	}
	return run, nil
}

func adminRecordApplied(st *state.State, event state.Event) bool {
	if st == nil {
		return false
	}
	for _, record := range st.AdminRecords {
		if record.Type == event.Type && record.Actor == event.Actor && record.Reason == event.Reason && record.EvidenceRef == event.EvidenceRef && record.Timestamp.Equal(event.At) {
			return true
		}
	}
	return false
}

func (s *FS) GetRun(ctx context.Context, runID string) (RunRecord, error) {
	if err := ctx.Err(); err != nil {
		return RunRecord{}, err
	}
	return s.readRun(runID)
}

// LoadRunState reads only the materialized checkpoint. Schedulers use it for
// cheap sticky-status filtering before taking a lease and loading evidence.
func (s *FS) LoadRunState(ctx context.Context, runID string) (*state.State, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.readState(runID)
}

func (s *FS) LoadRun(ctx context.Context, runID string) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	return s.loadRunSnapshot(ctx, runID)
}

// LoadRunView returns one coherent, read-only run snapshot. Append uses the
// same run lock, so evidence, manifest, and checkpoint cannot be observed from
// different append generations. The lock file is operational metadata outside
// the run history boundary.
func (s *FS) LoadRunView(ctx context.Context, runID string) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if err := safeSegment(runID); err != nil {
		return Snapshot{}, fmt.Errorf("invalid run id: %w", err)
	}
	exists, err := s.HasRunView(runID)
	if err != nil {
		return Snapshot{}, err
	}
	if !exists {
		return Snapshot{}, ErrNotFound
	}
	unlock, err := s.lockRunView(ctx, runID)
	if err != nil {
		return Snapshot{}, err
	}
	defer unlock()
	if s.viewerReadHook != nil {
		s.viewerReadHook()
	}
	return s.loadRunViewSnapshotAt(ctx, runID)
}

// loadRunSnapshot performs no locking. Callers that require a coherent view
// serialize it externally; legacy LoadRun intentionally retains its existing
// unlocked behavior.
func (s *FS) loadRunSnapshot(ctx context.Context, runID string) (Snapshot, error) {
	run, err := s.readRun(runID)
	if err != nil {
		return Snapshot{}, err
	}
	st, err := s.readState(runID)
	if err != nil {
		return Snapshot{}, err
	}
	manifest, err := s.ReadManifest(ctx, runID)
	if err != nil {
		return Snapshot{}, err
	}
	nodeLogs, err := s.readAllLogs(ctx, runID)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Run: run, State: st, Manifest: manifest, NodeLogs: nodeLogs}, nil
}

func (s *FS) ListRuns(ctx context.Context) ([]RunRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	runsDir := filepath.Join(s.root, "runs")
	entries, err := os.ReadDir(runsDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read runs dir: %w", err)
	}
	records := make([]RunRecord, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		run, err := s.readRun(entry.Name())
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			records = append(records, RunRecord{ID: entry.Name()})
			continue
		}
		records = append(records, run)
	}
	// Newest first, by creation time rather than by id. Ordering used to be a
	// side effect of run ids starting with the template id, which silently
	// coupled the list order to an id FORMAT; a run id whose prefix changes --
	// or a template renamed between runs -- would otherwise reshuffle history.
	// Ties fall back to the id so the order stays total and deterministic, and
	// legacy records with no CreatedAt sort last instead of leading.
	slices.SortFunc(records, func(a, b RunRecord) int {
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return b.CreatedAt.Compare(a.CreatedAt)
		}
		return strings.Compare(a.ID, b.ID)
	})
	return records, nil
}

// Append serializes one run-global compare-and-append transaction. It validates
// the manifest head against expectedSeq, assigns contiguous seq values, checks
// state anchors, and applies the whole batch through the reducer in memory
// before writing. It then persists in evidence.DualWriteProtocol order: owning
// log JSONL, manifest JSONL, state checkpoint temp-file+rename. Crashes or I/O
// errors between those writes are intentionally observable on reload as
// log-ahead-manifest, manifest-ahead-state, or torn-tail evidence diagnostics.
func (s *FS) Append(ctx context.Context, runID string, expectedSeq int64, entries []evidence.LogEntry) (AppendResult, error) {
	if err := ctx.Err(); err != nil {
		return AppendResult{}, err
	}
	if len(entries) == 0 {
		run, err := s.readRun(runID)
		if err != nil {
			return AppendResult{}, err
		}
		st, err := s.readState(run.ID)
		if err != nil {
			return AppendResult{}, err
		}
		return AppendResult{State: st}, nil
	}
	unlock, err := s.lockRun(ctx, runID)
	if err != nil {
		return AppendResult{}, err
	}
	defer unlock()

	manifest, err := s.ReadManifest(ctx, runID)
	if err != nil {
		return AppendResult{}, err
	}
	actualSeq := int64(0)
	previousChecksum := ""
	if len(manifest) > 0 {
		head := manifest[len(manifest)-1]
		actualSeq = head.Seq
		previousChecksum = head.Checksum
	}
	if actualSeq != expectedSeq {
		return AppendResult{}, &ConflictError{RunID: runID, ExpectedSeq: expectedSeq, ActualSeq: actualSeq}
	}
	st, err := s.readState(runID)
	if err != nil {
		return AppendResult{}, err
	}
	if diagnostics := evidence.VerifyStateAnchors(st, manifest); diagnostics.HasErrors() {
		return AppendResult{}, fmt.Errorf("%w: %v", ErrRunInconsistent, diagnostics)
	}

	planned := make([]plannedAppend, 0, len(entries))
	nextState := *st
	for i, entry := range entries {
		entry.SchemaVersion = evidence.LogEntrySchemaVersion
		entry.Seq = expectedSeq + int64(i) + 1
		if err := validateEntryScope(entry.Scope); err != nil {
			return AppendResult{}, err
		}
		if entry.Event != nil {
			event := *entry.Event
			event.Seq = entry.Seq
			event.LogChecksum = ""
			if err := validateLegacyAdminWrite(event); err != nil {
				return AppendResult{}, fmt.Errorf("validate event seq %d: %w", entry.Seq, err)
			}
			entry.Event = &event
		}
		manifestEntry, err := evidence.ManifestEntryForLog(entry, previousChecksum)
		if err != nil {
			return AppendResult{}, err
		}
		if entry.Event != nil {
			event := *entry.Event
			applied, err := state.Apply(nextState, event)
			if err != nil {
				return AppendResult{}, fmt.Errorf("apply event seq %d: %w", entry.Seq, err)
			}
			applied.LogChecksum = manifestEntry.Checksum
			nextState = applied
		} else {
			nextState.LastLogSeq = entry.Seq
			nextState.LogChecksum = manifestEntry.Checksum
		}
		previousChecksum = manifestEntry.Checksum
		planned = append(planned, plannedAppend{entry: entry, manifest: manifestEntry})
	}
	stateData, err := state.Encode(&nextState)
	if err != nil {
		return AppendResult{}, err
	}
	appendedEntries := make([]evidence.LogEntry, 0, len(planned))
	appendedManifest := make([]evidence.ManifestEntry, 0, len(planned))
	for _, item := range planned {
		if err := s.appendLogEntry(runID, item.entry); err != nil {
			return AppendResult{}, err
		}
		if err := s.appendManifestEntry(runID, item.manifest); err != nil {
			return AppendResult{}, err
		}
		appendedEntries = append(appendedEntries, item.entry)
		appendedManifest = append(appendedManifest, item.manifest)
	}
	if err := writeFileAtomic(filepath.Join(s.runDir(runID), "state.json"), stateData, 0o644); err != nil {
		return AppendResult{}, err
	}
	return AppendResult{Entries: appendedEntries, Manifest: appendedManifest, State: &nextState}, nil
}

// validateLegacyAdminWrite is the durable producer boundary. The reducer must
// continue accepting zero-At historical repair/program-opt-in events so old
// logs remain replayable, but no new append may create another such record.
func validateLegacyAdminWrite(event state.Event) error {
	switch event.Type {
	case state.EventAdminRepairRecorded, state.EventAdminProgramsAllowed:
		if event.At.IsZero() {
			return fmt.Errorf("%s requires a timestamp for new writes", event.Type)
		}
	}
	return nil
}

// validateInitialAdminWrites closes the other legacy checkpoint creation
// boundary. CreateRun accepts materialized state rather than events, so it must
// validate the persisted record and resolution timestamps before creating the
// run directory. Already-persisted checkpoints continue through predecode.
func validateInitialAdminWrites(records []state.AdminRecord) error {
	for i, record := range records {
		if record.Timestamp.IsZero() {
			return fmt.Errorf("initial admin record %d: %s requires a timestamp for new writes", i, record.Type)
		}
		if record.Resolution != nil && record.Resolution.Timestamp.IsZero() {
			return fmt.Errorf("initial admin record %d: %s requires a resolution timestamp for new writes", i, record.Type)
		}
		if record.Type == state.EventBlockResolutionRecorded && record.Resolution == nil {
			return fmt.Errorf("initial admin record %d: block_resolution_recorded requires a resolution payload for new writes", i)
		}
	}
	return nil
}

type plannedAppend struct {
	entry    evidence.LogEntry
	manifest evidence.ManifestEntry
}

func (s *FS) ReadManifest(ctx context.Context, runID string) ([]evidence.ManifestEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := s.readRun(runID); err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(s.runDir(runID), "manifest.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open manifest: %w", err)
	}
	defer f.Close()
	entries, err := evidence.ReadManifest(f)
	return entries, annotateReadError(err, "manifest.jsonl")
}

func (s *FS) ReadNodeLog(ctx context.Context, runID, nodeID string) ([]evidence.LogEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := s.readRun(runID); err != nil {
		return nil, err
	}
	if err := safeSegment(nodeID); err != nil {
		return nil, fmt.Errorf("invalid node id: %w", err)
	}
	f, err := os.Open(filepath.Join(s.runDir(runID), "nodes", nodeID, "log.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open node log: %w", err)
	}
	defer f.Close()
	entries, err := evidence.ReadNodeLog(nodeID, f)
	return entries, annotateReadError(err, filepath.ToSlash(filepath.Join("nodes", nodeID, "log.jsonl")))
}

func (s *FS) ReadRunLog(ctx context.Context, runID string) ([]evidence.LogEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := s.readRun(runID); err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(s.runDir(runID), "run", "log.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open run log: %w", err)
	}
	defer f.Close()
	entries, err := evidence.ReadNodeLog("", f)
	return entries, annotateReadError(err, filepath.ToSlash(filepath.Join("run", "log.jsonl")))
}

func annotateReadError(err error, file string) error {
	if err == nil {
		return nil
	}
	var readErr *evidence.ReadError
	if errors.As(err, &readErr) && readErr.File == "" {
		readErr.File = file
	}
	return err
}

func (s *FS) PutArtifact(ctx context.Context, runID, name string, r io.Reader) (ArtifactRecord, error) {
	if err := ctx.Err(); err != nil {
		return ArtifactRecord{}, err
	}
	if _, err := s.readRun(runID); err != nil {
		return ArtifactRecord{}, err
	}
	if r == nil {
		return ArtifactRecord{}, fmt.Errorf("nil artifact reader")
	}
	dir := filepath.Join(s.runDir(runID), "artifacts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ArtifactRecord{}, fmt.Errorf("create artifacts dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".artifact-*.tmp")
	if err != nil {
		return ArtifactRecord{}, fmt.Errorf("create artifact temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	h := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(tmp, h), r)
	if copyErr != nil {
		_ = tmp.Close()
		return ArtifactRecord{}, fmt.Errorf("write artifact: %w", copyErr)
	}
	if err := maybeSync(tmp); err != nil {
		_ = tmp.Close()
		return ArtifactRecord{}, fmt.Errorf("sync artifact: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return ArtifactRecord{}, fmt.Errorf("close artifact: %w", err)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	path := filepath.Join(dir, sum)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(tmpName, path); err != nil {
			return ArtifactRecord{}, fmt.Errorf("install artifact: %w", err)
		}
		_ = syncDir(dir)
	} else if err != nil {
		return ArtifactRecord{}, fmt.Errorf("stat artifact: %w", err)
	}
	return ArtifactRecord{Ref: artifactRefPrefix + sum, Name: name, Size: size, SHA256: sum, At: s.now().UTC()}, nil
}

func (s *FS) GetArtifact(ctx context.Context, runID, ref string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := s.readRun(runID); err != nil {
		return nil, err
	}
	sum, ok := strings.CutPrefix(ref, artifactRefPrefix)
	if !ok || !isHexSHA256(sum) {
		return nil, fmt.Errorf("invalid artifact ref %q", ref)
	}
	f, err := os.Open(filepath.Join(s.runDir(runID), "artifacts", sum))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("open artifact: %w", err)
	}
	return &verifyingReadCloser{
		rc:       f,
		hash:     sha256.New(),
		expected: sum,
	}, nil
}

type verifyingReadCloser struct {
	rc       io.ReadCloser
	hash     hash.Hash
	expected string
	checked  bool
	err      error
}

func (r *verifyingReadCloser) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	if n > 0 {
		_, _ = r.hash.Write(p[:n])
	}
	if errors.Is(err, io.EOF) {
		if verifyErr := r.verify(); verifyErr != nil {
			return n, verifyErr
		}
	}
	return n, err
}

func (r *verifyingReadCloser) Close() error {
	if !r.checked {
		if _, err := io.Copy(io.Discard, r); err != nil && !errors.Is(err, io.EOF) {
			r.err = err
		}
	}
	closeErr := r.rc.Close()
	if r.err != nil {
		return r.err
	}
	return closeErr
}

func (r *verifyingReadCloser) verify() error {
	if r.checked {
		return r.err
	}
	r.checked = true
	got := hex.EncodeToString(r.hash.Sum(nil))
	if got != r.expected {
		r.err = fmt.Errorf("%w: artifact ref sha256:%s points at sha256:%s", ErrContentMismatch, r.expected, got)
	}
	return r.err
}

func (s *FS) AcquireRunLease(ctx context.Context, runID, holder string, ttl time.Duration) (LeaseRecord, error) {
	if err := ctx.Err(); err != nil {
		return LeaseRecord{}, err
	}
	if _, err := s.readRun(runID); err != nil {
		return LeaseRecord{}, err
	}
	if err := validateLeaseRequest(holder, ttl); err != nil {
		return LeaseRecord{}, err
	}
	unlock, err := s.lockRun(ctx, runID)
	if err != nil {
		return LeaseRecord{}, err
	}
	defer unlock()

	now := s.now().UTC()
	lease, err := s.readLease(runID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return LeaseRecord{}, err
	}
	if err == nil && lease.ExpiresAt.After(now) && (lease.normalizedKind() != LeaseKindEngine || lease.Holder != holder || lease.Token != "" || lease.Generation != 0) {
		return LeaseRecord{}, fmt.Errorf("%w: run %q held by %q until %s", ErrLeaseHeld, runID, lease.Holder, lease.ExpiresAt.Format(time.RFC3339Nano))
	}
	next := LeaseRecord{RunID: runID, Holder: holder, Kind: LeaseKindEngine, ExpiresAt: now.Add(ttl), UpdatedAt: now}
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return LeaseRecord{}, fmt.Errorf("encode lease: %w", err)
	}
	data = append(data, '\n')
	if err := writeFileAtomic(filepath.Join(s.runDir(runID), "lease.json"), data, 0o644); err != nil {
		return LeaseRecord{}, err
	}
	return next, nil
}

func (s *FS) ReleaseRunLease(ctx context.Context, runID, holder string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	unlock, err := s.lockRun(ctx, runID)
	if err != nil {
		return err
	}
	defer unlock()
	lease, err := s.readLease(runID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if lease.normalizedKind() != LeaseKindEngine || lease.Holder != holder || lease.Token != "" || lease.Generation != 0 {
		return fmt.Errorf("%w: run %q held by %q", ErrLeaseHeld, runID, lease.Holder)
	}
	if err := os.Remove(filepath.Join(s.runDir(runID), "lease.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove lease: %w", err)
	}
	_ = syncDir(s.runDir(runID))
	return nil
}

func validateLeaseRequest(holder string, ttl time.Duration) error {
	if strings.TrimSpace(holder) == "" || len(holder) > MaxLeaseHolderBytes {
		return fmt.Errorf("lease holder must contain 1..%d bytes", MaxLeaseHolderBytes)
	}
	if ttl <= 0 || ttl > MaxLeaseTTL {
		return fmt.Errorf("lease ttl must be positive and at most %s", MaxLeaseTTL)
	}
	return nil
}

func (lease LeaseRecord) normalizedKind() LeaseKind {
	if lease.Kind == "" {
		return LeaseKindEngine
	}
	return lease.Kind
}

// AcquireEngineLease mints the opaque token+generation capability used by
// schema-8 engine appends. Unlike the legacy holder-only lease, a live lease
// is never silently replaced by the same holder.
func (s *FS) AcquireEngineLease(ctx context.Context, runID, holder string, ttl time.Duration) (EngineLease, error) {
	if err := ctx.Err(); err != nil {
		return EngineLease{}, err
	}
	if err := validateLeaseRequest(holder, ttl); err != nil {
		return EngineLease{}, err
	}
	if _, err := s.readRun(runID); err != nil {
		return EngineLease{}, err
	}
	unlock, err := s.lockRun(ctx, runID)
	if err != nil {
		return EngineLease{}, err
	}
	defer unlock()
	now := s.now().UTC()
	current, readErr := s.readLease(runID)
	if readErr != nil && !errors.Is(readErr, ErrNotFound) {
		return EngineLease{}, readErr
	}
	if readErr == nil && current.ExpiresAt.After(now) {
		return EngineLease{}, fmt.Errorf("%w: run %q held by %q until %s", ErrLeaseHeld, runID, current.Holder, current.ExpiresAt.Format(time.RFC3339Nano))
	}
	generation, err := s.issueLeaseGenerationUnlocked(runID)
	if err != nil {
		return EngineLease{}, err
	}
	if s.leaseGenerationAfterBurn != nil {
		if err := s.leaseGenerationAfterBurn(); err != nil {
			return EngineLease{}, err
		}
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return EngineLease{}, fmt.Errorf("generate engine lease token: %w", err)
	}
	next := LeaseRecord{RunID: runID, Holder: holder, Kind: LeaseKindEngine, Token: hex.EncodeToString(tokenBytes), Generation: generation, ExpiresAt: now.Add(ttl), UpdatedAt: now}
	if err := s.writeLease(next); err != nil {
		return EngineLease{}, err
	}
	if err := syncDir(s.runDir(runID)); err != nil {
		return EngineLease{}, err
	}
	return engineLeaseFromRecord(next), nil
}

func (s *FS) RenewEngineLease(ctx context.Context, lease EngineLease, ttl time.Duration) (EngineLease, error) {
	if err := ctx.Err(); err != nil {
		return EngineLease{}, err
	}
	if err := validateEngineLeaseInput(lease); err != nil {
		return EngineLease{}, err
	}
	if err := validateLeaseRequest(lease.Holder, ttl); err != nil {
		return EngineLease{}, err
	}
	unlock, err := s.lockRun(ctx, lease.RunID)
	if err != nil {
		return EngineLease{}, err
	}
	defer unlock()
	current, err := s.requireEngineLeaseUnlocked(lease)
	if err != nil {
		return EngineLease{}, err
	}
	now := s.now().UTC()
	current.ExpiresAt, current.UpdatedAt = now.Add(ttl), now
	if err := s.writeLease(current); err != nil {
		return EngineLease{}, err
	}
	return engineLeaseFromRecord(current), nil
}

func (s *FS) ReleaseEngineLease(ctx context.Context, lease EngineLease) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateEngineLeaseInput(lease); err != nil {
		return err
	}
	unlock, err := s.lockRun(ctx, lease.RunID)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := s.requireEngineLeaseUnlocked(lease); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(s.runDir(lease.RunID), "lease.json")); err != nil {
		return fmt.Errorf("remove engine lease: %w", err)
	}
	return syncDir(s.runDir(lease.RunID))
}

func validateEngineLeaseInput(lease EngineLease) error {
	if safeSegment(lease.RunID) != nil || strings.TrimSpace(lease.Holder) == "" || len(lease.Holder) > MaxLeaseHolderBytes ||
		len(lease.Token) != 64 || !isHexSHA256(lease.Token) || lease.Generation == 0 {
		return fmt.Errorf("invalid engine lease")
	}
	return nil
}

func (s *FS) requireEngineLeaseUnlocked(lease EngineLease) (LeaseRecord, error) {
	current, err := s.readLease(lease.RunID)
	if err != nil {
		return LeaseRecord{}, err
	}
	if current.normalizedKind() != LeaseKindEngine || current.Holder != lease.Holder || current.Token != lease.Token ||
		current.Generation != lease.Generation || !current.ExpiresAt.After(s.now().UTC()) {
		return LeaseRecord{}, fmt.Errorf("%w: engine lease is absent, expired, or has a different token/generation", ErrLeaseHeld)
	}
	return current, nil
}

func engineLeaseFromRecord(record LeaseRecord) EngineLease {
	return EngineLease{RunID: record.RunID, Holder: record.Holder, Token: record.Token, Generation: record.Generation, ExpiresAt: record.ExpiresAt}
}

func (s *FS) AcquireMaintenanceLease(ctx context.Context, runID, holder string, ttl time.Duration) (MaintenanceLease, error) {
	if err := ctx.Err(); err != nil {
		return MaintenanceLease{}, err
	}
	if err := validateLeaseRequest(holder, ttl); err != nil {
		return MaintenanceLease{}, err
	}
	if _, err := s.readRun(runID); err != nil {
		return MaintenanceLease{}, err
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return MaintenanceLease{}, fmt.Errorf("generate maintenance lease token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)
	unlock, err := s.lockRun(ctx, runID)
	if err != nil {
		return MaintenanceLease{}, err
	}
	defer unlock()
	now := s.now().UTC()
	current, readErr := s.readLease(runID)
	if readErr != nil && !errors.Is(readErr, ErrNotFound) {
		return MaintenanceLease{}, readErr
	}
	if readErr == nil && current.ExpiresAt.After(now) {
		return MaintenanceLease{}, fmt.Errorf("%w: run %q held by %q until %s", ErrLeaseHeld, runID, current.Holder, current.ExpiresAt.Format(time.RFC3339Nano))
	}
	generation, err := s.issueLeaseGenerationUnlocked(runID)
	if err != nil {
		return MaintenanceLease{}, err
	}
	if s.leaseGenerationAfterBurn != nil {
		if err := s.leaseGenerationAfterBurn(); err != nil {
			return MaintenanceLease{}, err
		}
	}
	next := LeaseRecord{RunID: runID, Holder: holder, Kind: LeaseKindMaintenance, Token: token, Generation: generation, ExpiresAt: now.Add(ttl), UpdatedAt: now}
	if err := s.writeLease(next); err != nil {
		return MaintenanceLease{}, err
	}
	if err := syncDir(s.runDir(runID)); err != nil {
		return MaintenanceLease{}, err
	}
	return maintenanceLeaseFromRecord(next), nil
}

func (s *FS) RenewMaintenanceLease(ctx context.Context, lease MaintenanceLease, ttl time.Duration) (MaintenanceLease, error) {
	if err := ctx.Err(); err != nil {
		return MaintenanceLease{}, err
	}
	if err := validateMaintenanceLeaseInput(lease); err != nil {
		return MaintenanceLease{}, err
	}
	if err := validateLeaseRequest(lease.Holder, ttl); err != nil {
		return MaintenanceLease{}, err
	}
	unlock, err := s.lockRun(ctx, lease.RunID)
	if err != nil {
		return MaintenanceLease{}, err
	}
	defer unlock()
	current, err := s.requireMaintenanceLeaseUnlocked(lease)
	if err != nil {
		return MaintenanceLease{}, err
	}
	now := s.now().UTC()
	current.ExpiresAt = now.Add(ttl)
	current.UpdatedAt = now
	if err := s.writeLease(current); err != nil {
		return MaintenanceLease{}, err
	}
	return maintenanceLeaseFromRecord(current), nil
}

func (s *FS) ReleaseMaintenanceLease(ctx context.Context, lease MaintenanceLease) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateMaintenanceLeaseInput(lease); err != nil {
		return err
	}
	unlock, err := s.lockRun(ctx, lease.RunID)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := s.requireMaintenanceLeaseUnlocked(lease); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(s.runDir(lease.RunID), "lease.json")); err != nil {
		return fmt.Errorf("remove maintenance lease: %w", err)
	}
	return syncDir(s.runDir(lease.RunID))
}

func validateMaintenanceLeaseInput(lease MaintenanceLease) error {
	if safeSegment(lease.RunID) != nil || strings.TrimSpace(lease.Holder) == "" || len(lease.Holder) > MaxLeaseHolderBytes || len(lease.Token) != 64 || !isHexSHA256(lease.Token) || lease.Generation == 0 {
		return fmt.Errorf("invalid maintenance lease")
	}
	return nil
}

func (s *FS) requireMaintenanceLeaseUnlocked(lease MaintenanceLease) (LeaseRecord, error) {
	current, err := s.readLease(lease.RunID)
	if err != nil {
		return LeaseRecord{}, err
	}
	if current.normalizedKind() != LeaseKindMaintenance || current.Holder != lease.Holder || current.Token != lease.Token || current.Generation != lease.Generation || !current.ExpiresAt.After(s.now().UTC()) {
		return LeaseRecord{}, fmt.Errorf("%w: maintenance lease is absent, expired, or has a different token", ErrLeaseHeld)
	}
	return current, nil
}

func maintenanceLeaseFromRecord(record LeaseRecord) MaintenanceLease {
	return MaintenanceLease{RunID: record.RunID, Holder: record.Holder, Token: record.Token, Generation: record.Generation, ExpiresAt: record.ExpiresAt}
}

type leaseGenerationState struct {
	Generation uint64 `json:"generation"`
}

func (s *FS) issueLeaseGenerationUnlocked(runID string) (uint64, error) {
	path := filepath.Join(s.runDir(runID), "lease-generation.json")
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("read durable lease generation: %w", err)
	}
	var state leaseGenerationState
	if err == nil {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&state); err != nil {
			return 0, fmt.Errorf("decode durable lease generation: %w", err)
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			return 0, fmt.Errorf("decode durable lease generation trailing data")
		}
	}
	lease, leaseErr := s.readLease(runID)
	if leaseErr != nil && !errors.Is(leaseErr, ErrNotFound) {
		return 0, leaseErr
	}
	if leaseErr == nil {
		if err := validateLeaseGenerationFloor(lease); err != nil {
			return 0, err
		}
		state.Generation = max(state.Generation, lease.Generation)
	}
	if state.Generation == math.MaxUint64 {
		return 0, fmt.Errorf("durable lease generation exhausted")
	}
	state.Generation++
	encoded, err := json.Marshal(state)
	if err != nil {
		return 0, err
	}
	encoded = append(encoded, '\n')
	if err := writeFileAtomic(path, encoded, 0o644); err != nil {
		return 0, err
	}
	if err := syncDir(s.runDir(runID)); err != nil {
		return 0, err
	}
	return state.Generation, nil
}

func validateLeaseGenerationFloor(lease LeaseRecord) error {
	kind := lease.normalizedKind()
	if kind != LeaseKindEngine && kind != LeaseKindMaintenance {
		return fmt.Errorf("invalid lease generation floor: unknown lease kind %q", lease.Kind)
	}
	if lease.Generation == 0 {
		if kind == LeaseKindEngine && lease.Token == "" || kind == LeaseKindMaintenance && len(lease.Token) == 64 && isHexSHA256(lease.Token) {
			return nil
		}
		return fmt.Errorf("invalid legacy lease generation floor")
	}
	if len(lease.Token) != 64 || !isHexSHA256(lease.Token) {
		return fmt.Errorf("invalid tokenized lease generation floor")
	}
	return nil
}

func (s *FS) writeLease(lease LeaseRecord) error {
	data, err := json.MarshalIndent(lease, "", "  ")
	if err != nil {
		return fmt.Errorf("encode lease: %w", err)
	}
	data = append(data, '\n')
	return writeFileAtomic(filepath.Join(s.runDir(lease.RunID), "lease.json"), data, 0o644)
}

func (s *FS) readRun(runID string) (RunRecord, error) {
	if err := safeSegment(runID); err != nil {
		return RunRecord{}, fmt.Errorf("invalid run id: %w", err)
	}
	data, err := os.ReadFile(filepath.Join(s.runDir(runID), "run.json"))
	if errors.Is(err, os.ErrNotExist) {
		return RunRecord{}, ErrNotFound
	}
	if err != nil {
		return RunRecord{}, fmt.Errorf("read run: %w", err)
	}
	var run RunRecord
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&run); err != nil {
		return RunRecord{}, &DecodeError{Component: "run record", Err: err}
	}
	return run, nil
}

func (s *FS) readState(runID string) (*state.State, error) {
	data, err := os.ReadFile(filepath.Join(s.runDir(runID), "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	st, err := state.Decode(data)
	if err != nil {
		return nil, &DecodeError{Component: "run state", Err: err}
	}
	return st, nil
}

func (s *FS) readAllLogs(ctx context.Context, runID string) ([]evidence.NodeLog, error) {
	nodesDir := filepath.Join(s.runDir(runID), "nodes")
	entries, err := os.ReadDir(nodesDir)
	if errors.Is(err, os.ErrNotExist) {
		entries = nil
	} else if err != nil {
		return nil, fmt.Errorf("read node log dirs: %w", err)
	}
	nodeIDs := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			nodeIDs = append(nodeIDs, entry.Name())
		}
	}
	slices.Sort(nodeIDs)
	out := make([]evidence.NodeLog, 0, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		logEntries, err := s.ReadNodeLog(ctx, runID, nodeID)
		if err != nil {
			return nil, err
		}
		out = append(out, evidence.NodeLog{NodeID: nodeID, Entries: logEntries})
	}
	runEntries, err := s.ReadRunLog(ctx, runID)
	if err != nil {
		return nil, err
	}
	if len(runEntries) > 0 {
		out = append(out, evidence.NodeLog{Entries: runEntries})
	}
	return out, nil
}

func (s *FS) readLease(runID string) (LeaseRecord, error) {
	data, err := os.ReadFile(filepath.Join(s.runDir(runID), "lease.json"))
	if errors.Is(err, os.ErrNotExist) {
		return LeaseRecord{}, ErrNotFound
	}
	if err != nil {
		return LeaseRecord{}, fmt.Errorf("read lease: %w", err)
	}
	var lease LeaseRecord
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&lease); err != nil {
		return LeaseRecord{}, fmt.Errorf("decode lease: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return LeaseRecord{}, fmt.Errorf("decode lease trailing data")
	}
	if lease.RunID != runID {
		return LeaseRecord{}, fmt.Errorf("decode lease: run id differs from containing run")
	}
	return lease, nil
}

func (s *FS) appendLogEntry(runID string, entry evidence.LogEntry) error {
	path, err := s.logPath(runID, entry.Scope)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}
	if err := syncDir(filepath.Dir(dir)); err != nil {
		return fmt.Errorf("sync log parent dir: %w", err)
	}
	return appendJSONL(path, func(w io.Writer) error { return evidence.AppendLogEntry(w, entry) })
}

func (s *FS) appendManifestEntry(runID string, entry evidence.ManifestEntry) error {
	return appendJSONL(filepath.Join(s.runDir(runID), "manifest.jsonl"), func(w io.Writer) error {
		return evidence.AppendManifestEntry(w, entry)
	})
}

func appendJSONL(path string, write func(io.Writer) error) error {
	_, statErr := os.Stat(path)
	created := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !created {
		return fmt.Errorf("stat append file: %w", statErr)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open append file: %w", err)
	}
	if err := write(f); err != nil {
		_ = f.Close()
		return fmt.Errorf("append jsonl: %w", err)
	}
	if err := maybeSync(f); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync append file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close append file: %w", err)
	}
	if created {
		if err := syncDir(filepath.Dir(path)); err != nil {
			return fmt.Errorf("sync append dir: %w", err)
		}
	}
	return nil
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

func (s *FS) lockRun(ctx context.Context, runID string) (func(), error) {
	if err := safeSegment(runID); err != nil {
		return func() {}, fmt.Errorf("invalid run id: %w", err)
	}
	lockValue, _ := processLocks.LoadOrStore(s.root+"\x00"+runID, newLocalRunLock())
	localLock := lockValue.(*localRunLock)
	if err := localLock.Lock(ctx, nil); err != nil {
		return func() {}, err
	}
	lockDir := filepath.Join(s.root, ".locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		localLock.Unlock()
		return func() {}, fmt.Errorf("create lock dir: %w", err)
	}
	fl := flock.New(filepath.Join(lockDir, runLockFileName(runID)))
	locked, err := fl.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		localLock.Unlock()
		return func() {}, fmt.Errorf("lock run: %w", err)
	}
	if !locked {
		localLock.Unlock()
		if err := ctx.Err(); err != nil {
			return func() {}, err
		}
		return func() {}, fmt.Errorf("lock run: lock not acquired")
	}
	return func() {
		_ = fl.Unlock()
		localLock.Unlock()
	}, nil
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

func (s *FS) runDir(runID string) string {
	return filepath.Join(s.root, "runs", runID)
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

func (s *FS) logPath(runID string, scope evidence.Scope) (string, error) {
	switch scope.Kind {
	case evidence.ScopeNode:
		if err := safeSegment(scope.ID); err != nil {
			return "", fmt.Errorf("invalid node id: %w", err)
		}
		return filepath.Join(s.runDir(runID), "nodes", scope.ID, "log.jsonl"), nil
	case evidence.ScopeRun:
		if scope.ID != "" {
			return "", fmt.Errorf("run scope must not set id")
		}
		return filepath.Join(s.runDir(runID), "run", "log.jsonl"), nil
	default:
		return "", fmt.Errorf("invalid scope kind %q", scope.Kind)
	}
}

func validateEntryScope(scope evidence.Scope) error {
	switch scope.Kind {
	case evidence.ScopeNode:
		if err := safeSegment(scope.ID); err != nil {
			return fmt.Errorf("invalid node scope id: %w", err)
		}
	case evidence.ScopeRun:
		if scope.ID != "" {
			return fmt.Errorf("run scope must not set id")
		}
	default:
		return fmt.Errorf("invalid scope kind %q", scope.Kind)
	}
	return nil
}

// runLockFileName and templateLockFileName are the ONLY places the advisory
// lock filenames are constructed. Both the plain and the viewer lock paths
// (lockRun/lockRunView, lockTemplate/lockTemplateView) route through them,
// because the two must resolve to the same file to be the same lock — the local
// semaphore keys already agree, so a filename drift would silently break mutual
// exclusion rather than fail loudly.
//
// Both kinds are prefixed. Run locks were once bare "<runID>.lock" while
// template locks were "template-<id>.lock", so a run whose id happened to be
// "template-<some template id>" resolved to the SAME lock file as that
// template. flock associates locks with the open file description, so two
// opens in one process conflict: CreateRun, which now holds the run lock while
// taking the template lock, would block on itself forever. Fixed prefixes on
// both sides make a collision unrepresentable regardless of id content.
func runLockFileName(runID string) string { return "run-" + runID + ".lock" }

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
