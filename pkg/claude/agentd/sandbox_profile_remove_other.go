//go:build !linux && !darwin

package agentd

import (
	"fmt"
	"runtime"
)

func removeDirAtNoFollow(_, _ string) (bool, error) {
	return false, fmt.Errorf("secure sandbox-profile directory removal is not supported on %s", runtime.GOOS)
}
