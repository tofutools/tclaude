package host

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteProtectedFile atomically publishes sensitive provider material with
// owner-only permissions. The containing directory must already be protected.
func WriteProtectedFile(path string, value []byte) error {
	info, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("inspect protected file directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("protected file directory is not owner-only")
	}
	return replaceProtectedFile(path, value)
}

// ReadProtectedFile reads a bounded owner-only regular file without following
// a symlink at the final path.
func ReadProtectedFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect protected file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() > limit {
		return nil, fmt.Errorf("protected file is not an owner-only bounded regular file")
	}
	value, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read protected file: %w", err)
	}
	return value, nil
}
