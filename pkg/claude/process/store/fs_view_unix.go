//go:build linux || darwin

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"golang.org/x/sys/unix"

	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
)

const (
	viewerDefaultMaxFileBytes        = int64(16 << 20)
	viewerDefaultMaxTotalBytes       = int64(64 << 20)
	viewerDefaultMaxRecords          = 100_000
	viewerDefaultMaxDirectoryEntries = 4_096
	viewerDirectoryBatch             = 128
)

// viewBudget bounds the v1 full-history viewer. Every regular file has a
// 16-MiB ceiling; one run snapshot has a 64-MiB cumulative byte ceiling,
// 100,000 evidence records, and 4,096 directory entries. Reads use limit+1 to
// catch concurrent growth after stat and check cancellation between chunks.
type viewBudget struct {
	ctx        context.Context
	maxFile    int64
	maxTotal   int64
	maxRecords int
	maxEntries int
	bytes      int64
	records    int
	entries    int
	readHook   func(string, int64)
	decodeHook func(string)
}

func (s *FS) newViewBudget(ctx context.Context) *viewBudget {
	budget := &viewBudget{ctx: ctx, maxFile: viewerDefaultMaxFileBytes, maxTotal: viewerDefaultMaxTotalBytes, maxRecords: viewerDefaultMaxRecords, maxEntries: viewerDefaultMaxDirectoryEntries, readHook: s.viewerReadChunkHook, decodeHook: s.viewerDecodeHook}
	if s.viewerMaxFileBytes > 0 {
		budget.maxFile = s.viewerMaxFileBytes
	}
	if s.viewerMaxTotalBytes > 0 {
		budget.maxTotal = s.viewerMaxTotalBytes
	}
	if s.viewerMaxRecords > 0 {
		budget.maxRecords = s.viewerMaxRecords
	}
	if s.viewerMaxDirectoryEntries > 0 {
		budget.maxEntries = s.viewerMaxDirectoryEntries
	}
	return budget
}

func (s *FS) lockRunView(ctx context.Context, runID string) (func(), error) {
	if err := safeSegment(runID); err != nil {
		return func() {}, fmt.Errorf("invalid run id: %w", err)
	}
	return s.lockView(ctx, s.root+"\x00"+runID, runID+".lock")
}

func (s *FS) lockTemplateView(ctx context.Context, id string) (func(), error) {
	if err := safeSegment(id); err != nil {
		return func() {}, fmt.Errorf("invalid template id: %w", err)
	}
	return s.lockView(ctx, s.root+"\x00template\x00"+id, "template-"+id+".lock")
}

