package agentd

import (
	"os/exec"
	"strings"
)

func readOneShotBootIdentity() string {
	data, err := exec.Command("sysctl", "-n", "kern.boottime").Output()
	if err != nil {
		return ""
	} // Missing evidence cannot authorize cleanup.
	return strings.TrimSpace(string(data))
}
