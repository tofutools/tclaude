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

func (s *FS) lockRunView(ctx context.Context, runID string) (func(), error) {
	return s.lockView(ctx, s.root+"\x00"+runID, runID+".lock")
}

func (s *FS) lockTemplateView(ctx context.Context, id string) (func(), error) {
	return s.lockView(ctx, s.root+"\x00template\x00"+id, "template-"+id+".lock")
}

func (s *FS) lockView(ctx context.Context, key, name string) (func(), error) {
	localValue, _ := processLocks.LoadOrStore(key, newLocalRunLock())
	local := localValue.(*localRunLock)
	if err := local.Lock(ctx); err != nil {
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

// loadRunViewSnapshotAt reads the complete viewer snapshot descriptor-relative
// to a no-follow run directory. Append holds the same run lock, and every
// consumed component is opened with O_NOFOLLOW before its type is checked.
func (s *FS) loadRunViewSnapshotAt(ctxErr func() error, runID string) (Snapshot, error) {
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
	if err := ctxErr(); err != nil {
		return Snapshot{}, err
	}

	runData, err := readViewRegularAt(runDir, "run.json", false)
	if err != nil {
		return Snapshot{}, classifyRequiredViewFile("run record", err)
	}
	var run RunRecord
	if err := json.NewDecoder(bytes.NewReader(runData)).Decode(&run); err != nil {
		return Snapshot{}, &DecodeError{Component: "run record", Err: err}
	}
	if run.ID != runID {
		return Snapshot{}, &DecodeError{Component: "run identity", Err: errors.New("record id does not match directory")}
	}

	stateData, err := readViewRegularAt(runDir, "state.json", false)
	if err != nil {
		return Snapshot{}, classifyRequiredViewFile("run state", err)
	}
	st, err := state.Decode(stateData)
	if err != nil {
		return Snapshot{}, &DecodeError{Component: "run state", Err: err}
	}

	manifestData, err := readViewRegularAt(runDir, "manifest.jsonl", true)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read viewer manifest: %w", err)
	}
	manifest, err := evidence.ReadManifest(bytes.NewReader(manifestData))
	if err != nil {
		return Snapshot{}, annotateReadError(err, "manifest.jsonl")
	}
	nodeLogs, err := readViewLogsAt(runDir)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Run: run, State: st, Manifest: manifest, NodeLogs: nodeLogs}, nil
}

func readViewLogsAt(runDir *os.File) ([]evidence.NodeLog, error) {
	var out []evidence.NodeLog
	nodes, err := openViewDirAt(runDir, "nodes")
	if err != nil && !errors.Is(err, unix.ENOENT) {
		return nil, fmt.Errorf("%w: open node log directory", ErrUnsafeRunPath)
	}
	if err == nil {
		names, readErr := nodes.Readdirnames(-1)
		if readErr != nil {
			nodes.Close()
			return nil, fmt.Errorf("read viewer node directory: %w", readErr)
		}
		slices.Sort(names)
		for _, nodeID := range names {
			if err := safeSegment(nodeID); err != nil {
				nodes.Close()
				return nil, fmt.Errorf("%w: invalid node log directory", ErrUnsafeRunPath)
			}
			nodeDir, openErr := openViewDirAt(nodes, nodeID)
			if openErr != nil {
				nodes.Close()
				return nil, fmt.Errorf("%w: open node log directory", ErrUnsafeRunPath)
			}
			data, fileErr := readViewRegularAt(nodeDir, "log.jsonl", true)
			nodeDir.Close()
			if fileErr != nil {
				nodes.Close()
				return nil, fmt.Errorf("read viewer node log: %w", fileErr)
			}
			entries, decodeErr := evidence.ReadNodeLog(nodeID, bytes.NewReader(data))
			if decodeErr != nil {
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
		data, fileErr := readViewRegularAt(runLogDir, "log.jsonl", true)
		runLogDir.Close()
		if fileErr != nil {
			return nil, fmt.Errorf("read viewer run log: %w", fileErr)
		}
		entries, decodeErr := evidence.ReadNodeLog("", bytes.NewReader(data))
		if decodeErr != nil {
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

func readViewRegularAt(parent *os.File, name string, missingEmpty bool) ([]byte, error) {
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
	return io.ReadAll(file)
}

// getTemplateExactBody reads immutable template content without following any
// template-tree symlink. Missing content remains a data condition; unsafe or
// ordinary I/O remains an infrastructure error for the HTTP boundary.
func (s *FS) getTemplateExactBody(id, hash string) ([]byte, error) {
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
	return readViewRegularAt(version, "template.json", false)
}
