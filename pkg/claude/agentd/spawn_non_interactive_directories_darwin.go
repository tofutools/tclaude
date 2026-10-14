package agentd

import (
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
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

func readOneShotOwnerIdentity(pid int) (string, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			return "", os.ErrNotExist
		}
		return "", err
	}
	if info.Proc.P_pid != int32(pid) {
		return "", os.ErrNotExist
	}
	return fmt.Sprintf("%d:%d", info.Proc.P_starttime.Sec, info.Proc.P_starttime.Usec), nil
}
