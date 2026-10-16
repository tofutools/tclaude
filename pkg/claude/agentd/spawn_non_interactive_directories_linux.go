package agentd

import (
	"os"
	"strings"
)

func readOneShotBootIdentity() string {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	} // Missing evidence cannot authorize cleanup.
	return strings.TrimSpace(string(data))
}

func readOneShotOwnerIdentity(pid int) (string, error) {
	return oneShotProcessStartTime(pid)
}