func (s *FS) lockView(ctx context.Context, key, name string) (func(), error) {
	localValue, _ := processLocks.LoadOrStore(key, newLocalRunLock())
	local := localValue.(*localRunLock)
	if err := local.Lock(ctx, nil); err != nil {
		return func() {}, err
	}
	lockDirPath := filepath.Join(s.root, ".locks")
	if err := os.Mkdir(lockDirPath, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		local.Unlock()
		return func() {}, err
	}
	lockDir, err := openViewDir(lockDirPath)
	if err != nil {
		local.Unlock()
		return func() {}, fmt.Errorf("%w: open viewer lock directory", ErrUnsafeRunPath)
	}
	fd, err := unix.Openat(int(lockDir.Fd()), name, unix.O_RDWR|unix.O_CLOEXEC|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
	lockDir.Close()
	if err != nil {
		local.Unlock()
		if errors.Is(err, unix.ELOOP) {
			return func() {}, ErrUnsafeRunPath
		}
		return func() {}, fmt.Errorf("open viewer lock: %w", err)
	}
	lockFile := os.NewFile(uintptr(fd), name)
	info, err := lockFile.Stat()
	if err != nil || !info.Mode().IsRegular() {
		lockFile.Close()
		local.Unlock()
		return func() {}, ErrUnsafeRunPath
	}
	for {
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			lockFile.Close()
			local.Unlock()
			return func() {}, err
		}
		select {
		case <-ctx.Done():
			lockFile.Close()
			local.Unlock()
			return func() {}, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	return func() {
		_ = unix.Flock(fd, unix.LOCK_UN)
		_ = lockFile.Close()
		local.Unlock()
	}, nil
}

// HasRunView confirms a run by safe directory identity rather than decoded
// run.json content. This lets corrupt or mismatched records remain inspectable
// without treating symlinks or unsafe filesystem objects as runs.
func (s *FS) HasRunView(runID string) (bool, error) {
	if err := safeSegment(runID); err != nil {
		return false, err
	}
	root, err := openViewDir(s.root)
	if err != nil {
		return false, err
	}
	defer root.Close()
	runs, err := openViewDirAt(root, "runs")
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer runs.Close()
	runDir, err := openViewDirAt(runs, runID)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: confirm run directory", ErrUnsafeRunPath)
	}
	runDir.Close()
	return true, nil
}

// hasTemplateExactView confirms that an exact template has a descriptor-safe
// persisted identity before the operational template lock creates any map or
// lock-file state. A regular pending intent counts as existing so the locked
// read can preserve ErrTemplateSavePending behavior.
func (s *FS) hasTemplateExactView(id, hash string) (bool, error) {
	if err := safeSegment(id); err != nil {
		return false, err
	}
	if !isHexSHA256(hash) {
		return false, fmt.Errorf("invalid template hash %q", hash)
	}
	root, err := openViewDir(s.root)
	if err != nil {
		return false, err
	}
	defer root.Close()
	templates, err := openViewDirAt(root, "templates")
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer templates.Close()
	idDir, err := openViewDirAt(templates, id)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer idDir.Close()

	var intentStat unix.Stat_t
	if err := unix.Fstatat(int(idDir.Fd()), ".attributed-save-intent.json", &intentStat, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		if intentStat.Mode&unix.S_IFMT != unix.S_IFREG {
			return false, ErrUnsafeRunPath
		}
		return true, nil
	} else if !errors.Is(err, unix.ENOENT) {
		return false, err
	}

	version, err := openViewDirAt(idDir, "sha256-"+hash)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer version.Close()
	return hasViewRegularAt(version, "template.json")
}

// loadRunViewSnapshotAt reads the complete viewer snapshot descriptor-relative
// to a no-follow run directory. Append holds the same run lock, and every
// consumed component is opened with O_NOFOLLOW before its type is checked.
func (s *FS) loadRunViewSnapshotAt(ctx context.Context, runID string) (Snapshot, error) {
	if err := safeSegment(runID); err != nil {
		return Snapshot{}, err
	}
	root, err := openViewDir(s.root)
	if err != nil {
		return Snapshot{}, fmt.Errorf("open process store root: %w", err)
	}
	defer root.Close()
	runs, err := openViewDirAt(root, "runs")
	if errors.Is(err, unix.ENOENT) {
		return Snapshot{}, ErrNotFound
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("open process runs: %w", err)
	}
	defer runs.Close()
	runDir, err := openViewDirAt(runs, runID)
	if errors.Is(err, unix.ENOENT) {
		return Snapshot{}, ErrNotFound
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("%w: open run directory", ErrUnsafeRunPath)
	}
	defer runDir.Close()
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	budget := s.newViewBudget(ctx)

	runData, err := readViewRegularAt(budget, runDir, "run.json", false)
	if err != nil {
		return Snapshot{}, classifyRequiredViewFile("run record", err)
	}
	var run RunRecord
	if err := runViewDecode(ctx, budget.decodeHook, "run", func() error { return decodeViewJSON(ctx, runData, &run, false) }); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return Snapshot{}, err
		}
		return Snapshot{}, &DecodeError{Component: "run record", Err: err}
	}
	if run.ID != runID {
		return Snapshot{}, &DecodeError{Component: "run identity", Err: errors.New("record id does not match directory")}
	}

	stateData, err := readViewRegularAt(budget, runDir, "state.json", false)
	if err != nil {
		return Snapshot{}, classifyRequiredViewFile("run state", err)
	}
	var st *state.State
	err = runViewDecode(ctx, budget.decodeHook, "state", func() error {
		var decodeErr error
		st, decodeErr = state.DecodeContext(ctx, stateData)
		return decodeErr
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return Snapshot{}, err
		}
		return Snapshot{}, &DecodeError{Component: "run state", Err: err}
	}

	manifestData, err := readViewRegularAt(budget, runDir, "manifest.jsonl", true)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read viewer manifest: %w", err)
	}
	if err := budget.addRecords(manifestData); err != nil {
		return Snapshot{}, err
	}
	var manifest []evidence.ManifestEntry
	err = runViewDecode(ctx, budget.decodeHook, "manifest", func() error {
		var decodeErr error
		manifest, decodeErr = evidence.ReadManifestContext(ctx, bytes.NewReader(manifestData))
		return decodeErr
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return Snapshot{}, err
		}
		return Snapshot{}, annotateReadError(err, "manifest.jsonl")
	}
	nodeLogs, err := readViewLogsAt(budget, runDir)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Run: run, State: st, Manifest: manifest, NodeLogs: nodeLogs}, nil
}

