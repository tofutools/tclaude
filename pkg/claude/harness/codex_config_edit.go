package harness

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/gofrs/flock"
)

const codexConfigEditMaxAttempts = 5

var codexConfigEditMu sync.Mutex

// EditCodexConfigFile serializes every tclaude-owned edit of Codex's global
// config. The advisory lock coordinates separate tclaude processes; the
// stale-read check retries when Codex (which does not take our lock) changes
// the file while an edit is being planned.
func EditCodexConfigFile(
	configPath string,
	defaultPerm os.FileMode,
	plan func([]byte) (bool, []byte, error),
) error {
	codexConfigEditMu.Lock()
	defer codexConfigEditMu.Unlock()

	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return fmt.Errorf("create Codex config directory: %w", err)
	}
	fileLock := flock.New(configPath + ".tclaude.lock")
	if err := fileLock.Lock(); err != nil {
		return fmt.Errorf("lock Codex config: %w", err)
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

		// A non-tclaude writer cannot honor our advisory lock. Recheck both
		// the symlink target and bytes immediately before replacement, and
		// re-plan from the new state if either changed.
		currentTarget, err := atomicWriteTarget(configPath)
		if err != nil {
			return fmt.Errorf("recheck Codex config target: %w", err)
		}
		current, err := readFileAllowMissing(currentTarget)
		if err != nil {
			return fmt.Errorf("recheck Codex config: %w", err)
		}
		if currentTarget != target || !bytes.Equal(current, before) {
			continue
		}

		perm := defaultPerm
		if fi, statErr := os.Stat(target); statErr == nil {
			perm = fi.Mode().Perm()
		}
		if err := atomicWriteFile(target, out, perm); err != nil {
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
