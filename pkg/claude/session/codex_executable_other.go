//go:build !unix

package session

import (
	"fmt"
	"os"
)

// codexExecutableAccess falls back to the mode bits off unix, where there is no
// access(2) to ask. tclaude does not target these platforms; the file exists so
// the package still builds on them.
func codexExecutableAccess(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%q is not executable", path)
	}
	return nil
}