func readViewLogsAt(budget *viewBudget, runDir *os.File) ([]evidence.NodeLog, error) {
	var out []evidence.NodeLog
	nodes, err := openViewDirAt(runDir, "nodes")
	if err != nil && !errors.Is(err, unix.ENOENT) {
		return nil, fmt.Errorf("%w: open node log directory", ErrUnsafeRunPath)
	}
	if err == nil {
		var names []string
		for {
			if err := budget.ctx.Err(); err != nil {
				nodes.Close()
				return nil, err
			}
			batch, readErr := nodes.Readdirnames(viewerDirectoryBatch)
			if len(batch) > 0 {
				budget.entries += len(batch)
				if budget.entries > budget.maxEntries {
					nodes.Close()
					return nil, ErrViewerResourceLimit
				}
				names = append(names, batch...)
			}
			if errors.Is(readErr, os.ErrClosed) {
				nodes.Close()
				return nil, readErr
			}
			if readErr != nil {
				if !errors.Is(readErr, io.EOF) {
					nodes.Close()
					return nil, readErr
				}
				if len(batch) == 0 {
					break
				}
			}
		}
		slices.Sort(names)
		for _, nodeID := range names {
			if err := budget.ctx.Err(); err != nil {
				nodes.Close()
				return nil, err
			}
			if err := safeSegment(nodeID); err != nil {
				nodes.Close()
				return nil, fmt.Errorf("%w: invalid node log directory", ErrUnsafeRunPath)
			}
			nodeDir, openErr := openViewDirAt(nodes, nodeID)
			if openErr != nil {
				nodes.Close()
				return nil, fmt.Errorf("%w: open node log directory", ErrUnsafeRunPath)
			}
			data, fileErr := readViewRegularAt(budget, nodeDir, "log.jsonl", true)
			nodeDir.Close()
			if fileErr != nil {
				nodes.Close()
				return nil, fmt.Errorf("read viewer node log: %w", fileErr)
			}
			if recordErr := budget.addRecords(data); recordErr != nil {
				nodes.Close()
				return nil, recordErr
			}
			var entries []evidence.LogEntry
			decodeErr := runViewDecode(budget.ctx, budget.decodeHook, "node log", func() error {
				var err error
				entries, err = evidence.ReadNodeLogContext(budget.ctx, nodeID, bytes.NewReader(data))
				return err
			})
			if decodeErr != nil {
				if errors.Is(decodeErr, context.Canceled) || errors.Is(decodeErr, context.DeadlineExceeded) {
					nodes.Close()
					return nil, decodeErr
				}
				nodes.Close()
				return nil, annotateReadError(decodeErr, "nodes/"+nodeID+"/log.jsonl")
			}
			out = append(out, evidence.NodeLog{NodeID: nodeID, Entries: entries})
		}
		nodes.Close()
	}

	runLogDir, err := openViewDirAt(runDir, "run")
	if err != nil && !errors.Is(err, unix.ENOENT) {
		return nil, fmt.Errorf("%w: open run log directory", ErrUnsafeRunPath)
	}
	if err == nil {
		data, fileErr := readViewRegularAt(budget, runLogDir, "log.jsonl", true)
		runLogDir.Close()
		if fileErr != nil {
			return nil, fmt.Errorf("read viewer run log: %w", fileErr)
		}
		if recordErr := budget.addRecords(data); recordErr != nil {
			return nil, recordErr
		}
		var entries []evidence.LogEntry
		decodeErr := runViewDecode(budget.ctx, budget.decodeHook, "run log", func() error {
			var err error
			entries, err = evidence.ReadNodeLogContext(budget.ctx, "", bytes.NewReader(data))
			return err
		})
		if decodeErr != nil {
			if errors.Is(decodeErr, context.Canceled) || errors.Is(decodeErr, context.DeadlineExceeded) {
				return nil, decodeErr
			}
			return nil, annotateReadError(decodeErr, "run/log.jsonl")
		}
		if len(entries) > 0 {
			out = append(out, evidence.NodeLog{Entries: entries})
		}
	}
	return out, nil
}

