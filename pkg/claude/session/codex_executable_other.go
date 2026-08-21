//go:build !unix

package session

import (
	"fmt"
	"io/fs"
	"os"
)

// codexExecutableAccess falls back to the mode bits off unix, where there is no
// access(2) to ask. tclaude does not target these platforms; the file exists so
// the package still builds on them. The refusal wraps fs.ErrPermission so the
// caller can classify it the way it classifies a real EACCES.
func codexExecutableAccess(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%q is not executable: %w", path, fs.ErrPermission)
	}
	return nil
}
