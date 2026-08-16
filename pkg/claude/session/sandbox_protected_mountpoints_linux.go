//go:build linux

package session

import (
	"fmt"
	"os"
)

func prepareTclaudeLayerProtectedMountpoints(paths []string) error {
	for _, path := range paths {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("prepare tclaude-layer protected mountpoint %q: %w", path, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("inspect tclaude-layer protected mountpoint %q: %w", path, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("tclaude-layer protected mountpoint %q is not a directory", path)
		}
	}
	return nil
}
