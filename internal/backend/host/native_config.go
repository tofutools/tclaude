package host

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

const nativeConfigEditMaxAttempts = 5
const nativeConfigLockRetry = 50 * time.Millisecond

var nativeConfigEditMu sync.Mutex
var nativeConfigLockTimeout = 10 * time.Second

type atomicFileReplacement struct{ path, tmpName, dir string }

// EditNativeConfigFile applies a provider-owned pure edit under a bounded lock,
// preserving symlinks, permissions and unrelated concurrent native changes.
// It never resolves an ambient config path or interprets native file contents.
func EditNativeConfigFile(label, path string, perm os.FileMode, plan func([]byte) (bool, []byte, error)) error {
	return editHarnessConfigFile(label, path, perm, plan, prepareAtomicWriteFile)
}
func editHarnessConfigFile(
	label string,
	configPath string,
	defaultPerm os.FileMode,
	plan func([]byte) (bool, []byte, error),
	prepare func(string, []byte, os.FileMode) (*atomicFileReplacement, error),
) error {
	nativeConfigEditMu.Lock()
	defer nativeConfigEditMu.Unlock()

	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return fmt.Errorf("create %s directory: %w", label, err)
	}
	fileLock := flock.New(configPath + ".tclaude.lock")
	lockCtx, cancelLock := context.WithTimeout(context.Background(), nativeConfigLockTimeout)
	defer cancelLock()
	locked, err := fileLock.TryLockContext(lockCtx, nativeConfigLockRetry)
	if err != nil {
		return fmt.Errorf("lock %s: %w", label, err)
	}
	if !locked {
		return fmt.Errorf("lock %s: timed out after %s", label, nativeConfigLockTimeout)
	}
	defer func() { _ = fileLock.Unlock() }()

	for attempt := 1; attempt <= nativeConfigEditMaxAttempts; attempt++ {
		target, err := atomicWriteTarget(configPath)
		if err != nil {
			return fmt.Errorf("resolve %s target: %w", label, err)
		}
		before, err := readFileAllowMissing(target)
		if err != nil {
			return fmt.Errorf("read %s: %w", label, err)
		}
		changed, out, err := plan(before)
		if err != nil {
			return err
		}
		if !changed {
			return nil
		}

		perm := defaultPerm
		if fi, statErr := os.Stat(target); statErr == nil {
			perm = fi.Mode().Perm()
		}
		replacement, err := prepare(target, out, perm)
		if err != nil {
			return err
		}

		// A non-tclaude writer cannot honor our advisory lock. Recheck both
		// the symlink target and bytes after the replacement has been fully
		// staged, then re-plan from the new state if either changed.
		currentTarget, err := atomicWriteTarget(configPath)
		if err != nil {
			replacement.discard()
			return fmt.Errorf("recheck %s target: %w", label, err)
		}
		current, err := readFileAllowMissing(currentTarget)
		if err != nil {
			replacement.discard()
			return fmt.Errorf("recheck %s: %w", label, err)
		}
		if currentTarget != target || !bytes.Equal(current, before) {
			replacement.discard()
			continue
		}
		if err := replacement.commit(); err != nil {
			replacement.discard()
			return err
		}
		return nil
	}
	return fmt.Errorf("%s kept changing during edit; retry later", label)
}

func readFileAllowMissing(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	return data, err
}

func atomicWriteTarget(path string) (string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return path, nil
		}
		return "", err
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return path, nil
	}
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve symlink %s: %w", path, err)
	}
	return target, nil
}

func prepareAtomicWriteFile(path string, data []byte, perm os.FileMode) (*atomicFileReplacement, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create temp config: %w", err)
	}
	tmpName := tmp.Name()
	replacement := &atomicFileReplacement{path: path, tmpName: tmpName, dir: dir}
	ok := false
	defer func() {
		if !ok {
			replacement.discard()
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("sync temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return nil, fmt.Errorf("chmod temp config: %w", err)
	}
	ok = true
	return replacement, nil
}

func (r *atomicFileReplacement) discard() {
	if r != nil && r.tmpName != "" {
		_ = os.Remove(r.tmpName)
	}
}

func (r *atomicFileReplacement) commit() error {
	if err := os.Rename(r.tmpName, r.path); err != nil {
		return fmt.Errorf("rename temp config into place: %w", err)
	}
	r.tmpName = ""
	// fsync the parent directory so the rename itself is durable across a hard
	// crash (the file content is already fsync'd above). Best-effort: a
	// directory that can't be opened/synced doesn't undo the successful write.
	if d, derr := os.Open(r.dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
