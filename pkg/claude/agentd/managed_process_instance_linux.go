//go:build linux

package agentd

import (
	"fmt"
)

func hostProcessInstance(pid int) (string, bool) {
	start, ok := debugProcStartTime(pid)
	if !ok || start == "" {
		return "", false
	}
	return fmt.Sprintf("linux-proc-start:%d:%s", pid, start), true
}
