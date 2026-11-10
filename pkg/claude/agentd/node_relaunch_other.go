//go:build !linux && !darwin

package agentd

import (
	"fmt"
	"os"
)

func runRelaunch(string, string, []string) int {
	fmt.Fprintln(os.Stderr, "Restart failed: relaunch is not supported on this platform")
	return 1
}
