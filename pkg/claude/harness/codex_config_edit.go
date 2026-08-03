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
	return editHarnessConfigFile("Codex config", configPath, defaultPerm, plan, prepare)
}

// EditCopilotConfigFile applies the same serialization to Copilot's
// COPILOT_HOME config.json, which needs it for the same reason and one more:
// two concurrent tclaude spawns pre-trusting DIFFERENT directories both
// read-modify-write the single `trustedFolders` array, so a plain
// read→plan→rename would silently drop one dir's entry and leave that pane
// parked on the modal it was seeded to clear. Read-modify-write under the lock
// with a post-stage recheck makes the two seeds compose instead.
//
// It shares the codex mutex and lock discipline deliberately: the concurrency
// hazard is per-FILE and the two files are distinct, so the only cost is that
// two edits of different harnesses' configs serialize against each other —
// which is microseconds on a path that runs at most once per spawned directory.
func EditCopilotConfigFile(
	configPath string,
	defaultPerm os.FileMode,
	plan func([]byte) (bool, []byte, error),
) error {
	return editHarnessConfigFile("Copilot config", configPath, defaultPerm, plan, prepareAtomicWriteFile)
}

// editHarnessConfigFile is the harness-agnostic mechanism behind both wrappers.
// label names the file in every error, so a refusal an operator reads still
// says which harness's configuration is at fault.
func editHarnessConfigFile(
	label string,
	configPath string,
	defaultPerm os.FileMode,
	plan func([]byte) (bool, []byte, error),
	prepare func(string, []byte, os.FileMode) (*atomicFileReplacement, error),
) error {
	codexConfigEditMu.Lock()
	defer codexConfigEditMu.Unlock()

	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return fmt.Errorf("create %s directory: %w", label, err)
	}
	fileLock := flock.New(configPath + ".tclaude.lock")
	lockCtx, cancelLock := context.WithTimeout(context.Background(), codexConfigLockTimeout)
	defer cancelLock()
	locked, err := fileLock.TryLockContext(lockCtx, codexConfigLockRetry)
	if err != nil {
		return fmt.Errorf("lock %s: %w", label, err)
	}
	if !locked {
		return fmt.Errorf("lock %s: timed out after %s", label, codexConfigLockTimeout)
	}
	defer func() { _ = fileLock.Unlock() }()

	for attempt := 1; attempt <= codexConfigEditMaxAttempts; attempt++ {
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
