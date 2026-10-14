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
