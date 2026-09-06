//go:build darwin

package agentd

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

func hostProcessInstance(pid int) (string, bool) {
	if pid <= 1 {
		return "", false
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	started := strings.Join(strings.Fields(string(out)), " ")
	if err != nil || started == "" {
		return "", false
	}
	return fmt.Sprintf("darwin-ps-start:%d:%s", pid, started), true
}
