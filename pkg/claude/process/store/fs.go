package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
)

const artifactRefPrefix = "artifact:sha256:"

type FS struct {
	root string
	now  func() time.Time
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

func (s *FS) PutTemplate(ctx context.Context, tmpl *model.Template) (TemplateRecord, error) {
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
		return TemplateRecord{ID: tmpl.ID, Ref: ref, SemanticHash: semanticHash, StoredAt: fileModTime(bodyPath)}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return TemplateRecord{}, fmt.Errorf("read existing template: %w", err)
	}
	if err := writeFileAtomic(bodyPath, body, 0o644); err != nil {
		return TemplateRecord{}, err
	}
	return TemplateRecord{ID: tmpl.ID, Ref: ref, SemanticHash: semanticHash, StoredAt: s.now().UTC()}, nil
}

func (s *FS) GetTemplate(ctx context.Context, ref string) (*model.Template, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id, hash, err := parseTemplateRef(ref)
	if err != nil {
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

func (s *FS) CreateRun(ctx context.Context, run RunRecord, initial state.State) (RunRecord, error) {
	if err := ctx.Err(); err != nil {
		return RunRecord{}, err
	}
	if err := safeSegment(run.ID); err != nil {
		return RunRecord{}, fmt.Errorf("invalid run id: %w", err)
	}
	if strings.TrimSpace(run.TemplateRef) == "" {
		return RunRecord{}, fmt.Errorf("templateRef is required")
	}
	unlock, err := s.lockRun(ctx, run.ID)
	if err != nil {
		return RunRecord{}, err
	}
	defer unlock()
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
		return RunRecord{}, fmt.Errorf("run %q already exists", run.ID)
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

func (s *FS) GetRun(ctx context.Context, runID string) (RunRecord, error) {
	if err := ctx.Err(); err != nil {
		return RunRecord{}, err
	}
	return s.readRun(runID)
}

func (s *FS) LoadRun(ctx context.Context, runID string) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
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
	return evidence.ReadManifest(f)
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
	return evidence.ReadNodeLog(nodeID, f)
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
	return evidence.ReadNodeLog("", f)
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
	if err := tmp.Sync(); err != nil {
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
	if strings.TrimSpace(holder) == "" {
		return LeaseRecord{}, fmt.Errorf("lease holder is required")
	}
	if ttl <= 0 {
		return LeaseRecord{}, fmt.Errorf("lease ttl must be positive")
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
	if err == nil && lease.Holder != holder && lease.ExpiresAt.After(now) {
		return LeaseRecord{}, fmt.Errorf("%w: run %q held by %q until %s", ErrLeaseHeld, runID, lease.Holder, lease.ExpiresAt.Format(time.RFC3339Nano))
	}
	next := LeaseRecord{RunID: runID, Holder: holder, ExpiresAt: now.Add(ttl), UpdatedAt: now}
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
	if lease.Holder != holder {
		return fmt.Errorf("%w: run %q held by %q", ErrLeaseHeld, runID, lease.Holder)
	}
	if err := os.Remove(filepath.Join(s.runDir(runID), "lease.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove lease: %w", err)
	}
	_ = syncDir(s.runDir(runID))
	return nil
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
	dec.DisallowUnknownFields()
	if err := dec.Decode(&run); err != nil {
		return RunRecord{}, fmt.Errorf("decode run: %w", err)
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
	return state.Decode(data)
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
	if err := f.Sync(); err != nil {
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
	if err := tmp.Sync(); err != nil {
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
	return d.Sync()
}

func (s *FS) lockRun(ctx context.Context, runID string) (func(), error) {
	if err := safeSegment(runID); err != nil {
		return func() {}, fmt.Errorf("invalid run id: %w", err)
	}
	lockValue, _ := processLocks.LoadOrStore(s.root+"\x00"+runID, newLocalRunLock())
	localLock := lockValue.(*localRunLock)
	if err := localLock.Lock(ctx); err != nil {
		return func() {}, err
	}
	lockDir := filepath.Join(s.root, ".locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		localLock.Unlock()
		return func() {}, fmt.Errorf("create lock dir: %w", err)
	}
	fl := flock.New(filepath.Join(lockDir, runID+".lock"))
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

type localRunLock struct {
	ch chan struct{}
}

func newLocalRunLock() *localRunLock {
	lock := &localRunLock{ch: make(chan struct{}, 1)}
	lock.ch <- struct{}{}
	return lock
}

func (l *localRunLock) Lock(ctx context.Context) error {
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

func safeSegment(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("empty path segment")
	}
	if value == "." || value == ".." || strings.ContainsAny(value, `/\`) {
		return fmt.Errorf("unsafe path segment %q", value)
	}
	return nil
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
