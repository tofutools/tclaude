package harness

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

const (
	codexConfigEditMaxAttempts = 5
	codexConfigLockRetry       = 50 * time.Millisecond
)

var (
	codexConfigEditMu      sync.Mutex
	codexConfigLockTimeout = 10 * time.Second
)

// EditCodexConfigFile serializes every tclaude-owned edit of Codex's global
// config. The advisory lock coordinates separate tclaude processes; the
// stale-read check retries when Codex (which does not take our lock) changes
// the file while an edit is being planned or staged. A non-cooperating writer
// can never be made fully transactional with an advisory lock, so the temp
// file is completely written and fsync'd before the final check to reduce the
// remaining check-to-rename window to one local rename.
func EditCodexConfigFile(
	configPath string,
	defaultPerm os.FileMode,
	plan func([]byte) (bool, []byte, error),
) error {
	return editCodexConfigFile(configPath, defaultPerm, plan, prepareAtomicWriteFile)
}

func editCodexConfigFile(
	configPath string,
	defaultPerm os.FileMode,
	plan func([]byte) (bool, []byte, error),
	prepare func(string, []byte, os.FileMode) (*atomicFileReplacement, error),
) error {
	codexConfigEditMu.Lock()
	defer codexConfigEditMu.Unlock()

	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return fmt.Errorf("create Codex config directory: %w", err)
	}
	fileLock := flock.New(configPath + ".tclaude.lock")
	lockCtx, cancelLock := context.WithTimeout(context.Background(), codexConfigLockTimeout)
	defer cancelLock()
	locked, err := fileLock.TryLockContext(lockCtx, codexConfigLockRetry)
	if err != nil {
		return fmt.Errorf("lock Codex config: %w", err)
	}
	if !locked {
		return fmt.Errorf("lock Codex config: timed out after %s", codexConfigLockTimeout)
	}
	defer func() { _ = fileLock.Unlock() }()

	for attempt := 1; attempt <= codexConfigEditMaxAttempts; attempt++ {
		target, err := atomicWriteTarget(configPath)
		if err != nil {
			return fmt.Errorf("resolve Codex config target: %w", err)
		}
		before, err := readFileAllowMissing(target)
		if err != nil {
			return fmt.Errorf("read Codex config: %w", err)
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
			return fmt.Errorf("recheck Codex config target: %w", err)
		}
		current, err := readFileAllowMissing(currentTarget)
		if err != nil {
			replacement.discard()
			return fmt.Errorf("recheck Codex config: %w", err)
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
	return fmt.Errorf("codex config kept changing during edit; retry later")
}

func readFileAllowMissing(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	return data, err
}
