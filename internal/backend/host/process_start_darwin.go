//go:build darwin

package host

import (
	"fmt"
	"strconv"

	"golang.org/x/sys/unix"
)

func processStartToken(pid int) (string, error) {
	process, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", err
	}
	if process.Proc.P_pid != int32(pid) {
		return "", fmt.Errorf("kernel returned process %d for pid %d", process.Proc.P_pid, pid)
	}
	started := process.Proc.P_starttime
	return strconv.FormatInt(started.Sec, 10) + ":" + strconv.FormatInt(int64(started.Usec), 10), nil
}

func processParentPID(pid int) (int, error) {
	process, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, err
	}
	return int(process.Eproc.Ppid), nil
}
