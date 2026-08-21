//go:build !unix || aix

package session

import (
	"fmt"
	"io/fs"
	"os"
)

// codexExecutableAccess falls back to the mode bits where the faccessat call
// the unix build uses is unavailable: off unix entirely, and on AIX, for which
// x/sys defines no AT_EACCESS. tclaude targets neither; the file exists so the
// package's build constraints stay total. The refusal wraps fs.ErrPermission so
// the caller can classify it the way it classifies a real EACCES.
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
