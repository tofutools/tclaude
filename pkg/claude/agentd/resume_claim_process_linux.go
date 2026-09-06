//go:build linux

package agentd

import (
	"fmt"
	"os"
	"strings"
)

func resumeClaimProcessCurrent(pid int, expected string) bool {
	if pid <= 1 || expected == "" {
		return false
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	fields := strings.Fields(string(b))
	return len(fields) > 21 && fields[21] == expected
}