func classifyRequiredViewFile(component string, err error) error {
	if errors.Is(err, unix.ENOENT) {
		return ErrNotFound
	}
	if errors.Is(err, ErrUnsafeRunPath) {
		return err
	}
	return fmt.Errorf("read viewer %s: %w", component, err)
}

func openViewDir(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func openViewDirAt(parent *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func readViewRegularAt(budget *viewBudget, parent *os.File, name string, missingEmpty bool) ([]byte, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) && missingEmpty {
		return nil, nil
	}
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, ErrUnsafeRunPath
		}
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, ErrUnsafeRunPath
	}
	remaining := budget.maxTotal - budget.bytes
	if info.Size() > budget.maxFile || info.Size() > remaining || remaining < 0 {
		return nil, ErrViewerResourceLimit
	}
	allowed := min(budget.maxFile, remaining)
	data := make([]byte, 0, min(info.Size(), allowed))
	chunk := make([]byte, 32<<10)
	for {
		if err := budget.ctx.Err(); err != nil {
			return nil, err
		}
		// Read at most the accepted payload plus one byte. The extra byte
		// detects growth after stat without allocating the grown file.
		limitPlusOne := allowed + 1
		remainingRead := limitPlusOne - int64(len(data))
		if remainingRead <= 0 {
			return nil, ErrViewerResourceLimit
		}
		readSize := min(int64(len(chunk)), remainingRead)
		n, readErr := file.Read(chunk[:readSize])
		if n > 0 {
			data = append(data, chunk[:n]...)
			if budget.readHook != nil {
				budget.readHook(name, int64(len(data)))
			}
			if int64(len(data)) > allowed {
				return nil, ErrViewerResourceLimit
			}
		}
		if errors.Is(readErr, os.ErrClosed) {
			return nil, readErr
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return nil, readErr
			}
			break
		}
	}
	budget.bytes += int64(len(data))
	return data, budget.ctx.Err()
}

func hasViewRegularAt(parent *os.File, name string) (bool, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return false, ErrUnsafeRunPath
		}
		return false, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, ErrUnsafeRunPath
	}
	return true, nil
}

// getTemplateExactBody reads immutable template content without following any
// template-tree symlink. Missing content remains a data condition; unsafe or
// ordinary I/O remains an infrastructure error for the HTTP boundary.
func (s *FS) getTemplateExactBody(ctx context.Context, id, hash string) ([]byte, error) {
	root, err := openViewDir(s.root)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	templates, err := openViewDirAt(root, "templates")
	if err != nil {
		return nil, err
	}
	defer templates.Close()
	idDir, err := openViewDirAt(templates, id)
	if err != nil {
		return nil, err
	}
	defer idDir.Close()
	var intentStat unix.Stat_t
	if err := unix.Fstatat(int(idDir.Fd()), ".attributed-save-intent.json", &intentStat, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		if intentStat.Mode&unix.S_IFMT != unix.S_IFREG {
			return nil, ErrUnsafeRunPath
		}
		return nil, ErrTemplateSavePending
	} else if !errors.Is(err, unix.ENOENT) {
		return nil, err
	}
	version, err := openViewDirAt(idDir, "sha256-"+hash)
	if err != nil {
		return nil, err
	}
	defer version.Close()
	return readViewRegularAt(s.newViewBudget(ctx), version, "template.json", false)
}

func (b *viewBudget) addRecords(data []byte) error {
	for start := 0; start < len(data); {
		if err := b.ctx.Err(); err != nil {
			return err
		}
		end := bytes.IndexByte(data[start:], '\n')
		if end < 0 {
			end = len(data) - start
		} else {
			end++
		}
		if len(bytes.TrimSpace(data[start:start+end])) > 0 {
			b.records++
			if b.records > b.maxRecords {
				return ErrViewerResourceLimit
			}
		}
		start += end
	}
	return nil
}

func runViewDecode(ctx context.Context, hook func(string), component string, decode func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if hook != nil {
		hook(component)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	err := decode()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

func decodeViewJSON(ctx context.Context, data []byte, dst any, disallowUnknown bool) error {
	dec := json.NewDecoder(&viewDecodeReader{ctx: ctx, reader: bytes.NewReader(data)})
	if disallowUnknown {
		dec.DisallowUnknownFields()
	}
	return dec.Decode(dst)
}

type viewDecodeReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *viewDecodeReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) > 32<<10 {
		p = p[:32<<10]
	}
	n, err := r.reader.Read(p)
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		return n, ctxErr
	}
	return n, err
}
