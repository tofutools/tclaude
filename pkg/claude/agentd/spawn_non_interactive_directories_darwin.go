package agentd

import (
	"golang.org/x/sys/unix"
	"strings"
)

func readOneShotBootIdentity() string {
	// Use the immutable boot-session UUID, not wall-clock boot time, and
	// query the kernel directly rather than trusting the daemon's PATH.
	data, err := unix.Sysctl("kern.bootsessionuuid")
	if err != nil {
		return ""
	} // Missing evidence cannot authorize cleanup.
	return strings.TrimSpace(data)
}
