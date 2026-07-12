//go:build !linux && !darwin

package agentd

import (
	"fmt"
	"os"
	"runtime"
)

func mkdirAllNoFollow(_ string, _ os.FileMode) error {
	return fmt.Errorf("secure sandbox-profile directory creation is not supported on %s", runtime.GOOS)
}
